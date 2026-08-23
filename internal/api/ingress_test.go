package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/mqttview/mqttview/internal/auth"
	"github.com/mqttview/mqttview/internal/config"
)

// Home Assistant mode end to end, through the router that actually runs. The
// unit tests prove the identity logic; these prove it is wired to it, which is
// the half that silently does not happen.

// ingressServer is a server in Home Assistant mode, with a client that sends
// what the Supervisor sends.
func ingressServer(t *testing.T) *testServer {
	t.Helper()
	return newTestServer(t, func(c *config.Config) {
		c.Auth.Mode = config.ModeIngress
		// httptest listens on loopback, so that is what stands in for the
		// Supervisor's address here. It is the only thing these tests fake.
		c.Auth.Ingress.TrustedProxies = []string{"127.0.0.1", "::1"}
	})
}

// asSupervisor issues a request the way the ingress proxy would.
func asSupervisor(t *testing.T, ts *testServer, method, path string, body io.Reader,
	headers map[string]string) *http.Response {
	t.Helper()

	req, err := http.NewRequest(method, ts.http.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(auth.IngressUserIDHeader, "ha-user-1")
	req.Header.Set(auth.IngressUserNameHeader, "dean")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if token := ts.csrfToken(); token != "" {
		req.Header.Set(auth.CSRFHeader, token)
	}

	resp, err := ts.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func TestHomeAssistantModeSignsSomebodyInWithoutALogin(t *testing.T) {
	ts := ingressServer(t)

	// httptest listens on 127.0.0.1, so the trusted proxy has to be that for
	// the request to be treated as having come through the Supervisor. This is
	// the one thing the test has to fake; everything else is real.
	resp := asSupervisor(t, ts, http.MethodGet, "/api/auth/me", nil, nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d: %s", resp.StatusCode, raw)
	}

	var me struct {
		Email    string `json:"email"`
		Name     string `json:"name"`
		Provider string `json:"provider"`
		Role     string `json:"role"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&me); err != nil {
		t.Fatal(err)
	}
	if me.Provider != "homeassistant" || me.Name != "dean" {
		t.Errorf("account = %+v", me)
	}
}

func TestHomeAssistantModeOffersNoLoginToAttack(t *testing.T) {
	ts := ingressServer(t)

	// Every local sign-in route is gone. A password endpoint that is still
	// reachable is still something to guess at, and there is no password to
	// guess: nobody ever set one.
	for _, route := range []struct {
		method, path string
	}{
		{http.MethodPost, "/api/auth/login"},
		{http.MethodPost, "/api/auth/logout"},
		{http.MethodPost, "/api/auth/password"},
		{http.MethodGet, "/api/auth/2fa"},
		{http.MethodPost, "/api/auth/2fa/enrol"},
		{http.MethodGet, "/api/auth/sso/x/start"},
		{http.MethodGet, "/api/auth/saml/x/metadata"},
		{http.MethodPost, "/api/auth/saml/x/acs"},
	} {
		resp := asSupervisor(t, ts, route.method, route.path, strings.NewReader("{}"), nil)
		status := resp.StatusCode
		resp.Body.Close()
		if status != http.StatusNotFound && status != http.StatusMethodNotAllowed {
			t.Errorf("%s %s is still reachable: %d", route.method, route.path, status)
		}
	}
}

func TestTheAPIIsClosedToAnythingNotComingThroughHomeAssistant(t *testing.T) {
	ts := newTestServer(t, func(c *config.Config) {
		c.Auth.Mode = config.ModeIngress
		// Nothing on this host is the Supervisor, which is what a published
		// port looks like from the outside.
		c.Auth.Ingress.TrustedProxies = []string{"172.30.32.2"}
	})

	for _, path := range []string{"/api/auth/me", "/api/connections", "/api/users", "/api/plugins"} {
		resp := ts.do(http.MethodGet, path, nil)
		status := resp.StatusCode
		resp.Body.Close()
		if status != http.StatusForbidden {
			t.Errorf("%s answered %d to a direct request, want 403", path, status)
		}
	}
}

func TestForgedIdentityHeadersFromOutsideAreRefused(t *testing.T) {
	ts := newTestServer(t, func(c *config.Config) {
		c.Auth.Mode = config.ModeIngress
		c.Auth.Ingress.TrustedProxies = []string{"172.30.32.2"}
		c.Auth.Ingress.AdminUsers = []string{"dean"}
	})

	req, err := http.NewRequest(http.MethodGet, ts.http.URL+"/api/auth/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(auth.IngressUserIDHeader, "ha-user-1")
	req.Header.Set(auth.IngressUserNameHeader, "dean")

	resp, err := ts.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// This is the attack the whole mode has to withstand: the headers say
	// "admin", and the only thing standing between that and an administrator
	// account is the source check.
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("forged identity headers were accepted: %d", resp.StatusCode)
	}

	users, err := ts.db.ListUsers()
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range users {
		if u.Provider == "homeassistant" {
			t.Fatal("a Home Assistant account was created from a forged header")
		}
	}
}

func TestTheAuthConfigTellsTheUIThereIsNoLoginPage(t *testing.T) {
	ts := ingressServer(t)

	var cfg struct {
		Mode       string `json:"mode"`
		AllowLocal bool   `json:"allowLocal"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/auth/config", nil), http.StatusOK, &cfg)

	// Public on purpose: the UI has to render something before anybody is
	// authenticated, and "which mode" is not a secret.
	if cfg.Mode != "ingress" {
		t.Errorf("mode = %q", cfg.Mode)
	}
	if cfg.AllowLocal {
		t.Error("ingress mode advertised a local login")
	}
}

func TestTheUIIsServedUnderTheIngressPrefix(t *testing.T) {
	ts := ingressServer(t)

	resp := asSupervisor(t, ts, http.MethodGet, "/", nil, map[string]string{
		auth.IngressPathHeader: "/api/hassio_ingress/TOKEN123",
	})
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	// Without this the browser resolves every asset, API call and WebSocket
	// against the Home Assistant root, and the panel is an empty box.
	if !strings.Contains(string(body), `<base href="/api/hassio_ingress/TOKEN123/" />`) {
		t.Fatalf("the base href was not rewritten: %s", body)
	}
}

func TestTheIngressPrefixIsIgnoredInStandaloneMode(t *testing.T) {
	ts := newTestServer(t)
	ts.login()

	// The header is not privileged outside ingress mode: anybody can send it,
	// and honouring it would let a link rewrite every URL in the page.
	req, err := http.NewRequest(http.MethodGet, ts.http.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(auth.IngressPathHeader, "/somewhere/else")

	resp, err := ts.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "/somewhere/else") {
		t.Fatalf("a standalone server honoured X-Ingress-Path: %s", body)
	}
}

func TestFramingIsRefusedInStandaloneAndAllowedForHomeAssistant(t *testing.T) {
	standalone := newTestServer(t)
	resp := standalone.do(http.MethodGet, "/api/health", nil)
	defer resp.Body.Close()

	// Clickjacking protection for an ordinary install: a page that frames
	// mqttview can overlay it and collect clicks meant for a broker command.
	if got := resp.Header.Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY", got)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP = %q", csp)
	}

	// And the opposite for Home Assistant, where the panel is an iframe on
	// Home Assistant's own origin. Getting this wrong shows a blank box with
	// an error only in the browser console.
	ing := ingressServer(t)
	resp = ing.do(http.MethodGet, "/api/health", nil)
	defer resp.Body.Close()

	if got := resp.Header.Get("X-Frame-Options"); got != "SAMEORIGIN" {
		t.Errorf("X-Frame-Options = %q, want SAMEORIGIN", got)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'self'") {
		t.Errorf("CSP = %q", csp)
	}
}

func TestNamedFrameAncestorsReplaceBothHeaders(t *testing.T) {
	ts := newTestServer(t, func(c *config.Config) {
		c.FrameAncestors = []string{"https://ha.example.com", "https://other.example.com"}
	})

	resp := ts.do(http.MethodGet, "/api/health", nil)
	defer resp.Body.Close()

	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors https://ha.example.com https://other.example.com") {
		t.Errorf("CSP = %q", csp)
	}
	// X-Frame-Options cannot express a list, so it is omitted rather than
	// being set to something that contradicts the CSP.
	if got := resp.Header.Get("X-Frame-Options"); got != "" {
		t.Errorf("X-Frame-Options = %q, want it omitted", got)
	}
}

func TestWritesStillNeedTheCSRFTokenInHomeAssistantMode(t *testing.T) {
	ts := ingressServer(t)

	// A read first, which is what issues the token: there is no login to do it.
	resp := asSupervisor(t, ts, http.MethodGet, "/api/auth/me", nil, nil)
	resp.Body.Close()

	req, err := http.NewRequest(http.MethodPost, ts.http.URL+"/api/connections",
		strings.NewReader(`{"name":"x","url":"mqtt://127.0.0.1:1","version":"3.1.1"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(auth.IngressUserIDHeader, "ha-user-1")
	// Deliberately no CSRF header: the panel is an iframe inside a page
	// mqttview does not control, and reading the cookie is what distinguishes
	// our own frontend from anything else that guessed the URL.
	resp, err = ts.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a write with no CSRF token gave %d, want 403", resp.StatusCode)
	}
}

func TestTheUIWorksAtTheRootWithoutAnIngressHeader(t *testing.T) {
	ts := ingressServer(t)

	// Ingress mode with no prefix header is what a request from the Supervisor
	// looks like on an older version. The page still has to render, at the
	// root, rather than serving a rewritten base pointing nowhere.
	resp := asSupervisor(t, ts, http.MethodGet, "/connections", nil, nil)
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `<base href="/" />`) {
		t.Fatalf("the base href was changed with no prefix to apply: %s", body)
	}
}

func TestAssetsAreServedUnderIngressWithoutRewriting(t *testing.T) {
	ts := ingressServer(t)

	// The Supervisor strips its prefix before proxying, so an asset arrives at
	// its ordinary path. Only index.html is ever rewritten; rewriting a
	// JavaScript bundle would corrupt it.
	resp := asSupervisor(t, ts, http.MethodGet, "/assets/index-abc.js", nil, map[string]string{
		auth.IngressPathHeader: "/api/hassio_ingress/TOKEN123",
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "console.log(1)" {
		t.Errorf("the asset was altered: %q", body)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q", cc)
	}
}

func TestAClientSideRouteFallsBackToTheIndexUnderIngress(t *testing.T) {
	ts := ingressServer(t)

	// Reloading the page on a deep link has to serve the app, with the prefix
	// applied — otherwise a refresh inside the panel breaks it.
	resp := asSupervisor(t, ts, http.MethodGet, "/connections/abc/edit", nil, map[string]string{
		auth.IngressPathHeader: "/api/hassio_ingress/TOKEN123",
	})
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `<base href="/api/hassio_ingress/TOKEN123/" />`) {
		t.Fatalf("a deep link did not get the prefix: %s", body)
	}
}

func TestAnIndexWithNoBaseTagStillGetsOne(t *testing.T) {
	ts := ingressServer(t)
	ts.frontendIndex(`<!doctype html><html><head><title>x</title></head><body></body></html>`)

	resp := asSupervisor(t, ts, http.MethodGet, "/", nil, map[string]string{
		auth.IngressPathHeader: "/api/hassio_ingress/TOKEN123",
	})
	defer resp.Body.Close()

	// A frontend built from a different tree has no tag to rewrite. Inserting
	// one beats serving a page whose every URL is wrong.
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `<base href="/api/hassio_ingress/TOKEN123/" />`) {
		t.Fatalf("no base tag was inserted: %s", body)
	}
}

func TestAPrefixWithAnythingUnexpectedInItIsDroppedEntirely(t *testing.T) {
	ts := ingressServer(t)

	// An ampersand is legal in a URL path but is not in an ingress prefix, and
	// the pattern is an allow-list: a character nobody anticipated is refused
	// rather than escaped and hoped for. The page then renders at the root,
	// which is wrong but harmless, instead of carrying an attacker's string.
	resp := asSupervisor(t, ts, http.MethodGet, "/", nil, map[string]string{
		auth.IngressPathHeader: "/api/hassio_ingress/a&b",
	})
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "a&b") || strings.Contains(string(body), "a&amp;b") {
		t.Fatalf("an unexpected character reached the page: %s", body)
	}
	if !strings.Contains(string(body), `<base href="/" />`) {
		t.Fatalf("the base href was not left alone: %s", body)
	}
}
