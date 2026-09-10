package api_test

import (
	"net/http"
	"strconv"
	"testing"
)

type publishEntry struct {
	ID       int64  `json:"id"`
	Topic    string `json:"topic"`
	Payload  string `json:"payload"`
	Base64   bool   `json:"payloadBase64"`
	QoS      byte   `json:"qos"`
	Retain   bool   `json:"retain"`
	Username string `json:"username"`
}

func publishHistory(t *testing.T, ts *testServer, connID, query string) []publishEntry {
	t.Helper()

	path := "/api/connections/" + connID + "/publishes"
	if query != "" {
		path += "?q=" + query
	}
	var out []publishEntry
	ts.decode(ts.do(http.MethodGet, path, nil), http.StatusOK, &out)
	return out
}

func TestEveryPublishIsRememberedNewestFirst(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID := subscribedBroker(t, ts)

	for _, v := range []string{"one", "two", "three"} {
		ts.decode(ts.do(http.MethodPost, "/api/connections/"+connID+"/publish",
			map[string]any{"topic": "cmd/a", "payload": v}), http.StatusOK, nil)
	}

	got := publishHistory(t, ts, connID, "")
	if len(got) != 3 {
		t.Fatalf("history has %d entries, want 3", len(got))
	}
	if got[0].Payload != "three" {
		t.Errorf("newest entry is %q, want %q", got[0].Payload, "three")
	}
	if got[0].Username != adminEmail {
		t.Errorf("entry records %q as the sender, want %q", got[0].Username, adminEmail)
	}
}

func TestThePublishHistorySearchesTopicsAndPayloads(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID := subscribedBroker(t, ts)

	ts.decode(ts.do(http.MethodPost, "/api/connections/"+connID+"/publish",
		map[string]any{"topic": "kitchen/light/set", "payload": "ON"}), http.StatusOK, nil)
	ts.decode(ts.do(http.MethodPost, "/api/connections/"+connID+"/publish",
		map[string]any{"topic": "garage/door/set", "payload": "OPEN"}), http.StatusOK, nil)

	if got := publishHistory(t, ts, connID, "kitchen"); len(got) != 1 || got[0].Topic != "kitchen/light/set" {
		t.Errorf("searching the topic gave %+v", got)
	}
	if got := publishHistory(t, ts, connID, "OPEN"); len(got) != 1 || got[0].Topic != "garage/door/set" {
		t.Errorf("searching the payload gave %+v", got)
	}
}

// A search for "50%" should find "50%", not everything beginning with 50.
func TestASearchForAWildcardCharacterIsTakenLiterally(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID := subscribedBroker(t, ts)

	ts.decode(ts.do(http.MethodPost, "/api/connections/"+connID+"/publish",
		map[string]any{"topic": "blind/a/set", "payload": "50%"}), http.StatusOK, nil)
	ts.decode(ts.do(http.MethodPost, "/api/connections/"+connID+"/publish",
		map[string]any{"topic": "blind/b/set", "payload": "5012"}), http.StatusOK, nil)

	got := publishHistory(t, ts, connID, "50%25") // %25 is "%" through a query string
	if len(got) != 1 {
		t.Fatalf("got %d matches for a literal %q, want 1: %+v", len(got), "50%", got)
	}
	if got[0].Payload != "50%" {
		t.Errorf("matched %q, want %q", got[0].Payload, "50%")
	}
}

// "Send that again" has to mean the same bytes, not whatever survived a round
// trip through a text field in the browser.
func TestRepublishingSendsTheStoredBytesRatherThanAnythingTheBrowserReturns(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID := subscribedBroker(t, ts)

	ts.decode(ts.do(http.MethodPost, "/api/connections/"+connID+"/publish",
		map[string]any{"topic": "bin/cmd", "payload": "//79", "payloadBase64": true}), http.StatusOK, nil)

	first := publishHistory(t, ts, connID, "")
	if len(first) != 1 {
		t.Fatalf("history has %d entries, want 1", len(first))
	}
	if !first[0].Base64 {
		t.Fatal("a binary payload was not marked as base64 in the history")
	}

	ts.decode(ts.do(http.MethodPost,
		"/api/connections/"+connID+"/publishes/"+strconv.FormatInt(first[0].ID, 10)+"/republish", nil),
		http.StatusOK, nil)

	after := publishHistory(t, ts, connID, "")
	if len(after) != 2 {
		t.Fatalf("history has %d entries after a republish, want 2", len(after))
	}
	if after[0].Payload != "//79" || !after[0].Base64 {
		t.Errorf("republished payload is %q (base64: %v), want the original bytes",
			after[0].Payload, after[0].Base64)
	}
}

func TestRepublishingSomethingFromAnotherConnectionIsNotFound(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	first := subscribedBroker(t, ts)
	second := subscribedBroker(t, ts)

	ts.decode(ts.do(http.MethodPost, "/api/connections/"+first+"/publish",
		map[string]any{"topic": "a/b", "payload": "x"}), http.StatusOK, nil)
	entries := publishHistory(t, ts, first, "")

	got := ts.status(http.MethodPost,
		"/api/connections/"+second+"/publishes/"+strconv.FormatInt(entries[0].ID, 10)+"/republish", nil)
	if got != http.StatusNotFound {
		t.Errorf("status = %d, want 404: the entry belongs to another connection", got)
	}
}

func TestClearingThePublishHistoryLeavesNothingBehind(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID := subscribedBroker(t, ts)

	ts.decode(ts.do(http.MethodPost, "/api/connections/"+connID+"/publish",
		map[string]any{"topic": "a/b", "payload": "x"}), http.StatusOK, nil)

	ts.decode(ts.do(http.MethodDelete, "/api/connections/"+connID+"/publishes", nil), http.StatusOK, nil)

	if got := publishHistory(t, ts, connID, ""); len(got) != 0 {
		t.Errorf("history still has %d entries after being cleared", len(got))
	}
}

type savedEntry struct {
	ID           string `json:"id"`
	ConnectionID string `json:"connectionId"`
	Folder       string `json:"folder"`
	Name         string `json:"name"`
	Topic        string `json:"topic"`
	Payload      string `json:"payload"`
	Base64       bool   `json:"payloadBase64"`
}

func TestASavedMessageCanBeKeptEditedAndSent(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID := subscribedBroker(t, ts)

	var created savedEntry
	ts.decode(ts.do(http.MethodPost, "/api/connections/"+connID+"/saved", map[string]any{
		"name": "kitchen on", "folder": "lights", "topic": "kitchen/light/set", "payload": "ON",
	}), http.StatusCreated, &created)

	if created.ConnectionID != connID {
		t.Errorf("saved against %q, want this connection", created.ConnectionID)
	}

	ts.decode(ts.do(http.MethodPut, "/api/connections/"+connID+"/saved/"+created.ID, map[string]any{
		"name": "kitchen off", "folder": "lights", "topic": "kitchen/light/set", "payload": "OFF",
	}), http.StatusOK, nil)

	var list []savedEntry
	ts.decode(ts.do(http.MethodGet, "/api/connections/"+connID+"/saved", nil), http.StatusOK, &list)
	if len(list) != 1 || list[0].Name != "kitchen off" || list[0].Payload != "OFF" {
		t.Fatalf("collection is %+v, want the edited message", list)
	}

	ts.decode(ts.do(http.MethodPost, "/api/connections/"+connID+"/saved/"+created.ID+"/publish", nil),
		http.StatusOK, nil)

	sent := publishHistory(t, ts, connID, "")
	if len(sent) != 1 || sent[0].Topic != "kitchen/light/set" || sent[0].Payload != "OFF" {
		t.Errorf("publishing the saved message sent %+v", sent)
	}

	ts.decode(ts.do(http.MethodDelete, "/api/connections/"+connID+"/saved/"+created.ID, nil),
		http.StatusNoContent, nil)

	ts.decode(ts.do(http.MethodGet, "/api/connections/"+connID+"/saved", nil), http.StatusOK, &list)
	if len(list) != 0 {
		t.Errorf("the message is still in the collection after being deleted: %+v", list)
	}
}

// A collection built against staging is worth having against production, so a
// message can belong to no connection at all.
func TestAGlobalSavedMessageAppearsForEveryConnection(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	first := subscribedBroker(t, ts)
	second := subscribedBroker(t, ts)

	ts.decode(ts.do(http.MethodPost, "/api/connections/"+first+"/saved", map[string]any{
		"name": "restart", "topic": "system/restart", "payload": "1", "global": true,
	}), http.StatusCreated, nil)

	var list []savedEntry
	ts.decode(ts.do(http.MethodGet, "/api/connections/"+second+"/saved", nil), http.StatusOK, &list)
	if len(list) != 1 || list[0].Name != "restart" {
		t.Errorf("the global message is not offered on another connection: %+v", list)
	}
	if list[0].ConnectionID != "" {
		t.Errorf("the global message is scoped to %q", list[0].ConnectionID)
	}
}

// A message saved against one broker must not be reachable by guessing its id
// on another.
func TestASavedMessageScopedToOneConnectionIsNotVisibleOnAnother(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	first := subscribedBroker(t, ts)
	second := subscribedBroker(t, ts)

	var created savedEntry
	ts.decode(ts.do(http.MethodPost, "/api/connections/"+first+"/saved", map[string]any{
		"name": "scoped", "topic": "a/b", "payload": "x",
	}), http.StatusCreated, &created)

	var list []savedEntry
	ts.decode(ts.do(http.MethodGet, "/api/connections/"+second+"/saved", nil), http.StatusOK, &list)
	if len(list) != 0 {
		t.Errorf("another connection's collection shows %+v", list)
	}

	got := ts.status(http.MethodPost,
		"/api/connections/"+second+"/saved/"+created.ID+"/publish", nil)
	if got != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for a message belonging to another connection", got)
	}
}

func TestASavedMessageNeedsANameAndAValidTopic(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID := subscribedBroker(t, ts)

	for _, c := range []struct {
		name string
		body map[string]any
	}{
		{"no name", map[string]any{"topic": "a/b", "payload": "x"}},
		{"no topic", map[string]any{"name": "n", "payload": "x"}},
		{"a wildcard, which cannot be published to", map[string]any{"name": "n", "topic": "a/#", "payload": "x"}},
		{"payload that is not the base64 it claims", map[string]any{
			"name": "n", "topic": "a/b", "payload": "!!!", "payloadBase64": true}},
	} {
		got := ts.status(http.MethodPost, "/api/connections/"+connID+"/saved", c.body)
		if got != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", c.name, got)
		}
	}
}

// Keeping and sending messages is control, and a viewer does not have it.
func TestAViewerCanReadTheHistoryButNotSendOrSave(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID := subscribedBroker(t, ts)

	const viewerEmail, viewerPassword = "viewer@example.com", "another-long-password"
	ts.decode(ts.do(http.MethodPost, "/api/users", map[string]any{
		"email": viewerEmail, "password": viewerPassword, "role": "viewer",
	}), http.StatusCreated, nil)

	viewer := ts.asUser(t, viewerEmail, viewerPassword)

	if got := viewer.status(http.MethodGet, "/api/connections/"+connID+"/publishes", nil); got != http.StatusOK {
		t.Errorf("a viewer cannot read the publish history: status = %d", got)
	}
	if got := viewer.status(http.MethodGet, "/api/connections/"+connID+"/saved", nil); got != http.StatusOK {
		t.Errorf("a viewer cannot read the collection: status = %d", got)
	}

	for _, c := range []struct {
		name, method, path string
		body               map[string]any
	}{
		{"save a message", http.MethodPost, "/api/connections/" + connID + "/saved",
			map[string]any{"name": "n", "topic": "a/b", "payload": "x"}},
		{"clear the history", http.MethodDelete, "/api/connections/" + connID + "/publishes", nil},
		{"republish", http.MethodPost, "/api/connections/" + connID + "/publishes/1/republish", nil},
	} {
		if got := viewer.status(c.method, c.path, c.body); got != http.StatusForbidden {
			t.Errorf("a viewer could %s: status = %d, want 403", c.name, got)
		}
	}
}
