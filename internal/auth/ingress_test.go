package auth

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mqttview/mqttview/internal/config"
	"github.com/mqttview/mqttview/internal/secrets"
	"github.com/mqttview/mqttview/internal/store"
)

// Home Assistant mode trades mqttview's own sign-in for the Supervisor's word.
// That trade is only sound while one thing holds: that the request really came
// from the Supervisor. Everything here is about that one thing.

func newIngressService(t *testing.T, mutate ...func(*config.IngressConfig)) (*Service, *store.Store) {
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
	cfg.Auth.Mode = config.ModeIngress
	for _, m := range mutate {
		m(&cfg.Auth.Ingress)
	}

	return New(db, cfg, box, slog.New(slog.NewTextHandler(io.Discard, nil))), db
}

// ingressRequest is what the Supervisor forwards.
func ingressRequest(from string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	r.RemoteAddr = from + ":54321"
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestOnlyTheSupervisorCanSayWhoYouAre(t *testing.T) {
	s, _ := newIngressService(t)

	// The same headers, from somewhere else. If this were accepted, anyone who
	// could reach the port would be an administrator by typing a header.
	_, err := s.AuthenticateIngress(ingressRequest("192.168.1.50", map[string]string{
		IngressUserIDHeader:   "abc123",
		IngressUserNameHeader: "attacker",
	}))
	if err == nil {
		t.Fatal("identity headers from an untrusted address were believed")
	}

	if _, err := s.AuthenticateIngress(ingressRequest("172.30.32.2", map[string]string{
		IngressUserIDHeader:   "abc123",
		IngressUserNameHeader: "dean",
	})); err != nil {
		t.Fatalf("a request from the Supervisor was refused: %v", err)
	}
}

func TestTrustedProxiesAcceptsACIDR(t *testing.T) {
	s, _ := newIngressService(t, func(c *config.IngressConfig) {
		c.TrustedProxies = []string{"172.30.32.0/23"}
	})

	if _, err := s.AuthenticateIngress(ingressRequest("172.30.33.7", map[string]string{
		IngressUserIDHeader: "abc123",
	})); err != nil {
		t.Errorf("an address inside the range was refused: %v", err)
	}
	if _, err := s.AuthenticateIngress(ingressRequest("172.30.34.7", map[string]string{
		IngressUserIDHeader: "abc123",
	})); err == nil {
		t.Error("an address outside the range was accepted")
	}
}

func TestAnAccountIsCreatedOnFirstSightAndReusedAfter(t *testing.T) {
	s, db := newIngressService(t)

	first, err := s.AuthenticateIngress(ingressRequest("172.30.32.2", map[string]string{
		IngressUserIDHeader:      "abc123",
		IngressUserNameHeader:    "dean",
		IngressUserDisplayHeader: "Dean",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if first.Name != "Dean" || first.Provider != ingressProvider {
		t.Errorf("account = %+v", first)
	}
	if first.Role != store.RoleOperator {
		t.Errorf("role = %q, want the configured default", first.Role)
	}

	// The same person on their next page load is the same account, not a
	// second one: a user list that grows by one per refresh is unusable.
	second, err := s.AuthenticateIngress(ingressRequest("172.30.32.2", map[string]string{
		IngressUserIDHeader: "abc123",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Errorf("a second request created another account: %q then %q", first.ID, second.ID)
	}

	users, err := db.ListUsers()
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 {
		t.Fatalf("the database holds %d users", len(users))
	}
}

func TestRenamingSomebodyInHomeAssistantKeepsTheirAccount(t *testing.T) {
	s, _ := newIngressService(t)

	before, err := s.AuthenticateIngress(ingressRequest("172.30.32.2", map[string]string{
		IngressUserIDHeader:   "abc123",
		IngressUserNameHeader: "dean",
	}))
	if err != nil {
		t.Fatal(err)
	}

	// The account is keyed on the stable id, so a rename does not strand it
	// along with whatever settings were attached to it.
	after, err := s.AuthenticateIngress(ingressRequest("172.30.32.2", map[string]string{
		IngressUserIDHeader:   "abc123",
		IngressUserNameHeader: "dean-renamed",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if after.ID != before.ID {
		t.Fatalf("a rename produced a new account: %q then %q", before.ID, after.ID)
	}
}

func TestAdminUsersAreGrantedAndRevokedOnTheNextRequest(t *testing.T) {
	s, _ := newIngressService(t, func(c *config.IngressConfig) {
		c.AdminUsers = []string{"dean"}
	})

	u, err := s.AuthenticateIngress(ingressRequest("172.30.32.2", map[string]string{
		IngressUserIDHeader:   "abc123",
		IngressUserNameHeader: "dean",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if u.Role != store.RoleAdmin {
		t.Fatalf("role = %q, want admin", u.Role)
	}

	// Taking somebody out of admin_users has to take effect, or the list is
	// something that can only ever be added to.
	s.cfg.Ingress.AdminUsers = nil
	u, err = s.AuthenticateIngress(ingressRequest("172.30.32.2", map[string]string{
		IngressUserIDHeader:   "abc123",
		IngressUserNameHeader: "dean",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if u.Role == store.RoleAdmin {
		t.Error("the admin role survived removal from admin_users")
	}
}

func TestASupervisorThatSendsNoIdentity(t *testing.T) {
	s, _ := newIngressService(t)

	// Refused rather than waved through: "no identity" must never become "some
	// identity" by default.
	_, err := s.AuthenticateIngress(ingressRequest("172.30.32.2", nil))
	if err == nil {
		t.Fatal("a request with no identity headers was accepted")
	}

	// Unless the operator has said what to do about it, which is the escape
	// hatch for a Supervisor too old to send them.
	s.cfg.Ingress.FallbackUser = "home"
	u, err := s.AuthenticateIngress(ingressRequest("172.30.32.2", nil))
	if err != nil {
		t.Fatalf("with a fallback configured: %v", err)
	}
	if u.Name != "home" {
		t.Errorf("account = %+v", u)
	}
}

func TestAUsernameAloneIsEnoughToIdentifySomebody(t *testing.T) {
	s, _ := newIngressService(t)

	u, err := s.AuthenticateIngress(ingressRequest("172.30.32.2", map[string]string{
		IngressUserNameHeader: "dean",
	}))
	if err != nil {
		t.Fatalf("a username without an id was refused: %v", err)
	}
	if u.ProviderSubject != "dean" {
		t.Errorf("subject = %q", u.ProviderSubject)
	}
}

func TestADisabledAccountStaysDisabled(t *testing.T) {
	s, db := newIngressService(t)

	u, err := s.AuthenticateIngress(ingressRequest("172.30.32.2", map[string]string{
		IngressUserIDHeader: "abc123",
	}))
	if err != nil {
		t.Fatal(err)
	}

	u.Disabled = true
	if err := db.UpdateUser(u); err != nil {
		t.Fatal(err)
	}

	// Home Assistant does not know mqttview disabled this account, so it will
	// keep forwarding the person. mqttview has to keep refusing.
	if _, err := s.AuthenticateIngress(ingressRequest("172.30.32.2", map[string]string{
		IngressUserIDHeader: "abc123",
	})); err == nil {
		t.Fatal("a disabled account was let back in")
	}
}

func TestTheAddressGivenToAnAccountIsUsable(t *testing.T) {
	s, _ := newIngressService(t)

	// Home Assistant has no email addresses, so one is synthesised — and it
	// still has to be a valid address, because the store validates one and the
	// account would otherwise fail to create.
	u, err := s.AuthenticateIngress(ingressRequest("172.30.32.2", map[string]string{
		IngressUserNameHeader: "Dean Gostiša!",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !store.ValidEmail(u.Email) {
		t.Errorf("the generated address %q is not valid", u.Email)
	}
}

func TestIngressMiddlewareRefusesWithoutPrompting(t *testing.T) {
	s, _ := newIngressService(t)

	w := httptest.NewRecorder()
	s.IngressMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the handler ran for an untrusted request")
	})).ServeHTTP(w, ingressRequest("10.0.0.1", nil))

	// 403 rather than 401: there is no credential that would change the
	// answer, so asking for one would be a lie.
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestIngressCSRFIssuesTheTokenItThenRequires(t *testing.T) {
	s, _ := newIngressService(t)

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Nobody signs in here, so a read is what has to set the cookie; without
	// that, no write could ever pass the check.
	w := httptest.NewRecorder()
	s.IngressCSRF(next).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/auth/me", nil))

	var token string
	for _, c := range w.Result().Cookies() {
		if c.Name == CSRFCookie {
			token = c.Value
		}
	}
	if token == "" {
		t.Fatal("a read did not issue a CSRF cookie")
	}

	write := httptest.NewRequest(http.MethodPost, "/api/connections", nil)
	write.AddCookie(&http.Cookie{Name: CSRFCookie, Value: token})
	write.Header.Set(CSRFHeader, token)

	w = httptest.NewRecorder()
	s.IngressCSRF(next).ServeHTTP(w, write)
	if w.Code != http.StatusOK {
		t.Errorf("a write with a matching token gave %d", w.Code)
	}

	// And the check still bites: a page that could not read the cookie cannot
	// produce the header.
	bare := httptest.NewRequest(http.MethodPost, "/api/connections", nil)
	bare.AddCookie(&http.Cookie{Name: CSRFCookie, Value: token})
	w = httptest.NewRecorder()
	s.IngressCSRF(next).ServeHTTP(w, bare)
	if w.Code != http.StatusForbidden {
		t.Errorf("a write with no header gave %d, want 403", w.Code)
	}
}

func TestIngressPathIsConstrainedToSomethingThatCanOnlyBeAPath(t *testing.T) {
	for name, header := range map[string]string{
		"another origin":      "//evil.example.com/",
		"a quote":             `/api/hassio_ingress/x" onload="alert(1)`,
		"an angle bracket":    "/api/hassio_ingress/<script>",
		"a relative path":     "api/hassio_ingress/x",
		"a newline":           "/api/hassio_ingress/x\n",
		"nothing at all":      "",
		"a control character": "/api/hassio_ingress/\x01",
		"an ampersand":        "/api/hassio_ingress/a&b",
		"a space":             "/api/hassio_ingress/a b",
		"a query string":      "/api/hassio_ingress/x?a=1",
		"a fragment":          "/api/hassio_ingress/x#y",
		"just a slash":        "/",
		"a percent escape":    "/api/hassio_ingress/%2e%2e",
		"a backslash":         `/api/hassio_ingress/x\y`,
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set(IngressPathHeader, header)
		if got := IngressPath(r); got != "" {
			t.Errorf("%s: IngressPath = %q, want it refused", name, got)
		}
	}

	// What a real one looks like: the token is base64url, and the trailing
	// slash is optional.
	for _, ok := range []string{
		"/api/hassio_ingress/AbC-123_x",
		"/api/hassio_ingress/AbC-123_x/",
		"/api/hassio_ingress/a.b~c-d_e",
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set(IngressPathHeader, ok)
		if got := IngressPath(r); got != strings.TrimRight(ok, "/") {
			t.Errorf("IngressPath(%q) = %q", ok, got)
		}
	}
}

func TestIngressModeFollowsTheConfiguration(t *testing.T) {
	s, _ := newIngressService(t)
	if !s.IngressMode() {
		t.Error("ingress mode is off with the mode set to ingress")
	}

	s.cfg.Mode = config.ModeStandalone
	if s.IngressMode() {
		t.Error("ingress mode is on in standalone")
	}
}

func TestAnUnreadableRemoteAddressIsRefused(t *testing.T) {
	s, _ := newIngressService(t)

	r := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	r.RemoteAddr = "not-an-address"
	r.Header.Set(IngressUserIDHeader, "abc123")

	// Failing closed: an address the server cannot parse is one it cannot
	// compare against the trusted list, which is not the same as a match.
	if _, err := s.AuthenticateIngress(r); err == nil {
		t.Fatal("a request with an unreadable source address was accepted")
	}
}

func TestAnUnusableDefaultRoleFallsBackToTheLeastPrivilege(t *testing.T) {
	s, _ := newIngressService(t, func(c *config.IngressConfig) {
		// Load() refuses this, but the service is also constructed directly by
		// tests and by anything embedding it, so it must not read a typo as
		// "grant everything".
		c.DefaultRole = "administrator"
	})

	u, err := s.AuthenticateIngress(ingressRequest("172.30.32.2", map[string]string{
		IngressUserIDHeader: "abc123",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if u.Role != store.RoleViewer {
		t.Errorf("role = %q, want viewer", u.Role)
	}
}

func TestAnIdentityWithNothingUsableInItStillProducesAnAccount(t *testing.T) {
	s, _ := newIngressService(t)

	// A subject of punctuation: nothing survives sanitising, and the account
	// still has to be creatable rather than failing on an empty address.
	u, err := s.AuthenticateIngress(ingressRequest("172.30.32.2", map[string]string{
		IngressUserIDHeader: "!!!",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !store.ValidEmail(u.Email) {
		t.Errorf("address = %q", u.Email)
	}
	if u.Name != "Home Assistant user" {
		t.Errorf("name = %q, want a placeholder rather than an empty label", u.Name)
	}
}

func TestABlankEntryInAdminUsersMatchesNobody(t *testing.T) {
	s, _ := newIngressService(t, func(c *config.IngressConfig) {
		// A trailing comma in the add-on option produces one of these. It must
		// not match an identity that happens to have an empty username.
		c.AdminUsers = []string{"", "  "}
	})

	u, err := s.AuthenticateIngress(ingressRequest("172.30.32.2", map[string]string{
		IngressUserIDHeader: "abc123",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if u.Role == store.RoleAdmin {
		t.Fatal("a blank admin_users entry granted admin")
	}
}

func TestTheFallbackAccountGetsAReadableAddress(t *testing.T) {
	s, _ := newIngressService(t, func(c *config.IngressConfig) {
		c.FallbackUser = "Family Tablet"
	})

	u, err := s.AuthenticateIngress(ingressRequest("172.30.32.2", nil))
	if err != nil {
		t.Fatal(err)
	}
	// The "fallback:" marker is an internal key, not something to show
	// somebody in a user list.
	if strings.Contains(u.Email, "fallback") {
		t.Errorf("address = %q", u.Email)
	}
	if !store.ValidEmail(u.Email) {
		t.Errorf("address %q is not valid", u.Email)
	}
}
