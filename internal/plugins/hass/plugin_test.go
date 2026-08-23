package hass

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mqttview/mqttview/internal/auth"
	"github.com/mqttview/mqttview/internal/mqttc"
	"github.com/mqttview/mqttview/internal/plugin"
	"github.com/mqttview/mqttview/internal/store"
)

// fakeHost records what the plugin asks the runtime to do, so a test can
// assert on a command without a broker.
type fakeHost struct {
	mu         sync.Mutex
	settings   map[string]any
	kv         map[string]string
	published  []mqttc.PublishRequest
	subscribed []string
	events     int
	// publishErr makes the broker refuse, so the handler's failure branch is
	// reachable without an actual broker.
	publishErr error
}

func newFakeHost(settings map[string]any) *fakeHost {
	if settings == nil {
		settings = map[string]any{}
	}
	return &fakeHost{settings: settings, kv: map[string]string{}}
}

func (h *fakeHost) Logger() *slog.Logger     { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
func (h *fakeHost) Store() plugin.KV         { return (*fakeKV)(h) }
func (h *fakeHost) Settings() map[string]any { return h.settings }
func (h *fakeHost) Connections() []*mqttc.Conn {
	return nil
}
func (h *fakeHost) Connection(string) (*mqttc.Conn, bool) { return nil, false }
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
	for _, s := range subs {
		h.subscribed = append(h.subscribed, s.Filter)
	}
	return nil
}
func (h *fakeHost) Unsubscribe(_ context.Context, _ string, filters []string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	remaining := h.subscribed[:0]
	for _, s := range h.subscribed {
		drop := false
		for _, f := range filters {
			if s == f {
				drop = true
			}
		}
		if !drop {
			remaining = append(remaining, s)
		}
	}
	h.subscribed = remaining
	return nil
}
func (h *fakeHost) Emit(string, any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events++
}

func (h *fakeHost) subscriptions() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.subscribed...)
}

func (h *fakeHost) commands() []mqttc.PublishRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]mqttc.PublishRequest(nil), h.published...)
}

type fakeKV fakeHost

func (k *fakeKV) Get(key string) (string, bool, error) {
	v, ok := (*fakeHost)(k).kv[key]
	return v, ok, nil
}
func (k *fakeKV) Set(key, value string) error { (*fakeHost)(k).kv[key] = value; return nil }
func (k *fakeKV) Delete(key string) error     { delete((*fakeHost)(k).kv, key); return nil }
func (k *fakeKV) All() (map[string]string, error) {
	out := map[string]string{}
	for kk, vv := range (*fakeHost)(k).kv {
		out[kk] = vv
	}
	return out, nil
}

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

// discover feeds one discovery config through the plugin.
func discover(t *testing.T, p *Plugin, topic, payload string) {
	t.Helper()
	p.HandleMessage(context.Background(), mqttc.Message{
		ConnectionID: "c1", Topic: topic, Payload: []byte(payload), ReceivedAt: time.Now(),
	})
}

func getJSON(t *testing.T, srv *httptest.Server, path string, out any) int {
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

func postJSON(t *testing.T, srv *httptest.Server, path string, body any, role string) (int, string) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(string(raw)))
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

// postRaw sends a body the JSON decoder will refuse.
func postRaw(t *testing.T, srv *httptest.Server, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
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

func TestMetaAndSubscriptions(t *testing.T) {
	meta := (&Plugin{}).Meta()
	if meta.ID != ID || meta.Panel == "" {
		t.Fatalf("meta = %+v", meta)
	}

	p, _, _ := startPlugin(t, map[string]any{"discoveryPrefix": "ha-test"})
	subs := p.Subscriptions()
	// Only the discovery prefix: state topics are subscribed as they are
	// learned, so enabling the plugin is not a firehose.
	if len(subs) != 1 || subs[0].Filter != "ha-test/#" {
		t.Fatalf("subscriptions = %+v", subs)
	}
}

func TestDiscoveryBuildsADeviceAndSubscribesToItsTopics(t *testing.T) {
	p, h, srv := startPlugin(t, nil)

	discover(t, p, "homeassistant/sensor/kitchen/temp/config", `{
		"name":"Temperature","uniq_id":"kitchen-temp","stat_t":"~/state","~":"home/kitchen",
		"dev":{"ids":["kitchen"],"name":"Kitchen"},"unit_of_meas":"°C"}`)

	var devices []HassDeviceLike
	if code := getJSON(t, srv, "/devices", &devices); code != http.StatusOK {
		t.Fatalf("devices = %d", code)
	}
	if len(devices) != 1 || len(devices[0].Entities) != 1 {
		t.Fatalf("devices = %+v", devices)
	}
	e := devices[0].Entities[0]
	// "~" expansion is the part real firmware relies on.
	if e.StateTopic != "home/kitchen/state" {
		t.Errorf("state topic = %q, want the ~ expanded", e.StateTopic)
	}
	if e.Unit != "°C" {
		t.Errorf("the abbreviated unit was not expanded: %q", e.Unit)
	}

	subs := h.subscriptions()
	found := false
	for _, s := range subs {
		if s == "home/kitchen/state" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the state topic was not subscribed: %+v", subs)
	}
}

func TestStateUpdatesReachTheEntity(t *testing.T) {
	p, _, srv := startPlugin(t, nil)
	discover(t, p, "homeassistant/sensor/kitchen/temp/config",
		`{"name":"T","uniq_id":"t1","state_topic":"home/t","value_template":"{{ value_json.temp | round(1) }}"}`)

	p.HandleMessage(context.Background(), mqttc.Message{
		ConnectionID: "c1", Topic: "home/t", Payload: []byte(`{"temp":21.567}`),
	})

	var entities []HassEntityLike
	getJSON(t, srv, "/entities", &entities)
	if len(entities) != 1 {
		t.Fatalf("entities = %+v", entities)
	}
	// JSON gives back a number here, so compare on the rendered form rather
	// than on the Go type.
	if entities[0].State == nil || fmt.Sprint(entities[0].State.Value) != "21.6" {
		t.Fatalf("value = %+v, want the template applied", entities[0].State)
	}
}

func TestAnUnsupportedTemplateShowsTheRawPayload(t *testing.T) {
	p, _, srv := startPlugin(t, nil)
	discover(t, p, "homeassistant/sensor/x/y/config",
		`{"name":"T","uniq_id":"t1","state_topic":"home/t","value_template":"{% for x in y %}{{x}}{% endfor %}"}`)

	p.HandleMessage(context.Background(), mqttc.Message{
		ConnectionID: "c1", Topic: "home/t", Payload: []byte(`{"a":1}`),
	})

	var entities []HassEntityLike
	getJSON(t, srv, "/entities", &entities)
	// It will not guess: the raw payload is shown and marked unsupported.
	if entities[0].State == nil || entities[0].State.Supported {
		t.Fatalf("an unevaluable template was reported as supported: %+v", entities[0].State)
	}
	if entities[0].State.Value != `{"a":1}` {
		t.Errorf("value = %v, want the raw payload", entities[0].State.Value)
	}
}

func TestAnEmptyConfigRemovesTheEntity(t *testing.T) {
	p, h, srv := startPlugin(t, nil)
	topic := "homeassistant/sensor/kitchen/temp/config"
	discover(t, p, topic, `{"name":"T","uniq_id":"t1","state_topic":"home/t"}`)

	var before []HassEntityLike
	getJSON(t, srv, "/entities", &before)
	if len(before) != 1 {
		t.Fatalf("setup: %+v", before)
	}

	// An empty retained payload is how Home Assistant deletes an entity.
	discover(t, p, topic, "")

	var after []HassEntityLike
	getJSON(t, srv, "/entities", &after)
	if len(after) != 0 {
		t.Fatalf("the entity survived deletion: %+v", after)
	}
	// Its state topic is unsubscribed too, or the plugin keeps paying for a
	// device that is gone.
	for _, s := range h.subscriptions() {
		if s == "home/t" {
			t.Error("the state topic is still subscribed")
		}
	}
}

func TestDeviceScopedDiscovery(t *testing.T) {
	p, _, srv := startPlugin(t, nil)

	// The 2024.10+ format: one payload describes every component.
	discover(t, p, "homeassistant/device/kitchen/config", `{
		"dev":{"ids":["kitchen"],"name":"Kitchen"},
		"o":{"name":"firmware"},
		"cmps":{
			"temp":{"platform":"sensor","name":"Temperature","state_topic":"home/t","unique_id":"k-t"},
			"light":{"platform":"switch","name":"Light","command_topic":"home/l/set","unique_id":"k-l"}
		}}`)

	var entities []HassEntityLike
	getJSON(t, srv, "/entities", &entities)
	if len(entities) != 2 {
		t.Fatalf("got %d entities from a device payload: %+v", len(entities), entities)
	}

	discover(t, p, "homeassistant/device/kitchen/config", "")
	getJSON(t, srv, "/entities", &entities)
	if len(entities) != 0 {
		t.Fatalf("removing the device left %d entities", len(entities))
	}
}

func TestStatusAndSingleEntityLookup(t *testing.T) {
	p, _, srv := startPlugin(t, nil)
	discover(t, p, "homeassistant/switch/x/y/config",
		`{"name":"S","uniq_id":"s1","command_topic":"home/s/set","state_topic":"home/s"}`)

	var status struct {
		DiscoveryPrefix string `json:"discoveryPrefix"`
		Devices         int    `json:"devices"`
		Entities        int    `json:"entities"`
		AllowControl    bool   `json:"allowControl"`
	}
	getJSON(t, srv, "/status", &status)
	if status.Entities != 1 || status.DiscoveryPrefix != "homeassistant" {
		t.Fatalf("status = %+v", status)
	}

	var entities []HassEntityLike
	getJSON(t, srv, "/entities", &entities)
	id := entities[0].ID

	if code := getJSON(t, srv, "/entities/"+id, nil); code != http.StatusOK {
		t.Errorf("entity lookup = %d", code)
	}
	if code := getJSON(t, srv, "/entities/nope", nil); code != http.StatusNotFound {
		t.Errorf("an unknown entity gave %d", code)
	}
}

func TestControlPublishesTheDevicesOwnPayload(t *testing.T) {
	p, h, srv := startPlugin(t, map[string]any{"allowControl": true})
	discover(t, p, "homeassistant/switch/x/y/config",
		`{"name":"S","uniq_id":"s1","command_topic":"home/s/set","payload_on":"TURN_ON"}`)

	var entities []HassEntityLike
	getJSON(t, srv, "/entities", &entities)
	id := entities[0].ID

	code, body := postJSON(t, srv, "/command",
		map[string]string{"entityId": id, "action": "turn_on"}, "")
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
	}

	sent := h.commands()
	if len(sent) != 1 || sent[0].Topic != "home/s/set" {
		t.Fatalf("published %+v", sent)
	}
	// The device advertised its own on payload, so that is what is sent.
	if string(sent[0].Payload) != "TURN_ON" {
		t.Errorf("payload = %q, want the advertised one", sent[0].Payload)
	}
}

func TestControlIsRefusedWhenOffOrUnauthorised(t *testing.T) {
	p, h, srv := startPlugin(t, map[string]any{"allowControl": false})
	discover(t, p, "homeassistant/switch/x/y/config",
		`{"name":"S","uniq_id":"s1","command_topic":"home/s/set"}`)

	var entities []HassEntityLike
	getJSON(t, srv, "/entities", &entities)
	id := entities[0].ID

	if code, _ := postJSON(t, srv, "/command",
		map[string]string{"entityId": id, "action": "turn_on"}, ""); code != http.StatusForbidden {
		t.Errorf("with control off: %d", code)
	}
	if code, _ := postJSON(t, srv, "/command",
		map[string]string{"entityId": id, "action": "turn_on"}, "viewer"); code != http.StatusForbidden {
		t.Errorf("as a viewer: %d", code)
	}
	if len(h.commands()) != 0 {
		t.Fatal("a refused command still reached the broker")
	}
}

func TestPinningIsRemembered(t *testing.T) {
	p, h, srv := startPlugin(t, nil)
	discover(t, p, "homeassistant/sensor/x/y/config",
		`{"name":"T","uniq_id":"t1","state_topic":"home/t","dev":{"ids":["kitchen"]}}`)

	var devices []HassDeviceLike
	getJSON(t, srv, "/devices", &devices)
	key := devices[0].Key

	code, body := postJSON(t, srv, "/pin",
		map[string]any{"deviceKey": key, "connectionId": "c1", "pinned": true}, "")
	if code != http.StatusOK {
		t.Fatalf("pin: %d %s", code, body)
	}

	getJSON(t, srv, "/devices", &devices)
	if !devices[0].Pinned {
		t.Error("the device did not come back pinned")
	}
	// It has to survive a restart, which means the KV store.
	if len(h.kv) == 0 {
		t.Error("the pin was not persisted")
	}

	if code, _ := postJSON(t, srv, "/pin",
		map[string]any{"deviceKey": "nope", "connectionId": "c1", "pinned": true}, ""); code != http.StatusNotFound {
		t.Errorf("pinning an unknown device gave %d", code)
	}
}

func TestAMalformedDiscoveryPayloadIsIgnored(t *testing.T) {
	p, _, srv := startPlugin(t, nil)

	discover(t, p, "homeassistant/sensor/x/y/config", `{not json`)
	discover(t, p, "homeassistant/sensor/x/y/config", `[]`)

	var entities []HassEntityLike
	getJSON(t, srv, "/entities", &entities)
	if len(entities) != 0 {
		t.Fatalf("rubbish became an entity: %+v", entities)
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

// The API shapes, mirrored here so the test reads the JSON a browser would
// rather than reaching into the registry.
type HassEntityLike struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Unit       string `json:"unit"`
	StateTopic string `json:"stateTopic"`
	State      *struct {
		Raw       string `json:"raw"`
		Value     any    `json:"value"`
		Supported bool   `json:"templateSupported"`
	} `json:"state"`
}

type HassDeviceLike struct {
	Key      string           `json:"key"`
	Name     string           `json:"name"`
	Pinned   bool             `json:"pinned"`
	Entities []HassEntityLike `json:"entities"`
}
