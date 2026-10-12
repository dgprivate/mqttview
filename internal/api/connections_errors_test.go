package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dgprivate/mqttview/internal/testutil"
)

// A connection whose broker does not answer is the ordinary case in this
// project: a laptop with a stale address, a broker that is down, a firewall.
// Every operation on one has to say which of those it is, and none of them may
// hang or report success.

// deadConnection creates a connection pointing at a port nothing listens on.
func deadConnection(t *testing.T, ts *testServer) string {
	t.Helper()
	var created struct {
		ID string `json:"id"`
	}
	ts.decode(ts.do(http.MethodPost, "/api/connections", map[string]any{
		"name": "dead", "url": "mqtt://127.0.0.1:1", "version": "3.1.1",
	}), http.StatusCreated, &created)
	if created.ID == "" {
		t.Fatal("the connection was created without an id")
	}
	return created.ID
}

func TestOperationsOnABrokerThatIsNotThere(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	id := deadConnection(t, ts)

	// Connecting fails at the transport, which is a 502: mqttview is fine, the
	// thing behind it is not.
	if code := ts.status(http.MethodPost, "/api/connections/"+id+"/connect", nil); code != http.StatusBadGateway {
		t.Errorf("connect gave %d, want 502", code)
	}
	if code := ts.status(http.MethodPost, "/api/connections/"+id+"/publish",
		map[string]any{"topic": "a/b", "payload": "x"}); code != http.StatusBadGateway {
		t.Errorf("publish gave %d, want 502", code)
	}
	// Subscribing, on the other hand, succeeds: a subscription is part of the
	// connection's definition, and it is sent to the broker when the session is
	// established. Refusing it while offline would mean a connection could
	// never be configured before it first connects.
	if code := ts.status(http.MethodPost, "/api/connections/"+id+"/subscribe",
		map[string]any{"subscriptions": []map[string]any{{"filter": "a/#"}}}); code != http.StatusOK {
		t.Errorf("subscribe gave %d, want 200", code)
	}
	if code := ts.status(http.MethodPost, "/api/connections/"+id+"/unsubscribe",
		map[string]any{"filters": []string{"a/#"}}); code != http.StatusOK {
		t.Errorf("unsubscribe gave %d, want 200", code)
	}
}

func TestPublishingBinaryThatIsNotBase64(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	id := deadConnection(t, ts)

	// The payload is rejected before the broker is ever consulted, so this is a
	// 400 and not the 502 an unreachable broker would give.
	code := ts.status(http.MethodPost, "/api/connections/"+id+"/publish",
		map[string]any{"topic": "a/b", "payload": "!!not base64!!", "payloadBase64": true})
	if code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", code)
	}
}

func TestDeletingAConnectionThatWasNeverThere(t *testing.T) {
	ts := newTestServer(t)
	ts.login()

	if code := ts.status(http.MethodDelete, "/api/connections/no-such-id", nil); code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", code)
	}
}

func TestUpdatingAConnectionToSomethingUnusable(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	id := deadConnection(t, ts)

	code := ts.status(http.MethodPut, "/api/connections/"+id,
		map[string]any{"name": "dead", "url": "gopher://h:70", "version": "3.1.1"})
	if code != http.StatusBadRequest {
		t.Errorf("an unusable URL gave %d, want 400", code)
	}
}

func TestCreatingAConnectionNeedsAdmin(t *testing.T) {
	ts := newTestServer(t)
	ts.login()

	// A viewer may look at everything and change nothing: connections carry
	// broker credentials, so creating one is an administrator's decision.
	var created struct {
		ID string `json:"id"`
	}
	ts.decode(ts.do(http.MethodPost, "/api/users", map[string]any{
		"email": "viewer@example.com", "password": "correct-horse-battery", "role": "viewer",
	}), http.StatusCreated, &created)

	viewer := ts.asUser(t, "viewer@example.com", "correct-horse-battery")
	code := viewer.status(http.MethodPost, "/api/connections",
		map[string]any{"name": "x", "url": "mqtt://127.0.0.1:1", "version": "3.1.1"})
	if code != http.StatusForbidden {
		t.Errorf("a viewer creating a connection got %d, want 403", code)
	}
}

func TestAutoConnectDoesNotBlockTheResponse(t *testing.T) {
	ts := newTestServer(t)
	ts.login()

	// autoConnect dials in the background precisely so that creating a
	// connection to an unreachable broker still answers immediately.
	start := time.Now()
	code := ts.status(http.MethodPost, "/api/connections", map[string]any{
		"name": "auto", "url": "mqtt://127.0.0.1:1", "version": "3.1.1",
		"autoConnect": true,
	})
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", code)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("creating the connection blocked for %v on the dial", elapsed)
	}
}

func TestReadsAgainstAConnectionWithNoTrafficYet(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	id := deadConnection(t, ts)

	// Nothing has been published, so a prefix or a topic that was never seen is
	// absent rather than empty: an empty result would read as "the topic exists
	// and has no children", which is a different fact.
	if code := ts.status(http.MethodGet, "/api/connections/"+id+"/tree?prefix=nothing/here", nil); code != http.StatusNotFound {
		t.Errorf("tree gave %d, want 404", code)
	}
	if code := ts.status(http.MethodGet, "/api/connections/"+id+"/topic?topic=nothing/here", nil); code != http.StatusNotFound {
		t.Errorf("topic gave %d, want 404", code)
	}
	if code := ts.status(http.MethodGet, "/api/connections/"+id+"/topic", nil); code != http.StatusBadRequest {
		t.Errorf("topic with no parameter gave %d, want 400", code)
	}

	// A malformed filter is the user's typo, not an empty result set.
	if code := ts.status(http.MethodGet, "/api/connections/"+id+"/messages?filter=a/%23/b", nil); code != http.StatusBadRequest {
		t.Errorf("a bad message filter gave %d, want 400", code)
	}
	if code := ts.status(http.MethodGet, "/api/connections/"+id+"/search?q=a/%23/b", nil); code != http.StatusBadRequest {
		t.Errorf("a bad search filter gave %d, want 400", code)
	}

	// A search without wildcards is a substring search and always answers.
	if code := ts.status(http.MethodGet, "/api/connections/"+id+"/search?q=kitchen", nil); code != http.StatusOK {
		t.Errorf("a plain search gave %d, want 200", code)
	}
}

func TestSubscriptionsSurviveARestart(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	id := deadConnection(t, ts)

	if code := ts.status(http.MethodPost, "/api/connections/"+id+"/subscribe",
		map[string]any{"subscriptions": []map[string]any{{"filter": "a/#"}}}); code != http.StatusOK {
		t.Fatalf("subscribe gave %d", code)
	}

	// The point of persisting is that the filter is there after a restart,
	// without anyone having to add it again.
	rec, err := ts.db.GetConnection(id)
	if err != nil {
		t.Fatalf("the connection is no longer in the database: %v", err)
	}
	var found bool
	for _, sub := range rec.Spec.Subscriptions {
		if sub.Filter == "a/#" {
			found = true
		}
	}
	if !found {
		t.Errorf("the subscription was not persisted: %+v", rec.Spec.Subscriptions)
	}
}

func TestClearingStateOnAQuietConnection(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	id := deadConnection(t, ts)

	if code := ts.status(http.MethodPost, "/api/connections/"+id+"/clear", nil); code != http.StatusOK {
		t.Errorf("clear gave %d, want 200", code)
	}

	var msgs []json.RawMessage
	ts.decode(ts.do(http.MethodGet, "/api/connections/"+id+"/messages", nil), http.StatusOK, &msgs)
	if len(msgs) != 0 {
		t.Errorf("history still holds %d messages after clearing", len(msgs))
	}
}

// The list is what the connections page renders. It must say whether a
// password is set and never what it is.
func TestTheConnectionListNeverCarriesAPassword(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	ts.decode(ts.do(http.MethodPost, "/api/connections", map[string]any{
		"name": "secret", "url": "mqtt://127.0.0.1:1", "version": "3.1.1",
		"username": "u", "password": "do-not-echo-this",
	}), http.StatusCreated, nil)

	resp := ts.do(http.MethodGet, "/api/connections", nil)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if strings.Contains(string(raw), "do-not-echo-this") {
		t.Fatal("the connection list contains the broker password")
	}
	var list []struct {
		Name        string `json:"name"`
		HasPassword bool   `json:"hasPassword"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || !list[0].HasPassword {
		t.Errorf("list is %+v, want one connection reporting a password", list)
	}
}

// A search with a wildcard in it is an MQTT filter, which is what somebody
// typing home/+/temp means; matching it as a substring would find nothing.
func TestASearchWithAWildcardMatchesLikeAFilter(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID := subscribedBroker(t, ts)

	publishAndWait(t, ts, connID, "home/kitchen/temp", "21", 1)
	publishAndWait(t, ts, connID, "home/kitchen/humidity", "40", 1)

	resp := ts.do(http.MethodGet, "/api/connections/"+connID+"/search?q=home/%2B/temp", nil)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "home/kitchen/temp") || strings.Contains(string(raw), "humidity") {
		t.Errorf("search for home/+/temp returned %s", raw)
	}
}

// AutoConnect dials in the background so the create answers at once; the
// connection still has to end up connected.
func TestAnAutoConnectingConnectionConnects(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	broker := ts.startBroker(t)

	var created struct {
		ID string `json:"id"`
	}
	ts.decode(ts.do(http.MethodPost, "/api/connections", map[string]any{
		"name": "auto", "url": broker.URL, "version": "3.1.1", "autoConnect": true,
	}), http.StatusCreated, &created)

	testutil.WaitFor(t, 10*time.Second, "the connection to come up on its own", func() bool {
		var view struct {
			Status struct {
				State string `json:"state"`
			} `json:"status"`
		}
		ts.decode(ts.do(http.MethodGet, "/api/connections/"+created.ID, nil), http.StatusOK, &view)
		return view.Status.State == "connected"
	})
}
