package plc

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"
)

// Registry holds the derived PLC state for every connection the plugin runs
// on. It is the only mutable state the plugin keeps: everything the UI shows
// is a projection of the retained and live messages fed through Apply.
type Registry struct {
	mu      sync.RWMutex
	conns   map[string]*connState
	journal *Journal
}

type connState struct {
	points    map[string]*Point    // keyed by topic
	lights    map[int]*Light       // keyed by DALI address
	shades    map[string]*Shade    // keyed by slug
	actuators map[string]*Actuator // keyed by group/slug
	meters    map[string]*Meter    // keyed by meter name
	meta      map[string]sensorMeta
	mappings  map[string]Mapping      // human names, keyed by PLC point name
	elec      map[string]*Electricity // keyed by metering point name
	watchdog  *Watchdog
	bridge    *Bridge
}

// NewRegistry returns an empty registry whose journal holds size edges.
func NewRegistry(size int) *Registry {
	return &Registry{conns: map[string]*connState{}, journal: NewJournal(size)}
}

// Journal exposes the edge log.
func (r *Registry) Journal() *Journal { return r.journal }

func (r *Registry) state(connID string) *connState {
	c, ok := r.conns[connID]
	if !ok {
		c = &connState{
			points:    map[string]*Point{},
			lights:    map[int]*Light{},
			shades:    map[string]*Shade{},
			actuators: map[string]*Actuator{},
			meters:    map[string]*Meter{},
			meta:      map[string]sensorMeta{},
			mappings:  map[string]Mapping{},
			elec:      map[string]*Electricity{},
		}
		r.conns[connID] = c
	}
	return c
}

// Apply folds one message into the registry, reporting whether it changed
// anything the UI would show. A malformed payload is dropped rather than
// clearing the last good value: a single bad publish should not blank a panel.
func (r *Registry) Apply(prefix, meterPrefix string, connID, topic string, payload []byte, received time.Time) bool {
	if rt, ok := parseRoute(prefix, topic); ok {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.applyPLC(r.state(connID), connID, rt, topic, payload, received)
	}
	if rt, ok := parseRoute(meterPrefix, topic); ok {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.applyMeter(r.state(connID), connID, rt, topic, payload, received)
	}
	return false
}

func (r *Registry) applyPLC(c *connState, connID string, rt route, topic string, payload []byte, received time.Time) bool {
	switch rt.Branch {
	case "digital":
		kind, ok := digitalKind(rt.Sub)
		if !ok {
			return false
		}
		return r.applyPoint(c, connID, kind, rt, topic, payload, received)

	case "dali":
		// The only sub-branch seen is "light"; anything else is ignored rather
		// than guessed at.
		if rt.Sub != "light" {
			return false
		}
		return applyLight(c, connID, rt, topic, payload, received)

	case "sensor":
		return applyMeta(c, rt.Leaf, payload)

	case "shade":
		return applyShade(c, connID, rt, topic, payload, received)

	case "access", "safety", "relay":
		return applyActuator(c, connID, rt, topic, payload, received)

	case "electricity":
		// "processed" carries the derived values; "status" is the raw feed of
		// the same instant. Preferring processed keeps imbalance and alarms.
		if rt.Sub != "processed" && rt.Sub != "status" {
			return false
		}
		return applyElectricity(c, connID, rt, topic, payload, received)

	case "watchdog":
		return applyWatchdog(c, connID, topic, payload, received)

	case "debug":
		if rt.Leaf != "health" {
			return false
		}
		return applyBridge(c, connID, topic, payload, received)
	}
	return false
}

func digitalKind(sub string) (Kind, bool) {
	switch sub {
	case "input":
		return KindInput, true
	case "output":
		return KindOutput, true
	case "temperature":
		return KindTemperature, true
	}
	return "", false
}

func (r *Registry) applyPoint(c *connState, connID string, kind Kind, rt route, topic string, payload []byte, received time.Time) bool {
	var env envelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return false
	}
	prev := c.points[topic]

	p := &Point{
		ConnectionID: connID,
		Topic:        topic,
		Kind:         kind,
		Address:      env.Address,
		Name:         env.Name,
		Device:       env.Device,
		UpdatedAt:    msTime(env.Timestamp, received),
	}
	if p.Address == 0 {
		if addr := atoiSafe(rt.Leaf); addr >= 0 {
			p.Address = addr
		}
	}

	// value is a bool for digital channels and a number for temperatures, so
	// it is decoded by trying both rather than by trusting the branch name.
	if len(env.Value) > 0 {
		var b bool
		if err := json.Unmarshal(env.Value, &b); err == nil {
			p.Bool = &b
		} else {
			var f float64
			if err := json.Unmarshal(env.Value, &f); err == nil {
				p.Number = &f
			}
		}
	}

	c.points[topic] = p

	// Only a genuine transition is journalled. The first value for a point is
	// skipped on purpose: connecting to a broker delivers hundreds of retained
	// messages at once, and none of them is a button being pressed.
	if p.Bool != nil && prev != nil && prev.Bool != nil && *prev.Bool != *p.Bool {
		e := Edge{
			ConnectionID: connID,
			Topic:        topic,
			Kind:         kind,
			Address:      p.Address,
			Name:         p.Name,
			From:         *prev.Bool,
			To:           *p.Bool,
			At:           p.UpdatedAt,
		}
		e.Label, e.Location, e.SensorType, e.AlarmZone, _ = describePoint(c, p.Name)
		r.journal.Append(e)
	}
	return true
}

func applyLight(c *connState, connID string, rt route, topic string, payload []byte, received time.Time) bool {
	var raw struct {
		envelope
		Status      int    `json:"status"`
		ActualLevel int    `json:"actual_level"`
		MaxLevel    int    `json:"max_level"`
		MinLevel    int    `json:"min_level"`
		FadeTime    int    `json:"fade_time"`
		FadeRate    int    `json:"fade_rate"`
		LastCommand string `json:"last_command"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(payload, &raw); err != nil {
		return false
	}

	addr := raw.Address
	if addr == 0 {
		if a := atoiSafe(rt.Leaf); a >= 0 {
			addr = a
		}
	}

	c.lights[addr] = &Light{
		ConnectionID: connID,
		Topic:        topic,
		Address:      addr,
		Name:         raw.Name,
		Device:       raw.Device,
		Status:       raw.Status,
		ActualLevel:  raw.ActualLevel,
		MaxLevel:     raw.MaxLevel,
		MinLevel:     raw.MinLevel,
		FadeTime:     raw.FadeTime,
		FadeRate:     raw.FadeRate,
		LastCommand:  raw.LastCommand,
		Error:        raw.Error,
		UpdatedAt:    msTime(raw.Timestamp, received),
	}
	return true
}

func applyMeta(c *connState, name string, payload []byte) bool {
	if name == "" {
		return false
	}
	var m sensorMeta
	if err := json.Unmarshal(payload, &m); err != nil {
		return false
	}
	c.meta[name] = m
	return true
}

func applyShade(c *connState, connID string, rt route, topic string, payload []byte, received time.Time) bool {
	var raw struct {
		Device      string `json:"device"`
		Slug        string `json:"slug"`
		Position    int    `json:"position"`
		LastCommand string `json:"last_command"`
	}
	if err := json.Unmarshal(payload, &raw); err != nil {
		return false
	}
	slug := raw.Slug
	if slug == "" {
		slug = rt.Leaf
	}
	if slug == "" {
		return false
	}
	c.shades[slug] = &Shade{
		ConnectionID: connID,
		Topic:        topic,
		Slug:         slug,
		Device:       raw.Device,
		Position:     raw.Position,
		LastCommand:  raw.LastCommand,
		UpdatedAt:    received,
	}
	return true
}

func applyActuator(c *connState, connID string, rt route, topic string, payload []byte, received time.Time) bool {
	slug := rt.Leaf
	if slug == "" {
		slug = rt.Sub
	}
	if slug == "" {
		return false
	}

	a := &Actuator{
		ConnectionID: connID,
		Topic:        topic,
		Group:        rt.Branch,
		Slug:         slug,
		UpdatedAt:    received,
	}

	// These branches have no shared schema, so a JSON object is kept whole and
	// a bare scalar is read as the state directly.
	trimmed := strings.TrimSpace(string(payload))

	// An empty payload is MQTT's way of clearing a retained message. Taking it
	// as a state would put a device in the panel with nothing to show, at the
	// exact moment the broker said to forget it.
	if trimmed == "" {
		return false
	}
	// Something that opens like an object and does not parse was meant to be
	// JSON and is broken. A scalar state is a word like "locked"; showing
	// `{"state":` as a device's state helps nobody.
	if strings.HasPrefix(trimmed, "{") && !json.Valid(payload) {
		return false
	}

	if strings.HasPrefix(trimmed, "{") {
		a.Raw = json.RawMessage(append([]byte(nil), payload...))
		var probe map[string]any
		if err := json.Unmarshal(payload, &probe); err == nil {
			a.State = firstString(probe, "state", "value", "status", "position")
		}
	} else {
		a.State = strings.Trim(trimmed, `"`)
	}

	c.actuators[rt.Branch+"/"+slug] = a
	return true
}

func applyElectricity(c *connState, connID string, rt route, topic string, payload []byte, received time.Time) bool {
	var raw struct {
		Timestamp float64 `json:"timestamp"`
		Name      string  `json:"name"`
		L1Voltage float64 `json:"L1_voltage"`
		L2Voltage float64 `json:"L2_voltage"`
		L3Voltage float64 `json:"L3_voltage"`
		L1Current float64 `json:"L1_current"`
		L2Current float64 `json:"L2_current"`
		L3Current float64 `json:"L3_current"`
		Frequency float64 `json:"frequency"`
		Imbalance *struct {
			Voltage float64 `json:"voltage"`
			Current float64 `json:"current"`
		} `json:"imbalance"`
		AlarmActive  bool     `json:"alarm_active"`
		ActiveAlarms []string `json:"active_alarms"`
	}
	if err := json.Unmarshal(payload, &raw); err != nil {
		return false
	}

	name := rt.Leaf
	if name == "" {
		name = raw.Name
	}
	e, ok := c.elec[name]
	if !ok {
		e = &Electricity{ConnectionID: connID, Name: name}
		c.elec[name] = e
	}
	e.Topic = topic
	e.Phases = [3]Phase{
		{Voltage: raw.L1Voltage, Current: raw.L1Current},
		{Voltage: raw.L2Voltage, Current: raw.L2Current},
		{Voltage: raw.L3Voltage, Current: raw.L3Current},
	}
	e.Frequency = raw.Frequency
	e.UpdatedAt = msTime(int64(raw.Timestamp), received)

	// Only the processed feed carries these, and the raw feed must not wipe
	// them when the two interleave.
	if raw.Imbalance != nil {
		e.VoltageImbalance = raw.Imbalance.Voltage
		e.CurrentImbalance = raw.Imbalance.Current
	}
	if rt.Sub == "processed" {
		e.AlarmActive = raw.AlarmActive
		e.ActiveAlarms = raw.ActiveAlarms
	}
	return true
}

func applyWatchdog(c *connState, connID, topic string, payload []byte, received time.Time) bool {
	var raw struct {
		UptimeS             int64  `json:"uptime_s"`
		Alive               bool   `json:"alive"`
		MQTTConnected       bool   `json:"mqtt_connected"`
		BackupMQTTConnected bool   `json:"backup_mqtt_connected"`
		DirectHAMode        bool   `json:"direct_ha_mode"`
		FallbackActive      bool   `json:"fallback_active"`
		AlarmMode           string `json:"alarm_mode"`
		AlarmTriggered      bool   `json:"alarm_triggered"`
		Ready               bool   `json:"ready"`
		PersistentValid     bool   `json:"persistent_valid"`
		Streams             map[string]struct {
			Count int64 `json:"count"`
			AgeS  int64 `json:"age_s"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(payload, &raw); err != nil {
		return false
	}

	w := &Watchdog{
		ConnectionID:        connID,
		Topic:               topic,
		UptimeS:             raw.UptimeS,
		Alive:               raw.Alive,
		MQTTConnected:       raw.MQTTConnected,
		BackupMQTTConnected: raw.BackupMQTTConnected,
		DirectHAMode:        raw.DirectHAMode,
		FallbackActive:      raw.FallbackActive,
		AlarmMode:           raw.AlarmMode,
		AlarmTriggered:      raw.AlarmTriggered,
		Ready:               raw.Ready,
		PersistentValid:     raw.PersistentValid,
		UpdatedAt:           received,
	}
	if len(raw.Streams) > 0 {
		w.Streams = make(map[string]Stream, len(raw.Streams))
		for k, v := range raw.Streams {
			w.Streams[k] = Stream{Count: v.Count, AgeS: v.AgeS}
		}
	}
	c.watchdog = w
	return true
}

func applyBridge(c *connState, connID, topic string, payload []byte, received time.Time) bool {
	var raw struct {
		Source      string  `json:"source"`
		NodeRedVer  string  `json:"nodered_version"`
		UptimeHours float64 `json:"uptime_hours"`
		Memory      struct {
			RSSMB float64 `json:"rss_mb"`
		} `json:"memory"`
		OS struct {
			FreeMemMB float64   `json:"freeMem_mb"`
			LoadAvg   []float64 `json:"loadAvg"`
		} `json:"os"`
	}
	if err := json.Unmarshal(payload, &raw); err != nil {
		return false
	}
	c.bridge = &Bridge{
		ConnectionID: connID,
		Topic:        topic,
		Source:       raw.Source,
		Version:      raw.NodeRedVer,
		UptimeHours:  raw.UptimeHours,
		RSSMB:        raw.Memory.RSSMB,
		FreeMemMB:    raw.OS.FreeMemMB,
		LoadAvg:      raw.OS.LoadAvg,
		UpdatedAt:    received,
	}
	return true
}

func (r *Registry) applyMeter(c *connState, connID string, rt route, topic string, payload []byte, received time.Time) bool {
	// Meters publish <prefix>/<name>/state and <prefix>/<name>/availability.
	name, leaf := rt.Branch, rt.Leaf
	if name == "" {
		return false
	}
	m, ok := c.meters[name]
	if !ok {
		m = &Meter{ConnectionID: connID, Name: name, Readings: map[string]float64{}}
		c.meters[name] = m
	}

	switch leaf {
	case "availability":
		m.Available = strings.EqualFold(strings.Trim(strings.TrimSpace(string(payload)), `"`), "online")
		m.UpdatedAt = received
		return true
	case "state":
		var raw map[string]any
		if err := json.Unmarshal(payload, &raw); err != nil {
			return false
		}
		readings := make(map[string]float64, len(raw))
		for k, v := range raw {
			if f, ok := v.(float64); ok {
				readings[k] = f
			}
		}
		m.Topic = topic
		m.Readings = readings
		m.UpdatedAt = received
		return true
	}
	return false
}

// firstString returns the first key present in m whose value is a string or a
// number, rendered as text.
func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		switch v := m[k].(type) {
		case string:
			return v
		case bool:
			if v {
				return "ON"
			}
			return "OFF"
		case float64:
			return trimFloat(v)
		}
	}
	return ""
}

func trimFloat(f float64) string {
	b, err := json.Marshal(f)
	if err != nil {
		return ""
	}
	return string(b)
}

// State is the whole projection the UI reads in one request.
type State struct {
	Points      []*Point       `json:"points"`
	Lights      []*Light       `json:"lights"`
	Shades      []*Shade       `json:"shades"`
	Actuators   []*Actuator    `json:"actuators"`
	Electricity []*Electricity `json:"electricity"`
	Meters      []*Meter       `json:"meters"`
	Watchdog    *Watchdog      `json:"watchdog,omitempty"`
	Bridge      *Bridge        `json:"bridge,omitempty"`
	Summary     Summary        `json:"summary"`
}

// Summary is the headline count set, cheap enough to poll.
type Summary struct {
	Inputs          int `json:"inputs"`
	Outputs         int `json:"outputs"`
	Temperatures    int `json:"temperatures"`
	Lights          int `json:"lights"`
	LightsWithError int `json:"lightsWithError"`
	LightsOn        int `json:"lightsOn"`
	Shades          int `json:"shades"`
	Actuators       int `json:"actuators"`
	Meters          int `json:"meters"`
	Described       int `json:"described"`
	ActiveInputs    int `json:"activeInputs"`
}

// Snapshot projects one connection's state, joining each point with its human
// description. The join happens here rather than on write because the two
// streams arrive in no fixed order.
func (r *Registry) Snapshot(connID string) State {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := State{
		Points:      []*Point{},
		Lights:      []*Light{},
		Shades:      []*Shade{},
		Actuators:   []*Actuator{},
		Electricity: []*Electricity{},
		Meters:      []*Meter{},
	}

	for id, c := range r.conns {
		if connID != "" && id != connID {
			continue
		}
		for _, p := range c.points {
			cp := *p
			label, location, sensorType, alarmZone, known := describePoint(c, cp.Name)
			if known {
				cp.Label, cp.Location, cp.SensorType, cp.AlarmZone = label, location, sensorType, alarmZone
				if m, ok := c.meta[cp.Name]; ok {
					cp.State = m.State
				}
				out.Summary.Described++
			}
			switch cp.Kind {
			case KindInput:
				out.Summary.Inputs++
			case KindOutput:
				out.Summary.Outputs++
			case KindTemperature:
				out.Summary.Temperatures++
			}
			if cp.Bool != nil && *cp.Bool {
				out.Summary.ActiveInputs++
			}
			out.Points = append(out.Points, &cp)
		}
		for _, l := range c.lights {
			cl := *l
			out.Summary.Lights++
			if cl.Error != "" {
				out.Summary.LightsWithError++
			}
			if cl.ActualLevel > 0 {
				out.Summary.LightsOn++
			}
			out.Lights = append(out.Lights, &cl)
		}
		for _, s := range c.shades {
			cs := *s
			out.Summary.Shades++
			out.Shades = append(out.Shades, &cs)
		}
		for _, a := range c.actuators {
			ca := *a
			out.Summary.Actuators++
			out.Actuators = append(out.Actuators, &ca)
		}
		for _, e := range c.elec {
			ce := *e
			out.Electricity = append(out.Electricity, &ce)
		}
		for _, m := range c.meters {
			cm := *m
			cm.Units = unitsFor(cm.Readings)
			out.Summary.Meters++
			out.Meters = append(out.Meters, &cm)
		}
		if c.watchdog != nil {
			cw := *c.watchdog
			out.Watchdog = &cw
		}
		if c.bridge != nil {
			cb := *c.bridge
			out.Bridge = &cb
		}
	}

	sortState(&out)
	return out
}

func sortState(s *State) {
	sort.Slice(s.Points, func(i, j int) bool {
		if s.Points[i].Kind != s.Points[j].Kind {
			return s.Points[i].Kind < s.Points[j].Kind
		}
		return s.Points[i].Address < s.Points[j].Address
	})
	sort.Slice(s.Lights, func(i, j int) bool { return s.Lights[i].Address < s.Lights[j].Address })
	sort.Slice(s.Shades, func(i, j int) bool { return s.Shades[i].Slug < s.Shades[j].Slug })
	sort.Slice(s.Actuators, func(i, j int) bool {
		if s.Actuators[i].Group != s.Actuators[j].Group {
			return s.Actuators[i].Group < s.Actuators[j].Group
		}
		return s.Actuators[i].Slug < s.Actuators[j].Slug
	})
	sort.Slice(s.Electricity, func(i, j int) bool { return s.Electricity[i].Name < s.Electricity[j].Name })
	sort.Slice(s.Meters, func(i, j int) bool { return s.Meters[i].Name < s.Meters[j].Name })
}

// Stats returns the point and light counts across every connection, for the
// status endpoint and the coalesced change events.
func (r *Registry) Stats() (points, lights int) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, c := range r.conns {
		points += len(c.points)
		lights += len(c.lights)
	}
	return points, lights
}

// Forget drops everything known about a connection.
func (r *Registry) Forget(connID string) {
	r.mu.Lock()
	delete(r.conns, connID)
	r.mu.Unlock()
}
