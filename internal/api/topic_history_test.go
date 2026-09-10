package api_test

import (
	"encoding/csv"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dgprivate/mqttview/internal/testutil"
)

// subscribedBroker connects and subscribes to everything, because a published
// message only comes back — and so only reaches the topic log — through a
// subscription that matches it.
func subscribedBroker(t *testing.T, ts *testServer) string {
	t.Helper()

	connID, _ := connectedBroker(t, ts)
	ts.decode(ts.do(http.MethodPost, "/api/connections/"+connID+"/subscribe",
		map[string]any{"subscriptions": []map[string]any{{"filter": "#", "qos": 0}}}),
		http.StatusOK, nil)
	return connID
}

// publishAndWait publishes a payload and blocks until the topic log has as
// many entries as expected, because a publish loops back through the broker.
func publishAndWait(t *testing.T, ts *testServer, connID, topic, payload string, want int) {
	t.Helper()

	ts.decode(ts.do(http.MethodPost, "/api/connections/"+connID+"/publish", map[string]any{
		"topic": topic, "payload": payload, "qos": 0,
	}), http.StatusOK, nil)

	testutil.WaitFor(t, 10*time.Second, "the message to reach the topic log", func() bool {
		return len(topicHistory(t, ts, connID, topic)) >= want
	})
}

type historyEntry struct {
	Seq        uint64    `json:"seq"`
	ReceivedAt time.Time `json:"receivedAt"`
	Payload    string    `json:"payload"`
	Base64     bool      `json:"base64"`
	Size       int       `json:"size"`
	QoS        byte      `json:"qos"`
	Retain     bool      `json:"retain"`
	Truncated  bool      `json:"truncated"`
}

func topicHistory(t *testing.T, ts *testServer, connID, topic string) []historyEntry {
	t.Helper()

	resp := ts.do(http.MethodGet, "/api/connections/"+connID+"/topic/history?topic="+topic, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var body struct {
		Entries []historyEntry `json:"entries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode history: %v", err)
	}
	return body.Entries
}

func TestATopicsHistoryComesBackInTheOrderItHappened(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID := subscribedBroker(t, ts)

	for i, v := range []string{"first", "second", "third"} {
		publishAndWait(t, ts, connID, "sensor/a", v, i+1)
	}

	got := topicHistory(t, ts, connID, "sensor/a")
	if len(got) != 3 {
		t.Fatalf("history has %d entries, want 3", len(got))
	}
	for i, want := range []string{"first", "second", "third"} {
		if got[i].Payload != want {
			t.Errorf("entry %d is %q, want %q", i, got[i].Payload, want)
		}
	}
}

// The whole reason the per-topic log exists: traffic on one topic must not
// take another topic's past with it.
func TestOneTopicsTrafficDoesNotEraseAnothersHistory(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID := subscribedBroker(t, ts)

	publishAndWait(t, ts, connID, "quiet/one", "kept", 1)
	for i := range 40 {
		publishAndWait(t, ts, connID, "busy/one", strings.Repeat("x", 10)+string(rune('a'+i%26)), i+1)
	}

	got := topicHistory(t, ts, connID, "quiet/one")
	if len(got) != 1 || got[0].Payload != "kept" {
		t.Errorf("the quiet topic's history is %+v, want the single message it received", got)
	}
}

func TestADiffOffersTheCurrentValueAndTheOneBeforeIt(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID := subscribedBroker(t, ts)

	publishAndWait(t, ts, connID, "sensor/b", `{"t":20}`, 1)
	publishAndWait(t, ts, connID, "sensor/b", `{"t":21}`, 2)

	var body struct {
		Current  historyEntry  `json:"current"`
		Previous *historyEntry `json:"previous"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/connections/"+connID+"/topic/diff?topic=sensor/b", nil),
		http.StatusOK, &body)

	if body.Current.Payload != `{"t":21}` {
		t.Errorf("current is %q, want the newest message", body.Current.Payload)
	}
	if body.Previous == nil {
		t.Fatal("no previous message offered after two messages")
	}
	if body.Previous.Payload != `{"t":20}` {
		t.Errorf("previous is %q, want the earlier message", body.Previous.Payload)
	}
}

// Returning the same message twice would render as "no changes" and be read
// as "nothing happened", which is a different claim entirely.
func TestATopicSeenOnceOffersNothingToCompareAgainst(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID := subscribedBroker(t, ts)

	publishAndWait(t, ts, connID, "sensor/once", "only", 1)

	var body struct {
		Current  historyEntry  `json:"current"`
		Previous *historyEntry `json:"previous"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/connections/"+connID+"/topic/diff?topic=sensor/once", nil),
		http.StatusOK, &body)

	if body.Previous != nil {
		t.Errorf("a previous message was offered (%q) after a single message", body.Previous.Payload)
	}
}

func TestExportingATopicProducesASpreadsheetWithAHeaderRow(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID := subscribedBroker(t, ts)

	publishAndWait(t, ts, connID, "sensor/c", "11.5", 1)
	publishAndWait(t, ts, connID, "sensor/c", "12.5", 2)

	resp := ts.do(http.MethodGet, "/api/connections/"+connID+"/topic/export?topic=sensor/c&format=csv", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment;") {
		t.Errorf("Content-Disposition is %q; a browser would render this instead of saving it", cd)
	}

	rows, err := csv.NewReader(resp.Body).ReadAll()
	if err != nil {
		t.Fatalf("the export is not valid CSV: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want a header and two messages", len(rows))
	}
	if rows[0][0] != "received_at" || rows[0][6] != "payload" {
		t.Errorf("header row is %v, want named columns", rows[0])
	}
	if rows[1][6] != "11.5" || rows[2][6] != "12.5" {
		t.Errorf("payload column holds %q and %q, want the two published values", rows[1][6], rows[2][6])
	}
}

func TestExportingAsJSONReturnsTheSameEntriesAsTheTimeline(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID := subscribedBroker(t, ts)

	publishAndWait(t, ts, connID, "sensor/d", "alpha", 1)

	var body struct {
		Topic   string         `json:"topic"`
		Entries []historyEntry `json:"entries"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/connections/"+connID+"/topic/export?topic=sensor/d&format=json", nil),
		http.StatusOK, &body)

	if body.Topic != "sensor/d" || len(body.Entries) != 1 || body.Entries[0].Payload != "alpha" {
		t.Errorf("export is %+v, want the one message on sensor/d", body)
	}
}

func TestAnUnknownExportFormatIsRefused(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID := subscribedBroker(t, ts)
	publishAndWait(t, ts, connID, "sensor/e", "x", 1)

	if got := ts.status(http.MethodGet, "/api/connections/"+connID+"/topic/export?topic=sensor/e&format=xlsx", nil); got != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an unsupported format", got)
	}
}

func TestChartingReadsANumberOutOfAJSONPayload(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID := subscribedBroker(t, ts)

	publishAndWait(t, ts, connID, "sensor/f", `{"env":{"temp":19.5}}`, 1)
	publishAndWait(t, ts, connID, "sensor/f", `{"env":{"temp":20.5}}`, 2)

	var body struct {
		Points []struct {
			Value float64 `json:"v"`
		} `json:"points"`
		Skipped int `json:"skipped"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/connections/"+connID+"/topic/series?topic=sensor/f&field=env.temp", nil),
		http.StatusOK, &body)

	if len(body.Points) != 2 {
		t.Fatalf("got %d points, want 2", len(body.Points))
	}
	if body.Points[0].Value != 19.5 || body.Points[1].Value != 20.5 {
		t.Errorf("values are %v and %v, want 19.5 and 20.5", body.Points[0].Value, body.Points[1].Value)
	}
}

// A topic that carries a status string between readings is normal, and must
// not fail the whole request.
func TestAPayloadThatIsNotANumberIsSkippedRatherThanFailing(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID := subscribedBroker(t, ts)

	publishAndWait(t, ts, connID, "sensor/g", "21.0", 1)
	publishAndWait(t, ts, connID, "sensor/g", "unavailable", 2)
	publishAndWait(t, ts, connID, "sensor/g", "22.0", 3)

	var body struct {
		Points []struct {
			V float64 `json:"v"`
		} `json:"points"`
		Skipped int `json:"skipped"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/connections/"+connID+"/topic/series?topic=sensor/g", nil),
		http.StatusOK, &body)

	if len(body.Points) != 2 {
		t.Errorf("got %d points, want the 2 numeric ones", len(body.Points))
	}
	if body.Skipped != 1 {
		t.Errorf("reported %d skipped, want 1 — the UI says so rather than showing a gap", body.Skipped)
	}
}

func TestTheFieldChooserListsWhatCanActuallyBeCharted(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID := subscribedBroker(t, ts)

	publishAndWait(t, ts, connID, "sensor/h",
		`{"temp":21.5,"name":"kitchen","on":true,"nested":{"hum":40}}`, 1)

	var body struct {
		Fields []string `json:"fields"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/connections/"+connID+"/topic/fields?topic=sensor/h", nil),
		http.StatusOK, &body)

	has := func(want string) bool {
		for _, f := range body.Fields {
			if f == want {
				return true
			}
		}
		return false
	}
	for _, want := range []string{"temp", "on", "nested.hum"} {
		if !has(want) {
			t.Errorf("%q is chartable but was not offered; got %v", want, body.Fields)
		}
	}
	if has("name") {
		t.Errorf("%q is a string and cannot be charted, but was offered", "name")
	}
}

func TestABinaryPayloadComesBackAsBase64RatherThanMojibake(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID := subscribedBroker(t, ts)

	// 0xff is not valid UTF-8; a JSON encoder would replace it silently.
	ts.decode(ts.do(http.MethodPost, "/api/connections/"+connID+"/publish", map[string]any{
		"topic": "bin/a", "payload": "//79", "payloadBase64": true,
	}), http.StatusOK, nil)

	testutil.WaitFor(t, 10*time.Second, "the binary message to arrive", func() bool {
		return len(topicHistory(t, ts, connID, "bin/a")) >= 1
	})

	got := topicHistory(t, ts, connID, "bin/a")
	if !got[0].Base64 {
		t.Fatalf("entry is %+v, want it flagged as base64", got[0])
	}
	if got[0].Payload != "//79" {
		t.Errorf("payload round-tripped to %q, want %q", got[0].Payload, "//79")
	}
}

func TestTheseEndpointsSayWhichParameterIsMissing(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID := subscribedBroker(t, ts)

	for _, path := range []string{"history", "diff", "series", "fields", "export"} {
		got := ts.status(http.MethodGet, "/api/connections/"+connID+"/topic/"+path, nil)
		if got != http.StatusBadRequest {
			t.Errorf("%s without a topic returned %d, want 400", path, got)
		}
	}
}

func TestATopicWithNoHistoryIsNotFoundRatherThanEmpty(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID := subscribedBroker(t, ts)

	for _, path := range []string{"diff", "export"} {
		got := ts.status(http.MethodGet, "/api/connections/"+connID+"/topic/"+path+"?topic=never/seen", nil)
		if got != http.StatusNotFound {
			t.Errorf("%s for an unseen topic returned %d, want 404", path, got)
		}
	}
}

func TestABadSinceWindowIsRefusedWithAnExample(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID := subscribedBroker(t, ts)
	publishAndWait(t, ts, connID, "sensor/i", "1", 1)

	resp := ts.do(http.MethodGet, "/api/connections/"+connID+"/topic/series?topic=sensor/i&since=yesterday", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "15m") {
		t.Errorf("the error does not show an acceptable value: %s", raw)
	}
}

func TestTopicHistoryNeedsASession(t *testing.T) {
	ts := newTestServer(t)

	for _, path := range []string{"history", "diff", "series", "fields", "export"} {
		got := ts.status(http.MethodGet, "/api/connections/any/topic/"+path+"?topic=a/b", nil)
		if got != http.StatusUnauthorized {
			t.Errorf("%s without a session returned %d, want 401", path, got)
		}
	}
}

// "This broker has no clients" and "this broker will not say" are different
// facts, and a page of zeroes cannot tell them apart. The endpoint reports
// which one it is.
func TestTheBrokerStatusPageSaysWhenItIsCollectingNothing(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID := subscribedBroker(t, ts)

	var body struct {
		Enabled   bool `json:"enabled"`
		Connected bool `json:"connected"`
		Stats     struct {
			Available bool `json:"available"`
			Clients   struct {
				Connected *int64 `json:"connected"`
			} `json:"clients"`
		} `json:"stats"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/connections/"+connID+"/sys", nil), http.StatusOK, &body)

	if body.Enabled {
		t.Error("broker statistics are on for a connection that never asked for them")
	}
	if body.Stats.Available {
		t.Error("statistics are reported as available with no subscription held")
	}
	if body.Stats.Clients.Connected != nil {
		t.Errorf("a client count of %d was reported without collecting anything",
			*body.Stats.Clients.Connected)
	}
	if !body.Connected {
		t.Error("the connection is live but the page says otherwise")
	}
}

func TestTurningTheBrokerStatisticsOnIsRememberedOnTheConnection(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID := subscribedBroker(t, ts)

	var conn struct {
		Name    string `json:"name"`
		URL     string `json:"url"`
		Version string `json:"version"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/connections/"+connID, nil), http.StatusOK, &conn)

	ts.decode(ts.do(http.MethodPut, "/api/connections/"+connID, map[string]any{
		"name": conn.Name, "url": conn.URL, "version": conn.Version,
		"sysStats": true,
	}), http.StatusOK, nil)

	var body struct {
		Enabled bool `json:"enabled"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/connections/"+connID+"/sys", nil), http.StatusOK, &body)
	if !body.Enabled {
		t.Error("the setting did not survive being written to the connection")
	}
}

func TestTheBrokerStatusPageNeedsASession(t *testing.T) {
	ts := newTestServer(t)

	if got := ts.status(http.MethodGet, "/api/connections/any/sys", nil); got != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", got)
	}
}
