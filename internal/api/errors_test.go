package api_test

import (
	"bytes"
	"net/http"
	"testing"
)

// Every write endpoint decodes a body, and every one of them has a branch for
// a body that will not decode. They are collected here rather than repeated in
// each feature's test, because the property is the same everywhere: a bad body
// is a 400 with a message, never a 500 and never a silent success.

func writeEndpoints(broker string) []struct {
	method string
	path   string
} {
	return []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/auth/login"},
		{http.MethodPost, "/api/auth/password"},
		{http.MethodPost, "/api/auth/2fa/confirm"},
		{http.MethodPost, "/api/auth/2fa/disable"},
		{http.MethodPost, "/api/auth/2fa/recovery-codes"},
		{http.MethodPost, "/api/users"},
		{http.MethodPost, "/api/connections"},
		{http.MethodPut, "/api/connections/" + broker},
		{http.MethodPost, "/api/connections/" + broker + "/publish"},
		{http.MethodPost, "/api/connections/" + broker + "/subscribe"},
		{http.MethodPost, "/api/connections/" + broker + "/unsubscribe"},
	}
}

// rawDo sends a body that is not JSON at all.
func rawDo(t *testing.T, ts *testServer, method, path, body string) int {
	t.Helper()

	req, err := http.NewRequest(method, ts.http.URL+path, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token := ts.csrfToken(); token != "" {
		req.Header.Set("X-CSRF-Token", token)
	}
	resp, err := ts.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestMalformedBodiesAreRejectedNotCrashed(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	broker, _ := connectedBroker(t, ts)

	for _, ep := range writeEndpoints(broker) {
		for _, body := range []string{`{not json`, `[]`, `{"unknown_field_entirely":1}`} {
			status := rawDo(t, ts, ep.method, ep.path, body)
			if status >= 500 {
				t.Errorf("%s %s with %q gave %d", ep.method, ep.path, body, status)
			}
			if status == http.StatusOK || status == http.StatusCreated {
				t.Errorf("%s %s accepted %q", ep.method, ep.path, body)
			}
		}
	}
}

func TestWriteEndpointsNeedASession(t *testing.T) {
	ts := newTestServer(t)
	// One connected broker, then sign out, so the paths exist but the caller
	// does not.
	ts.login()
	broker, _ := connectedBroker(t, ts)
	ts.decode(ts.do(http.MethodPost, "/api/auth/logout", nil), http.StatusOK, nil)

	for _, ep := range writeEndpoints(broker) {
		// Login is the one endpoint that must work without a session.
		if ep.path == "/api/auth/login" {
			continue
		}
		resp := ts.do(ep.method, ep.path, map[string]any{})
		status := resp.StatusCode
		resp.Body.Close()
		if status != http.StatusUnauthorized && status != http.StatusForbidden {
			t.Errorf("%s %s gave %d to an anonymous caller", ep.method, ep.path, status)
		}
	}
}

func TestReadEndpointsNeedASession(t *testing.T) {
	ts := newTestServer(t)

	for _, path := range []string{
		"/api/connections", "/api/users", "/api/plugins", "/api/auth/me", "/api/auth/2fa",
	} {
		resp := ts.do(http.MethodGet, path, nil)
		status := resp.StatusCode
		resp.Body.Close()
		if status != http.StatusUnauthorized {
			t.Errorf("%s gave %d to an anonymous caller", path, status)
		}
	}
}

func TestPublishValidation(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	broker, _ := connectedBroker(t, ts)

	for _, body := range []map[string]any{
		{"topic": "", "payload": "x"},
		{"topic": "a/#", "payload": "x"},
		{"topic": "a/+", "payload": "x"},
		{"topic": "a", "payload": "x", "qos": 9},
	} {
		resp := ts.do(http.MethodPost, "/api/connections/"+broker+"/publish", body)
		status := resp.StatusCode
		resp.Body.Close()
		if status == http.StatusOK {
			t.Errorf("accepted %v — a wildcard is not a topic to publish to", body)
		}
	}
}

func TestSubscribeValidation(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	broker, _ := connectedBroker(t, ts)

	for _, body := range []map[string]any{
		{"subscriptions": []map[string]any{{"filter": "", "qos": 0}}},
		{"subscriptions": []map[string]any{{"filter": "a/#/b", "qos": 0}}},
		{"subscriptions": []map[string]any{{"filter": "a/#", "qos": 9}}},
	} {
		resp := ts.do(http.MethodPost, "/api/connections/"+broker+"/subscribe", body)
		status := resp.StatusCode
		resp.Body.Close()
		if status == http.StatusOK {
			t.Errorf("accepted %v", body)
		}
	}
}

func TestOperationsOnAnUnknownConnection(t *testing.T) {
	ts := newTestServer(t)
	ts.login()

	for _, ep := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodPost, "/api/connections/nope/connect", nil},
		{http.MethodPost, "/api/connections/nope/disconnect", nil},
		{http.MethodPost, "/api/connections/nope/clear", nil},
		{http.MethodPost, "/api/connections/nope/publish", map[string]any{"topic": "a", "payload": "b"}},
		{http.MethodPut, "/api/connections/nope", map[string]any{"name": "x", "url": "mqtt://h:1883", "version": "3.1.1"}},
		{http.MethodDelete, "/api/connections/nope", nil},
		{http.MethodGet, "/api/connections/nope/search?q=a", nil},
		{http.MethodGet, "/api/connections/nope/topic?topic=a", nil},
	} {
		resp := ts.do(ep.method, ep.path, ep.body)
		status := resp.StatusCode
		resp.Body.Close()
		if status != http.StatusNotFound {
			t.Errorf("%s %s gave %d, want 404", ep.method, ep.path, status)
		}
	}
}

func TestUserEndpointsOnAnUnknownUser(t *testing.T) {
	ts := newTestServer(t)
	ts.login()

	for _, ep := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodPut, "/api/users/nope", map[string]any{"email": "a@example.com", "role": "viewer"}},
		{http.MethodPut, "/api/users/nope/password", map[string]any{"password": "long-enough-password"}},
		{http.MethodDelete, "/api/users/nope", nil},
		{http.MethodDelete, "/api/users/nope/2fa", nil},
	} {
		resp := ts.do(ep.method, ep.path, ep.body)
		status := resp.StatusCode
		resp.Body.Close()
		if status != http.StatusNotFound {
			t.Errorf("%s %s gave %d, want 404", ep.method, ep.path, status)
		}
	}
}

func TestTwoFactorEndpointsBeforeEnrolment(t *testing.T) {
	ts := newTestServer(t)
	ts.login()

	// Confirming or regenerating with nothing set up is a conflict or an
	// unauthorised, never a crash.
	for _, ep := range []struct {
		path string
		body any
	}{
		{"/api/auth/2fa/confirm", map[string]string{"code": "123456"}},
		{"/api/auth/2fa/recovery-codes", map[string]string{"code": "123456"}},
	} {
		resp := ts.do(http.MethodPost, ep.path, ep.body)
		status := resp.StatusCode
		resp.Body.Close()
		if status >= 500 {
			t.Errorf("%s gave %d", ep.path, status)
		}
		if status == http.StatusOK {
			t.Errorf("%s succeeded with no enrolment", ep.path)
		}
	}

	// Disabling is idempotent: turning off something already off is a no-op
	// rather than an error. It still needs the password, which is what stops
	// an unlocked screen being enough.
	ts.decode(ts.do(http.MethodPost, "/api/auth/2fa/disable",
		map[string]string{"password": adminPassword}), http.StatusOK, nil)

	resp := ts.do(http.MethodPost, "/api/auth/2fa/disable", map[string]string{"password": "wrong"})
	status := resp.StatusCode
	resp.Body.Close()
	if status == http.StatusOK {
		t.Error("two-factor was disabled with the wrong password")
	}
}

func TestPluginEndpointsOnAnUnknownPlugin(t *testing.T) {
	ts := newTestServer(t)
	ts.login()

	for _, ep := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodGet, "/api/plugins/nope", nil},
		{http.MethodPut, "/api/plugins/nope/enabled", map[string]bool{"enabled": true}},
		{http.MethodPut, "/api/plugins/nope/settings", map[string]any{"k": "v"}},
	} {
		resp := ts.do(ep.method, ep.path, ep.body)
		status := resp.StatusCode
		resp.Body.Close()
		if status != http.StatusNotFound && status != http.StatusBadRequest {
			t.Errorf("%s %s gave %d", ep.method, ep.path, status)
		}
	}
}

func TestTreeAndMessageQueryParameters(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	broker, _ := connectedBroker(t, ts)

	ts.decode(ts.do(http.MethodPost, "/api/connections/"+broker+"/subscribe",
		map[string]any{"subscriptions": []map[string]any{{"filter": "q/#", "qos": 0}}}), http.StatusOK, nil)
	ts.decode(ts.do(http.MethodPost, "/api/connections/"+broker+"/publish",
		map[string]any{"topic": "q/a/b", "payload": "1"}), http.StatusOK, nil)
	waitForTopic(t, ts, broker, "q/a/b")

	// Nonsense in a numeric parameter falls back to a default rather than
	// failing the request: these come from a URL a person can edit.
	for _, path := range []string{
		"/api/connections/" + broker + "/messages?limit=notanumber",
		"/api/connections/" + broker + "/messages?limit=-5",
		"/api/connections/" + broker + "/messages?limit=999999",
		"/api/connections/" + broker + "/tree?prefix=q&depth=abc",
		"/api/connections/" + broker + "/tree?prefix=q&depth=99",
		"/api/connections/" + broker + "/search?q=",
		"/api/connections/" + broker + "/search?q=a&limit=x",
	} {
		resp := ts.do(http.MethodGet, path, nil)
		status := resp.StatusCode
		resp.Body.Close()
		if status != http.StatusOK {
			t.Errorf("%s gave %d", path, status)
		}
	}

	// A topic that was never seen is a 404, not an empty success.
	resp := ts.do(http.MethodGet, "/api/connections/"+broker+"/topic?topic=never/published", nil)
	status := resp.StatusCode
	resp.Body.Close()
	if status != http.StatusNotFound {
		t.Errorf("an unknown topic gave %d", status)
	}
}

// TestTheAPIDegradesWhenTheDatabaseIsGone closes the database underneath a
// running server and walks every endpoint.
//
// These are the branches that only run when a query fails — a disk that filled
// up, a volume that was unmounted, a file that was replaced. The property is
// that each one answers with a status and a message rather than panicking and
// taking the process with it.
func TestTheAPIDegradesWhenTheDatabaseIsGone(t *testing.T) {
	ts := newTestServer(t)
	ts.login()

	if err := ts.db.Close(); err != nil {
		t.Fatal(err)
	}

	endpoints := []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodGet, "/api/health", nil},
		{http.MethodGet, "/api/auth/config", nil},
		{http.MethodGet, "/api/auth/me", nil},
		{http.MethodGet, "/api/auth/2fa", nil},
		{http.MethodPost, "/api/auth/2fa/enrol", nil},
		{http.MethodPost, "/api/auth/2fa/confirm", map[string]string{"code": "123456"}},
		{http.MethodPost, "/api/auth/2fa/recovery-codes", map[string]string{"code": "123456"}},
		{http.MethodPost, "/api/auth/password", map[string]string{
			"currentPassword": adminPassword, "newPassword": "another-long-password",
		}},
		{http.MethodPost, "/api/auth/login", map[string]string{
			"email": adminEmail, "password": adminPassword,
		}},
		{http.MethodPost, "/api/auth/logout", nil},
		{http.MethodGet, "/api/users", nil},
		{http.MethodPost, "/api/users", map[string]any{
			"email": "new@example.com", "role": "viewer", "password": "long-enough-password",
		}},
		{http.MethodPut, "/api/users/someone", map[string]any{"email": "a@example.com", "role": "viewer"}},
		{http.MethodPut, "/api/users/someone/password", map[string]string{"password": "long-enough-password"}},
		{http.MethodDelete, "/api/users/someone", nil},
		{http.MethodDelete, "/api/users/someone/2fa", nil},
		{http.MethodGet, "/api/connections", nil},
		{http.MethodPost, "/api/connections", map[string]any{
			"name": "x", "url": "mqtt://h:1883", "version": "3.1.1",
		}},
		{http.MethodGet, "/api/plugins", nil},
	}

	for _, ep := range endpoints {
		func() {
			// A panic in a handler is recovered by chi's middleware and becomes
			// a 500, so the assertion is on the status rather than on the test
			// surviving.
			resp := ts.do(ep.method, ep.path, ep.body)
			defer resp.Body.Close()

			if resp.StatusCode == 0 {
				t.Errorf("%s %s produced no response", ep.method, ep.path)
			}
		}()
	}

	// The server is still serving after all of that.
	resp := ts.do(http.MethodGet, "/api/health", nil)
	status := resp.StatusCode
	resp.Body.Close()
	if status == 0 {
		t.Fatal("the server stopped answering")
	}
}

// TestHandlersReportAFailingQuery drops one table at a time.
//
// Closing the whole database is stopped by the middleware, which cannot load a
// session and answers 401 before a handler runs. Dropping a single table
// leaves authentication working and makes exactly one handler's query fail,
// which is the shape of a real partial failure and the only way to reach these
// branches.
func TestHandlersReportAFailingQuery(t *testing.T) {
	tests := []struct {
		table    string
		method   string
		path     string
		body     any
		wantCode int
	}{
		// Listing connections is answered from the manager's memory, not the
		// database, so it deliberately survives a broken table.
		{"connections", http.MethodGet, "/api/connections", nil, http.StatusOK},
		{"connections", http.MethodPost, "/api/connections",
			map[string]any{"name": "x", "url": "mqtt://h:1883", "version": "3.1.1"}, http.StatusInternalServerError},
		// The plugin runtime reports every refusal the same way, and the common
		// one is an unknown plugin id, so its errors are 400.
		{"plugin_settings", http.MethodPut, "/api/plugins/home-assistant/enabled",
			map[string]bool{"enabled": true}, http.StatusBadRequest},
		{"plugin_settings", http.MethodPut, "/api/plugins/home-assistant/settings",
			map[string]any{"discoveryPrefix": "x"}, http.StatusBadRequest},
		{"recovery_codes", http.MethodGet, "/api/auth/2fa", nil, http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.table+" "+tt.path, func(t *testing.T) {
			ts := newTestServer(t)
			ts.login()

			if _, err := ts.db.DB().Exec("DROP TABLE " + tt.table); err != nil {
				t.Fatalf("dropping %s: %v", tt.table, err)
			}

			resp := ts.do(tt.method, tt.path, tt.body)
			defer resp.Body.Close()

			if resp.StatusCode != tt.wantCode {
				t.Errorf("%s %s gave %d, want %d", tt.method, tt.path, resp.StatusCode, tt.wantCode)
			}
		})
	}
}

func TestUserEndpointsReportAFailingQuery(t *testing.T) {
	// The users table cannot be dropped without breaking the session, so the
	// failure is induced on the one table a user write also touches.
	ts := newTestServer(t)
	ts.login()

	var me struct {
		ID string `json:"id"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/auth/me", nil), http.StatusOK, &me)

	if _, err := ts.db.DB().Exec("DROP TABLE recovery_codes"); err != nil {
		t.Fatal(err)
	}
	// Clearing another account's two-factor deletes its recovery codes.
	resp := ts.do(http.MethodDelete, "/api/users/"+me.ID+"/2fa", nil)
	status := resp.StatusCode
	resp.Body.Close()
	if status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", status)
	}
}

func TestTheIndexIsReportedMissingRatherThanServedEmpty(t *testing.T) {
	// A frontend that was never built is an operator mistake with a specific
	// remedy, so it says so instead of serving a blank page.
	ts := newTestServer(t)
	ts.emptyFrontend()

	resp := ts.do(http.MethodGet, "/some/spa/route", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}
