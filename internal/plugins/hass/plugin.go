// Package hass implements the Home Assistant MQTT discovery standard as an
// mqttview plugin.
//
// Home Assistant's convention is that a device announces itself by publishing
// a retained JSON config to `<prefix>/<component>/[<node>/]<object>/config`,
// then reports values on the topics that config names. Zigbee2MQTT, Tasmota,
// ESPHome, Shelly, Z-Wave JS and many others all speak it, which makes it the
// closest thing MQTT has to a device model.
//
// The plugin reads those announcements, builds a device and entity registry,
// follows the state and availability topics it learns about, and lets a user
// act on the entities that expose a command topic.
package hass

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/dgprivate/mqttview/internal/auth"
	"github.com/dgprivate/mqttview/internal/httpx"
	"github.com/dgprivate/mqttview/internal/mqttc"
	"github.com/dgprivate/mqttview/internal/plugin"
	"github.com/dgprivate/mqttview/internal/store"
)

// ID is the plugin's registry identifier.
const ID = "home-assistant"

func init() {
	plugin.Register(ID, func() plugin.Plugin { return &Plugin{registry: NewRegistry()} })
}

// Plugin is the Home Assistant discovery plugin.
type Plugin struct {
	host     plugin.Host
	registry *Registry

	mu     sync.RWMutex
	prefix string
	// subscribed tracks which state topics have already been subscribed, per
	// connection, so a re-announcement does not re-issue every SUBSCRIBE.
	subscribed map[string]map[string]struct{}

	// coalesce batches UI notifications: a Zigbee network coming online can
	// produce hundreds of updates in a second, and the browser only needs the
	// end state.
	dirty     map[string]struct{}
	dirtyMu   sync.Mutex
	flushStop chan struct{}
	closeOnce sync.Once
}

// Meta describes the plugin.
func (p *Plugin) Meta() plugin.Meta {
	return plugin.Meta{
		ID:          ID,
		Name:        "Home Assistant",
		Version:     "1.0.0",
		Description: "Discovers devices published with the Home Assistant MQTT discovery standard and lets you view and control them.",
		Author:      "mqttview",
		URL:         "https://www.home-assistant.io/integrations/mqtt/#mqtt-discovery",
		Panel:       "home-assistant",
		SettingsSchema: []plugin.SettingField{
			{
				Key:         "discoveryPrefix",
				Label:       "Discovery prefix",
				Type:        "string",
				Default:     "homeassistant",
				Description: "The topic prefix devices publish their configuration under. Home Assistant's default is 'homeassistant'.",
			},
			{
				Key:         "subscribeQos",
				Label:       "Subscription QoS",
				Type:        "select",
				Default:     "0",
				Description: "QoS used for the plugin's own subscriptions.",
				Options: []plugin.SettingOption{
					{Value: "0", Label: "0 - at most once"},
					{Value: "1", Label: "1 - at least once"},
					{Value: "2", Label: "2 - exactly once"},
				},
			},
			{
				Key:         "allowControl",
				Label:       "Allow control",
				Type:        "bool",
				Default:     true,
				Description: "When off, the plugin only observes: no command is ever published to a device.",
			},
		},
	}
}

// Init reads settings and restores pinned devices.
func (p *Plugin) Init(_ context.Context, host plugin.Host) error {
	p.host = host
	if p.registry == nil {
		p.registry = NewRegistry()
	}
	p.subscribed = map[string]map[string]struct{}{}
	p.dirty = map[string]struct{}{}

	settings := host.Settings()
	p.mu.Lock()
	p.prefix = strings.Trim(settingString(settings, "discoveryPrefix", "homeassistant"), "/")
	if p.prefix == "" {
		p.prefix = "homeassistant"
	}
	p.mu.Unlock()

	if stored, err := host.Store().All(); err == nil {
		pins := map[string]bool{}
		for k, v := range stored {
			if strings.HasPrefix(k, "pin:") {
				pins[strings.TrimPrefix(k, "pin:")] = v == "1"
			}
		}
		p.registry.LoadPinned(pins)
	} else {
		host.Logger().Warn("could not read pinned devices", "error", err)
	}

	p.flushStop = make(chan struct{})
	// The channel is passed in rather than read from the field, so Close can
	// never race with the loop over the same variable.
	go p.flushLoop(p.flushStop)

	host.Logger().Info("home assistant discovery started", "prefix", p.prefix)
	return nil
}

// Subscriptions asks for the discovery tree only. Everything else is
// subscribed on demand as configs are read, so enabling the plugin does not
// turn mqttview into a firehose.
func (p *Plugin) Subscriptions() []mqttc.Subscription {
	p.mu.RLock()
	prefix := p.prefix
	p.mu.RUnlock()
	return []mqttc.Subscription{{Filter: prefix + "/#", QoS: p.qos()}}
}

func (p *Plugin) qos() byte {
	if p.host == nil {
		return 0
	}
	raw := settingString(p.host.Settings(), "subscribeQos", "0")
	q, err := strconv.Atoi(raw)
	if err != nil || q < 0 || q > 2 {
		return 0
	}
	return byte(q)
}

// HandleMessage processes one MQTT message: either a discovery config or a
// state update for something already discovered.
func (p *Plugin) HandleMessage(ctx context.Context, msg mqttc.Message) {
	p.mu.RLock()
	prefix := p.prefix
	p.mu.RUnlock()

	if dt, ok := parseDiscoveryTopic(prefix, msg.Topic); ok {
		p.handleDiscovery(ctx, msg, dt)
		return
	}
	// Not a config topic: route it as state. Messages under the discovery
	// prefix that match nothing are ignored, which is normal.
	for _, e := range p.registry.ApplyState(msg.ConnectionID, msg.Topic, msg.Payload) {
		p.markDirty(e.ConnectionID)
	}
}

func (p *Plugin) handleDiscovery(ctx context.Context, msg mqttc.Message, dt discoveryTopic) {
	log := p.host.Logger()

	// An empty retained payload is how Home Assistant removes an entity.
	if len(strings.TrimSpace(string(msg.Payload))) == 0 {
		if dt.DeviceScoped {
			p.removeDeviceScoped(ctx, msg.ConnectionID, dt)
		} else {
			orphaned := p.registry.Remove(msg.ConnectionID, dt.topicOf())
			p.unsubscribeTopics(ctx, msg.ConnectionID, orphaned)
		}
		p.markDirty(msg.ConnectionID)
		return
	}

	cfg, err := parseConfig(msg.Payload)
	if err != nil {
		log.Warn("ignoring malformed discovery payload", "topic", msg.Topic, "error", err)
		return
	}

	if dt.DeviceScoped {
		p.applyDeviceScoped(ctx, msg.ConnectionID, dt, cfg)
		p.markDirty(msg.ConnectionID)
		return
	}

	_, topics := p.registry.Upsert(msg.ConnectionID, dt, cfg)
	p.subscribeTopics(ctx, msg.ConnectionID, topics)
	p.markDirty(msg.ConnectionID)
}

// applyDeviceScoped handles the 2024.10+ format where one payload under
// `<prefix>/device/<id>/config` carries every component of a device.
func (p *Plugin) applyDeviceScoped(ctx context.Context, connID string, dt discoveryTopic, cfg entityConfig) {
	components, ok := cfg["components"].(map[string]any)
	if !ok {
		p.host.Logger().Warn("device discovery payload has no components", "object", dt.ObjectID)
		return
	}
	shared := map[string]any{}
	for k, v := range cfg {
		if k != "components" {
			shared[k] = v
		}
	}

	for componentID, raw := range components {
		sub, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		// Each component inherits the device and origin blocks.
		merged := entityConfig{}
		for k, v := range shared {
			merged[k] = v
		}
		for k, v := range sub {
			merged[k] = v
		}

		platform := merged.str("platform")
		if platform == "" {
			p.host.Logger().Warn("device component has no platform",
				"object", dt.ObjectID, "component", componentID)
			continue
		}
		child := discoveryTopic{
			Prefix:    dt.Prefix,
			Component: platform,
			NodeID:    dt.ObjectID,
			ObjectID:  componentID,
		}
		_, topics := p.registry.Upsert(connID, child, merged)
		p.subscribeTopics(ctx, connID, topics)
	}
}

// removeDeviceScoped deletes every entity that came from one device-scoped
// discovery message.
func (p *Plugin) removeDeviceScoped(ctx context.Context, connID string, dt discoveryTopic) {
	for _, e := range p.registry.Entities(connID) {
		if e.NodeID != dt.ObjectID {
			continue
		}
		orphaned := p.registry.Remove(connID, e.DiscoveryTopic)
		p.unsubscribeTopics(ctx, connID, orphaned)
	}
}

func (p *Plugin) subscribeTopics(ctx context.Context, connID string, topics []string) {
	if len(topics) == 0 {
		return
	}

	p.mu.Lock()
	seen, ok := p.subscribed[connID]
	if !ok {
		seen = map[string]struct{}{}
		p.subscribed[connID] = seen
	}
	var fresh []mqttc.Subscription
	qos := p.qos()
	for _, t := range topics {
		if _, dup := seen[t]; dup {
			continue
		}
		if err := mqttc.ValidateFilter(t); err != nil {
			continue // a device advertised something that is not a topic
		}
		seen[t] = struct{}{}
		fresh = append(fresh, mqttc.Subscription{Filter: t, QoS: qos})
	}
	p.mu.Unlock()

	if len(fresh) == 0 {
		return
	}
	if err := p.host.Subscribe(ctx, connID, fresh); err != nil {
		p.host.Logger().Warn("subscribing to entity topics failed", "connection", connID, "error", err)
	}
}

func (p *Plugin) unsubscribeTopics(ctx context.Context, connID string, topics []string) {
	if len(topics) == 0 {
		return
	}
	p.mu.Lock()
	if seen, ok := p.subscribed[connID]; ok {
		for _, t := range topics {
			delete(seen, t)
		}
	}
	p.mu.Unlock()

	if err := p.host.Unsubscribe(ctx, connID, topics); err != nil {
		p.host.Logger().Debug("unsubscribing from entity topics failed", "connection", connID, "error", err)
	}
}

// markDirty schedules a UI notification for a connection.
func (p *Plugin) markDirty(connID string) {
	p.dirtyMu.Lock()
	p.dirty[connID] = struct{}{}
	p.dirtyMu.Unlock()
}

// flushLoop emits at most one update per connection every 250ms.
func (p *Plugin) flushLoop(stop <-chan struct{}) {
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()

	for {
		select {
		case <-stop:
			return
		case <-t.C:
			p.dirtyMu.Lock()
			if len(p.dirty) == 0 {
				p.dirtyMu.Unlock()
				continue
			}
			conns := make([]string, 0, len(p.dirty))
			for c := range p.dirty {
				conns = append(conns, c)
			}
			p.dirty = map[string]struct{}{}
			p.dirtyMu.Unlock()

			for _, connID := range conns {
				devices, entities := p.registry.Stats()
				p.host.Emit("changed", map[string]any{
					"connectionId": connID,
					"devices":      devices,
					"entities":     entities,
				})
			}
		}
	}
}

// Close stops the batching goroutine. It is safe to call more than once.
func (p *Plugin) Close() error {
	p.closeOnce.Do(func() {
		if p.flushStop != nil {
			close(p.flushStop)
		}
	})
	return nil
}

// Routes exposes the plugin's HTTP API under /api/p/home-assistant.
func (p *Plugin) Routes(r chi.Router) {
	r.Get("/status", p.handleStatus)
	r.Get("/devices", p.handleDevices)
	r.Get("/entities", p.handleEntities)
	// A wildcard, not "{id}": an entity ID is "<connection>|<discovery topic>"
	// and therefore contains slashes, which a single-segment parameter cannot
	// match. Percent-encoding does not help — Go decodes the path before chi
	// routes it — so these 404ed for every entity there has ever been.
	r.Get("/entities/*", p.handleEntity)
	// The ID travels in the body for the same reason.
	r.Post("/command", p.handleCommand)
	r.Post("/pin", p.handlePin)
}

func (p *Plugin) handleStatus(w http.ResponseWriter, _ *http.Request) {
	devices, entities := p.registry.Stats()
	p.mu.RLock()
	prefix := p.prefix
	p.mu.RUnlock()

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"discoveryPrefix": prefix,
		"devices":         devices,
		"entities":        entities,
		"allowControl":    settingBool(p.host.Settings(), "allowControl", true),
	})
}

func (p *Plugin) handleDevices(w http.ResponseWriter, r *http.Request) {
	connID := r.URL.Query().Get("connectionId")
	httpx.WriteJSON(w, http.StatusOK, p.registry.Devices(connID))
}

func (p *Plugin) handleEntities(w http.ResponseWriter, r *http.Request) {
	connID := r.URL.Query().Get("connectionId")
	httpx.WriteJSON(w, http.StatusOK, p.registry.Entities(connID))
}

func (p *Plugin) handleEntity(w http.ResponseWriter, r *http.Request) {
	// The ID contains slashes, so it arrives as the escaped wildcard value.
	id := entityIDFrom(r)
	e, ok := p.registry.Entity(id)
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, "no such entity")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, e)
}

type commandRequest struct {
	// EntityID is in the body rather than the path because it contains the
	// discovery topic, slashes and all.
	EntityID string `json:"entityId"`
	Action   string `json:"action"`
	Value    string `json:"value"`
}

func (p *Plugin) handleCommand(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFrom(r.Context())
	if !ok || !user.Role.AtLeast(store.RoleOperator) {
		httpx.WriteError(w, http.StatusForbidden, "controlling devices requires the operator role")
		return
	}
	if !settingBool(p.host.Settings(), "allowControl", true) {
		httpx.WriteError(w, http.StatusForbidden, "control is disabled in this plugin's settings")
		return
	}

	var req commandRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	e, ok := p.registry.Entity(req.EntityID)
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, "no such entity")
		return
	}

	cmd, err := BuildCommand(e, req.Action, req.Value)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	if err := p.host.Publish(ctx, e.ConnectionID, mqttc.PublishRequest{
		Topic:   cmd.Topic,
		Payload: cmd.Payload,
		QoS:     cmd.QoS,
		Retain:  cmd.Retain,
	}); err != nil {
		httpx.WriteError(w, http.StatusBadGateway, err.Error())
		return
	}

	p.host.Logger().Info("entity command",
		"user", user.Email, "entity", e.Name, "action", req.Action,
		"topic", cmd.Topic, "payload", string(cmd.Payload))

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"topic":   cmd.Topic,
		"payload": string(cmd.Payload),
	})
}

type pinRequest struct {
	ConnectionID string `json:"connectionId"`
	// DeviceKey is in the body for the same reason an entity ID is.
	DeviceKey string `json:"deviceKey"`
	Pinned    bool   `json:"pinned"`
}

func (p *Plugin) handlePin(w http.ResponseWriter, r *http.Request) {
	var req pinRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	storeKey, ok := p.registry.SetPinned(req.ConnectionID, req.DeviceKey, req.Pinned)
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, "no such device")
		return
	}
	value := "0"
	if req.Pinned {
		value = "1"
	}
	if err := p.host.Store().Set("pin:"+storeKey, value); err != nil {
		p.host.Logger().Warn("saving pin state failed", "device", storeKey, "error", err)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"pinned": req.Pinned})
}

// entityIDFrom reads the wildcard the entity routes are declared with. An
// entity ID embeds a discovery topic, so it spans several path segments.
func entityIDFrom(r *http.Request) string {
	if id := chi.URLParam(r, "*"); id != "" {
		return id
	}
	return strings.TrimPrefix(r.URL.Path, "/entities/")
}

func settingString(settings map[string]any, key, def string) string {
	if v, ok := settings[key].(string); ok && v != "" {
		return v
	}
	if v, ok := settings[key].(float64); ok {
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return def
}

func settingBool(settings map[string]any, key string, def bool) bool {
	switch v := settings[key].(type) {
	case bool:
		return v
	case string:
		b, err := strconv.ParseBool(v)
		if err == nil {
			return b
		}
	}
	return def
}

// compile-time check that the plugin satisfies the interface.
var _ plugin.Plugin = (*Plugin)(nil)
