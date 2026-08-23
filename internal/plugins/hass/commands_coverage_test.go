package hass

import (
	"strings"
	"testing"
)

// Every action mqttview will send, and the payload each one produces.
//
// The point is not the switch statement — it is that a device gets to name its
// own payloads. A lock that expects "SECURE" rather than "LOCK" is not
// unusual, and sending the wrong word is a lock that silently does nothing.

func TestEveryActionUsesTheDevicesOwnPayload(t *testing.T) {
	tests := []struct {
		action    string
		component string
		cfg       entityConfig
		want      string
	}{
		{"lock", "lock", entityConfig{"payload_lock": "SECURE"}, "SECURE"},
		{"unlock", "lock", entityConfig{"payload_unlock": "FREE"}, "FREE"},
		{"open", "cover", entityConfig{"payload_open": "UP"}, "UP"},
		{"close", "cover", entityConfig{"payload_close": "DOWN"}, "DOWN"},
		{"stop", "cover", entityConfig{"payload_stop": "HALT"}, "HALT"},
		{"start", "vacuum", entityConfig{"payload_start": "go"}, "go"},
		{"pause", "vacuum", entityConfig{"payload_pause": "wait"}, "wait"},
		{"return_to_base", "vacuum", entityConfig{"payload_return_to_base": "home"}, "home"},
		{"locate", "vacuum", entityConfig{"payload_locate": "beep"}, "beep"},
		{"clean_spot", "vacuum", entityConfig{"payload_clean_spot": "spot"}, "spot"},
		{"install", "update", entityConfig{"payload_install": "upgrade"}, "upgrade"},
	}

	for _, tt := range tests {
		t.Run(tt.action, func(t *testing.T) {
			cfg := entityConfig{"command_topic": "dev/cmd"}
			for k, v := range tt.cfg {
				cfg[k] = v
			}
			e := &Entity{Component: tt.component, Config: cfg, CommandTopic: "dev/cmd"}

			cmd, err := BuildCommand(e, tt.action, "")
			if err != nil {
				t.Fatalf("%s: %v", tt.action, err)
			}
			if string(cmd.Payload) != tt.want {
				t.Errorf("payload = %q, want the device's own %q", cmd.Payload, tt.want)
			}
			if cmd.Topic != "dev/cmd" {
				t.Errorf("topic = %q", cmd.Topic)
			}
		})
	}
}

func TestEveryActionHasADefaultWhenTheDeviceSaysNothing(t *testing.T) {
	// Most devices publish no payload_* keys at all and expect the Home
	// Assistant defaults. Getting one of those wrong is a button that appears
	// to work and does not.
	defaults := map[string]string{
		"lock": "LOCK", "unlock": "UNLOCK",
		"open": "OPEN", "close": "CLOSE", "stop": "STOP",
		"start": "start", "pause": "pause", "return_to_base": "return_to_base",
		"locate": "locate", "clean_spot": "clean_spot", "install": "install",
	}
	for action, want := range defaults {
		e := &Entity{
			Component:    "cover",
			Config:       entityConfig{"command_topic": "dev/cmd"},
			CommandTopic: "dev/cmd",
		}
		cmd, err := BuildCommand(e, action, "")
		if err != nil {
			t.Errorf("%s: %v", action, err)
			continue
		}
		if string(cmd.Payload) != want {
			t.Errorf("%s defaulted to %q, want %q", action, cmd.Payload, want)
		}
	}
}

func TestNumericActionsGoToTheirOwnTopics(t *testing.T) {
	// Each of these has a topic of its own; sending a position to the plain
	// command topic would be read as an on/off payload.
	for _, tt := range []struct {
		action, key, value, wantTopic, wantPayload string
	}{
		{"set_position", "set_position_topic", "75", "dev/pos", "75"},
		{"set_tilt", "tilt_command_topic", "30", "dev/tilt", "30"},
		{"set_humidity", "target_humidity_command_topic", "55", "dev/hum", "55"},
		{"set_color_temp", "color_temp_command_topic", "370", "dev/ct", "370"},
		{"set_temperature", "temperature_command_topic", "21.5", "dev/temp", "21.5"},
	} {
		e := &Entity{
			Component:    "climate",
			Config:       entityConfig{"command_topic": "dev/cmd", tt.key: tt.wantTopic},
			CommandTopic: "dev/cmd",
		}
		cmd, err := BuildCommand(e, tt.action, tt.value)
		if err != nil {
			t.Errorf("%s: %v", tt.action, err)
			continue
		}
		if cmd.Topic != tt.wantTopic {
			t.Errorf("%s went to %q, want %q", tt.action, cmd.Topic, tt.wantTopic)
		}
		if string(cmd.Payload) != tt.wantPayload {
			t.Errorf("%s payload = %q", tt.action, cmd.Payload)
		}
	}
}

func TestAValueOutsideWhatTheDeviceAcceptsIsRefusedHere(t *testing.T) {
	// The device would refuse it anyway, silently. Refusing locally means the
	// person sees why instead of watching nothing happen.
	e := &Entity{
		Component: "climate",
		Config: entityConfig{
			"command_topic": "dev/cmd", "temperature_command_topic": "dev/temp",
			"min_temp": 5.0, "max_temp": 30.0,
		},
		CommandTopic: "dev/cmd",
	}

	for _, tc := range []struct{ value, want string }{
		{"35", "at most 30"},
		{"1", "at least 5"},
		{"warm", "number"},
	} {
		_, err := BuildCommand(e, "set_temperature", tc.value)
		if err == nil {
			t.Errorf("%q was accepted", tc.value)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("error for %q was %q, want it to mention %q", tc.value, err, tc.want)
		}
	}
}

func TestSetChecksWhatTheComponentAllows(t *testing.T) {
	number := &Entity{
		Component:    "number",
		Config:       entityConfig{"command_topic": "dev/cmd", "min": 0.0, "max": 10.0},
		CommandTopic: "dev/cmd",
	}
	if _, err := BuildCommand(number, "set", "11"); err == nil {
		t.Error("a number above max was accepted")
	}
	if _, err := BuildCommand(number, "set", "5"); err != nil {
		t.Errorf("a number in range was refused: %v", err)
	}

	sel := &Entity{
		Component:    "select",
		Config:       entityConfig{"command_topic": "dev/cmd", "options": []any{"eco", "boost"}},
		CommandTopic: "dev/cmd",
	}
	if _, err := BuildCommand(sel, "set", "turbo"); err == nil {
		t.Error("an option the device never offered was accepted")
	}
	if _, err := BuildCommand(sel, "set", "eco"); err != nil {
		t.Errorf("an offered option was refused: %v", err)
	}

	text := &Entity{
		Component:    "text",
		Config:       entityConfig{"command_topic": "dev/cmd", "max": 4.0},
		CommandTopic: "dev/cmd",
	}
	if _, err := BuildCommand(text, "set", "far too long"); err == nil {
		t.Error("text past the device's maximum was accepted")
	}
	if _, err := BuildCommand(text, "set", "ok"); err != nil {
		t.Errorf("short text was refused: %v", err)
	}
}

func TestAnActionWithNoTopicToSendItOn(t *testing.T) {
	// A device that advertises an action but no topic for it is misconfigured.
	// Publishing to "" would go to the broker as an invalid topic.
	e := &Entity{
		Component:    "cover",
		Config:       entityConfig{"command_topic": "dev/cmd"},
		CommandTopic: "dev/cmd",
	}
	if _, err := BuildCommand(e, "set_position", "50"); err == nil {
		t.Error("set_position was built without a set_position_topic")
	}
}
