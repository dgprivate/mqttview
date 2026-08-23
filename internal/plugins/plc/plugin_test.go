package plc

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/dgprivate/mqttview/internal/auth"
	"github.com/dgprivate/mqttview/internal/mqttc"
	"github.com/dgprivate/mqttview/internal/plugin"
	"github.com/dgprivate/mqttview/internal/store"
	"github.com/dgprivate/mqttview/internal/testutil"
)

// fakeHost is the plugin runtime, reduced to what a plugin can actually
// observe. Publishing records rather than sends, which is what lets a test
// assert on a house command without a house.
type fakeHost struct {
	mu         sync.Mutex
	settings   map[string]any
	kv         map[string]string
	conns      []*mqttc.Conn
	published  []mqttc.PublishRequest
	subscribed []mqttc.Subscription
	events     []string
	// publishErr makes the broker refuse, which is how the command endpoint's
	// failure branch is reached without a broker.
	publishErr error
}

func newFakeHost(settings map[string]any) *fakeHost {
	if settings == nil {
		settings = map[string]any{}
	}
	return &fakeHost{settings: settings, kv: map[string]string{}}
}

func (h *fakeHost) Logger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
func (h *fakeHost) Store() plugin.KV     { return (*fakeKV)(h) }
func (h *fakeHost) Settings() map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.settings
}
func (h *fakeHost) Connections() []*mqttc.Conn { return h.conns }
func (h *fakeHost) Connection(string) (*mqttc.Conn, bool) {
	return nil, false
}
func (h *fakeHost) Publish(_ context.Context, _ string, req mqttc.PublishRequest) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.publishErr != nil {
		return h.publishErr
	}
	h.published = append(h.published, req)
	return nil
}
func (h *fakeHost) Subscribe(_ context.Context, _ string, subs []mqttc.Subscription) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.subscribed = append(h.subscribed, subs...)
	return nil
}
func (h *fakeHost) Unsubscribe(context.Context, string, []string) error { return nil }
func (h *fakeHost) Emit(event string, _ any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, event)
}

func (h *fakeHost) sentCommands() []mqttc.PublishRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]mqttc.PublishRequest(nil), h.published...)
}

type fakeKV fakeHost

func (k *fakeKV) Get(key string) (string, bool, error) {
	h := (*fakeHost)(k)
	h.mu.Lock()
	defer h.mu.Unlock()
	v, ok := h.kv[key]
	return v, ok, nil
}
func (k *fakeKV) Set(key, value string) error {
	h := (*fakeHost)(k)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.kv[key] = value
	return nil
}
func (k *fakeKV) Delete(key string) error {
	h := (*fakeHost)(k)
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.kv, key)
	return nil
}
func (k *fakeKV) All() (map[string]string, error) {
	h := (*fakeHost)(k)
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]string{}
	for kk, vv := range h.kv {
		out[kk] = vv
	}
	return out, nil
}

// startPlugin builds a plugin on a fake host and mounts its routes, with a
// signed-in operator on every request unless the test says otherwise.
func startPlugin(t *testing.T, settings map[string]any) (*Plugin, *fakeHost, *httptest.Server) {
	t.Helper()

	h := newFakeHost(settings)
	p := &Plugin{}
	if err := p.Init(context.Background(), h); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			user := store.User{ID: "u1", Email: "operator@example.com", Role: store.RoleOperator}
			if req.Header.Get("X-Test-Role") == "viewer" {
				user.Role = store.RoleViewer
			}
			next.ServeHTTP(w, req.WithContext(auth.WithUser(req.Context(), user)))
		})
	})
	p.Routes(r)

	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return p, h, srv
}

func get(t *testing.T, srv *httptest.Server, path string, out any) int {
	t.Helper()
	resp, err := srv.Client().Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func post(t *testing.T, srv *httptest.Server, method, path string, body any, role string) (int, string) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if role != "" {
		req.Header.Set("X-Test-Role", role)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// postRawPLC sends a body the JSON decoder will refuse.
func postRawPLC(t *testing.T, srv *httptest.Server, method, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// twoConnections builds the pair a second PLC would produce, so a test can
// check that an ambiguous command is refused rather than guessed at.
func twoConnections(t *testing.T) []*mqttc.Conn {
	t.Helper()
	m := mqttc.NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { m.Shutdown(context.Background()) })

	var out []*mqttc.Conn
	for _, id := range []string{"c1", "c2"} {
		spec := mqttc.ConnectionSpec{ID: id, Name: id, URL: "mqtt://127.0.0.1:1", Version: mqttc.V311}
		if err := spec.Normalize(); err != nil {
			t.Fatal(err)
		}
		c, err := m.Upsert(context.Background(), spec)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

func TestPluginMetaDescribesItself(t *testing.T) {
	meta := (&Plugin{}).Meta()

	if meta.ID != ID || meta.Panel == "" || meta.Description == "" {
		t.Fatalf("meta is incomplete: %+v", meta)
	}
	// Control settings must exist and default to off, because the panel reads
	// these to decide whether to render a button that acts on a house.
	found := map[string]bool{}
	for _, f := range meta.SettingsSchema {
		found[f.Key] = true
		if f.Key == "allowControl" || f.Key == "allowDigitalOutputs" {
			if f.Default != false {
				t.Errorf("setting %q defaults to %v, want false", f.Key, f.Default)
			}
		}
	}
	for _, want := range []string{"topicPrefix", "meterPrefix", "allowControl", "allowDigitalOutputs", "commandTopic"} {
		if !found[want] {
			t.Errorf("setting %q is missing", want)
		}
	}
}

func TestSubscriptionsFollowTheConfiguredPrefixes(t *testing.T) {
	p, _, _ := startPlugin(t, map[string]any{"topicPrefix": "house", "meterPrefix": "meters"})

	subs := p.Subscriptions()
	if len(subs) != 2 || subs[0].Filter != "house/#" || subs[1].Filter != "meters/#" {
		t.Fatalf("subscriptions = %+v", subs)
	}

	// An empty meter prefix means the meters are somebody else's problem.
	p2, _, _ := startPlugin(t, map[string]any{"meterPrefix": ""})
	if subs := p2.Subscriptions(); len(subs) != 1 {
		t.Fatalf("subscriptions with no meter prefix = %+v", subs)
	}
}

func TestQoSSettingIsHonouredAndBounded(t *testing.T) {
	for _, tc := range []struct {
		setting string
		want    byte
	}{{"0", 0}, {"1", 1}, {"2", 2}, {"9", 0}, {"nonsense", 0}} {
		p, _, _ := startPlugin(t, map[string]any{"subscribeQos": tc.setting})
		if got := p.Subscriptions()[0].QoS; got != tc.want {
			t.Errorf("qos %q gave %d, want %d", tc.setting, got, tc.want)
		}
	}
}

func TestHandleMessageBuildsStateAndNotifiesTheBrowser(t *testing.T) {
	p, h, srv := startPlugin(t, nil)

	p.HandleMessage(context.Background(), mqttc.Message{
		ConnectionID: "c1",
		Topic:        "plc/digital/input/245",
		Payload:      []byte(`{"address":245,"name":"DI-31-5","value":false}`),
		ReceivedAt:   time.Now(),
	})

	var state State
	if code := get(t, srv, "/state", &state); code != http.StatusOK {
		t.Fatalf("state = %d", code)
	}
	if len(state.Points) != 1 || state.Points[0].Name != "DI-31-5" {
		t.Fatalf("state = %+v", state.Points)
	}

	// The batching loop emits at most four times a second; wait for one.
	testutil.WaitFor(t, 5*time.Second, "a change event to reach the browser", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return len(h.events) > 0
	})
}

func TestAMessageOutsideThePrefixIsIgnored(t *testing.T) {
	p, _, srv := startPlugin(t, nil)

	p.HandleMessage(context.Background(), mqttc.Message{
		ConnectionID: "c1", Topic: "homeassistant/sensor/x/state", Payload: []byte(`{}`),
	})

	var state State
	get(t, srv, "/state", &state)
	if len(state.Points) != 0 {
		t.Fatalf("an unrelated topic became state: %+v", state.Points)
	}
}

func TestStatusReportsWhatTheUIReads(t *testing.T) {
	p, _, srv := startPlugin(t, nil)
	p.HandleMessage(context.Background(), mqttc.Message{
		ConnectionID: "c1", Topic: "plc/dali/light/1",
		Payload: []byte(`{"address":1,"name":"LI-1","actual_level":254}`),
	})

	var status struct {
		TopicPrefix string `json:"topicPrefix"`
		Lights      int    `json:"lights"`
		ReadOnly    bool   `json:"readOnly"`
	}
	if code := get(t, srv, "/status", &status); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if status.TopicPrefix != "plc" || status.Lights != 1 {
		t.Fatalf("status = %+v", status)
	}
	// Control is off by default, so the panel must be told it is read only.
	if !status.ReadOnly {
		t.Error("readOnly is false even though control is off")
	}
}

func TestEdgesEndpoint(t *testing.T) {
	p, _, srv := startPlugin(t, nil)

	for _, v := range []string{"false", "true", "false"} {
		p.HandleMessage(context.Background(), mqttc.Message{
			ConnectionID: "c1", Topic: "plc/digital/input/99",
			Payload: []byte(`{"address":99,"name":"DI-9-9","value":` + v + `}`),
		})
	}

	var page struct {
		Edges []Edge `json:"edges"`
		Seq   uint64 `json:"seq"`
	}
	get(t, srv, "/edges", &page)
	if len(page.Edges) != 2 {
		t.Fatalf("got %d edges, want 2 (the first value is not a transition)", len(page.Edges))
	}

	var rising struct {
		Edges []Edge `json:"edges"`
	}
	get(t, srv, "/edges?rising=true", &rising)
	if len(rising.Edges) != 1 || !rising.Edges[0].Rising() {
		t.Fatalf("rising filter gave %+v", rising.Edges)
	}

	var since struct {
		Edges []Edge `json:"edges"`
	}
	get(t, srv, "/edges?since=1", &since)
	if len(since.Edges) != 1 || since.Edges[0].Seq != 2 {
		t.Fatalf("since filter gave %+v", since.Edges)
	}

	// A long poll with nothing to report comes back empty rather than hanging.
	start := time.Now()
	var waited struct {
		Edges []Edge `json:"edges"`
	}
	get(t, srv, "/edges?since=2&waitMs=300", &waited)
	if len(waited.Edges) != 0 {
		t.Errorf("the poll returned %+v", waited.Edges)
	}
	if time.Since(start) < 200*time.Millisecond {
		t.Error("the poll did not wait")
	}
}

func TestCommandsAreRefusedUntilControlIsOn(t *testing.T) {
	_, h, srv := startPlugin(t, nil)

	var catalogue struct {
		Commands            []commandSpec `json:"commands"`
		AllowControl        bool          `json:"allowControl"`
		AllowDigitalOutputs bool          `json:"allowDigitalOutputs"`
	}
	get(t, srv, "/commands", &catalogue)
	if len(catalogue.Commands) == 0 || catalogue.AllowControl || catalogue.AllowDigitalOutputs {
		t.Fatalf("catalogue = %+v", catalogue)
	}

	code, body := post(t, srv, http.MethodPost, "/command",
		CommandRequest{Target: "dali", Command: "off", Address: 5}, "")
	if code != http.StatusForbidden || !strings.Contains(body, "switched off") {
		t.Fatalf("status %d: %s", code, body)
	}
	if len(h.sentCommands()) != 0 {
		t.Fatal("a command was published while control was off")
	}
}

func TestCommandsNeedTheOperatorRole(t *testing.T) {
	_, h, srv := startPlugin(t, map[string]any{"allowControl": true})

	code, _ := post(t, srv, http.MethodPost, "/command",
		CommandRequest{Target: "dali", Command: "off", Address: 5}, "viewer")
	if code != http.StatusForbidden {
		t.Fatalf("a viewer got %d, want 403", code)
	}
	if len(h.sentCommands()) != 0 {
		t.Fatal("a viewer's command was published")
	}
}

func TestAllowedCommandIsPublishedToTheCommandTopic(t *testing.T) {
	_, h, srv := startPlugin(t, map[string]any{
		"allowControl": true, "commandTopic": "house/command",
	})

	code, body := post(t, srv, http.MethodPost, "/command",
		CommandRequest{ConnectionID: "c1", Target: "dali", Command: "arc", Address: 5, Params: []string{"120"}}, "")
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
	}

	sent := h.sentCommands()
	if len(sent) != 1 {
		t.Fatalf("published %d commands", len(sent))
	}
	if sent[0].Topic != "house/command" {
		t.Errorf("topic = %q", sent[0].Topic)
	}
	// QoS 1: a lost command is worse than a repeated one.
	if sent[0].QoS != 1 {
		t.Errorf("qos = %d, want 1", sent[0].QoS)
	}
	if string(sent[0].Payload) != `{"target":"dali","command":"arc","address":5,"params":["120"]}` {
		t.Errorf("payload = %s", sent[0].Payload)
	}
}

func TestDigitalOutputsNeedTheirOwnSwitch(t *testing.T) {
	_, h, srv := startPlugin(t, map[string]any{"allowControl": true})

	// DALI is allowed by the general switch; driving an output is not, because
	// among the eighty are a door lock and a water valve.
	code, body := post(t, srv, http.MethodPost, "/command",
		CommandRequest{ConnectionID: "c1", Target: "digital", Command: "set", Address: 12, Params: []string{"true"}}, "")
	if code != http.StatusForbidden || !strings.Contains(body, "digital outputs") {
		t.Fatalf("status %d: %s", code, body)
	}
	if len(h.sentCommands()) != 0 {
		t.Fatal("an output was driven with only the DALI switch on")
	}

	_, _, srv2 := startPlugin(t, map[string]any{"allowControl": true, "allowDigitalOutputs": true})
	if code, body := post(t, srv2, http.MethodPost, "/command",
		CommandRequest{ConnectionID: "c1", Target: "digital", Command: "set", Address: 12, Params: []string{"true"}}, ""); code != http.StatusOK {
		t.Fatalf("with both switches on: %d %s", code, body)
	}
}

func TestCommandsOutsideTheCatalogueAreRefusedEvenWithControlOn(t *testing.T) {
	_, h, srv := startPlugin(t, map[string]any{
		"allowControl": true, "allowDigitalOutputs": true,
	})

	for _, req := range []CommandRequest{
		{ConnectionID: "c1", Target: "safety", Command: "reset_water_valve"},
		{ConnectionID: "c1", Target: "alarm", Command: "panic"},
		{ConnectionID: "c1", Target: "system", Command: "wipe_persistent_magic"},
		{ConnectionID: "c1", Target: "mode", Command: "set_direct_ha", Params: []string{"true"}},
	} {
		code, _ := post(t, srv, http.MethodPost, "/command", req, "")
		if code == http.StatusOK {
			t.Errorf("%s/%s was sent", req.Target, req.Command)
		}
	}
	if len(h.sentCommands()) != 0 {
		t.Fatal("something outside the catalogue reached the broker")
	}
}

func TestMappingsRoundTripThroughTheStore(t *testing.T) {
	p, h, srv := startPlugin(t, nil)
	p.HandleMessage(context.Background(), mqttc.Message{
		ConnectionID: "c1", Topic: "plc/digital/input/99",
		Payload: []byte(`{"address":99,"name":"DI-9-9","value":false}`),
	})

	code, body := post(t, srv, http.MethodPut, "/mappings", mappingRequest{
		ConnectionID: "c1", Name: "DI-9-9", Label: "Kitchen switch", Location: "kitchen",
	}, "")
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
	}

	var mappings []Mapping
	get(t, srv, "/mappings?connectionId=c1", &mappings)
	if len(mappings) != 1 || mappings[0].Label != "Kitchen switch" {
		t.Fatalf("mappings = %+v", mappings)
	}

	// It is persisted, not only held in memory, or it would not survive a
	// restart.
	stored, _ := h.Store().All()
	if len(stored) != 1 {
		t.Fatalf("the store holds %d entries", len(stored))
	}

	var state State
	get(t, srv, "/state?connectionId=c1", &state)
	if state.Points[0].Label != "Kitchen switch" {
		t.Errorf("the name did not reach the projection: %+v", state.Points[0])
	}

	// Clearing every field is the delete.
	if code, _ := post(t, srv, http.MethodPut, "/mappings",
		mappingRequest{ConnectionID: "c1", Name: "DI-9-9"}, ""); code != http.StatusOK {
		t.Fatalf("clearing gave %d", code)
	}
	stored, _ = h.Store().All()
	if len(stored) != 0 {
		t.Errorf("the cleared mapping is still stored: %+v", stored)
	}
}

func TestMappingValidation(t *testing.T) {
	_, _, srv := startPlugin(t, nil)

	if code, _ := post(t, srv, http.MethodPut, "/mappings",
		mappingRequest{ConnectionID: "c1", Label: "no name"}, ""); code != http.StatusBadRequest {
		t.Errorf("a mapping with no point name gave %d", code)
	}
	if code, _ := post(t, srv, http.MethodPut, "/mappings",
		mappingRequest{ConnectionID: "c1", Name: "DI-1", Label: "x"}, "viewer"); code != http.StatusForbidden {
		t.Errorf("a viewer named a point")
	}
}

func TestStoredMappingsAreRestoredOnInit(t *testing.T) {
	h := newFakeHost(nil)
	if err := h.Store().Set(mappingKey("c1", "DI-9-9"),
		`{"name":"DI-9-9","label":"Restored"}`); err != nil {
		t.Fatal(err)
	}

	p := &Plugin{}
	if err := p.Init(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()

	got := p.registry.Mappings("c1")
	if len(got) != 1 || got[0].Label != "Restored" {
		t.Fatalf("mappings were not restored: %+v", got)
	}
}

func TestCloseIsSafeTwice(t *testing.T) {
	p, _, _ := startPlugin(t, nil)
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("a second Close returned %v", err)
	}
}
