package hass

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// Availability is an entity's online state as reported by its availability
// topics.
type Availability string

const (
	// AvailabilityUnknown means the entity declares no availability topic, or
	// none has been received yet.
	AvailabilityUnknown Availability = "unknown"
	AvailabilityOnline  Availability = "online"
	AvailabilityOffline Availability = "offline"
)

// Device is a physical or logical device that groups entities. Home Assistant
// derives it from the `device` block of a discovery payload.
type Device struct {
	// Key is unique within a broker connection.
	Key              string     `json:"key"`
	ConnectionID     string     `json:"connectionId"`
	Name             string     `json:"name"`
	Manufacturer     string     `json:"manufacturer,omitempty"`
	Model            string     `json:"model,omitempty"`
	ModelID          string     `json:"modelId,omitempty"`
	SWVersion        string     `json:"swVersion,omitempty"`
	HWVersion        string     `json:"hwVersion,omitempty"`
	SerialNumber     string     `json:"serialNumber,omitempty"`
	SuggestedArea    string     `json:"suggestedArea,omitempty"`
	ConfigurationURL string     `json:"configurationUrl,omitempty"`
	Identifiers      []string   `json:"identifiers,omitempty"`
	Connections      [][]string `json:"connections,omitempty"`
	ViaDevice        string     `json:"viaDevice,omitempty"`
	// Origin names the software that announced the device, e.g. "Zigbee2MQTT".
	Origin string `json:"origin,omitempty"`
	// Pinned entities appear first in the UI. Persisted in the plugin's KV.
	Pinned    bool      `json:"pinned"`
	FirstSeen time.Time `json:"firstSeen"`
	LastSeen  time.Time `json:"lastSeen"`
}

// EntityState is the last value seen for an entity.
type EntityState struct {
	// Raw is the payload exactly as it arrived.
	Raw string `json:"raw"`
	// Value is the result of applying the entity's value_template.
	Value any `json:"value"`
	// TemplateSupported is false when the entity's value_template used Jinja
	// features mqttview does not evaluate; Value is then the raw payload.
	TemplateSupported bool      `json:"templateSupported"`
	UpdatedAt         time.Time `json:"updatedAt"`
}

// Entity is one Home Assistant entity discovered over MQTT.
type Entity struct {
	// ID is stable for the lifetime of the discovery topic.
	ID             string `json:"id"`
	ConnectionID   string `json:"connectionId"`
	DiscoveryTopic string `json:"discoveryTopic"`
	Component      string `json:"component"`
	NodeID         string `json:"nodeId,omitempty"`
	ObjectID       string `json:"objectId"`
	UniqueID       string `json:"uniqueId,omitempty"`
	Name           string `json:"name"`
	DeviceKey      string `json:"deviceKey"`

	DeviceClass string `json:"deviceClass,omitempty"`
	StateClass  string `json:"stateClass,omitempty"`
	Unit        string `json:"unit,omitempty"`
	Icon        string `json:"icon,omitempty"`
	Category    string `json:"category,omitempty"`

	StateTopic   string `json:"stateTopic,omitempty"`
	CommandTopic string `json:"commandTopic,omitempty"`

	// Controllable is true when mqttview knows how to act on this entity.
	Controllable bool     `json:"controllable"`
	Actions      []string `json:"actions,omitempty"`

	Availability Availability `json:"availability"`
	State        *EntityState `json:"state,omitempty"`
	// Attributes come from json_attributes_topic.
	Attributes map[string]any `json:"attributes,omitempty"`
	// Config is the full normalised discovery payload, so the UI can show
	// every key the device published.
	Config map[string]any `json:"config"`

	DiscoveredAt time.Time `json:"discoveredAt"`
	UpdatedAt    time.Time `json:"updatedAt"`

	// Internal fields, not serialised.
	valueTemplate string
	availSpecs    []availabilitySpec
	attrTopic     string
	attrTemplate  string
	availState    map[string]Availability
}

// Registry holds every discovered device and entity, indexed for O(1) routing
// of incoming state messages.
type Registry struct {
	mu       sync.RWMutex
	devices  map[string]*Device // keyed by connectionID|deviceKey
	entities map[string]*Entity // keyed by connectionID|discoveryTopic

	// Topic indexes, keyed by connectionID|topic. One topic can feed many
	// entities, which is normal for devices that publish a single JSON blob.
	stateIndex map[string][]*Entity
	availIndex map[string][]*Entity
	attrIndex  map[string][]*Entity

	pinned map[string]bool
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		devices:    map[string]*Device{},
		entities:   map[string]*Entity{},
		stateIndex: map[string][]*Entity{},
		availIndex: map[string][]*Entity{},
		attrIndex:  map[string][]*Entity{},
		pinned:     map[string]bool{},
	}
}

func indexKey(connID, topic string) string { return connID + "|" + topic }

// Upsert records a discovery config and returns the entity along with the
// topics that must now be subscribed.
func (r *Registry) Upsert(connID string, dt discoveryTopic, cfg entityConfig) (*Entity, []string) {
	now := time.Now()
	id := indexKey(connID, dt.topicOf())

	r.mu.Lock()
	defer r.mu.Unlock()

	existing := r.entities[id]
	e := &Entity{
		ID:             id,
		ConnectionID:   connID,
		DiscoveryTopic: dt.topicOf(),
		Component:      dt.Component,
		NodeID:         dt.NodeID,
		ObjectID:       dt.ObjectID,
		UniqueID:       cfg.str("unique_id"),
		DeviceClass:    cfg.str("device_class"),
		StateClass:     cfg.str("state_class"),
		Unit:           cfg.str("unit_of_measurement"),
		Icon:           cfg.str("icon"),
		Category:       cfg.str("entity_category"),
		StateTopic:     cfg.str("state_topic"),
		CommandTopic:   cfg.str("command_topic"),
		Config:         cfg,
		Availability:   AvailabilityUnknown,
		DiscoveredAt:   now,
		UpdatedAt:      now,
		valueTemplate:  cfg.str("value_template"),
		availSpecs:     cfg.availabilityTopics(),
		attrTopic:      cfg.str("json_attributes_topic"),
		attrTemplate:   cfg.str("json_attributes_template"),
		availState:     map[string]Availability{},
	}
	e.Actions = actionsFor(dt.Component, cfg)
	e.Controllable = len(e.Actions) > 0

	device := r.upsertDevice(connID, cfg, now)
	if device != nil {
		e.DeviceKey = device.Key
	}
	e.Name = entityName(cfg, device, dt)

	// Carry the last known state across a re-announcement: devices re-publish
	// their discovery config on every boot, and losing the value each time
	// would make the UI flicker to "unknown".
	if existing != nil {
		e.State = existing.State
		e.Attributes = existing.Attributes
		e.Availability = existing.Availability
		e.DiscoveredAt = existing.DiscoveredAt
		for k, v := range existing.availState {
			e.availState[k] = v
		}
		r.unindex(existing)
	}

	r.entities[id] = e
	topics := r.index(e)
	return e, topics
}

// upsertDevice merges the device block of a discovery payload.
func (r *Registry) upsertDevice(connID string, cfg entityConfig, now time.Time) *Device {
	devMap := cfg.submap("device")
	if devMap == nil {
		return nil
	}
	dev := entityConfig(devMap)

	key := deviceKey(dev)
	if key == "" {
		return nil
	}
	full := indexKey(connID, key)

	d, ok := r.devices[full]
	if !ok {
		d = &Device{
			Key:          key,
			ConnectionID: connID,
			FirstSeen:    now,
			Pinned:       r.pinned[full],
		}
		r.devices[full] = d
	}

	// Later announcements may fill in fields the first one omitted, so only
	// non-empty values overwrite.
	setIf(&d.Name, dev.str("name"))
	setIf(&d.Manufacturer, dev.str("manufacturer"))
	setIf(&d.Model, dev.str("model"))
	setIf(&d.ModelID, dev.str("model_id"))
	setIf(&d.SWVersion, dev.str("sw_version"))
	setIf(&d.HWVersion, dev.str("hw_version"))
	setIf(&d.SerialNumber, dev.str("serial_number"))
	setIf(&d.SuggestedArea, dev.str("suggested_area"))
	setIf(&d.ConfigurationURL, dev.str("configuration_url"))
	setIf(&d.ViaDevice, dev.str("via_device"))
	if ids := dev.strings("identifiers"); len(ids) > 0 {
		d.Identifiers = ids
	}
	if cns := connectionPairs(dev); len(cns) > 0 {
		d.Connections = cns
	}
	if origin := entityConfig(cfg.submap("origin")); origin != nil {
		setIf(&d.Origin, origin.str("name"))
	}
	if d.Name == "" {
		d.Name = key
	}
	d.LastSeen = now
	return d
}

// Remove deletes an entity, returning the topics that no longer need a
// subscription.
func (r *Registry) Remove(connID, discoveryTopicPath string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	id := indexKey(connID, discoveryTopicPath)
	e, ok := r.entities[id]
	if !ok {
		return nil
	}
	r.unindex(e)
	delete(r.entities, id)

	// Drop the device once its last entity is gone.
	if e.DeviceKey != "" {
		stillUsed := false
		for _, other := range r.entities {
			if other.ConnectionID == connID && other.DeviceKey == e.DeviceKey {
				stillUsed = true
				break
			}
		}
		if !stillUsed {
			delete(r.devices, indexKey(connID, e.DeviceKey))
		}
	}
	return r.orphanedTopics(connID, e)
}

// index adds an entity to the topic indexes and returns the topics it needs.
func (r *Registry) index(e *Entity) []string {
	var topics []string
	add := func(m map[string][]*Entity, topic string) {
		if topic == "" {
			return
		}
		k := indexKey(e.ConnectionID, topic)
		m[k] = append(m[k], e)
		topics = append(topics, topic)
	}

	add(r.stateIndex, e.StateTopic)
	for _, spec := range e.availSpecs {
		add(r.availIndex, spec.Topic)
	}
	add(r.attrIndex, e.attrTopic)

	// Some components report through a topic other than state_topic.
	for _, key := range extraStateTopicKeys {
		if t := entityConfig(e.Config).str(key); t != "" {
			add(r.stateIndex, t)
		}
	}
	return dedupe(topics)
}

// extraStateTopicKeys are the additional read topics mqttview follows so that
// covers, lights and fans show their real state rather than only their
// primary state topic.
var extraStateTopicKeys = []string{
	"position_topic",
	"brightness_state_topic",
	"percentage_state_topic",
	"preset_mode_state_topic",
	"mode_state_topic",
	"temperature_state_topic",
	"current_temperature_topic",
	"target_humidity_state_topic",
	"effect_state_topic",
	"color_temp_state_topic",
	"rgb_state_topic",
	"oscillation_state_topic",
	"tilt_status_topic",
	"action_topic",
}

func (r *Registry) unindex(e *Entity) {
	remove := func(m map[string][]*Entity, topic string) {
		if topic == "" {
			return
		}
		k := indexKey(e.ConnectionID, topic)
		list := m[k]
		out := list[:0]
		for _, item := range list {
			if item.ID != e.ID {
				out = append(out, item)
			}
		}
		if len(out) == 0 {
			delete(m, k)
		} else {
			m[k] = out
		}
	}

	remove(r.stateIndex, e.StateTopic)
	for _, spec := range e.availSpecs {
		remove(r.availIndex, spec.Topic)
	}
	remove(r.attrIndex, e.attrTopic)
	for _, key := range extraStateTopicKeys {
		if t := entityConfig(e.Config).str(key); t != "" {
			remove(r.stateIndex, t)
		}
	}
}

// orphanedTopics lists the topics of e that no remaining entity uses.
func (r *Registry) orphanedTopics(connID string, e *Entity) []string {
	var out []string
	check := func(m map[string][]*Entity, topic string) {
		if topic == "" {
			return
		}
		if len(m[indexKey(connID, topic)]) == 0 {
			out = append(out, topic)
		}
	}
	check(r.stateIndex, e.StateTopic)
	for _, spec := range e.availSpecs {
		check(r.availIndex, spec.Topic)
	}
	check(r.attrIndex, e.attrTopic)
	return dedupe(out)
}

// ApplyState routes a non-discovery message to the entities that care about
// it and returns those that changed.
func (r *Registry) ApplyState(connID, topic string, payload []byte) []*Entity {
	r.mu.Lock()
	defer r.mu.Unlock()

	k := indexKey(connID, topic)
	now := time.Now()
	touched := map[string]*Entity{}

	for _, e := range r.stateIndex[k] {
		tpl := e.valueTemplate
		if topic != e.StateTopic {
			// A secondary topic (brightness, position, …) has its own
			// template; using the primary one would misread the payload.
			tpl = secondaryTemplateFor(e, topic)
		}
		res := renderTemplate(tpl, payload)
		e.State = &EntityState{
			Raw:               string(payload),
			Value:             res.Value,
			TemplateSupported: res.Supported,
			UpdatedAt:         now,
		}
		e.UpdatedAt = now
		touched[e.ID] = e
	}

	for _, e := range r.availIndex[k] {
		for _, spec := range e.availSpecs {
			if spec.Topic != topic {
				continue
			}
			res := renderTemplate(spec.Template, payload)
			value := toString(res.Value)
			switch value {
			case spec.Online:
				e.availState[spec.Topic] = AvailabilityOnline
			case spec.Offline:
				e.availState[spec.Topic] = AvailabilityOffline
			default:
				e.availState[spec.Topic] = AvailabilityUnknown
			}
		}
		e.Availability = combineAvailability(e)
		e.UpdatedAt = now
		touched[e.ID] = e
	}

	for _, e := range r.attrIndex[k] {
		attrs := parseAttributes(e.attrTemplate, payload)
		if attrs != nil {
			e.Attributes = attrs
			e.UpdatedAt = now
			touched[e.ID] = e
		}
	}

	out := make([]*Entity, 0, len(touched))
	for _, e := range touched {
		out = append(out, e)
	}
	return out
}

// secondaryTemplateFor finds the value template belonging to a non-primary
// state topic, e.g. brightness_value_template for brightness_state_topic.
func secondaryTemplateFor(e *Entity, topic string) string {
	cfg := entityConfig(e.Config)
	// The naming convention is consistent: <x>_state_topic pairs with
	// <x>_value_template, and position_topic with position_template.
	for _, key := range extraStateTopicKeys {
		if cfg.str(key) != topic {
			continue
		}
		switch {
		case strings.HasSuffix(key, "_state_topic"):
			base := strings.TrimSuffix(key, "_state_topic")
			if t := cfg.str(base + "_value_template"); t != "" {
				return t
			}
			return cfg.str(base + "_state_template")
		case key == "position_topic":
			return cfg.str("position_template")
		case key == "current_temperature_topic":
			return cfg.str("current_temperature_template")
		case key == "tilt_status_topic":
			return cfg.str("tilt_status_template")
		case key == "action_topic":
			return cfg.str("action_template")
		}
	}
	return ""
}

// combineAvailability applies availability_mode: an entity with several
// sources is offline if any source says so, unless the mode says otherwise.
func combineAvailability(e *Entity) Availability {
	if len(e.availState) == 0 {
		return AvailabilityUnknown
	}
	mode := entityConfig(e.Config).strDefault("availability_mode", "latest")

	var online, offline int
	for _, v := range e.availState {
		switch v {
		case AvailabilityOnline:
			online++
		case AvailabilityOffline:
			offline++
		}
	}
	switch mode {
	case "all":
		if offline > 0 {
			return AvailabilityOffline
		}
		if online > 0 {
			return AvailabilityOnline
		}
	case "any":
		if online > 0 {
			return AvailabilityOnline
		}
		if offline > 0 {
			return AvailabilityOffline
		}
	default: // "latest" — with a single topic this is the common case
		if online > 0 {
			return AvailabilityOnline
		}
		if offline > 0 {
			return AvailabilityOffline
		}
	}
	return AvailabilityUnknown
}

func parseAttributes(template string, payload []byte) map[string]any {
	res := renderTemplate(template, payload)
	if m, ok := res.Value.(map[string]any); ok {
		return m
	}
	// With no template the payload itself should be a JSON object.
	if template == "" {
		var m map[string]any
		if err := jsonUnmarshal(payload, &m); err == nil {
			return m
		}
	}
	return nil
}

// Entities returns a snapshot of every entity, optionally filtered by
// connection.
func (r *Registry) Entities(connID string) []*Entity {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]*Entity, 0, len(r.entities))
	for _, e := range r.entities {
		if connID != "" && e.ConnectionID != connID {
			continue
		}
		out = append(out, e.snapshot())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Entity looks one entity up by ID.
func (r *Registry) Entity(id string) (*Entity, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entities[id]
	return e.snapshot(), ok
}

// snapshot copies an entity so a caller can read it after the lock is gone.
//
// A shallow copy is enough, and that is a property of how the registry
// updates: State and Attributes are replaced with new values rather than
// modified in place, so a copy taken under the lock stays consistent. It is
// not a style choice — handing out live pointers meant an HTTP handler
// serialising an entity while a message updated it, which the race detector
// caught in the plugin's end-to-end test and which a browser polling /devices
// on a busy broker would hit for real.
func (e *Entity) snapshot() *Entity {
	if e == nil {
		return nil
	}
	c := *e
	return &c
}

func snapshotAll(in []*Entity) []*Entity {
	out := make([]*Entity, 0, len(in))
	for _, e := range in {
		out = append(out, e.snapshot())
	}
	return out
}

// DeviceView is a device with its entities attached, which is how the UI
// renders it.
type DeviceView struct {
	Device
	Entities []*Entity `json:"entities"`
}

// Devices returns devices with their entities, pinned first then by name.
// Entities with no device are grouped under a synthetic "Ungrouped" device.
func (r *Registry) Devices(connID string) []DeviceView {
	r.mu.RLock()
	defer r.mu.RUnlock()

	byDevice := map[string][]*Entity{}
	var orphans []*Entity
	for _, e := range r.entities {
		if connID != "" && e.ConnectionID != connID {
			continue
		}
		if e.DeviceKey == "" {
			orphans = append(orphans, e)
			continue
		}
		k := indexKey(e.ConnectionID, e.DeviceKey)
		byDevice[k] = append(byDevice[k], e)
	}

	out := make([]DeviceView, 0, len(byDevice)+1)
	for k, entities := range byDevice {
		d, ok := r.devices[k]
		if !ok {
			continue
		}
		sortEntities(entities)
		out = append(out, DeviceView{Device: *d, Entities: snapshotAll(entities)})
	}
	if len(orphans) > 0 {
		sortEntities(orphans)
		orphans = snapshotAll(orphans)
		out = append(out, DeviceView{
			Device: Device{
				Key:          "__ungrouped__",
				ConnectionID: connID,
				Name:         "Ungrouped entities",
			},
			Entities: orphans,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Pinned != out[j].Pinned {
			return out[i].Pinned
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

// SetPinned marks a device as pinned and reports the storage key so the caller
// can persist it.
func (r *Registry) SetPinned(connID, deviceKey string, pinned bool) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	full := indexKey(connID, deviceKey)
	d, ok := r.devices[full]
	if !ok {
		return "", false
	}
	d.Pinned = pinned
	r.pinned[full] = pinned
	return full, true
}

// LoadPinned restores pin state saved in the plugin's KV store.
func (r *Registry) LoadPinned(keys map[string]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, v := range keys {
		r.pinned[k] = v
		if d, ok := r.devices[k]; ok {
			d.Pinned = v
		}
	}
}

// ClearConnection drops everything learned from one broker, which is what
// should happen when that connection is deleted.
func (r *Registry) ClearConnection(connID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for id, e := range r.entities {
		if e.ConnectionID == connID {
			r.unindex(e)
			delete(r.entities, id)
		}
	}
	for k, d := range r.devices {
		if d.ConnectionID == connID {
			delete(r.devices, k)
		}
	}
}

// Stats summarises the registry for the plugin's status endpoint.
func (r *Registry) Stats() (devices, entities int) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.devices), len(r.entities)
}

func sortEntities(list []*Entity) {
	sort.Slice(list, func(i, j int) bool {
		if list[i].Component != list[j].Component {
			return list[i].Component < list[j].Component
		}
		return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name)
	})
}

// deviceKey derives a stable key from the device block, preferring an
// identifier and falling back to a MAC-style connection pair.
func deviceKey(dev entityConfig) string {
	if ids := dev.strings("identifiers"); len(ids) > 0 {
		sorted := append([]string(nil), ids...)
		sort.Strings(sorted)
		return "id:" + sorted[0]
	}
	if pairs := connectionPairs(dev); len(pairs) > 0 {
		return "cn:" + strings.Join(pairs[0], ":")
	}
	if name := dev.str("name"); name != "" {
		return "name:" + name
	}
	return ""
}

// connectionPairs reads the device `connections` field, which is a list of
// [type, value] pairs such as ["mac", "aa:bb:cc:dd:ee:ff"].
func connectionPairs(dev entityConfig) [][]string {
	raw, ok := dev["connections"].([]any)
	if !ok {
		return nil
	}
	var out [][]string
	for _, item := range raw {
		pair, ok := item.([]any)
		if !ok || len(pair) != 2 {
			continue
		}
		a, aok := pair[0].(string)
		b, bok := pair[1].(string)
		if aok && bok {
			out = append(out, []string{a, b})
		}
	}
	return out
}

// entityName mirrors Home Assistant's naming: an entity with no name of its
// own inherits the device name, and a named entity under a named device is
// shown as "Device Name".
func entityName(cfg entityConfig, device *Device, dt discoveryTopic) string {
	name := cfg.str("name")
	deviceName := ""
	if device != nil {
		deviceName = device.Name
	}

	switch {
	case name != "" && deviceName != "" && !strings.HasPrefix(strings.ToLower(name), strings.ToLower(deviceName)):
		return deviceName + " " + name
	case name != "":
		return name
	case deviceName != "":
		return deviceName
	case cfg.str("object_id") != "":
		return cfg.str("object_id")
	default:
		return dt.ObjectID
	}
}

func (dt discoveryTopic) topicOf() string {
	parts := []string{dt.Prefix, dt.Component}
	if dt.NodeID != "" {
		parts = append(parts, dt.NodeID)
	}
	parts = append(parts, dt.ObjectID, "config")
	return strings.Join(parts, "/")
}

func setIf(dst *string, v string) {
	if v != "" {
		*dst = v
	}
}

func dedupe(in []string) []string {
	if len(in) < 2 {
		return in
	}
	seen := make(map[string]struct{}, len(in))
	out := in[:0]
	for _, s := range in {
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
