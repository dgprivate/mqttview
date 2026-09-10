package api_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/dgprivate/mqttview/internal/testutil"
)

type recordingsResponse struct {
	Recording bool `json:"recording"`
	Messages  []struct {
		Topic   string `json:"topic"`
		Payload string `json:"payload"`
	} `json:"messages"`
	Stats struct {
		Rows int64 `json:"rows"`
	} `json:"stats"`
	Recorder struct {
		Written uint64 `json:"written"`
		Dropped uint64 `json:"dropped"`
	} `json:"recorder"`
}

func recordings(t *testing.T, ts *testServer, connID, query string) recordingsResponse {
	t.Helper()

	var out recordingsResponse
	ts.decode(ts.do(http.MethodGet, "/api/connections/"+connID+"/recordings?"+query, nil),
		http.StatusOK, &out)
	return out
}

// Recording is the one setting that makes the database grow with broker
// traffic, so it is off until somebody says otherwise.
func TestNothingIsRecordedUntilAConnectionAsksForIt(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID := subscribedBroker(t, ts)

	publishAndWait(t, ts, connID, "not/recorded", "x", 1)

	got := recordings(t, ts, connID, "")
	if got.Recording {
		t.Error("a connection that never asked for it reports itself as recording")
	}
	if len(got.Messages) != 0 {
		t.Errorf("recorded %d messages without being asked", len(got.Messages))
	}
}

func TestTurningRecordingOnWritesMessagesToDisk(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID := subscribedBroker(t, ts)

	var conn struct {
		Name, URL, Version string
	}
	ts.decode(ts.do(http.MethodGet, "/api/connections/"+connID, nil), http.StatusOK, &conn)
	ts.decode(ts.do(http.MethodPut, "/api/connections/"+connID, map[string]any{
		"name": conn.Name, "url": conn.URL, "version": conn.Version,
		"subscriptions": []map[string]any{{"filter": "#", "qos": 0}},
		"recordToDisk":  true,
	}), http.StatusOK, nil)

	ts.decode(ts.do(http.MethodPost, "/api/connections/"+connID+"/publish",
		map[string]any{"topic": "recorded/topic", "payload": "kept"}), http.StatusOK, nil)

	// The write is batched onto another goroutine, so it is not there the
	// instant the publish returns.
	testutil.WaitFor(t, 15*time.Second, "the message to be written to disk", func() bool {
		return len(recordings(t, ts, connID, "").Messages) > 0
	})

	got := recordings(t, ts, connID, "")
	if !got.Recording {
		t.Error("the connection does not report itself as recording")
	}
	if got.Messages[0].Topic != "recorded/topic" || got.Messages[0].Payload != "kept" {
		t.Errorf("recorded %+v", got.Messages[0])
	}
	if got.Stats.Rows < 1 {
		t.Errorf("stats report %d rows", got.Stats.Rows)
	}
}

// Answering a question about Tuesday with everything ever recorded is worse
// than refusing.
func TestAWindowThatCannotBeReadIsRefused(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID := subscribedBroker(t, ts)

	for _, q := range []string{"since=yesterday", "until=soon", "since=2026-13-45"} {
		got := ts.status(http.MethodGet, "/api/connections/"+connID+"/recordings?"+q, nil)
		if got != http.StatusBadRequest {
			t.Errorf("%s returned %d, want 400", q, got)
		}
	}
}

func TestDeletingRecordingsIsOperatorOnlyAndRecorded(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID := subscribedBroker(t, ts)

	const viewerEmail, viewerPassword = "viewer3@example.com", "a-third-long-password"
	ts.decode(ts.do(http.MethodPost, "/api/users", map[string]any{
		"email": viewerEmail, "password": viewerPassword, "role": "viewer",
	}), http.StatusCreated, nil)

	viewer := ts.asUser(t, viewerEmail, viewerPassword)
	if got := viewer.status(http.MethodDelete, "/api/connections/"+connID+"/recordings", nil); got != http.StatusForbidden {
		t.Errorf("a viewer deleted recordings: status = %d, want 403", got)
	}

	ts.decode(ts.do(http.MethodDelete, "/api/connections/"+connID+"/recordings", nil), http.StatusOK, nil)

	var entries []struct {
		Action string `json:"action"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/audit", nil), http.StatusOK, &entries)
	var found bool
	for _, e := range entries {
		if e.Action == "delete recordings" {
			found = true
		}
	}
	if !found {
		t.Error("deleting recordings was not written to the audit log")
	}
}
