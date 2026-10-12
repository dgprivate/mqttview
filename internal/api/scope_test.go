package api_test

import (
	"net/http"
	"testing"
)

// Every route under /api/connections/{id} resolves the id before it does
// anything else. An id that names nothing is a 404 everywhere — not a 400 for
// a body the handler should never have read, and not a 500 from following a
// nil connection. TestOperationsOnAnUnknownConnection covers the original
// routes; this covers every one added since, so a new route that forgets the
// lookup fails here.
func TestEveryRouteUnderAnUnknownConnectionIsNotFound(t *testing.T) {
	ts := newTestServer(t)
	ts.login()

	const base = "/api/connections/nope"
	saved := map[string]any{"name": "n", "topic": "a/b", "payload": "x"}

	for _, ep := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodGet, base, nil},
		{http.MethodPost, base + "/subscribe", map[string]any{"subscriptions": []map[string]any{{"filter": "a/#"}}}},
		{http.MethodPost, base + "/unsubscribe", map[string]any{"filters": []string{"a/#"}}},
		{http.MethodGet, base + "/tree", nil},
		{http.MethodGet, base + "/messages", nil},
		{http.MethodGet, base + "/topic/history?topic=a", nil},
		{http.MethodGet, base + "/topic/diff?topic=a", nil},
		{http.MethodGet, base + "/topic/series?topic=a", nil},
		{http.MethodGet, base + "/topic/fields?topic=a", nil},
		{http.MethodGet, base + "/topic/export?topic=a", nil},
		{http.MethodGet, base + "/topic/decode?topic=a", nil},
		{http.MethodGet, base + "/topic/raw?topic=a", nil},
		{http.MethodGet, base + "/graph", nil},
		{http.MethodGet, base + "/sys", nil},
		{http.MethodGet, base + "/collect?seconds=1", nil},
		{http.MethodPost, base + "/retained/clear", map[string]any{"topic": "a/b"}},
		{http.MethodGet, base + "/publishes", nil},
		{http.MethodDelete, base + "/publishes", nil},
		{http.MethodPost, base + "/publishes/1/republish", nil},
		{http.MethodGet, base + "/saved", nil},
		{http.MethodPost, base + "/saved", saved},
		{http.MethodPut, base + "/saved/some-id", saved},
		{http.MethodDelete, base + "/saved/some-id", nil},
		{http.MethodPost, base + "/saved/some-id/publish", nil},
	} {
		if got := ts.status(ep.method, ep.path, ep.body); got != http.StatusNotFound {
			t.Errorf("%s %s gave %d, want 404", ep.method, ep.path, got)
		}
	}
}

// The decode and raw views read the tree's last value rather than the topic
// log, so they have their own "never seen" branch. Asking without a topic is
// the caller's mistake (400); asking for one nobody has published is a fact
// about the broker (404). Both must be distinct from an empty success, which
// would render as a payload of nothing.
func TestTheDecodeAndRawViewsNeedATopicThatHasBeenSeen(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID := deadConnection(t, ts)

	for _, view := range []string{"decode", "raw"} {
		path := "/api/connections/" + connID + "/topic/" + view
		if got := ts.status(http.MethodGet, path, nil); got != http.StatusBadRequest {
			t.Errorf("%s without a topic gave %d, want 400", view, got)
		}
		if got := ts.status(http.MethodGet, path+"?topic=never/seen", nil); got != http.StatusNotFound {
			t.Errorf("%s of an unseen topic gave %d, want 404", view, got)
		}
	}
}
