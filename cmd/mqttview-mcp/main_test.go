package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dgprivate/mqttview/internal/plugins/plc"
)

// fakeMqttview stands in for a running server: enough of the API for the MCP
// client to be exercised without one.
type fakeMqttview struct {
	t        *testing.T
	srv      *httptest.Server
	logins   atomic.Int32
	edges    []plc.Edge
	seq      uint64
	state    plc.State
	status   Status
	mappings []plc.Mapping
	// expire makes the next authenticated read return 401 once, so the
	// re-login path can be exercised.
	expire atomic.Bool
	// mappingError makes the plugin refuse a write with that message.
	mappingError string
}

func newFake(t *testing.T) *fakeMqttview {
	t.Helper()
	f := &fakeMqttview{t: t}
	f.status = Status{TopicPrefix: "plc", MeterPrefix: "mbus2mqtt", Points: 2, Lights: 1, ReadOnly: true}

	mux := http.NewServeMux()

	mux.HandleFunc("/api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Email, Password string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Email != "person@example.com" || body.Password != "right" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.logins.Add(1)
		http.SetCookie(w, &http.Cookie{Name: "mqttview_session", Value: "s", Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: "mqttview_csrf", Value: "csrf-token", Path: "/"})
		w.WriteHeader(http.StatusOK)
	})

	authed := func(fn http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if f.expire.Swap(false) {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if _, err := r.Cookie("mqttview_session"); err != nil {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			fn(w, r)
		}
	}

	mux.HandleFunc(pluginPath+"/status", authed(func(w http.ResponseWriter, _ *http.Request) {
		f.status.Seq = f.seq
		writeJSON(w, f.status)
	}))
	mux.HandleFunc(pluginPath+"/state", authed(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, f.state)
	}))
	mux.HandleFunc(pluginPath+"/edges", authed(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var since uint64
		if v := q.Get("since"); v != "" {
			since, _ = strconv.ParseUint(v, 10, 64)
		}
		var out []plc.Edge
		for _, e := range f.edges {
			// The real endpoint returns only what is newer, which is what makes
			// "the next button pressed" mean anything.
			if since > 0 && e.Seq <= since {
				continue
			}
			if q.Get("rising") == "true" && !e.Rising() {
				continue
			}
			if k := q.Get("kind"); k != "" && string(e.Kind) != k {
				continue
			}
			out = append(out, e)
		}
		writeJSON(w, EdgePage{Edges: out, Seq: f.seq})
	}))
	mux.HandleFunc(pluginPath+"/mappings", authed(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			writeJSON(w, f.mappings)
			return
		}
		// The write path is the one that needs the double-submit token.
		if r.Header.Get("X-CSRF-Token") != "csrf-token" {
			w.WriteHeader(http.StatusForbidden)
			writeJSON(w, map[string]string{"error": "CSRF token missing or invalid"})
			return
		}
		if f.mappingError != "" {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]string{"error": f.mappingError})
			return
		}
		var m plc.Mapping
		_ = json.NewDecoder(r.Body).Decode(&m)
		f.mappings = append(f.mappings, m)
		writeJSON(w, m)
	}))

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// client builds a client pointed at this fake server.
func (f *fakeMqttview) client(t *testing.T) *Client {
	t.Helper()
	c, err := NewClient(f.srv.URL, "person@example.com", "right")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func newTestTools(t *testing.T, f *fakeMqttview) *tools {
	t.Helper()
	c, err := NewClient(f.srv.URL, "person@example.com", "right")
	if err != nil {
		t.Fatal(err)
	}
	return &tools{client: c}
}

func TestNewClientRejectsAnUnusableURL(t *testing.T) {
	if _, err := NewClient("://not a url", "a", "b"); err == nil {
		t.Fatal("an unparseable URL was accepted")
	}
}

func TestLoginFailsLoudly(t *testing.T) {
	f := newFake(t)

	c, err := NewClient(f.srv.URL, "person@example.com", "wrong")
	if err != nil {
		t.Fatal(err)
	}
	err = c.Login(context.Background())
	if err == nil {
		t.Fatal("a wrong password logged in")
	}
	// The message has to name the account, because this is what an operator
	// sees when the server will not start.
	if !strings.Contains(err.Error(), "person@example.com") {
		t.Errorf("error %q does not name the account", err)
	}
}

func TestTheClientLogsInOnceAndReusesTheSession(t *testing.T) {
	f := newFake(t)
	c, _ := NewClient(f.srv.URL, "person@example.com", "right")

	for i := 0; i < 3; i++ {
		if _, err := c.Status(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.logins.Load(); n != 1 {
		t.Fatalf("logged in %d times, want 1", n)
	}
}

func TestTheClientLogsInAgainWhenTheSessionExpires(t *testing.T) {
	f := newFake(t)
	c, _ := NewClient(f.srv.URL, "person@example.com", "right")

	if _, err := c.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A session that has been swept out from under a long-running MCP server
	// must be recovered from, not reported as an error to the agent.
	f.expire.Store(true)
	if _, err := c.Status(context.Background()); err != nil {
		t.Fatalf("the client did not recover from an expired session: %v", err)
	}
	if n := f.logins.Load(); n != 2 {
		t.Fatalf("logged in %d times, want 2", n)
	}
}

func TestAMissingPluginIsSaidPlainly(t *testing.T) {
	f := newFake(t)
	c, _ := NewClient(f.srv.URL, "person@example.com", "right")

	// Nothing is registered at this path, so the mux answers 404 — which is
	// what a disabled plugin looks like from here.
	var out any
	err := c.get(context.Background(), "/does-not-exist", nil, &out)
	if err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("error = %v, want it to name the disabled plugin", err)
	}
}

func TestWaitForSignalAnchorsOnTheCurrentEnd(t *testing.T) {
	f := newFake(t)
	// History exists, but "the next button pressed" must not be answered by it.
	f.edges = []plc.Edge{{Seq: 1, Name: "DI-1-1", To: true, At: time.Now()}}
	f.seq = 1

	tl := newTestTools(t, f)
	_, out, err := tl.waitForSignal(context.Background(), nil, waitInput{TimeoutSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Signals) != 0 || !out.TimedOut {
		t.Fatalf("history was returned as if it were new: %+v", out)
	}
	if !strings.Contains(out.Note, "since=1") {
		t.Errorf("the note should tell the agent where to resume: %q", out.Note)
	}
}

func TestWaitForSignalReturnsAMatchingEdge(t *testing.T) {
	f := newFake(t)
	f.edges = []plc.Edge{{
		Seq: 5, Name: "DI-9-9", Address: 99, Label: "Kitchen switch",
		Location: "kitchen", Kind: plc.KindInput, From: false, To: true, At: time.Now(),
	}}
	f.seq = 5

	tl := newTestTools(t, f)
	// since is given, so the anchor step is skipped and the edge is visible.
	_, out, err := tl.waitForSignal(context.Background(), nil, waitInput{Since: 4, TimeoutSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Signals) != 1 {
		t.Fatalf("got %d signals: %+v", len(out.Signals), out)
	}
	s := out.Signals[0]
	if s.Name != "DI-9-9" || s.Label != "Kitchen switch" || s.Transition != "off -> on" {
		t.Errorf("signal flattened wrongly: %+v", s)
	}
	if out.TimedOut {
		t.Error("timed_out was set even though a signal arrived")
	}
}

func TestWaitForSignalCapsTheTimeout(t *testing.T) {
	f := newFake(t)
	tl := newTestTools(t, f)

	// An agent asking for an hour must not get one: the HTTP call and the
	// long poll behind it are both bounded.
	start := time.Now()
	_, out, err := tl.waitForSignal(context.Background(), nil, waitInput{Since: 1, TimeoutSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	if !out.TimedOut {
		t.Error("expected a timeout")
	}
	if time.Since(start) > time.Minute {
		t.Errorf("the call took %v, which is past the cap", time.Since(start))
	}
}

func TestRecentSignalsFilters(t *testing.T) {
	f := newFake(t)
	f.edges = []plc.Edge{
		{Seq: 1, Name: "DI-1", Kind: plc.KindInput, From: true, To: false, At: time.Now()},
		{Seq: 2, Name: "DI-2", Kind: plc.KindInput, From: false, To: true, At: time.Now()},
	}
	f.seq = 2
	tl := newTestTools(t, f)

	_, all, err := tl.recentSignals(context.Background(), nil, recentInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Signals) != 2 {
		t.Fatalf("got %d signals", len(all.Signals))
	}

	_, rising, err := tl.recentSignals(context.Background(), nil, recentInput{RisingOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rising.Signals) != 1 || rising.Signals[0].Name != "DI-2" {
		t.Fatalf("rising filter gave %+v", rising.Signals)
	}
}

func TestRecentSignalsSaysWhenTheLogIsEmpty(t *testing.T) {
	tl := newTestTools(t, newFake(t))

	_, out, err := tl.recentSignals(context.Background(), nil, recentInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Signals) != 0 || out.Note == "" {
		t.Fatalf("an empty log should explain itself: %+v", out)
	}
}

func TestFindPointsSearchesEveryField(t *testing.T) {
	f := newFake(t)
	yes, no := true, false
	temp := 21.5
	f.state = plc.State{Points: []*plc.Point{
		{Name: "DI-31-5", Address: 245, Kind: plc.KindInput, Label: "Hallway motion", Location: "hallway", Bool: &yes},
		{Name: "DO-1-1", Address: 1, Kind: plc.KindOutput, Bool: &no},
		{Name: "T-2-1", Address: 9, Kind: plc.KindTemperature, Number: &temp},
	}}
	tl := newTestTools(t, f)

	for _, q := range []string{"hallway", "DI-31", "245", "motion"} {
		_, out, err := tl.findPoints(context.Background(), nil, findInput{Query: q})
		if err != nil {
			t.Fatal(err)
		}
		if out.Total != 1 || out.Points[0].Name != "DI-31-5" {
			t.Errorf("query %q found %+v", q, out.Points)
		}
	}

	_, out, err := tl.findPoints(context.Background(), nil, findInput{Kind: "temperature"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Total != 1 || out.Points[0].Value != "21.50" {
		t.Fatalf("temperature lookup gave %+v", out.Points)
	}

	// on/off is what an agent can reason about; a raw bool is not.
	_, all, _ := tl.findPoints(context.Background(), nil, findInput{})
	values := map[string]string{}
	for _, p := range all.Points {
		values[p.Name] = p.Value
	}
	if values["DI-31-5"] != "on" || values["DO-1-1"] != "off" {
		t.Errorf("boolean values rendered as %+v", values)
	}
}

func TestFindPointsSaysWhenItTruncated(t *testing.T) {
	f := fakeWithPoints(t, 60)
	tl := newTestTools(t, f)

	_, out, err := tl.findPoints(context.Background(), nil, findInput{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	// Silent truncation reads as "that is all of them", which is how a point
	// gets missed.
	if len(out.Points) != 10 || out.Total != 60 || out.Skipped != 50 || out.Note == "" {
		t.Fatalf("truncation was not reported: len=%d total=%d skipped=%d note=%q",
			len(out.Points), out.Total, out.Skipped, out.Note)
	}
}

func TestLightsAndOnlyErrors(t *testing.T) {
	f := newFake(t)
	f.state = plc.State{
		Lights: []*plc.Light{
			{Address: 1, Name: "LI-1", ActualLevel: 254},
			{Address: 2, Name: "LI-2", Error: "no reply"},
		},
		Summary: plc.Summary{Lights: 2, LightsOn: 1, LightsWithError: 1},
	}
	tl := newTestTools(t, f)

	_, all, err := tl.lights(context.Background(), nil, lightsInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Lights) != 2 || all.Total != 2 || all.On != 1 || all.WithErrors != 1 {
		t.Fatalf("lights summary wrong: %+v", all)
	}
	if all.Lights[0].Percent != 100 {
		t.Errorf("a full ballast reported %d%%", all.Lights[0].Percent)
	}

	_, faults, err := tl.lights(context.Background(), nil, lightsInput{OnlyErrors: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(faults.Lights) != 1 || faults.Lights[0].Name != "LI-2" {
		t.Fatalf("only_errors gave %+v", faults.Lights)
	}
}

func TestOverviewSummarises(t *testing.T) {
	f := newFake(t)
	f.state = plc.State{
		Summary:  plc.Summary{Inputs: 264, Outputs: 80, Lights: 64, Shades: 1},
		Watchdog: &plc.Watchdog{Alive: true, UptimeS: 7200, AlarmMode: "disarmed", Streams: map[string]plc.Stream{"di": {AgeS: 3}}},
		Electricity: []*plc.Electricity{{
			Name:      "house",
			Phases:    [3]plc.Phase{{Voltage: 230, Current: 1}, {Voltage: 231, Current: 2}, {Voltage: 232, Current: 3}},
			Frequency: 50,
		}},
		Meters: []*plc.Meter{{
			Name: "water", Available: true,
			Readings: map[string]float64{"water_volume_total": 53.6},
			Units:    map[string]string{"water_volume_total": "m³"},
		}},
	}
	tl := newTestTools(t, f)

	_, out, err := tl.overview(context.Background(), nil, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Counts.Inputs != 264 || out.Shades != 1 || out.TopicPrefix != "plc" {
		t.Fatalf("counts wrong: %+v", out)
	}
	if out.Watchdog == nil || out.Watchdog.AlarmMode != "disarmed" || out.Watchdog.UptimeHours != 2 {
		t.Fatalf("watchdog wrong: %+v", out.Watchdog)
	}
	if out.Watchdog.Streams["di"] != 3 {
		t.Errorf("stream ages wrong: %+v", out.Watchdog.Streams)
	}
	if len(out.Electricity) != 1 || out.Electricity[0].VoltagesV[0] != 230 {
		t.Fatalf("electricity wrong: %+v", out.Electricity)
	}
	// The unit rides along, because an agent reading "53.6" has nowhere else
	// to learn what it is.
	if len(out.Meters) != 1 || out.Meters[0].Units["water_volume_total"] != "m³" {
		t.Fatalf("meter units lost: %+v", out.Meters)
	}
}

func TestOverviewSaysWhenNothingHasArrived(t *testing.T) {
	f := newFake(t)
	f.status.Points = 0
	tl := newTestTools(t, f)

	_, out, err := tl.overview(context.Background(), nil, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Note == "" {
		t.Fatal("an empty PLC should explain itself rather than look healthy")
	}
}

func TestNamePointWritesAndClears(t *testing.T) {
	f := newFake(t)
	tl := newTestTools(t, f)

	_, out, err := tl.namePoint(context.Background(), nil, nameInput{
		Name: "DI-9-9", Label: "Kitchen switch", Location: "kitchen", Notes: "turns on LI-5",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Removed || out.Saved.Label != "Kitchen switch" {
		t.Fatalf("naming gave %+v", out)
	}
	if !strings.Contains(out.Note, "Kitchen switch") {
		t.Errorf("the note should quote the new name: %q", out.Note)
	}
	if len(f.mappings) != 1 {
		t.Fatalf("the server received %d mappings", len(f.mappings))
	}

	// Every field empty is the delete.
	_, cleared, err := tl.namePoint(context.Background(), nil, nameInput{Name: "DI-9-9"})
	if err != nil {
		t.Fatal(err)
	}
	if !cleared.Removed || !strings.Contains(cleared.Note, "removed") {
		t.Fatalf("clearing gave %+v", cleared)
	}
}

func TestNamePointNeedsAName(t *testing.T) {
	tl := newTestTools(t, newFake(t))

	if _, _, err := tl.namePoint(context.Background(), nil, nameInput{Label: "no name"}); err == nil {
		t.Fatal("a mapping with no point name was accepted")
	}
}

// fakeWithPoints builds a server holding n points, for the truncation path.
func fakeWithPoints(t *testing.T, n int) *fakeMqttview {
	t.Helper()
	f := newFake(t)
	off := false
	for i := 0; i < n; i++ {
		f.state.Points = append(f.state.Points, &plc.Point{
			Name: fmt.Sprintf("DI-%d", i), Address: i, Kind: plc.KindInput, Bool: &off,
		})
	}
	return f
}

func TestEnvFallsBackToTheDefault(t *testing.T) {
	if got := env("MQTTVIEW_TEST_ABSENT", "fallback"); got != "fallback" {
		t.Errorf("env = %q", got)
	}
	t.Setenv("MQTTVIEW_TEST_PRESENT", "value")
	if got := env("MQTTVIEW_TEST_PRESENT", "fallback"); got != "value" {
		t.Errorf("env = %q", got)
	}
}

func TestNewServerRegistersEveryTool(t *testing.T) {
	f := newFake(t)
	c, _ := NewClient(f.srv.URL, "person@example.com", "right")

	// The server is built without connecting anything; this is a wiring check,
	// so that a tool added to the code but not to newServer is caught.
	if s := newServer(c, ""); s == nil {
		t.Fatal("newServer returned nothing")
	}
}

// The startup path: what happens before a single tool is ever called. Each of
// these is a mistake somebody will make, and each has to say which one it was.

func TestRunRefusesToStartWithoutCredentials(t *testing.T) {
	t.Setenv("MQTTVIEW_EMAIL", "")
	t.Setenv("MQTTVIEW_PASSWORD", "")

	err := run(context.Background(), nil)
	if err == nil {
		t.Fatal("the server started with no credentials")
	}
	if !strings.Contains(err.Error(), "MQTTVIEW_EMAIL") {
		t.Errorf("error %q does not name the variable to set", err)
	}
}

func TestRunRejectsAnUnusableURL(t *testing.T) {
	err := run(context.Background(), []string{
		"-url", "://nonsense", "-email", "a@example.com", "-password", "p",
	})
	if err == nil {
		t.Fatal("an unparseable URL was accepted")
	}
}

func TestRunReportsARefusedLogin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid credentials"}`))
	}))
	defer srv.Close()

	err := run(context.Background(), []string{
		"-url", srv.URL, "-email", "a@example.com", "-password", "wrong",
	})
	if err == nil {
		t.Fatal("a refused login started the server anyway")
	}
	// The agent transcript has to say why, not just that something failed.
	if !strings.Contains(err.Error(), "credentials") && !strings.Contains(err.Error(), "401") {
		t.Errorf("error %q does not explain the refusal", err)
	}
}

func TestRunRejectsAnUnknownFlag(t *testing.T) {
	if err := run(context.Background(), []string{"-nonsense"}); err == nil {
		t.Fatal("an unknown flag was accepted")
	}
}

// What the agent is told when mqttview itself is the problem. An MCP tool that
// returns an empty result on failure is the worst case here: the agent then
// writes PLC logic against a signal list it believes is complete.

func TestAnUnreachableMqttviewIsReportedNotSwallowed(t *testing.T) {
	// A port nothing listens on: the connection is refused immediately.
	c, err := NewClient("http://127.0.0.1:1", "person@example.com", "right")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.Login(ctx); err == nil {
		t.Fatal("login against a closed port succeeded")
	} else if !strings.Contains(err.Error(), "cannot reach mqttview") {
		t.Errorf("error %q does not say the server was unreachable", err)
	}

	if _, err := c.Status(ctx); err == nil {
		t.Error("a status read against a closed port succeeded")
	}
	if _, err := c.SetMapping(ctx, "", plc.Mapping{Name: "DI-1-1", Label: "x"}); err == nil {
		t.Error("a write against a closed port succeeded")
	}
}

func TestAServerErrorIsPassedThroughWithItsStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/auth/login" {
			http.SetCookie(w, &http.Cookie{Name: "mqttview_session", Value: "s", Path: "/"})
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "person@example.com", "right")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	_, err = c.Status(ctx)
	if err == nil {
		t.Fatal("a 500 was treated as a reading")
	}
	// "Internal Server Error" and not "the plugin is not enabled": the two
	// send whoever is reading in opposite directions.
	if !strings.Contains(err.Error(), "Internal Server Error") {
		t.Errorf("error %q does not carry the status", err)
	}
}

func TestAResponseThatIsNotJSONIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/auth/login" {
			http.SetCookie(w, &http.Cookie{Name: "mqttview_session", Value: "s", Path: "/"})
			w.WriteHeader(http.StatusOK)
			return
		}
		// A proxy's HTML error page is the realistic version of this.
		_, _ = w.Write([]byte("<html>gateway timeout</html>"))
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "person@example.com", "right")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := c.Status(context.Background()); err == nil {
		t.Fatal("an HTML page was accepted as a status")
	}
}

func TestARefusedNamingCarriesTheReason(t *testing.T) {
	f := newFake(t)
	c := f.client(t)

	// The plugin refuses a name it cannot attach to a point, and the agent
	// needs the reason to correct itself rather than retry the same thing.
	f.mappingError = "name is required: it is the PLC's own name for the point"

	_, err := c.SetMapping(context.Background(), "", plc.Mapping{Name: "", Label: "Kitchen"})
	if err == nil {
		t.Fatal("an empty name was accepted")
	}
	if !strings.Contains(err.Error(), "name is required") {
		t.Errorf("error %q does not carry the plugin's reason", err)
	}
}

func TestASessionThatExpiresDuringAWriteIsNotSilentlyLost(t *testing.T) {
	f := newFake(t)
	c := f.client(t)

	if err := c.Login(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.expire.Store(true)

	// A write does not retry on 401; it says so, because retrying a write
	// without knowing whether the first one landed is worse than failing.
	if _, err := c.SetMapping(context.Background(), "", plc.Mapping{Name: "DI-1-1", Label: "Kitchen"}); err == nil {
		t.Fatal("a write against an expired session reported success")
	}
}
