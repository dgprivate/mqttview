package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dgprivate/mqttview/internal/store"
)

func TestAStoredHashThatCannotBeParsedNeverVerifies(t *testing.T) {
	good, err := HashPassword(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(good, "$")
	with := func(i int, v string) string {
		p := append([]string(nil), parts...)
		p[i] = v
		return strings.Join(p, "$")
	}

	// Each of these is one field of a real hash damaged. Whatever the damage,
	// the answer has to be "no", never a match against a guessable key.
	for name, bad := range map[string]string{
		"a version that is not a number":  with(2, "v=x"),
		"an argon2 version we do not run": with(2, "v=16"),
		"parameters that do not parse":    with(3, "m=x"),
		"a salt that is not base64":       with(4, "###"),
		"a key that is not base64":        with(5, "###"),
	} {
		ok, err := VerifyPassword(bad, testPassword)
		if ok || err == nil {
			t.Errorf("%s: verified=%v err=%v", name, ok, err)
		}
	}
}

func TestPasswordsHaveABoundedLength(t *testing.T) {
	// Bounded, because argon2 over an arbitrarily long input is a way to make
	// the server burn CPU on every unauthenticated sign-in attempt.
	if _, err := HashPassword(strings.Repeat("a", 1025)); err == nil {
		t.Error("a 1025-character password was accepted")
	}
	if _, err := HashPassword(strings.Repeat("a", 1024)); err != nil {
		t.Errorf("a 1024-character password was refused: %v", err)
	}
}

func TestAnAccountWithACorruptHashCannotSignIn(t *testing.T) {
	svc, db := newTestService(t)
	if _, _, err := svc.BootstrapAdmin("person@example.com", testPassword); err != nil {
		t.Fatal(err)
	}
	u, _ := db.GetUserByEmail("person@example.com")
	if err := db.SetPasswordHash(u.ID, "$argon2id$v=19$m=x$garbage$garbage"); err != nil {
		t.Fatal(err)
	}

	// The same answer as a wrong password: neither a way in nor a different
	// error that tells somebody the account is in a strange state.
	if _, err := svc.Login(context.Background(), "person@example.com", testPassword, "10.0.0.1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("got %v, want ErrInvalidCredentials", err)
	}
}

func TestBootstrapAdminInventsAnAddressWhenGivenNone(t *testing.T) {
	svc, db := newTestService(t)

	created, _, err := svc.BootstrapAdmin("", testPassword)
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	// Somebody has to be able to sign in to a fresh install.
	if _, err := svc.Login(context.Background(), "admin@localhost", testPassword, "10.0.0.1"); err != nil {
		t.Fatalf("the invented administrator cannot sign in: %v", err)
	}
	if u, _ := db.GetUserByEmail("admin@localhost"); u.Role != store.RoleAdmin {
		t.Errorf("role = %q", u.Role)
	}
}

func TestARoleGuardWithNobodySignedInRefuses(t *testing.T) {
	svc, _ := newTestService(t)

	// RequireRole normally sits behind Middleware; mounted without it, by
	// mistake, it must refuse rather than wave the request through.
	reached := false
	h := svc.RequireRole(store.RoleViewer)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if reached || rec.Code != http.StatusUnauthorized {
		t.Errorf("reached=%v status=%d", reached, rec.Code)
	}
}

func TestAProxyWithASingleForwardedAddressIsBelievedOnlyWhenTrusted(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "192.0.2.1:5000"
	r.Header.Set("X-Forwarded-For", " 203.0.113.7 ")

	trusting, _ := newTestService(t)
	trusting.cfg.TrustProxyHeaders = true
	if got := trusting.ClientIP(r); got != "203.0.113.7" {
		t.Errorf("trusted proxy: ClientIP = %q", got)
	}

	sceptical, _ := newTestService(t)
	if got := sceptical.ClientIP(r); got != "192.0.2.1" {
		t.Errorf("untrusted header was believed: ClientIP = %q", got)
	}
}

func TestAnAbsurdUserAgentIsStoredTruncated(t *testing.T) {
	svc, db := newTestService(t)
	if _, _, err := svc.BootstrapAdmin("person@example.com", testPassword); err != nil {
		t.Fatal(err)
	}
	u, _ := db.GetUserByEmail("person@example.com")

	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("User-Agent", strings.Repeat("x", 10_000))
	rec := httptest.NewRecorder()
	if err := svc.IssueSession(rec, r, u); err != nil {
		t.Fatalf("a long User-Agent prevented sign-in: %v", err)
	}

	var token string
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookie {
			token = c.Value
		}
	}
	sess, _, err := db.SessionUser(hashToken(token))
	if err != nil {
		t.Fatalf("the issued session is not usable: %v", err)
	}
	if len(sess.UserAgent) != 255 {
		t.Errorf("stored user agent length = %d, want 255", len(sess.UserAgent))
	}
}

func TestLogoutClearsTheCookiesEvenWhenTheDatabaseIsGone(t *testing.T) {
	svc, db := newTestService(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.AddCookie(&http.Cookie{Name: SessionCookie, Value: "token"})
	rec := httptest.NewRecorder()
	if err := svc.Logout(rec, r); err != nil {
		t.Fatal(err)
	}

	// The browser must stop presenting the session whatever happened to the
	// row behind it.
	cleared := map[string]bool{}
	for _, c := range rec.Result().Cookies() {
		if c.MaxAge < 0 {
			cleared[c.Name] = true
		}
	}
	if !cleared[SessionCookie] || !cleared[CSRFCookie] {
		t.Errorf("cookies cleared: %v", cleared)
	}
}

func TestStaleLimiterEntriesAreForgotten(t *testing.T) {
	l := newAttemptLimiter(1, time.Minute)
	for i := 0; i < 100; i++ {
		l.allow(string(rune('a' + i%26)))
	}
	for _, e := range l.entries {
		e.start = time.Now().Add(-2 * time.Minute)
	}

	// A distributed guessing run leaves one entry per address it tried; once
	// the window has passed they must not accumulate forever.
	if !l.allow("fresh") {
		t.Fatal("a first attempt was refused")
	}
	if len(l.entries) != 1 {
		t.Errorf("%d entries survived their window", len(l.entries))
	}
}
