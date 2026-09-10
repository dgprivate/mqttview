package api_test

import (
	"net/http"
	"strconv"
	"testing"
)

func TestClearingARetainedMessageIsRecordedWithWhoDidIt(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID := subscribedBroker(t, ts)

	publishAndWait(t, ts, connID, "retained/one", "value", 1)

	var body struct {
		Topic string `json:"topic"`
	}
	ts.decode(ts.do(http.MethodPost, "/api/connections/"+connID+"/retained/clear",
		map[string]any{"topic": "retained/one"}), http.StatusOK, &body)

	if body.Topic != "retained/one" {
		t.Errorf("cleared %q", body.Topic)
	}

	var entries []struct {
		Username string `json:"username"`
		Action   string `json:"action"`
		Target   string `json:"target"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/audit", nil), http.StatusOK, &entries)

	if len(entries) != 1 {
		t.Fatalf("the audit log has %d entries, want 1", len(entries))
	}
	if entries[0].Action != "clear retained" {
		t.Errorf("action = %q", entries[0].Action)
	}
	if entries[0].Username != adminEmail {
		t.Errorf("recorded %q as the actor, want %q", entries[0].Username, adminEmail)
	}
	if entries[0].Target == "" {
		t.Error("the entry does not say what was cleared")
	}
}

// A retain-clear published to "home/#" is not a bulk delete; some brokers
// would take it as a literal topic name, and it is an error either way.
func TestAWildcardCannotBeUsedToClearRetainedMessages(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID := subscribedBroker(t, ts)

	for _, topic := range []string{"home/#", "home/+/light", "", "#"} {
		got := ts.status(http.MethodPost, "/api/connections/"+connID+"/retained/clear",
			map[string]any{"topic": topic})
		if got != http.StatusBadRequest {
			t.Errorf("clearing %q returned %d, want 400", topic, got)
		}
	}
}

// mqttview only knows the topics this connection has seen. Refusing on that
// basis would make the feature useless exactly when it is needed — a retained
// message nobody here subscribed to is still a retained message.
func TestATopicMqttviewHasNotSeenCanStillBeCleared(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID := subscribedBroker(t, ts)

	var body struct {
		HadRetainedValue bool `json:"hadRetainedValue"`
	}
	ts.decode(ts.do(http.MethodPost, "/api/connections/"+connID+"/retained/clear",
		map[string]any{"topic": "never/seen/here"}), http.StatusOK, &body)

	if body.HadRetainedValue {
		t.Error("reported clearing a value it had never seen")
	}
}

func TestAViewerCannotClearRetainedMessages(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID := subscribedBroker(t, ts)

	const viewerEmail, viewerPassword = "viewer2@example.com", "yet-another-long-password"
	ts.decode(ts.do(http.MethodPost, "/api/users", map[string]any{
		"email": viewerEmail, "password": viewerPassword, "role": "viewer",
	}), http.StatusCreated, nil)

	viewer := ts.asUser(t, viewerEmail, viewerPassword)
	got := viewer.status(http.MethodPost, "/api/connections/"+connID+"/retained/clear",
		map[string]any{"topic": "a/b"})
	if got != http.StatusForbidden {
		t.Errorf("status = %d, want 403", got)
	}
}

// The log names accounts and what they did with them.
func TestTheAuditLogIsAdminOnly(t *testing.T) {
	ts := newTestServer(t)
	ts.login()

	const opEmail, opPassword = "operator@example.com", "operator-long-password"
	ts.decode(ts.do(http.MethodPost, "/api/users", map[string]any{
		"email": opEmail, "password": opPassword, "role": "operator",
	}), http.StatusCreated, nil)

	operator := ts.asUser(t, opEmail, opPassword)
	if got := operator.status(http.MethodGet, "/api/audit", nil); got != http.StatusForbidden {
		t.Errorf("an operator read the audit log: status = %d, want 403", got)
	}
	if got := ts.status(http.MethodGet, "/api/audit", nil); got != http.StatusOK {
		t.Errorf("an admin could not read the audit log: status = %d", got)
	}
}

func TestClearingRetainedNeedsASession(t *testing.T) {
	ts := newTestServer(t)

	got := ts.status(http.MethodPost, "/api/connections/any/retained/clear",
		map[string]any{"topic": "a/b"})
	if got != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", got)
	}
}

func TestTheLogViewShowsWhatWasLogged(t *testing.T) {
	ts := newTestServer(t)
	ts.login()

	// Any request produces a log line; a publish produces one that says so.
	connID := subscribedBroker(t, ts)
	ts.decode(ts.do(http.MethodPost, "/api/connections/"+connID+"/publish",
		map[string]any{"topic": "logged/topic", "payload": "x"}), http.StatusOK, nil)

	var body struct {
		Records []struct {
			Level   string            `json:"level"`
			Message string            `json:"message"`
			Attrs   map[string]string `json:"attrs"`
			Seq     uint64            `json:"seq"`
		} `json:"records"`
		Seq uint64 `json:"seq"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/logs", nil), http.StatusOK, &body)

	var found bool
	for _, r := range body.Records {
		if r.Message == "publish" && r.Attrs["topic"] == "logged/topic" {
			found = true
		}
	}
	if !found {
		t.Errorf("the publish is not in the log view; %d records were returned", len(body.Records))
	}
	if body.Seq == 0 {
		t.Error("no sequence number was returned, so a view cannot poll for what is new")
	}
}

func TestTheLogViewFiltersByLevel(t *testing.T) {
	ts := newTestServer(t)
	ts.login()

	var body struct {
		Records []struct {
			Level string `json:"level"`
		} `json:"records"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/logs?level=error", nil), http.StatusOK, &body)

	for _, r := range body.Records {
		if r.Level != "ERROR" {
			t.Errorf("filtering at error returned a %s record", r.Level)
		}
	}
}

// A poll that saw nothing still has to move forward, or the next read repeats
// everything it already had.
func TestPollingTheLogViewOnlyReturnsWhatIsNew(t *testing.T) {
	ts := newTestServer(t)
	ts.login()

	var first struct {
		Seq uint64 `json:"seq"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/logs", nil), http.StatusOK, &first)

	var second struct {
		Records []struct {
			Seq uint64 `json:"seq"`
		} `json:"records"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/logs?since="+strconv.FormatUint(first.Seq, 10), nil),
		http.StatusOK, &second)

	for _, r := range second.Records {
		if r.Seq <= first.Seq {
			t.Errorf("record %d was returned again after being seen", r.Seq)
		}
	}
}

func TestAnUnknownLogLevelIsRefused(t *testing.T) {
	ts := newTestServer(t)
	ts.login()

	if got := ts.status(http.MethodGet, "/api/logs?level=verbose", nil); got != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", got)
	}
}

// Log lines name accounts, topics and broker hostnames.
func TestTheLogViewIsAdminOnly(t *testing.T) {
	ts := newTestServer(t)
	ts.login()

	const opEmail, opPassword = "op2@example.com", "operator-long-password-2"
	ts.decode(ts.do(http.MethodPost, "/api/users", map[string]any{
		"email": opEmail, "password": opPassword, "role": "operator",
	}), http.StatusCreated, nil)

	operator := ts.asUser(t, opEmail, opPassword)
	if got := operator.status(http.MethodGet, "/api/logs", nil); got != http.StatusForbidden {
		t.Errorf("an operator read the log view: status = %d, want 403", got)
	}
}
