package hass

import (
	"strings"
	"testing"
)

// Every component type mqttview claims to control, exercised through the same
// path the API uses. The point is that a command is built from what the device
// itself advertised, not from an assumption about how devices behave.

func entityFor(component string, cfg entityConfig) *Entity {
	reg := NewRegistry()
	dt := discoveryTopic{Prefix: "homeassistant", Component: component, ObjectID: "obj"}
	e, _ := reg.Upsert("c1", dt, cfg)
	return e
}

func TestActionsAreOfferedPerComponent(t *testing.T) {
	tests := []struct {
		component string
		cfg       entityConfig
		want      []string
	}{
		{"switch", entityConfig{"command_topic": "c"}, []string{"turn_on", "turn_off", "toggle"}},
		{"light", entityConfig{"command_topic": "c", "brightness_command_topic": "b"}, []string{"turn_on", "set_brightness"}},
		{"fan", entityConfig{"command_topic": "c", "percentage_command_topic": "p"}, []string{"turn_on", "set_percentage"}},
		{"lock", entityConfig{"command_topic": "c"}, []string{"lock", "unlock"}},
		{"cover", entityConfig{"command_topic": "c"}, []string{"open", "close", "stop"}},
		{"button", entityConfig{"command_topic": "c"}, []string{"press"}},
		{"number", entityConfig{"command_topic": "c"}, []string{"set"}},
		{"select", entityConfig{"command_topic": "c", "options": []any{"a", "b"}}, []string{"set"}},
		{"climate", entityConfig{"temperature_command_topic": "t"}, []string{"set_temperature"}},
		{"humidifier", entityConfig{"command_topic": "c"}, []string{"turn_on", "turn_off"}},
		{"vacuum", entityConfig{"command_topic": "c"}, []string{"start", "stop"}},
		{"update", entityConfig{"command_topic": "c"}, []string{"install"}},
		{"alarm_control_panel", entityConfig{"command_topic": "c"}, []string{"disarm"}},
		{"siren", entityConfig{"command_topic": "c"}, []string{"turn_on", "turn_off"}},
	}

	for _, tt := range tests {
		t.Run(tt.component, func(t *testing.T) {
			e := entityFor(tt.component, tt.cfg)
			got := map[string]bool{}
			for _, a := range e.Actions {
				got[a] = true
			}
			for _, want := range tt.want {
				if !got[want] {
					t.Errorf("%s does not offer %q; it offers %v", tt.component, want, e.Actions)
				}
			}
			if !e.Controllable {
				t.Errorf("%s with a command topic is not controllable", tt.component)
			}
		})
	}
}

func TestAnEntityWithNoCommandTopicIsNotControllable(t *testing.T) {
	e := entityFor("sensor", entityConfig{"state_topic": "s"})
	if e.Controllable || len(e.Actions) != 0 {
		t.Fatalf("a read-only sensor was offered %v", e.Actions)
	}
}

func TestBuildCommandUsesWhatTheDeviceAdvertised(t *testing.T) {
	tests := []struct {
		name      string
		component string
		cfg       entityConfig
		action    string
		value     string
		wantTopic string
		wantBody  string
	}{
		{
			name: "a switch with custom payloads", component: "switch",
			cfg:       entityConfig{"command_topic": "home/s/set", "payload_on": "ON!", "payload_off": "OFF!"},
			action:    "turn_on",
			wantTopic: "home/s/set", wantBody: "ON!",
		},
		{
			name: "a light's brightness has its own topic", component: "light",
			cfg:    entityConfig{"command_topic": "l/set", "brightness_command_topic": "l/bri"},
			action: "set_brightness", value: "128",
			wantTopic: "l/bri", wantBody: "128",
		},
		{
			name: "a lock", component: "lock",
			cfg:       entityConfig{"command_topic": "d/set", "payload_lock": "LOCK"},
			action:    "lock",
			wantTopic: "d/set", wantBody: "LOCK",
		},
		{
			name: "a cover open", component: "cover",
			cfg:       entityConfig{"command_topic": "c/set", "payload_open": "UP"},
			action:    "open",
			wantTopic: "c/set", wantBody: "UP",
		},
		{
			name: "a button press", component: "button",
			cfg:       entityConfig{"command_topic": "b/set", "payload_press": "GO"},
			action:    "press",
			wantTopic: "b/set", wantBody: "GO",
		},
		{
			name: "a raw publish", component: "switch",
			cfg:    entityConfig{"command_topic": "s/set"},
			action: "publish", value: "anything",
			wantTopic: "s/set", wantBody: "anything",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, err := BuildCommand(entityFor(tt.component, tt.cfg), tt.action, tt.value)
			if err != nil {
				t.Fatalf("BuildCommand: %v", err)
			}
			if cmd.Topic != tt.wantTopic {
				t.Errorf("topic = %q, want %q", cmd.Topic, tt.wantTopic)
			}
			if string(cmd.Payload) != tt.wantBody {
				t.Errorf("payload = %q, want %q", cmd.Payload, tt.wantBody)
			}
		})
	}
}

func TestBuildCommandValidatesAgainstWhatTheDeviceAllows(t *testing.T) {
	tests := []struct {
		name      string
		component string
		cfg       entityConfig
		action    string
		value     string
		wantErr   string
	}{
		{
			name: "an unknown action", component: "switch",
			cfg: entityConfig{"command_topic": "c"}, action: "explode",
			wantErr: "unknown action",
		},
		{
			name: "an action the device does not offer", component: "sensor",
			cfg: entityConfig{"state_topic": "s"}, action: "turn_on",
			wantErr: "",
		},
		{
			name: "a number outside the advertised range", component: "number",
			cfg:    entityConfig{"command_topic": "c", "min": 0.0, "max": 10.0},
			action: "set", value: "99",
			wantErr: "at most 10",
		},
		{
			name: "an option the device did not list", component: "select",
			cfg:    entityConfig{"command_topic": "c", "options": []any{"a", "b"}},
			action: "set", value: "c",
			wantErr: "one of",
		},
		{
			name: "a brightness that is not a number", component: "light",
			cfg:    entityConfig{"command_topic": "c", "brightness_command_topic": "b"},
			action: "set_brightness", value: "bright",
			wantErr: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := BuildCommand(entityFor(tt.component, tt.cfg), tt.action, tt.value)
			if err == nil {
				t.Fatal("the command was accepted")
			}
			if tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestSelectAcceptsAnAdvertisedOption(t *testing.T) {
	cmd, err := BuildCommand(
		entityFor("select", entityConfig{"command_topic": "c", "options": []any{"eco", "boost"}}),
		"set", "boost")
	if err != nil {
		t.Fatal(err)
	}
	if string(cmd.Payload) != "boost" {
		t.Errorf("payload = %q", cmd.Payload)
	}
}

func TestNumberAcceptsAValueInRange(t *testing.T) {
	cmd, err := BuildCommand(
		entityFor("number", entityConfig{"command_topic": "c", "min": 0.0, "max": 100.0}),
		"set", "42")
	if err != nil {
		t.Fatal(err)
	}
	if string(cmd.Payload) != "42" {
		t.Errorf("payload = %q", cmd.Payload)
	}
}

func TestRetainAndQoSFollowTheDiscoveryConfig(t *testing.T) {
	cmd, err := BuildCommand(
		entityFor("switch", entityConfig{"command_topic": "c", "retain": true, "qos": 2.0}),
		"turn_on", "")
	if err != nil {
		t.Fatal(err)
	}
	// A device that asked for retained commands gets them; guessing either way
	// changes what happens after a broker restart.
	if !cmd.Retain || cmd.QoS != 2 {
		t.Errorf("retain=%v qos=%d, want the advertised values", cmd.Retain, cmd.QoS)
	}
}

func TestConfigAccessorsAreForgivingAboutTypes(t *testing.T) {
	// Real firmware sends numbers as strings and booleans as "true", so each
	// accessor takes what it is given rather than what the spec says.
	cfg := entityConfig{
		"a": "text", "b": 12.5, "c": true, "d": false,
		"strTrue": "yes", "strFalse": "off", "numTrue": 1.0, "numZero": 0.0,
		"numStr": "3.5", "list": []any{"x", "y", 1.0}, "single": "only",
		"nested": map[string]any{"k": "v"},
	}

	if cfg.str("a") != "text" || cfg.str("b") != "12.5" || cfg.str("c") != "true" || cfg.str("d") != "false" {
		t.Error("str did not render the scalar types")
	}
	if cfg.str("missing") != "" {
		t.Error("str invented a value")
	}
	if cfg.strDefault("missing", "fallback") != "fallback" || cfg.strDefault("a", "fallback") != "text" {
		t.Error("strDefault is wrong")
	}

	for key, want := range map[string]bool{
		"c": true, "d": false, "strTrue": true, "strFalse": false, "numTrue": true, "numZero": false,
	} {
		if got := cfg.boolean(key, !want); got != want {
			t.Errorf("boolean(%q) = %v, want %v", key, got, want)
		}
	}
	if !cfg.boolean("missing", true) {
		t.Error("boolean ignored its default")
	}
	if cfg.boolean("a", false) {
		t.Error("an unparseable string should fall back to the default")
	}

	if v, ok := cfg.number("b"); !ok || v != 12.5 {
		t.Errorf("number = %v %v", v, ok)
	}
	if v, ok := cfg.number("numStr"); !ok || v != 3.5 {
		t.Errorf("number from a string = %v %v", v, ok)
	}
	if _, ok := cfg.number("a"); ok {
		t.Error("number parsed a word")
	}

	if got := cfg.strings("list"); len(got) != 2 {
		t.Errorf("strings dropped the non-strings wrongly: %v", got)
	}
	if got := cfg.strings("single"); len(got) != 1 || got[0] != "only" {
		t.Errorf("a lone string should read as a one-element list: %v", got)
	}
	if cfg.strings("missing") != nil {
		t.Error("strings invented a list")
	}
	if cfg.submap("nested")["k"] != "v" {
		t.Error("submap did not read the nested block")
	}
	if cfg.submap("a") != nil {
		t.Error("submap read a scalar as a map")
	}
}

func TestTrimFloatDropsTrailingZeros(t *testing.T) {
	for in, want := range map[float64]string{
		1: "1", 1.5: "1.5", 21.50: "21.5", 0: "0", -3.25: "-3.25",
	} {
		if got := trimFloat(in); got != want {
			t.Errorf("trimFloat(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestJSONAttributesAreParsed(t *testing.T) {
	// json_attributes_topic carries a whole object, optionally through a
	// template that picks a sub-object out of it.
	got := parseAttributes("", []byte(`{"rssi":-60,"linkquality":120}`))
	if got == nil || got["rssi"] != float64(-60) {
		t.Fatalf("attributes = %+v", got)
	}
	if parseAttributes("", []byte("not json")) != nil {
		t.Error("rubbish became attributes")
	}

	nested := parseAttributes("{{ value_json.state }}", []byte(`{"state":{"a":1}}`))
	if nested == nil || nested["a"] != float64(1) {
		t.Errorf("a template did not select the sub-object: %+v", nested)
	}
}

func TestClearConnectionDropsOnlyThatBroker(t *testing.T) {
	reg := NewRegistry()
	for _, conn := range []string{"c1", "c2"} {
		reg.Upsert(conn, discoveryTopic{Prefix: "homeassistant", Component: "sensor", ObjectID: "o"},
			entityConfig{"name": "T", "state_topic": "t", "unique_id": conn})
	}

	reg.ClearConnection("c1")
	if len(reg.Entities("c1")) != 0 {
		t.Error("the cleared connection still has entities")
	}
	if len(reg.Entities("c2")) != 1 {
		t.Error("clearing one connection took another's entities with it")
	}
}

// Every remaining action, each against a device that advertised the topic it
// belongs on. The point of the table is that a command goes to the topic the
// device named for it, not to the generic command topic.
func TestEveryActionGoesToItsOwnTopic(t *testing.T) {
	tests := []struct {
		component string
		cfg       entityConfig
		action    string
		value     string
		wantTopic string
		wantBody  string
	}{
		{"cover", entityConfig{"command_topic": "c", "set_position_topic": "c/pos"},
			"set_position", "60", "c/pos", "60"},
		{"cover", entityConfig{"command_topic": "c", "tilt_command_topic": "c/tilt"},
			"set_tilt", "30", "c/tilt", "30"},
		{"fan", entityConfig{"command_topic": "f", "percentage_command_topic": "f/pct"},
			"set_percentage", "50", "f/pct", "50"},
		{"fan", entityConfig{"command_topic": "f", "oscillation_command_topic": "f/osc", "payload_oscillation_on": "SWING"},
			"set_oscillation", "true", "f/osc", "SWING"},
		{"fan", entityConfig{"command_topic": "f", "preset_mode_command_topic": "f/preset", "preset_modes": []any{"eco"}},
			"set_preset", "eco", "f/preset", "eco"},
		{"climate", entityConfig{"temperature_command_topic": "cl/temp"},
			"set_temperature", "21.5", "cl/temp", "21.5"},
		{"climate", entityConfig{"mode_command_topic": "cl/mode", "modes": []any{"heat", "off"}},
			"set_mode", "heat", "cl/mode", "heat"},
		{"climate", entityConfig{"fan_mode_command_topic": "cl/fan", "fan_modes": []any{"low"}},
			"set_fan_mode", "low", "cl/fan", "low"},
		{"climate", entityConfig{"swing_mode_command_topic": "cl/swing", "swing_modes": []any{"on"}},
			"set_swing_mode", "on", "cl/swing", "on"},
		{"humidifier", entityConfig{"command_topic": "h", "target_humidity_command_topic": "h/hum"},
			"set_humidity", "45", "h/hum", "45"},
		{"light", entityConfig{"command_topic": "l", "color_temp_command_topic": "l/ct"},
			"set_color_temp", "300", "l/ct", "300"},
		{"light", entityConfig{"command_topic": "l", "effect_command_topic": "l/fx", "effect_list": []any{"rainbow"}},
			"set_effect", "rainbow", "l/fx", "rainbow"},
		{"vacuum", entityConfig{"command_topic": "v"}, "pause", "", "v", "pause"},
		{"vacuum", entityConfig{"command_topic": "v"}, "return_to_base", "", "v", "return_to_base"},
		{"vacuum", entityConfig{"command_topic": "v"}, "locate", "", "v", "locate"},
		{"vacuum", entityConfig{"command_topic": "v"}, "clean_spot", "", "v", "clean_spot"},
		{"alarm_control_panel", entityConfig{"command_topic": "a"}, "arm_home", "", "a", "ARM_HOME"},
		{"alarm_control_panel", entityConfig{"command_topic": "a"}, "arm_away", "", "a", "ARM_AWAY"},
		{"alarm_control_panel", entityConfig{"command_topic": "a"}, "arm_night", "", "a", "ARM_NIGHT"},
		{"alarm_control_panel", entityConfig{"command_topic": "a"}, "disarm", "", "a", "DISARM"},
		{"scene", entityConfig{"command_topic": "s"}, "press", "", "s", "ON"},
		{"update", entityConfig{"command_topic": "u"}, "install", "", "u", "install"},
		{"text", entityConfig{"command_topic": "t"}, "set", "hello", "t", "hello"},
	}

	for _, tt := range tests {
		name := tt.component + "/" + tt.action
		t.Run(name, func(t *testing.T) {
			cmd, err := BuildCommand(entityFor(tt.component, tt.cfg), tt.action, tt.value)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if cmd.Topic != tt.wantTopic {
				t.Errorf("topic = %q, want %q", cmd.Topic, tt.wantTopic)
			}
			if string(cmd.Payload) != tt.wantBody {
				t.Errorf("payload = %q, want %q", cmd.Payload, tt.wantBody)
			}
		})
	}
}

func TestToggleDependsOnTheLastKnownState(t *testing.T) {
	reg := NewRegistry()
	dt := discoveryTopic{Prefix: "homeassistant", Component: "switch", ObjectID: "s"}
	cfg := entityConfig{"command_topic": "s/set", "state_topic": "s/state"}
	e, _ := reg.Upsert("c1", dt, cfg)

	// MQTT has no toggle, so it is derived. With no state yet, the safe guess
	// is to turn on.
	cmd, err := BuildCommand(e, "toggle", "")
	if err != nil {
		t.Fatal(err)
	}
	if string(cmd.Payload) != "ON" {
		t.Errorf("with no state, toggle sent %q", cmd.Payload)
	}

	reg.ApplyState("c1", "s/state", []byte("ON"))
	e, _ = reg.Entity(e.ID)
	cmd, err = BuildCommand(e, "toggle", "")
	if err != nil {
		t.Fatal(err)
	}
	if string(cmd.Payload) != "OFF" {
		t.Errorf("while on, toggle sent %q, want OFF", cmd.Payload)
	}
}

func TestClimatePowerHasItsOwnTopic(t *testing.T) {
	// A thermostat's on/off is not its mode topic, and sending to the wrong
	// one silently does nothing.
	cfg := entityConfig{"mode_command_topic": "cl/mode", "power_command_topic": "cl/power"}

	for _, action := range []string{"turn_on", "turn_off"} {
		cmd, err := BuildCommand(entityFor("climate", cfg), action, "")
		if err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		if cmd.Topic != "cl/power" {
			t.Errorf("%s went to %q, want the power topic", action, cmd.Topic)
		}
	}
}

func TestActionsThatNeedATopicTheDeviceDidNotAdvertise(t *testing.T) {
	// The device offers no brightness topic, so the action is not available
	// and must be refused rather than sent to the generic command topic —
	// where it would be read as an on/off payload.
	for _, tc := range []struct {
		component, action string
		cfg               entityConfig
	}{
		{"light", "set_brightness", entityConfig{"command_topic": "l"}},
		{"cover", "set_position", entityConfig{"command_topic": "c"}},
		{"climate", "set_temperature", entityConfig{"command_topic": "cl"}},
		{"fan", "set_percentage", entityConfig{"command_topic": "f"}},
		{"light", "set_effect", entityConfig{"command_topic": "l"}},
	} {
		if _, err := BuildCommand(entityFor(tc.component, tc.cfg), tc.action, "50"); err == nil {
			t.Errorf("%s/%s was accepted with no topic for it", tc.component, tc.action)
		}
	}
}

func TestModeAndPresetMustBeOneTheDeviceListed(t *testing.T) {
	for _, tc := range []struct {
		component, action string
		cfg               entityConfig
	}{
		{"climate", "set_mode", entityConfig{"mode_command_topic": "m", "modes": []any{"heat"}}},
		{"climate", "set_fan_mode", entityConfig{"fan_mode_command_topic": "f", "fan_modes": []any{"low"}}},
		{"fan", "set_preset", entityConfig{"preset_mode_command_topic": "p", "preset_modes": []any{"eco"}}},
		{"light", "set_effect", entityConfig{"effect_command_topic": "e", "effect_list": []any{"rainbow"}}},
	} {
		if _, err := BuildCommand(entityFor(tc.component, tc.cfg), tc.action, "not-on-the-list"); err == nil {
			t.Errorf("%s/%s accepted a value the device never advertised", tc.component, tc.action)
		}
	}
}

func TestPercentagesAreBounded(t *testing.T) {
	cfg := entityConfig{"command_topic": "f", "percentage_command_topic": "f/pct"}

	for _, bad := range []string{"-1", "101", "half"} {
		if _, err := BuildCommand(entityFor("fan", cfg), "set_percentage", bad); err == nil {
			t.Errorf("percentage %q was accepted", bad)
		}
	}
	for _, good := range []string{"0", "50", "100"} {
		if _, err := BuildCommand(entityFor("fan", cfg), "set_percentage", good); err != nil {
			t.Errorf("percentage %q was refused: %v", good, err)
		}
	}
}

func TestBrightnessIsBoundedToTheAdvertisedScale(t *testing.T) {
	cfg := entityConfig{
		"command_topic": "l", "brightness_command_topic": "l/bri", "brightness_scale": 100.0,
	}
	if _, err := BuildCommand(entityFor("light", cfg), "set_brightness", "101"); err == nil {
		t.Error("a brightness past the advertised scale was accepted")
	}
	if _, err := BuildCommand(entityFor("light", cfg), "set_brightness", "100"); err != nil {
		t.Errorf("the top of the scale was refused: %v", err)
	}
}

func TestOscillationTakesEitherSpelling(t *testing.T) {
	cfg := entityConfig{"command_topic": "f", "oscillation_command_topic": "f/osc"}

	for _, on := range []string{"true", "on", "1", "TRUE"} {
		cmd, err := BuildCommand(entityFor("fan", cfg), "set_oscillation", on)
		if err != nil {
			t.Fatalf("%q: %v", on, err)
		}
		if string(cmd.Payload) != "oscillate_on" && string(cmd.Payload) != "ON" {
			t.Errorf("%q sent %q", on, cmd.Payload)
		}
	}
	for _, off := range []string{"false", "off", "0"} {
		cmd, err := BuildCommand(entityFor("fan", cfg), "set_oscillation", off)
		if err != nil {
			t.Fatalf("%q: %v", off, err)
		}
		if string(cmd.Payload) == "oscillate_on" || string(cmd.Payload) == "ON" {
			t.Errorf("%q sent the on payload %q", off, cmd.Payload)
		}
	}
}
