package api_test

import (
	"net/http"
	"sync"
	"testing"
	"time"
)

type collection struct {
	Filter    string         `json:"filter"`
	Seconds   float64        `json:"seconds"`
	Messages  []historyEntry `json:"messages"`
	Topics    map[string]int `json:"topics"`
	Dropped   int            `json:"dropped"`
	Truncated bool           `json:"truncated"`
}

func collect(t *testing.T, ts *testServer, connID, query string) collection {
	t.Helper()

	var got collection
	ts.decode(ts.do(http.MethodGet, "/api/connections/"+connID+"/collect?"+query, nil),
		http.StatusOK, &got)
	return got
}

// The point of the endpoint: a caller with no broker credentials asks what the
// broker is doing and gets told.
func TestAWindowReturnsWhatArrivedDuringIt(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID, _ := connectedBroker(t, ts)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Inside the window, not before it.
		time.Sleep(300 * time.Millisecond)
		for _, v := range []string{"one", "two"} {
			ts.decode(ts.do(http.MethodPost, "/api/connections/"+connID+"/publish",
				map[string]any{"topic": "watch/me", "payload": v}), http.StatusOK, nil)
		}
	}()

	got := collect(t, ts, connID, "seconds=3&filter=watch/%23")
	wg.Wait()

	if len(got.Messages) != 2 {
		t.Fatalf("collected %d messages, want 2: %+v", len(got.Messages), got.Messages)
	}
	if got.Messages[0].Payload != "one" || got.Messages[1].Payload != "two" {
		t.Errorf("collected %q and %q, want them in the order they arrived",
			got.Messages[0].Payload, got.Messages[1].Payload)
	}
	if got.Topics["watch/me"] != 2 {
		t.Errorf("topic counts are %v, want watch/me twice", got.Topics)
	}
	if got.Truncated {
		t.Error("a two-message window reported itself truncated")
	}
}

// Collecting must work without the connection already subscribing to the
// filter, or the caller would need to change the connection first — which is
// exactly the setting-up this is meant to avoid.
func TestCollectingSubscribesForTheWindowWithoutBeingAsked(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	// Deliberately no subscription of its own.
	connID, _ := connectedBroker(t, ts)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(300 * time.Millisecond)
		ts.decode(ts.do(http.MethodPost, "/api/connections/"+connID+"/publish",
			map[string]any{"topic": "unsubscribed/topic", "payload": "hello"}), http.StatusOK, nil)
	}()

	got := collect(t, ts, connID, "seconds=3")
	wg.Wait()

	if len(got.Messages) != 1 || got.Messages[0].Payload != "hello" {
		t.Fatalf("collected %+v, want the one message on a topic nobody had subscribed to", got.Messages)
	}

	// And the connection's stored subscriptions are untouched by it.
	var conn struct {
		Subscriptions []struct {
			Filter string `json:"filter"`
		} `json:"subscriptions"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/connections/"+connID, nil), http.StatusOK, &conn)
	if len(conn.Subscriptions) != 0 {
		t.Errorf("collecting left %+v on the connection", conn.Subscriptions)
	}
}

// Two agents watching at once is the normal case. The first to finish must not
// unsubscribe the filter out from under the second.
func TestTwoOverlappingWindowsBothSeeTheTraffic(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID, _ := connectedBroker(t, ts)

	var (
		wg            sync.WaitGroup
		short, longer collection
	)

	wg.Add(2)
	go func() {
		defer wg.Done()
		short = collect(t, ts, connID, "seconds=1&filter=shared/%23")
	}()
	go func() {
		defer wg.Done()
		longer = collect(t, ts, connID, "seconds=4&filter=shared/%23")
	}()

	// Once inside both windows, and once after the short one has ended.
	time.Sleep(300 * time.Millisecond)
	ts.decode(ts.do(http.MethodPost, "/api/connections/"+connID+"/publish",
		map[string]any{"topic": "shared/a", "payload": "during both"}), http.StatusOK, nil)
	time.Sleep(1500 * time.Millisecond)
	ts.decode(ts.do(http.MethodPost, "/api/connections/"+connID+"/publish",
		map[string]any{"topic": "shared/a", "payload": "after the short one"}), http.StatusOK, nil)

	wg.Wait()

	if len(short.Messages) != 1 {
		t.Errorf("the short window collected %d messages, want 1", len(short.Messages))
	}
	if len(longer.Messages) != 2 {
		t.Fatalf("the longer window collected %d messages, want 2: the short one unsubscribed underneath it",
			len(longer.Messages))
	}
	if longer.Messages[1].Payload != "after the short one" {
		t.Errorf("the longer window missed the message published after the short one ended")
	}
}

// A caller that receives exactly its limit cannot tell a complete answer from
// a cut-off one unless it is told.
func TestReachingTheLimitIsReportedRatherThanImplied(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID, _ := connectedBroker(t, ts)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(200 * time.Millisecond)
		for range 6 {
			ts.decode(ts.do(http.MethodPost, "/api/connections/"+connID+"/publish",
				map[string]any{"topic": "busy/one", "payload": "x"}), http.StatusOK, nil)
		}
	}()

	got := collect(t, ts, connID, "seconds=3&limit=2&filter=busy/%23")
	wg.Wait()

	if len(got.Messages) != 2 {
		t.Fatalf("collected %d messages against a limit of 2", len(got.Messages))
	}
	if !got.Truncated {
		t.Error("the limit was reached but truncated is false")
	}
	if got.Dropped < 1 {
		t.Errorf("dropped = %d, want the messages that did not fit to be counted", got.Dropped)
	}
}

// Nothing happening is an answer, not a failure.
func TestAQuietWindowIsAnEmptyResultRatherThanAnError(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID, _ := connectedBroker(t, ts)

	got := collect(t, ts, connID, "seconds=1&filter=nothing/here/%23")
	if len(got.Messages) != 0 {
		t.Errorf("collected %+v from a silent filter", got.Messages)
	}
	if got.Truncated {
		t.Error("an empty window reported itself truncated")
	}
}

func TestACollectionRefusesArgumentsItCannotHonour(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID, _ := connectedBroker(t, ts)

	for _, c := range []struct{ name, query string }{
		{"a window longer than the ceiling", "seconds=600"},
		{"a window of nothing", "seconds=0"},
		{"a limit of nothing", "limit=0"},
		{"a limit beyond the ceiling", "limit=99999"},
		{"a filter that is not a filter", "filter=a/%23/b"},
	} {
		got := ts.status(http.MethodGet, "/api/connections/"+connID+"/collect?"+c.query, nil)
		if got != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", c.name, got)
		}
	}
}

// Holding the request open for the window and then reporting nothing would
// look identical to a broker that is simply quiet.
func TestCollectingFromADisconnectedBrokerSaysSoImmediately(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID, _ := connectedBroker(t, ts)

	ts.decode(ts.do(http.MethodPost, "/api/connections/"+connID+"/disconnect", nil), http.StatusOK, nil)

	started := time.Now()
	got := ts.status(http.MethodGet, "/api/connections/"+connID+"/collect?seconds=30", nil)
	if got != http.StatusConflict {
		t.Errorf("status = %d, want 409", got)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("took %s to refuse; it waited out the window first", elapsed)
	}
}

func TestCollectingNeedsASession(t *testing.T) {
	ts := newTestServer(t)

	if got := ts.status(http.MethodGet, "/api/connections/any/collect?seconds=1", nil); got != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", got)
	}
}
