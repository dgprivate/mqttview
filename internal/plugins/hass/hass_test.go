package hass

import (
	"testing"
)

func TestParseDiscoveryTopic(t *testing.T) {
	cases := []struct {
		topic     string
		wantOK    bool
		component string
		node      string
		object    string
		device    bool
	}{
		{"homeassistant/sensor/kitchen_temp/config", true, "sensor", "", "kitchen_temp", false},
		{"homeassistant/binary_sensor/node1/door/config", true, "binary_sensor", "node1", "door", false},
		{"homeassistant/device/0x1234/config", true, "device", "", "0x1234", true},
		{"homeassistant/sensor/kitchen_temp/state", false, "", "", "", false},
		{"homeassistant/sensor/config", false, "", "", "", false},
		{"other/sensor/x/config", false, "", "", "", false},
		{"homeassistant/a/b/c/d/config", false, "", "", "", false},
	}

	for _, tc := range cases {
		dt, ok := parseDiscoveryTopic("homeassistant", tc.topic)
		if ok != tc.wantOK {
			t.Errorf("parseDiscoveryTopic(%q) ok = %v, want %v", tc.topic, ok, tc.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if dt.Component != tc.component || dt.NodeID != tc.node ||
			dt.ObjectID != tc.object || dt.DeviceScoped != tc.device {
			t.Errorf("parseDiscoveryTopic(%q) = %+v", tc.topic, dt)
		}
		if got := dt.topicOf(); got != tc.topic {
			t.Errorf("topicOf() = %q, want round-trip to %q", got, tc.topic)
		}
	}
}

// TestParseConfigExpandsAbbreviations uses a payload in the shape Tasmota and
// Zigbee2MQTT actually publish: abbreviated keys plus the "~" base topic.
func TestParseConfigExpandsAbbreviations(t *testing.T) {
	payload := []byte(`{
        "~": "zigbee2mqtt/kitchen_sensor",
        "name": "Temperature",
        "stat_t": "~/state",
        "cmd_t": "~/set",
        "avty_t": "~/availability",
        "val_tpl": "{{ value_json.temperature }}",
        "unit_of_meas": "°C",
        "dev_cla": "temperature",
        "uniq_id": "kitchen_temp_z2m",
        "dev": {
            "ids": ["0x00124b0022"],
            "name": "Kitchen sensor",
            "mf": "Aqara",
            "mdl": "WSDCGQ11LM",
            "sw": "1.2.3"
        },
        "o": {"name": "Zigbee2MQTT", "sw": "1.35.0"}
    }`)

	cfg, err := parseConfig(payload)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}

	if got := cfg.str("state_topic"); got != "zigbee2mqtt/kitchen_sensor/state" {
		t.Errorf("state_topic = %q", got)
	}
	if got := cfg.str("command_topic"); got != "zigbee2mqtt/kitchen_sensor/set" {
		t.Errorf("command_topic = %q", got)
	}
	if got := cfg.str("availability_topic"); got != "zigbee2mqtt/kitchen_sensor/availability" {
		t.Errorf("availability_topic = %q", got)
	}
	if got := cfg.str("unit_of_measurement"); got != "°C" {
		t.Errorf("unit_of_measurement = %q", got)
	}
	if got := cfg.str("device_class"); got != "temperature" {
		t.Errorf("device_class = %q", got)
	}

	dev := entityConfig(cfg.submap("device"))
	if got := dev.str("manufacturer"); got != "Aqara" {
		t.Errorf("device.manufacturer = %q", got)
	}
	if got := dev.str("sw_version"); got != "1.2.3" {
		t.Errorf("device.sw_version = %q", got)
	}
	origin := entityConfig(cfg.submap("origin"))
	if got := origin.str("name"); got != "Zigbee2MQTT" {
		t.Errorf("origin.name = %q", got)
	}
	if _, leftover := cfg["~"]; leftover {
		t.Error("the ~ key should be consumed after expansion")
	}
}

func TestTopicBaseExpandsTrailingTilde(t *testing.T) {
	cfg, err := parseConfig([]byte(`{"~":"base/x","stat_t":"prefix/~"}`))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if got := cfg.str("state_topic"); got != "prefix/base/x" {
		t.Errorf("state_topic = %q, want prefix/base/x", got)
	}
}

func TestRenderTemplate(t *testing.T) {
	cases := []struct {
		name      string
		tpl       string
		payload   string
		want      any
		supported bool
	}{
		{"no template", "", "21.5", "21.5", true},
		{"value", "{{ value }}", "21.5", "21.5", true},
		{"value_json path", "{{ value_json.temperature }}", `{"temperature":21.5}`, 21.5, true},
		{"nested path", "{{ value_json.sensor.temp }}", `{"sensor":{"temp":3}}`, float64(3), true},
		{"bracket path", "{{ value_json['a']['b'] }}", `{"a":{"b":"x"}}`, "x", true},
		{"array index", "{{ value_json.list[1] }}", `{"list":[1,2]}`, float64(2), true},
		{"round filter", "{{ value_json.t | round(1) }}", `{"t":21.4567}`, 21.5, true},
		{"int filter", "{{ value_json.t | int }}", `{"t":21.9}`, int64(21), true},
		{"missing key yields null", "{{ value_json.nope }}", `{"a":1}`, nil, true},
		{"unsupported jinja", "{% if x %}a{% endif %}", "raw", "raw", false},
		{"unsupported filter", "{{ value | timestamp_local }}", "raw", "raw", false},
		{"non-json payload", "{{ value_json.a }}", "not json", "not json", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderTemplate(tc.tpl, []byte(tc.payload))
			if got.Supported != tc.supported {
				t.Fatalf("Supported = %v, want %v (value %v)", got.Supported, tc.supported, got.Value)
			}
			if got.Value != tc.want {
				t.Errorf("Value = %#v, want %#v", got.Value, tc.want)
			}
		})
	}
}

func TestRegistryTracksStateAndAvailability(t *testing.T) {
	reg := NewRegistry()
	cfg, err := parseConfig([]byte(`{
        "~": "home/light1",
        "name": "Ceiling",
        "stat_t": "~/state",
        "cmd_t": "~/set",
        "avty_t": "~/status",
        "pl_avail": "online",
        "pl_not_avail": "offline",
        "dev": {"ids": ["light1"], "name": "Ceiling light"}
    }`))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}

	dt, _ := parseDiscoveryTopic("homeassistant", "homeassistant/light/ceiling/config")
	entity, topics := reg.Upsert("conn1", dt, cfg)

	if entity.Name != "Ceiling light Ceiling" {
		t.Errorf("entity name = %q", entity.Name)
	}
	if !entity.Controllable {
		t.Error("an entity with a command topic should be controllable")
	}
	if !contains(topics, "home/light1/state") || !contains(topics, "home/light1/status") {
		t.Errorf("subscribe topics = %v", topics)
	}

	if updated := reg.ApplyState("conn1", "home/light1/state", []byte("ON")); len(updated) != 1 {
		t.Fatalf("state update touched %d entities, want 1", len(updated))
	}
	if entity.State == nil || entity.State.Value != "ON" {
		t.Errorf("state = %+v", entity.State)
	}

	reg.ApplyState("conn1", "home/light1/status", []byte("online"))
	if entity.Availability != AvailabilityOnline {
		t.Errorf("availability = %q, want online", entity.Availability)
	}
	reg.ApplyState("conn1", "home/light1/status", []byte("offline"))
	if entity.Availability != AvailabilityOffline {
		t.Errorf("availability = %q, want offline", entity.Availability)
	}

	devices := reg.Devices("conn1")
	if len(devices) != 1 || devices[0].Name != "Ceiling light" || len(devices[0].Entities) != 1 {
		t.Fatalf("devices = %+v", devices)
	}

	// Re-announcing the same entity must not lose the last known state.
	reg.Upsert("conn1", dt, cfg)
	again, ok := reg.Entity(entity.ID)
	if !ok || again.State == nil || again.State.Value != "ON" {
		t.Error("state was lost when the device re-announced itself")
	}

	// An empty payload deletes the entity and frees its topics.
	orphaned := reg.Remove("conn1", dt.topicOf())
	if !contains(orphaned, "home/light1/state") {
		t.Errorf("orphaned topics = %v", orphaned)
	}
	if d, _ := reg.Stats(); d != 0 {
		t.Error("removing the last entity should drop its device")
	}
}

func TestDeviceScopedDiscoveryConfigShape(t *testing.T) {
	cfg, err := parseConfig([]byte(`{
        "dev": {"ids": ["0xabc"], "name": "Multi sensor"},
        "o": {"name": "ESPHome"},
        "~": "esp/multi",
        "cmps": {
            "temp": {"p": "sensor", "stat_t": "~/temperature", "dev_cla": "temperature"},
            "sw":   {"p": "switch", "cmd_t": "~/relay/set", "stat_t": "~/relay"}
        }
    }`))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}

	comps, ok := cfg["components"].(map[string]any)
	if !ok || len(comps) != 2 {
		t.Fatalf("components = %#v", cfg["components"])
	}
	temp := entityConfig(comps["temp"].(map[string]any))
	if temp.str("platform") != "sensor" {
		t.Errorf("platform = %q", temp.str("platform"))
	}
	if got := temp.str("state_topic"); got != "esp/multi/temperature" {
		t.Errorf("component state_topic = %q, want the shared ~ applied", got)
	}
}

func TestBuildCommand(t *testing.T) {
	newEntity := func(component, payload string) *Entity {
		cfg, err := parseConfig([]byte(payload))
		if err != nil {
			t.Fatalf("parseConfig: %v", err)
		}
		reg := NewRegistry()
		dt, _ := parseDiscoveryTopic("homeassistant", "homeassistant/"+component+"/x/config")
		e, _ := reg.Upsert("c", dt, cfg)
		return e
	}

	t.Run("switch on and off", func(t *testing.T) {
		e := newEntity("switch", `{"cmd_t":"home/relay/set","pl_on":"1","pl_off":"0"}`)
		cmd, err := BuildCommand(e, "turn_on", "")
		if err != nil {
			t.Fatalf("turn_on: %v", err)
		}
		if cmd.Topic != "home/relay/set" || string(cmd.Payload) != "1" {
			t.Errorf("turn_on = %s -> %q", cmd.Topic, cmd.Payload)
		}
		cmd, _ = BuildCommand(e, "turn_off", "")
		if string(cmd.Payload) != "0" {
			t.Errorf("turn_off payload = %q", cmd.Payload)
		}
	})

	t.Run("light brightness respects scale", func(t *testing.T) {
		e := newEntity("light", `{"cmd_t":"l/set","bri_cmd_t":"l/bri","bri_scl":100}`)
		if _, err := BuildCommand(e, "set_brightness", "150"); err == nil {
			t.Error("a brightness above the scale should be rejected")
		}
		cmd, err := BuildCommand(e, "set_brightness", "40")
		if err != nil {
			t.Fatalf("set_brightness: %v", err)
		}
		if cmd.Topic != "l/bri" || string(cmd.Payload) != "40" {
			t.Errorf("set_brightness = %s -> %q", cmd.Topic, cmd.Payload)
		}
	})

	t.Run("number honours min and max", func(t *testing.T) {
		e := newEntity("number", `{"cmd_t":"n/set","min":5,"max":10}`)
		if _, err := BuildCommand(e, "set", "12"); err == nil {
			t.Error("a value above max should be rejected")
		}
		if _, err := BuildCommand(e, "set", "7"); err != nil {
			t.Errorf("a value inside the range should be accepted: %v", err)
		}
	})

	t.Run("select honours options", func(t *testing.T) {
		e := newEntity("select", `{"cmd_t":"s/set","ops":["low","high"]}`)
		if _, err := BuildCommand(e, "set", "medium"); err == nil {
			t.Error("an option outside the list should be rejected")
		}
		if _, err := BuildCommand(e, "set", "high"); err != nil {
			t.Errorf("a listed option should be accepted: %v", err)
		}
	})

	t.Run("cover position", func(t *testing.T) {
		e := newEntity("cover", `{"cmd_t":"c/set","set_pos_t":"c/pos"}`)
		if !contains(e.Actions, "set_position") || !contains(e.Actions, "open") {
			t.Fatalf("actions = %v", e.Actions)
		}
		cmd, err := BuildCommand(e, "set_position", "60")
		if err != nil {
			t.Fatalf("set_position: %v", err)
		}
		if cmd.Topic != "c/pos" || string(cmd.Payload) != "60" {
			t.Errorf("set_position = %s -> %q", cmd.Topic, cmd.Payload)
		}
	})

	t.Run("unknown component still allows raw publish", func(t *testing.T) {
		e := newEntity("weird_thing", `{"cmd_t":"w/cmd"}`)
		if !contains(e.Actions, "publish") {
			t.Fatalf("actions = %v", e.Actions)
		}
		cmd, err := BuildCommand(e, "publish", "anything")
		if err != nil {
			t.Fatalf("publish: %v", err)
		}
		if string(cmd.Payload) != "anything" {
			t.Errorf("payload = %q", cmd.Payload)
		}
	})

	t.Run("no command topic means no actions", func(t *testing.T) {
		e := newEntity("sensor", `{"stat_t":"s/state"}`)
		if e.Controllable || len(e.Actions) != 0 {
			t.Errorf("a read-only sensor should expose no actions, got %v", e.Actions)
		}
	})
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
