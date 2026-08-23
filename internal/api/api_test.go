package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"testing/fstest"
	"time"

	"github.com/mqttview/mqttview/internal/api"
	"github.com/mqttview/mqttview/internal/auth"
	"github.com/mqttview/mqttview/internal/config"
	"github.com/mqttview/mqttview/internal/hub"
	"github.com/mqttview/mqttview/internal/mqttc"
	"github.com/mqttview/mqttview/internal/plugin"
	"github.com/mqttview/mqttview/internal/secrets"
	"github.com/mqttview/mqttview/internal/store"
	"github.com/mqttview/mqttview/internal/testutil"

	// Register the bundled plugin so the end-to-end test covers it.
	_ "github.com/mqttview/mqttview/internal/plugins/hass"
)

const (
	// The account newTestServer bootstraps. Named so a test cannot drift from
	// the harness by typing a different address.
	adminEmail    = "admin@example.com"
	adminPassword = "correct-horse-battery"
)

// testServer is a fully wired mqttview, backed by a temporary database.
type testServer struct {
	t      *testing.T
	http   *httptest.Server
	client *http.Client
	// suppressCSRF omits the header, so a test can prove the check bites.
	suppressCSRF bool
	mqtt         *mqttc.Manager
	// db is exposed so a test can take the database away and check that the
	// API degrades rather than panics.
	db *store.Store
	// opts is kept so a test can rebuild the server with one thing changed.
	opts api.Options
}

func newTestServer(t *testing.T, mutate ...func(*config.Config)) *testServer {
	t.Helper()

	dir := t.TempDir()
	key, err := secrets.LoadOrCreateKey("", dir)
	if err != nil {
		t.Fatalf("secret key: %v", err)
	}
	box, err := secrets.New(key)
	if err != nil {
		t.Fatalf("secret box: %v", err)
	}
	db, err := store.Open(dir+"/test.db", box)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	cfg := config.Default()
	cfg.DataDir = dir
	cfg.BaseURL = "http://127.0.0.1"
	for _, m := range mutate {
		m(&cfg)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	authSvc := auth.New(db, cfg, box, log)
	if _, _, err := authSvc.BootstrapAdmin(adminEmail, adminPassword); err != nil {
		t.Fatalf("bootstrap admin: %v", err)
	}

	mgr := mqttc.NewManager(log)
	t.Cleanup(func() { mgr.Shutdown(newContext()) })

	h := hub.New(log, nil)
	mgr.AddObserver(mqttc.Observer{OnMessage: h.BroadcastMessage, OnStatus: h.BroadcastStatus})

	plugins := plugin.NewRuntime(db, mgr, log, h.BroadcastEvent)
	if err := plugins.Start(newContext(), map[string]plugin.Defaults{}); err != nil {
		t.Fatalf("start plugins: %v", err)
	}
	t.Cleanup(plugins.Stop)

	opts := api.Options{
		Config: cfg, Log: log, Store: db, Auth: authSvc,
		MQTT: mgr, Hub: h, Plugins: plugins, Version: "test",
		// A stand-in for the built frontend, so the SPA fallback is exercised
		// and a redirect to /login lands somewhere rather than 404ing.
		Web: testFrontend(),
	}
	srv := api.New(opts)

	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	ts := &testServer{
		t:      t,
		http:   httpSrv,
		client: &http.Client{Jar: jar, Timeout: 30 * time.Second},
		mqtt:   mgr,
		db:     db,
		opts:   opts,
	}
	return ts
}

func (ts *testServer) do(method, path string, body any) *http.Response {
	ts.t.Helper()

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			ts.t.Fatalf("encode request: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, ts.http.URL+path, reader)
	if err != nil {
		ts.t.Fatalf("build request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Read the token from the jar on every request rather than caching it: a
	// fresh sign-in issues a new one, and a test that forgets to re-read it
	// fails with a CSRF error that has nothing to do with what it is testing.
	if token := ts.csrfToken(); token != "" && !ts.suppressCSRF {
		req.Header.Set(auth.CSRFHeader, token)
	}

	resp, err := ts.client.Do(req)
	if err != nil {
		ts.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// decode reads a JSON response, failing the test on an unexpected status.
func (ts *testServer) decode(resp *http.Response, wantStatus int, out any) {
	ts.t.Helper()
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		ts.t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, wantStatus, raw)
	}
	if out == nil || len(raw) == 0 {
		return
	}
	if err := json.Unmarshal(raw, out); err != nil {
		ts.t.Fatalf("decode response: %v; body: %s", err, raw)
	}
}

// csrfToken returns the double-submit token the server last set.
func (ts *testServer) csrfToken() string {
	for _, c := range ts.client.Jar.Cookies(mustParseURL(ts.t, ts.http.URL)) {
		if c.Name == auth.CSRFCookie {
			return c.Value
		}
	}
	return ""
}

func (ts *testServer) login() {
	ts.t.Helper()

	resp := ts.do(http.MethodPost, "/api/auth/login", map[string]string{
		"email": adminEmail, "password": adminPassword,
	})
	ts.decode(resp, http.StatusOK, nil)

	if ts.csrfToken() == "" {
		ts.t.Fatal("login did not set a CSRF cookie")
	}
}

// status makes a request and returns only its code, for the many checks whose
// whole point is which status was chosen.
func (ts *testServer) status(method, path string, body any) int {
	ts.t.Helper()
	resp := ts.do(method, path, body)
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// asUser signs a second account in against the same server, with its own cookie
// jar, so a test can check what a different role is allowed to do.
func (ts *testServer) asUser(t *testing.T, email, password string) *testServer {
	t.Helper()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	other := &testServer{
		t:      t,
		http:   ts.http,
		client: &http.Client{Jar: jar, Timeout: 30 * time.Second},
		mqtt:   ts.mqtt,
		db:     ts.db,
		opts:   ts.opts,
	}
	other.decode(other.do(http.MethodPost, "/api/auth/login", map[string]string{
		"email": email, "password": password,
	}), http.StatusOK, nil)
	return other
}

func TestUnauthenticatedRequestsAreRejected(t *testing.T) {
	ts := newTestServer(t)

	resp := ts.do(http.MethodGet, "/api/connections", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestLoginRejectsWrongPassword(t *testing.T) {
	ts := newTestServer(t)

	resp := ts.do(http.MethodPost, "/api/auth/login", map[string]string{
		"email": adminEmail, "password": "wrong",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestCSRFIsRequiredForWrites(t *testing.T) {
	ts := newTestServer(t)
	ts.login()

	ts.suppressCSRF = true
	resp := ts.do(http.MethodPost, "/api/connections", map[string]any{"name": "x", "url": "tcp://127.0.0.1:1883"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status without a CSRF header = %d, want 403", resp.StatusCode)
	}
	ts.suppressCSRF = false
}

// TestConnectionLifecycle covers the path a user actually walks: log in,
// create a broker connection, connect, publish, and read it back out of the
// topic tree.
func TestConnectionLifecycle(t *testing.T) {
	broker := testutil.StartBroker(t)
	ts := newTestServer(t)
	ts.login()

	var created struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		HasPassword bool   `json:"hasPassword"`
		Status      struct {
			State string `json:"state"`
		} `json:"status"`
	}
	resp := ts.do(http.MethodPost, "/api/connections", map[string]any{
		"name":          "test broker",
		"url":           broker.URL,
		"version":       "3.1.1",
		"cleanStart":    true,
		"password":      "hunter2",
		"subscriptions": []map[string]any{{"filter": "demo/#", "qos": 1}},
	})
	ts.decode(resp, http.StatusCreated, &created)

	if created.ID == "" {
		t.Fatal("created connection has no id")
	}
	if !created.HasPassword {
		t.Error("hasPassword should be true after supplying one")
	}

	// The password must never come back out of the API.
	resp = ts.do(http.MethodGet, "/api/connections/"+created.ID, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if bytes.Contains(body, []byte("hunter2")) {
		t.Fatal("the broker password leaked in the connection response")
	}

	resp = ts.do(http.MethodPost, "/api/connections/"+created.ID+"/connect", nil)
	ts.decode(resp, http.StatusOK, &created)
	if created.Status.State != "connected" {
		t.Fatalf("state = %q, want connected", created.Status.State)
	}

	time.Sleep(200 * time.Millisecond)

	resp = ts.do(http.MethodPost, "/api/connections/"+created.ID+"/publish", map[string]any{
		"topic": "demo/sensor", "payload": "42", "qos": 1,
	})
	ts.decode(resp, http.StatusOK, nil)

	// Wait for the message to loop back through the broker.
	var topic struct {
		Value struct {
			Payload []byte `json:"payload"`
		} `json:"value"`
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp = ts.do(http.MethodGet, "/api/connections/"+created.ID+"/topic?topic=demo/sensor", nil)
		if resp.StatusCode == http.StatusOK {
			ts.decode(resp, http.StatusOK, &topic)
			break
		}
		resp.Body.Close()
		time.Sleep(100 * time.Millisecond)
	}
	if string(topic.Value.Payload) != "42" {
		t.Fatalf("topic tree payload = %q, want 42", topic.Value.Payload)
	}

	// Deleting removes it from both the manager and the database.
	resp = ts.do(http.MethodDelete, "/api/connections/"+created.ID, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}
	if _, ok := ts.mqtt.Get(created.ID); ok {
		t.Error("the connection is still registered after deletion")
	}
}

// TestHomeAssistantPluginEndToEnd enables the bundled plugin, publishes a real
// discovery payload, and checks that a device with a live value comes back out
// of the plugin's own API.
func TestHomeAssistantPluginEndToEnd(t *testing.T) {
	broker := testutil.StartBroker(t)
	ts := newTestServer(t)
	ts.login()

	var created struct {
		ID string `json:"id"`
	}
	resp := ts.do(http.MethodPost, "/api/connections", map[string]any{
		"name": "ha broker", "url": broker.URL, "version": "5.0", "cleanStart": true,
	})
	ts.decode(resp, http.StatusCreated, &created)

	resp = ts.do(http.MethodPost, "/api/connections/"+created.ID+"/connect", nil)
	ts.decode(resp, http.StatusOK, nil)

	resp = ts.do(http.MethodPut, "/api/plugins/home-assistant/enabled", map[string]any{"enabled": true})
	ts.decode(resp, http.StatusOK, nil)

	time.Sleep(300 * time.Millisecond)

	broker.Publish(t, "homeassistant/sensor/kitchen/config", []byte(`{
        "~": "home/kitchen",
        "name": "Temperature",
        "stat_t": "~/temp",
        "val_tpl": "{{ value_json.temperature }}",
        "unit_of_meas": "°C",
        "uniq_id": "kitchen_temp",
        "dev": {"ids": ["kitchen-node"], "name": "Kitchen node", "mf": "ACME"}
    }`), true)

	// Give the plugin time to subscribe to the state topic it just learned
	// about before the state is published.
	time.Sleep(500 * time.Millisecond)
	broker.Publish(t, "home/kitchen/temp", []byte(`{"temperature": 21.5}`), true)

	type entity struct {
		Name  string `json:"name"`
		Unit  string `json:"unit"`
		State *struct {
			Value             any  `json:"value"`
			TemplateSupported bool `json:"templateSupported"`
		} `json:"state"`
	}
	type device struct {
		Name         string   `json:"name"`
		Manufacturer string   `json:"manufacturer"`
		Entities     []entity `json:"entities"`
	}

	var devices []device
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp = ts.do(http.MethodGet, "/api/p/home-assistant/devices", nil)
		devices = nil
		ts.decode(resp, http.StatusOK, &devices)
		if len(devices) == 1 && len(devices[0].Entities) == 1 && devices[0].Entities[0].State != nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	if len(devices) != 1 {
		t.Fatalf("discovered %d devices, want 1", len(devices))
	}
	d := devices[0]
	if d.Name != "Kitchen node" || d.Manufacturer != "ACME" {
		t.Errorf("device = %+v", d)
	}
	if len(d.Entities) != 1 {
		t.Fatalf("device has %d entities, want 1", len(d.Entities))
	}
	e := d.Entities[0]
	if e.Name != "Kitchen node Temperature" {
		t.Errorf("entity name = %q", e.Name)
	}
	if e.Unit != "°C" {
		t.Errorf("unit = %q", e.Unit)
	}
	if e.State == nil {
		t.Fatal("entity has no state; the plugin did not follow its state topic")
	}
	if !e.State.TemplateSupported {
		t.Error("the value_template should have been evaluated")
	}
	if v, ok := e.State.Value.(float64); !ok || v != 21.5 {
		t.Errorf("state value = %#v, want 21.5", e.State.Value)
	}
}

// TestPluginRoutesAreGatedOnEnablement makes sure a disabled plugin exposes no
// HTTP surface.
func TestPluginRoutesAreGatedOnEnablement(t *testing.T) {
	ts := newTestServer(t)
	ts.login()

	resp := ts.do(http.MethodGet, "/api/p/home-assistant/status", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for a disabled plugin", resp.StatusCode)
	}

	resp = ts.do(http.MethodGet, "/api/p/no-such-plugin/status", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for an unknown plugin", resp.StatusCode)
	}
}

func TestLastAdminCannotBeDemoted(t *testing.T) {
	ts := newTestServer(t)
	ts.login()

	var users []struct {
		ID    string `json:"id"`
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	resp := ts.do(http.MethodGet, "/api/users", nil)
	ts.decode(resp, http.StatusOK, &users)
	if len(users) != 1 {
		t.Fatalf("got %d users, want 1", len(users))
	}

	resp = ts.do(http.MethodPut, "/api/users/"+users[0].ID, map[string]any{
		"email": users[0].Email, "name": "Admin", "role": "viewer", "disabled": false,
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 when demoting the last admin", resp.StatusCode)
	}
}

func newContext() context.Context { return context.Background() }

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url %q: %v", raw, err)
	}
	return u
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}

// testFrontend is the smallest thing the SPA handler will serve: an index and
// one hashed asset, which is enough to cover both cache-control branches.
func testFrontend() fs.FS {
	return fstest.MapFS{
		"index.html":          {Data: []byte(`<!doctype html><div id="root"></div>`)},
		"assets/index-abc.js": {Data: []byte("console.log(1)")},
		"favicon.ico":         {Data: []byte("icon")},
	}
}

// emptyFrontend swaps in a filesystem with no index.html, which is what an
// install looks like when somebody built the binary without building the UI.
func (ts *testServer) emptyFrontend() {
	opts := ts.opts
	opts.Web = fstest.MapFS{}
	ts.http.Config.Handler = api.New(opts).Handler()
}
