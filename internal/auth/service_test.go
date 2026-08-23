package auth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mqttview/mqttview/internal/config"
	"github.com/mqttview/mqttview/internal/secrets"
	"github.com/mqttview/mqttview/internal/store"
)

const testPassword = "correct-horse-battery-staple"

func newTestService(t *testing.T, mutate ...func(*config.Config)) (*Service, *store.Store) {
	t.Helper()

	dir := t.TempDir()
	key, err := secrets.LoadOrCreateKey("", dir)
	if err != nil {
		t.Fatal(err)
	}
	box, err := secrets.New(key)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(dir, "test.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	cfg := config.Default()
	cfg.DataDir = dir
	cfg.BaseURL = "https://mqttview.example.com"
	for _, m := range mutate {
		m(&cfg)
	}
	return New(db, cfg, box, slog.New(slog.NewTextHandler(io.Discard, nil))), db
}

func TestPasswordHashingRoundTrip(t *testing.T) {
	hash, err := HashPassword(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	// argon2id, so the hash is long, salted and different every time.
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Fatalf("hash = %q, want an argon2id encoding", hash)
	}
	second, _ := HashPassword(testPassword)
	if hash == second {
		t.Fatal("the same password hashed to the same string, so it is unsalted")
	}

	ok, err := VerifyPassword(hash, testPassword)
	if err != nil || !ok {
		t.Fatalf("the right password did not verify: %v %v", ok, err)
	}
	ok, err = VerifyPassword(hash, "wrong")
	if err != nil || ok {
		t.Fatalf("the wrong password verified: %v %v", ok, err)
	}
}

func TestVerifyPasswordRejectsUnusableHashes(t *testing.T) {
	for _, bad := range []string{"", "plaintext", "$argon2id$broken", "$argon2id$v=19$m=1,t=1,p=1$###$###"} {
		if _, err := VerifyPassword(bad, testPassword); err == nil {
			t.Errorf("hash %q was treated as usable", bad)
		}
	}
}

func TestBootstrapAdminOnlyRunsOnAnEmptyDatabase(t *testing.T) {
	svc, db := newTestService(t)

	created, generated, err := svc.BootstrapAdmin("first@example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	if !created || generated == "" {
		t.Fatalf("nothing was created: created=%v generated=%q", created, generated)
	}
	u, err := db.GetUserByEmail("first@example.com")
	if err != nil || u.Role != store.RoleAdmin {
		t.Fatalf("the bootstrap account is not an admin: %+v %v", u, err)
	}

	// A second call must do nothing: it runs on every start, and creating an
	// account each time would be a back door.
	created, _, err = svc.BootstrapAdmin("second@example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("a second administrator was bootstrapped over an existing database")
	}
}

func TestLoginPathways(t *testing.T) {
	svc, db := newTestService(t)
	if _, _, err := svc.BootstrapAdmin("person@example.com", testPassword); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if _, err := svc.Login(ctx, "person@example.com", testPassword, "10.0.0.1"); err != nil {
		t.Fatalf("a correct password was refused: %v", err)
	}

	// Both of these must look identical to a caller, or the response
	// distinguishes "no such account" from "wrong password".
	for _, tc := range []struct{ email, password string }{
		{"person@example.com", "wrong"},
		{"nobody@example.com", testPassword},
	} {
		_, err := svc.Login(ctx, tc.email, tc.password, "10.0.0.2")
		if !errors.Is(err, ErrInvalidCredentials) {
			t.Errorf("%s gave %v, want ErrInvalidCredentials", tc.email, err)
		}
	}

	u, _ := db.GetUserByEmail("person@example.com")
	u.Disabled = true
	if err := db.UpdateUser(u); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Login(ctx, "person@example.com", testPassword, "10.0.0.3"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("a disabled account signed in: %v", err)
	}
}

func TestLocalLoginCanBeTurnedOff(t *testing.T) {
	svc, _ := newTestService(t, func(c *config.Config) {
		c.Auth.AllowLocal = false
	})
	if _, _, err := svc.BootstrapAdmin("person@example.com", testPassword); err != nil {
		t.Fatal(err)
	}

	_, err := svc.Login(context.Background(), "person@example.com", testPassword, "10.0.0.1")
	if !errors.Is(err, ErrLocalLoginDisabled) {
		t.Fatalf("got %v, want ErrLocalLoginDisabled", err)
	}
}

func TestRepeatedFailuresAreRateLimited(t *testing.T) {
	svc, _ := newTestService(t)
	if _, _, err := svc.BootstrapAdmin("person@example.com", testPassword); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	var limited bool
	for i := 0; i < 30; i++ {
		_, err := svc.Login(ctx, "person@example.com", "wrong", "10.0.0.9")
		if err != nil && !errors.Is(err, ErrInvalidCredentials) {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("guessing was never throttled")
	}

	// The limit is per address, so one attacker cannot lock everybody out.
	if _, err := svc.Login(ctx, "person@example.com", testPassword, "10.0.0.10"); err != nil {
		t.Fatalf("a different address was caught by the limit: %v", err)
	}
}

func TestSessionIssueAuthenticateAndLogout(t *testing.T) {
	svc, db := newTestService(t)
	if _, _, err := svc.BootstrapAdmin("person@example.com", testPassword); err != nil {
		t.Fatal(err)
	}
	u, _ := db.GetUserByEmail("person@example.com")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
	if err := svc.IssueSession(rec, req, u); err != nil {
		t.Fatal(err)
	}

	var session, csrf *http.Cookie
	for _, c := range rec.Result().Cookies() {
		switch c.Name {
		case SessionCookie:
			session = c
		case CSRFCookie:
			csrf = c
		}
	}
	if session == nil || csrf == nil {
		t.Fatal("login did not set both cookies")
	}
	// The session cookie must be unreadable by script; the CSRF one must be
	// readable, because the SPA has to echo it back.
	if !session.HttpOnly {
		t.Error("the session cookie is not HttpOnly")
	}
	if csrf.HttpOnly {
		t.Error("the CSRF cookie is HttpOnly, so the SPA cannot echo it")
	}
	if !session.Secure {
		t.Error("the session cookie is not Secure even though base_url is https")
	}

	authedReq := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	authedReq.AddCookie(session)
	got, err := svc.Authenticate(authedReq)
	if err != nil || got.ID != u.ID {
		t.Fatalf("Authenticate: %+v %v", got, err)
	}

	// The stored token is a hash, so a database leak does not yield a usable
	// cookie.
	if _, _, err := db.SessionUser(session.Value); err == nil {
		t.Error("the raw cookie value resolved a session, so it is stored unhashed")
	}

	outRec := httptest.NewRecorder()
	if err := svc.Logout(outRec, authedReq); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(authedReq); err == nil {
		t.Error("a logged-out session still authenticates")
	}
}

func TestAuthenticateRejectsRubbish(t *testing.T) {
	svc, _ := newTestService(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if _, err := svc.Authenticate(req); err == nil {
		t.Error("a request with no cookie authenticated")
	}
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: "not-a-real-token"})
	if _, err := svc.Authenticate(req); err == nil {
		t.Error("an invented token authenticated")
	}
}

func TestMiddlewareAndRoleGuards(t *testing.T) {
	svc, db := newTestService(t)
	if _, _, err := svc.BootstrapAdmin("person@example.com", testPassword); err != nil {
		t.Fatal(err)
	}
	u, _ := db.GetUserByEmail("person@example.com")

	rec := httptest.NewRecorder()
	if err := svc.IssueSession(rec, httptest.NewRequest(http.MethodPost, "/", nil), u); err != nil {
		t.Fatal(err)
	}
	cookies := rec.Result().Cookies()

	reached := false
	handler := svc.Middleware(svc.RequireRole(store.RoleAdmin)(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			reached = true
			if got, ok := UserFrom(r.Context()); !ok || got.ID != u.ID {
				t.Error("the handler did not receive the user")
			}
			w.WriteHeader(http.StatusOK)
		})))

	req := httptest.NewRequest(http.MethodGet, "/api/users", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, req)
	if !reached || out.Code != http.StatusOK {
		t.Fatalf("an admin was refused: status %d", out.Code)
	}

	// Without a session the middleware stops it before the role guard.
	anon := httptest.NewRecorder()
	handler.ServeHTTP(anon, httptest.NewRequest(http.MethodGet, "/api/users", nil))
	if anon.Code != http.StatusUnauthorized {
		t.Errorf("an anonymous request got %d, want 401", anon.Code)
	}

	// A viewer must not reach an admin route.
	u.Role = store.RoleViewer
	if err := db.UpdateUser(u); err != nil {
		t.Fatal(err)
	}
	viewer := httptest.NewRecorder()
	handler.ServeHTTP(viewer, req)
	if viewer.Code != http.StatusForbidden {
		t.Errorf("a viewer got %d on an admin route, want 403", viewer.Code)
	}
}

func TestCSRFOnlyGuardsWrites(t *testing.T) {
	svc, _ := newTestService(t)
	handler := svc.CSRF(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Reads carry no token by design: the SAML assertion post and every GET
	// would otherwise need one.
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(method, "/", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s was refused: %d", method, rec.Code)
		}
	}

	tests := []struct {
		name          string
		cookie        string
		header        string
		wantForbidden bool
	}{
		{"matching", "token", "token", false},
		{"missing header", "token", "", true},
		{"missing cookie", "", "token", true},
		{"mismatched", "token", "different", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			if tt.cookie != "" {
				req.AddCookie(&http.Cookie{Name: CSRFCookie, Value: tt.cookie})
			}
			if tt.header != "" {
				req.Header.Set(CSRFHeader, tt.header)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			forbidden := rec.Code == http.StatusForbidden
			if forbidden != tt.wantForbidden {
				t.Errorf("status %d, wantForbidden=%v", rec.Code, tt.wantForbidden)
			}
		})
	}
}

func TestSanitizeNextKeepsRedirectsOnThisOrigin(t *testing.T) {
	// An open redirect after login is how a phishing page borrows the domain.
	for _, bad := range []string{
		"https://evil.example.net", "//evil.example.net", "http://evil", "javascript:alert(1)", "",
	} {
		if got := sanitizeNext(bad); got != "/" {
			t.Errorf("sanitizeNext(%q) = %q, want /", bad, got)
		}
	}
	for _, good := range []string{"/", "/connections", "/connections/abc?tab=1"} {
		if got := sanitizeNext(good); got != good {
			t.Errorf("sanitizeNext(%q) = %q", good, got)
		}
	}
}

func TestCheckDomain(t *testing.T) {
	// No list means any domain the provider vouches for.
	if err := checkDomain("person@anywhere.com", nil); err != nil {
		t.Errorf("an empty allow-list refused a domain: %v", err)
	}
	if err := checkDomain("person@example.com", []string{"Example.COM", "other.com"}); err != nil {
		t.Errorf("the comparison is case sensitive: %v", err)
	}
	err := checkDomain("person@evil.net", []string{"example.com"})
	if err == nil || !strings.Contains(err.Error(), "evil.net") {
		t.Errorf("error = %v, want it to name the refused domain", err)
	}
}

func TestResolveFederatedUserLinksAndCreates(t *testing.T) {
	svc, db := newTestService(t, func(c *config.Config) { c.Auth.AllowSignup = true })

	// A brand new identity creates an account, and the admin list decides the
	// role on that first sign-in.
	u, err := svc.resolveFederatedUser("idp", "subject-1", "boss@example.com", "Boss",
		[]string{"boss@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if u.Role != store.RoleAdmin || u.Provider != "idp" {
		t.Fatalf("created user: %+v", u)
	}

	// The same subject returns the same account rather than a second one.
	again, err := svc.resolveFederatedUser("idp", "subject-1", "boss@example.com", "Boss", nil)
	if err != nil || again.ID != u.ID {
		t.Fatalf("a repeat sign-in created a new account: %+v %v", again, err)
	}

	// An existing local account with the same verified address is the same
	// person, so it is linked rather than duplicated.
	local, err := db.CreateUser(store.User{
		ID: "local-1", Email: "person@example.com", Role: store.RoleOperator, PasswordHash: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	linked, err := svc.resolveFederatedUser("idp", "subject-2", "person@example.com", "Person", nil)
	if err != nil {
		t.Fatal(err)
	}
	if linked.ID != local.ID {
		t.Fatalf("linking created a duplicate: %s vs %s", linked.ID, local.ID)
	}
	if linked.Role != store.RoleOperator {
		t.Errorf("linking changed the role to %q", linked.Role)
	}
}

func TestResolveFederatedUserRefusesWhenSignupIsOff(t *testing.T) {
	svc, _ := newTestService(t, func(c *config.Config) { c.Auth.AllowSignup = false })

	_, err := svc.resolveFederatedUser("idp", "subject-1", "stranger@example.com", "", nil)
	if err == nil || !strings.Contains(err.Error(), "ask an administrator") {
		t.Fatalf("error = %v, want a refusal that says what to do", err)
	}
}

func TestResolveFederatedUserRefusesADisabledAccount(t *testing.T) {
	svc, db := newTestService(t, func(c *config.Config) { c.Auth.AllowSignup = true })

	u, err := svc.resolveFederatedUser("idp", "s1", "person@example.com", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	u.Disabled = true
	if err := db.UpdateUser(u); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.resolveFederatedUser("idp", "s1", "person@example.com", "", nil); err == nil {
		t.Fatal("a disabled account signed in through SSO")
	}
}

func TestAnAddressAlreadyLinkedElsewhereIsRefused(t *testing.T) {
	svc, _ := newTestService(t, func(c *config.Config) { c.Auth.AllowSignup = true })

	if _, err := svc.resolveFederatedUser("idp", "subject-1", "person@example.com", "", nil); err != nil {
		t.Fatal(err)
	}
	// A second identity claiming the same address must not take it over.
	_, err := svc.resolveFederatedUser("idp", "subject-2", "person@example.com", "", nil)
	if err == nil || !strings.Contains(err.Error(), "already linked") {
		t.Fatalf("error = %v, want a refusal", err)
	}
}

func TestProviderInfosListsOnlyEnabledOIDCProviders(t *testing.T) {
	svc, _ := newTestService(t, func(c *config.Config) {
		c.Auth.Providers = map[string]config.ProviderConfig{
			"google": {Enabled: true, DisplayName: "Google", Issuer: "https://i", ClientID: "a", ClientSecret: "b"},
			"old":    {Enabled: false, DisplayName: "Old"},
			"bare":   {Enabled: true, Issuer: "https://i", ClientID: "a", ClientSecret: "b"},
		}
	})

	infos := svc.ProviderInfos()
	if len(infos) != 2 {
		t.Fatalf("got %d providers: %+v", len(infos), infos)
	}
	for _, i := range infos {
		if i.ID == "old" {
			t.Error("a disabled provider was offered")
		}
		if i.DisplayName == "" {
			t.Errorf("provider %q has no label for its button", i.ID)
		}
	}
}

func TestPurgeSessionsRemovesTheExpiredOnes(t *testing.T) {
	svc, db := newTestService(t)
	if _, _, err := svc.BootstrapAdmin("person@example.com", testPassword); err != nil {
		t.Fatal(err)
	}
	u, _ := db.GetUserByEmail("person@example.com")

	if err := db.CreateSession(store.Session{
		ID: "old", UserID: u.ID,
		CreatedAt: time.Now().Add(-2 * time.Hour), ExpiresAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	svc.PurgeSessions()

	if _, _, err := db.SessionUser("old"); err == nil {
		t.Error("an expired session survived the sweep")
	}
}

func TestConfigIsExposedForTheLoginPage(t *testing.T) {
	svc, _ := newTestService(t)
	if !svc.Config().AllowLocal {
		t.Error("Config() did not carry the auth settings")
	}
}

// TestEveryPathReportsAFailingDatabase closes the store underneath the service
// and calls everything that touches it.
//
// These are the `if err != nil` branches after a query — the ones that only run
// when the disk fills or the volume goes away. The property is that each one
// returns the failure rather than carrying on with a zero value, because
// carrying on here means signing somebody in.
func TestEveryPathReportsAFailingDatabase(t *testing.T) {
	svc, db := newTestService(t)
	if _, _, err := svc.BootstrapAdmin("person@example.com", testPassword); err != nil {
		t.Fatal(err)
	}
	u, err := db.GetUserByEmail("person@example.com")
	if err != nil {
		t.Fatal(err)
	}

	// Enrol first, so the two-factor paths have something to work on.
	enrolment, err := svc.BeginTwoFactorEnrolment(u)
	if err != nil {
		t.Fatal(err)
	}
	u, _ = db.GetUser(u.ID)
	code, _ := TOTPCodeAt(enrolment.Secret, time.Now())
	if _, err := svc.ConfirmTwoFactorEnrolment(u, code); err != nil {
		t.Fatal(err)
	}
	u, _ = db.GetUser(u.ID)

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	checks := map[string]func() error{
		"Login": func() error {
			_, err := svc.Login(ctx, u.Email, testPassword, "10.0.0.1")
			return err
		},
		"CompleteLogin": func() error {
			_, err := svc.CompleteLogin(ctx, u.Email, testPassword, "000000", "10.0.0.2")
			return err
		},
		"TwoFactorStatusFor": func() error {
			_, err := svc.TwoFactorStatusFor(u)
			return err
		},
		"BeginTwoFactorEnrolment": func() error {
			fresh := u
			fresh.TOTPSecretEnc = ""
			fresh.TOTPConfirmedAt = nil
			_, err := svc.BeginTwoFactorEnrolment(fresh)
			return err
		},
		"RegenerateRecoveryCodes": func() error {
			_, err := svc.RegenerateRecoveryCodes(u)
			return err
		},
		"DisableTwoFactor": func() error { return svc.DisableTwoFactor(u) },
		"VerifySecondFactor": func() error {
			return svc.VerifySecondFactor(ctx, u, "aaaa-bbbb-cccc")
		},
		"resolveFederatedUser": func() error {
			_, err := svc.resolveFederatedUser("idp", "s", "x@example.com", "", nil)
			return err
		},
		"IssueSession": func() error {
			return svc.IssueSession(httptest.NewRecorder(),
				httptest.NewRequest(http.MethodPost, "/", nil), u)
		},
		"BootstrapAdmin": func() error {
			_, _, err := svc.BootstrapAdmin("another@example.com", testPassword)
			return err
		},
	}

	for name, fn := range checks {
		if err := fn(); err == nil {
			t.Errorf("%s succeeded with no database", name)
		}
	}

	// The ones that only log: they must return rather than panic.
	svc.PurgeSessions()
	if _, err := svc.Authenticate(httptest.NewRequest(http.MethodGet, "/", nil)); err == nil {
		t.Error("Authenticate succeeded with no database")
	}
}

func TestConfirmingTwoFactorNeedsADecryptableSecret(t *testing.T) {
	svc, db := newTestService(t)
	if _, _, err := svc.BootstrapAdmin("person@example.com", testPassword); err != nil {
		t.Fatal(err)
	}
	u, _ := db.GetUserByEmail("person@example.com")

	// A secret that will not decrypt — the encryption key was rotated, say —
	// must be reported rather than silently treated as a wrong code.
	u.TOTPSecretEnc = "not-ciphertext-from-this-key"
	if _, err := svc.ConfirmTwoFactorEnrolment(u, "123456"); err == nil {
		t.Error("an undecryptable secret was accepted")
	}

	u.TOTPConfirmedAt = &time.Time{}
	*u.TOTPConfirmedAt = time.Now()
	if err := svc.VerifySecondFactor(context.Background(), u, "123456"); err == nil {
		t.Error("an undecryptable secret verified a code")
	}
}
