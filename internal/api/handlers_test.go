package api_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	josejwt "github.com/go-jose/go-jose/v4/jwt"

	"github.com/mqttview/mqttview/internal/api"
	"github.com/mqttview/mqttview/internal/config"
	"github.com/mqttview/mqttview/internal/store"
	"github.com/mqttview/mqttview/internal/testutil"
)

// connectedBroker starts an in-process broker, registers it and connects, then
// returns the connection ID and its URL. Several tests need a live connection
// and none of them is about the setting-up.
func connectedBroker(t *testing.T, ts *testServer) (id, url string) {
	t.Helper()

	broker := testutil.StartBroker(t)
	var created struct {
		ID string `json:"id"`
	}
	ts.decode(ts.do(http.MethodPost, "/api/connections", map[string]any{
		"name": "test broker", "url": broker.URL, "version": "3.1.1", "cleanStart": true,
	}), http.StatusCreated, &created)

	ts.decode(ts.do(http.MethodPost, "/api/connections/"+created.ID+"/connect", nil), http.StatusOK, nil)
	return created.ID, broker.URL
}

// waitForTopic blocks until a topic shows up in the derived tree, because a
// publish loops back through the broker asynchronously.
func waitForTopic(t *testing.T, ts *testServer, connID, topic string) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp := ts.do(http.MethodGet, "/api/connections/"+connID+"/topic?topic="+topic, nil)
		status := resp.StatusCode
		resp.Body.Close()
		if status == http.StatusOK {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("topic %q never arrived", topic)
}

func TestHealthIsPublic(t *testing.T) {
	ts := newTestServer(t)

	// Deliberately unauthenticated: a load balancer has no session.
	var health struct {
		Status      string `json:"status"`
		Connections int    `json:"connections"`
		Version     string `json:"version"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/health", nil), http.StatusOK, &health)
	if health.Status != "ok" {
		t.Fatalf("health = %+v", health)
	}
}

func TestAuthConfigTellsTheLoginPageWhatToOffer(t *testing.T) {
	ts := newTestServer(t)

	var cfg struct {
		AllowLocal     bool `json:"allowLocal"`
		NeedsBootstrap bool `json:"needsBootstrap"`
		Providers      []struct {
			ID string `json:"id"`
		} `json:"providers"`
		SAMLProviders []struct {
			ID string `json:"id"`
		} `json:"samlProviders"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/auth/config", nil), http.StatusOK, &cfg)

	if !cfg.AllowLocal {
		t.Error("local login should be offered")
	}
	// The harness bootstraps an admin, so the first-run hint must be off.
	if cfg.NeedsBootstrap {
		t.Error("needsBootstrap is set even though an account exists")
	}
	if cfg.Providers == nil || cfg.SAMLProviders == nil {
		t.Error("both provider lists should be present, even when empty")
	}
}

func TestChangePassword(t *testing.T) {
	ts := newTestServer(t)
	ts.login()

	// The current password is required: an unlocked screen must not be enough
	// to take an account over.
	resp := ts.do(http.MethodPost, "/api/auth/password", map[string]string{
		"currentPassword": "wrong", "newPassword": "a-new-long-password",
	})
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("the password changed without the current one")
	}

	ts.decode(ts.do(http.MethodPost, "/api/auth/password", map[string]string{
		"currentPassword": adminPassword, "newPassword": "a-new-long-password",
	}), http.StatusOK, nil)

	// Changing it signs the other browsers out, so this client has to log in
	// again — with the new password.
	ts.decode(ts.do(http.MethodPost, "/api/auth/login",
		map[string]string{"email": adminEmail, "password": "a-new-long-password"}), http.StatusOK, nil)
}

func TestChangePasswordRefusesAWeakOne(t *testing.T) {
	ts := newTestServer(t)
	ts.login()

	resp := ts.do(http.MethodPost, "/api/auth/password", map[string]string{
		"currentPassword": adminPassword, "newPassword": "short",
	})
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("a short password was accepted")
	}
}

func TestUserAdministration(t *testing.T) {
	ts := newTestServer(t)
	ts.login()

	var created store.User
	ts.decode(ts.do(http.MethodPost, "/api/users", map[string]any{
		"email": "operator@example.com", "name": "Operator",
		"role": "operator", "password": "another-long-password",
	}), http.StatusCreated, &created)
	if created.ID == "" || created.Role != store.RoleOperator {
		t.Fatalf("created: %+v", created)
	}
	// A password must never come back out of the API.
	if created.PasswordHash != "" {
		t.Error("the response carried a password hash")
	}

	var users []store.User
	ts.decode(ts.do(http.MethodGet, "/api/users", nil), http.StatusOK, &users)
	if len(users) != 2 {
		t.Fatalf("got %d users, want 2", len(users))
	}

	ts.decode(ts.do(http.MethodPut, "/api/users/"+created.ID,
		map[string]any{"email": "operator@example.com", "name": "Renamed", "role": "viewer"}),
		http.StatusOK, nil)

	ts.decode(ts.do(http.MethodPut, "/api/users/"+created.ID+"/password",
		map[string]string{"password": "reset-by-an-admin-now"}), http.StatusOK, nil)

	// The reset works: the new password signs in.
	ts.decode(ts.do(http.MethodPost, "/api/auth/logout", nil), http.StatusOK, nil)
	ts.decode(ts.do(http.MethodPost, "/api/auth/login",
		map[string]string{"email": "operator@example.com", "password": "reset-by-an-admin-now"}),
		http.StatusOK, nil)

	ts.login()
	ts.decode(ts.do(http.MethodDelete, "/api/users/"+created.ID, nil), http.StatusNoContent, nil)
}

func TestTheLastAdministratorIsProtected(t *testing.T) {
	ts := newTestServer(t)
	ts.login()

	var me store.User
	ts.decode(ts.do(http.MethodGet, "/api/auth/me", nil), http.StatusOK, &me)

	// Demoting or deleting the only admin would leave nobody able to manage
	// the server, which is not a state the API should let you reach.
	resp := ts.do(http.MethodPut, "/api/users/"+me.ID,
		map[string]any{"email": me.Email, "name": me.Name, "role": "viewer"})
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Error("the last administrator was demoted")
	}

	resp = ts.do(http.MethodDelete, "/api/users/"+me.ID, nil)
	resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		t.Error("the last administrator was deleted")
	}
}

func TestCreateUserValidation(t *testing.T) {
	ts := newTestServer(t)
	ts.login()

	for _, body := range []map[string]any{
		{"email": "", "role": "viewer", "password": "long-enough-password"},
		{"email": "not-an-email", "role": "viewer", "password": "long-enough-password"},
		{"email": "x@example.com", "role": "wizard", "password": "long-enough-password"},
		{"email": "x@example.com", "role": "viewer", "password": "short"},
		{"email": adminEmail, "role": "viewer", "password": "long-enough-password"},
	} {
		resp := ts.do(http.MethodPost, "/api/users", body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusCreated {
			t.Errorf("accepted %v", body)
		}
	}
}

func TestAdminClearsAnotherUsersTwoFactor(t *testing.T) {
	ts := newTestServer(t)
	ts.login()

	// The lost-phone path: it needs no code, because the whole reason for
	// asking is that the person cannot produce one.
	var me store.User
	ts.decode(ts.do(http.MethodGet, "/api/auth/me", nil), http.StatusOK, &me)
	enrol(t, ts)

	ts.decode(ts.do(http.MethodDelete, "/api/users/"+me.ID+"/2fa", nil), http.StatusOK, nil)

	var status struct {
		Enabled bool `json:"enabled"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/auth/2fa", nil), http.StatusOK, &status)
	if status.Enabled {
		t.Fatal("two-factor survived an administrator clearing it")
	}
}

func TestClearingTwoFactorForANonexistentUser(t *testing.T) {
	ts := newTestServer(t)
	ts.login()

	resp := ts.do(http.MethodDelete, "/api/users/nope/2fa", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestPluginListingAndSettings(t *testing.T) {
	ts := newTestServer(t)
	ts.login()

	var plugins []struct {
		Meta struct {
			ID string `json:"id"`
		} `json:"meta"`
		Enabled bool `json:"enabled"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/plugins", nil), http.StatusOK, &plugins)
	if len(plugins) == 0 {
		t.Fatal("no plugins were listed")
	}

	id := plugins[0].Meta.ID
	ts.decode(ts.do(http.MethodGet, "/api/plugins/"+id, nil), http.StatusOK, nil)

	ts.decode(ts.do(http.MethodPut, "/api/plugins/"+id+"/enabled",
		map[string]bool{"enabled": true}), http.StatusOK, nil)

	ts.decode(ts.do(http.MethodPut, "/api/plugins/"+id+"/settings",
		map[string]any{"discoveryPrefix": "homeassistant"}), http.StatusOK, nil)

	resp := ts.do(http.MethodGet, "/api/plugins/does-not-exist", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown plugin gave %d", resp.StatusCode)
	}
}

func TestConnectionValidationAndNotFound(t *testing.T) {
	ts := newTestServer(t)
	ts.login()

	for _, body := range []map[string]any{
		{"name": "", "url": "mqtt://h:1883", "version": "3.1.1"},
		{"name": "x", "url": "", "version": "3.1.1"},
		{"name": "x", "url": "gopher://h:70", "version": "3.1.1"},
		{"name": "x", "url": "mqtt://h:1883", "version": "9"},
	} {
		resp := ts.do(http.MethodPost, "/api/connections", body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusCreated {
			t.Errorf("accepted %v", body)
		}
	}

	for _, path := range []string{
		"/api/connections/nope",
		"/api/connections/nope/tree",
		"/api/connections/nope/messages",
	} {
		resp := ts.do(http.MethodGet, path, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s gave %d, want 404", path, resp.StatusCode)
		}
	}
}

func TestSubscriptionsAndTreeReads(t *testing.T) {
	ts := newTestServer(t)
	ts.login()

	broker, _ := connectedBroker(t, ts)

	// Subscribing persists on the spec, so a reconnect keeps it.
	ts.decode(ts.do(http.MethodPost, "/api/connections/"+broker+"/subscribe",
		map[string]any{"subscriptions": []map[string]any{{"filter": "test/#", "qos": 0}}}),
		http.StatusOK, nil)

	var conn struct {
		Subscriptions []struct {
			Filter string `json:"filter"`
		} `json:"subscriptions"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/connections/"+broker, nil), http.StatusOK, &conn)
	if len(conn.Subscriptions) != 1 || conn.Subscriptions[0].Filter != "test/#" {
		t.Fatalf("subscription not persisted: %+v", conn.Subscriptions)
	}

	ts.decode(ts.do(http.MethodPost, "/api/connections/"+broker+"/publish",
		map[string]any{"topic": "test/a", "payload": "hello", "qos": 0}), http.StatusOK, nil)
	waitForTopic(t, ts, broker, "test/a")

	// Every read path over the derived state.
	ts.decode(ts.do(http.MethodGet, "/api/connections/"+broker+"/tree", nil), http.StatusOK, nil)
	ts.decode(ts.do(http.MethodGet, "/api/connections/"+broker+"/messages?limit=10", nil), http.StatusOK, nil)
	ts.decode(ts.do(http.MethodGet, "/api/connections/"+broker+"/search?q=test", nil), http.StatusOK, nil)
	ts.decode(ts.do(http.MethodGet, "/api/connections/"+broker+"/topic?topic=test/a", nil), http.StatusOK, nil)

	ts.decode(ts.do(http.MethodPost, "/api/connections/"+broker+"/unsubscribe",
		map[string]any{"filters": []string{"test/#"}}), http.StatusOK, nil)

	// Clearing throws away the tree and history without disconnecting.
	ts.decode(ts.do(http.MethodPost, "/api/connections/"+broker+"/clear", nil), http.StatusOK, nil)

	var tree struct {
		Children []any `json:"children"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/connections/"+broker+"/tree", nil), http.StatusOK, &tree)
	if len(tree.Children) != 0 {
		t.Errorf("the tree survived a clear: %+v", tree)
	}
}

func TestDisconnectAndReconnect(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	broker, _ := connectedBroker(t, ts)

	ts.decode(ts.do(http.MethodPost, "/api/connections/"+broker+"/disconnect", nil), http.StatusOK, nil)

	var view struct {
		Status struct {
			State string `json:"state"`
		} `json:"status"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/connections/"+broker, nil), http.StatusOK, &view)
	if view.Status.State == "connected" {
		t.Fatal("the connection is still up after disconnecting")
	}

	ts.decode(ts.do(http.MethodPost, "/api/connections/"+broker+"/connect", nil), http.StatusOK, nil)
}

func TestUpdatingAConnectionKeepsSecretsWhenOmitted(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	broker, brokerURL := connectedBroker(t, ts)

	// The UI never receives the password, so an edit that does not mention it
	// must not wipe it. Renaming is the common case.
	ts.decode(ts.do(http.MethodPut, "/api/connections/"+broker, map[string]any{
		"name": "renamed", "url": brokerURL,
		"version": "3.1.1", "keepAlive": 30, "cleanStart": true,
		"tls": map[string]any{}, "subscriptions": []any{},
	}), http.StatusOK, nil)

	var view struct {
		Name string `json:"name"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/connections/"+broker, nil), http.StatusOK, &view)
	if view.Name != "renamed" {
		t.Fatalf("name = %q", view.Name)
	}
}

func TestTheFrontendIsServedForUnknownPaths(t *testing.T) {
	ts := newTestServer(t)

	// The SPA router owns anything that is not /api, so a deep link reloads
	// rather than 404ing.
	resp := ts.do(http.MethodGet, "/connections/abc", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("content type = %q", ct)
	}
}

func TestUnknownAPIPathsAre404JSON(t *testing.T) {
	ts := newTestServer(t)

	resp := ts.do(http.MethodGet, "/api/nope", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("a 404 under /api should still be JSON: %v", err)
	}
	if body["error"] == "" {
		t.Error("the 404 carried no message")
	}
}

func TestOriginPatterns(t *testing.T) {
	if got := api.OriginPatterns("https://mqtt.example.com:8443"); len(got) != 1 || got[0] != "mqtt.example.com:8443" {
		t.Errorf("OriginPatterns = %v", got)
	}
	// An unusable base URL yields no patterns, which the websocket library
	// treats as same-origin only — the safe direction to fail in.
	for _, bad := range []string{"", "://nonsense", "not a url"} {
		if got := api.OriginPatterns(bad); len(got) != 0 {
			t.Errorf("OriginPatterns(%q) = %v, want none", bad, got)
		}
	}
}

func TestSAMLMetadataIsRefusedForAnUnknownProvider(t *testing.T) {
	ts := newTestServer(t)

	resp := ts.do(http.MethodGet, "/api/auth/saml/nope/metadata", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestSSOStartIsRefusedForAnUnknownProvider(t *testing.T) {
	ts := newTestServer(t)

	for _, path := range []string{"/api/auth/sso/nope/start", "/api/auth/saml/nope/start"} {
		resp := ts.do(http.MethodGet, path, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s gave %d, want 400", path, resp.StatusCode)
		}
	}
}

func TestSAMLAssertionWithNoStateGoesBackToLogin(t *testing.T) {
	ts := newTestServer(t)

	// No in-flight cookie, so the assertion cannot be tied to a login this
	// server started. The browser is sent back to the login page with a
	// readable reason rather than shown a stack trace or an XML error.
	req, err := http.NewRequest(http.MethodPost, ts.http.URL+"/api/auth/saml/nope/acs", nil)
	if err != nil {
		t.Fatal(err)
	}
	noFollow := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := noFollow.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want a redirect", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/login?error=") {
		t.Errorf("Location = %q, want the login page with a reason", loc)
	}
}

// fakeOIDC is a minimal identity provider, enough for the API layer's SSO
// endpoints to be walked end to end.
type fakeOIDC struct {
	srv   *httptest.Server
	key   *rsa.PrivateKey
	nonce string
}

func newFakeOIDC(t *testing.T) *fakeOIDC {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeOIDC{key: key}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                f.srv.URL,
			"authorization_endpoint":                f.srv.URL + "/authorize",
			"token_endpoint":                        f.srv.URL + "/token",
			"jwks_uri":                              f.srv.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: key.Public(), KeyID: "k", Algorithm: "RS256", Use: "sig",
		}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		signer, err := jose.NewSigner(
			jose.SigningKey{Algorithm: jose.RS256, Key: key},
			(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k"))
		if err != nil {
			t.Error(err)
			return
		}
		raw, err := josejwt.Signed(signer).Claims(map[string]any{
			"iss": f.srv.URL, "aud": "cid", "sub": "sub-1",
			"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
			"nonce": f.nonce, "email": "sso@example.com", "email_verified": true, "name": "SSO Person",
		}).Serialize()
		if err != nil {
			t.Error(err)
			return
		}
		// The content type matters: without it the oauth2 client parses the
		// body as form-encoded and finds no access token.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at", "token_type": "Bearer", "id_token": raw,
		})
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func TestSSOThroughTheAPI(t *testing.T) {
	idp := newFakeOIDC(t)

	ts := newTestServer(t, func(c *config.Config) {
		c.Auth.AllowSignup = true
		c.Auth.Providers = map[string]config.ProviderConfig{
			"test": {Enabled: true, DisplayName: "Test", Issuer: idp.srv.URL,
				ClientID: "cid", ClientSecret: "csecret"},
		}
	})

	// The login page is told the provider exists.
	var cfg struct {
		Providers []struct {
			ID, DisplayName string
		} `json:"providers"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/auth/config", nil), http.StatusOK, &cfg)
	if len(cfg.Providers) != 1 || cfg.Providers[0].ID != "test" {
		t.Fatalf("providers = %+v", cfg.Providers)
	}

	// Start: a redirect to the provider, with the state cookie set.
	noFollow := &http.Client{
		Jar: ts.client.Jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequest(http.MethodGet, ts.http.URL+"/api/auth/sso/test/start?next=/connections", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := noFollow.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("start gave %d", resp.StatusCode)
	}

	redirect, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	idp.nonce = redirect.Query().Get("nonce")
	state := redirect.Query().Get("state")

	// Callback: the provider sends the browser back with a code.
	cb := ts.http.URL + "/api/auth/sso/test/callback?state=" + url.QueryEscape(state) + "&code=abc"
	req, err = http.NewRequest(http.MethodGet, cb, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = noFollow.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback gave %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/connections" {
		t.Errorf("landed on %q, want the page the login started from", loc)
	}

	// The session works, and the account was created from the assertion.
	var me store.User
	ts.decode(ts.do(http.MethodGet, "/api/auth/me", nil), http.StatusOK, &me)
	if me.Email != "sso@example.com" || me.Provider != "test" {
		t.Fatalf("signed in as %+v", me)
	}
}

func TestSSOCallbackFailureGoesBackToLogin(t *testing.T) {
	idp := newFakeOIDC(t)
	ts := newTestServer(t, func(c *config.Config) {
		c.Auth.Providers = map[string]config.ProviderConfig{
			"test": {Enabled: true, Issuer: idp.srv.URL, ClientID: "cid", ClientSecret: "csecret"},
		}
	})

	noFollow := &http.Client{
		Jar:           ts.client.Jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	// No state cookie, so nothing ties this callback to a login.
	req, err := http.NewRequest(http.MethodGet, ts.http.URL+"/api/auth/sso/test/callback?state=x&code=y", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := noFollow.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/login?error=") {
		t.Errorf("Location = %q, want the login page with a reason", loc)
	}
}
