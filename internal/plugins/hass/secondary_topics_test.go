package hass

import (
	"fmt"
	"testing"
)

// A Home Assistant entity publishes more than one state topic: a light reports
// its brightness separately from its on/off state, a cover its position, a
// climate device its current temperature. Each has its own template, and using
// the primary one on a secondary topic misreads the payload — a light would
// show its brightness as its state.

func TestEachSecondaryStateTopicUsesItsOwnTemplate(t *testing.T) {
	tests := []struct {
		name  string
		cfg   entityConfig
		topic string
		body  string
		want  string
	}{
		{
			name: "brightness",
			cfg: entityConfig{
				"name": "L", "unique_id": "l1", "state_topic": "l/state",
				"value_template":            "{{ value_json.state }}",
				"brightness_state_topic":    "l/bri",
				"brightness_value_template": "{{ value_json.bri }}",
			},
			topic: "l/bri", body: `{"bri":128}`, want: "128",
		},
		{
			name: "a light whose secondary topic uses _state_template",
			cfg: entityConfig{
				"name": "L", "unique_id": "l2", "state_topic": "l/state",
				"color_temp_state_topic":    "l/ct",
				"color_temp_state_template": "{{ value_json.ct }}",
			},
			topic: "l/ct", body: `{"ct":370}`, want: "370",
		},
		{
			name: "a cover's position",
			cfg: entityConfig{
				"name": "C", "unique_id": "c1", "state_topic": "c/state",
				"position_topic":    "c/pos",
				"position_template": "{{ value_json.p }}",
			},
			topic: "c/pos", body: `{"p":75}`, want: "75",
		},
		{
			name: "a cover's tilt",
			cfg: entityConfig{
				"name": "C", "unique_id": "c2", "state_topic": "c/state",
				"tilt_status_topic":    "c/tilt",
				"tilt_status_template": "{{ value_json.t }}",
			},
			topic: "c/tilt", body: `{"t":30}`, want: "30",
		},
		{
			name: "a thermostat's current temperature",
			cfg: entityConfig{
				"name": "T", "unique_id": "t1", "state_topic": "t/state",
				"current_temperature_topic":    "t/cur",
				"current_temperature_template": "{{ value_json.temp }}",
			},
			topic: "t/cur", body: `{"temp":21.5}`, want: "21.5",
		},
		{
			name: "a thermostat's action",
			cfg: entityConfig{
				"name": "T", "unique_id": "t2", "state_topic": "t/state",
				"action_topic":    "t/action",
				"action_template": "{{ value_json.a }}",
			},
			topic: "t/action", body: `{"a":"heating"}`, want: "heating",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := NewRegistry()
			e, topics := upsert(t, reg, "light", "x", tt.cfg)

			// The secondary topic is subscribed to, or nothing would arrive.
			var subscribed bool
			for _, topic := range topics {
				if topic == tt.topic {
					subscribed = true
				}
			}
			if !subscribed {
				t.Fatalf("%s is not among the subscribed topics %v", tt.topic, topics)
			}

			if changed := reg.ApplyState("c1", tt.topic, []byte(tt.body)); len(changed) != 1 {
				t.Fatalf("the message changed %d entities", len(changed))
			}

			got, _ := reg.Entity(e.ID)
			if got.State == nil {
				t.Fatal("no state was recorded")
			}
			if v := fmt.Sprint(got.State.Value); v != tt.want {
				t.Errorf("value = %q, want %q", v, tt.want)
			}
			if !got.State.TemplateSupported {
				t.Error("the secondary template was not evaluated")
			}
		})
	}
}

func TestASecondaryTopicWithNoTemplateShowsThePayload(t *testing.T) {
	reg := NewRegistry()
	// Many devices publish a bare number on the brightness topic and declare
	// no template at all. There is nothing to evaluate, and the payload is the
	// value rather than an error.
	e, _ := upsert(t, reg, "light", "x", entityConfig{
		"name": "L", "unique_id": "l3", "state_topic": "l/state",
		"brightness_state_topic": "l/bri",
	})

	reg.ApplyState("c1", "l/bri", []byte("200"))

	got, _ := reg.Entity(e.ID)
	if got.State == nil || fmt.Sprint(got.State.Value) != "200" {
		t.Fatalf("state = %+v", got.State)
	}
}

func TestAnEntityIsOfflineIfAnySourceSaysSo(t *testing.T) {
	reg := NewRegistry()
	// Two availability topics under availability_mode "all": a device whose
	// bridge is up but whose radio is down is not available, and showing it as
	// available means a command silently goes nowhere.
	e, _ := upsert(t, reg, "sensor", "s1", entityConfig{
		"name": "S", "unique_id": "s1", "state_topic": "s/state",
		"availability": []any{
			map[string]any{"topic": "bridge/status"},
			map[string]any{"topic": "s/status"},
		},
		"availability_mode": "all",
	})

	reg.ApplyState("c1", "bridge/status", []byte("online"))
	reg.ApplyState("c1", "s/status", []byte("offline"))

	got, _ := reg.Entity(e.ID)
	if got.Availability == AvailabilityOnline {
		t.Errorf("availability = %q with one source offline", got.Availability)
	}

	reg.ApplyState("c1", "s/status", []byte("online"))
	got, _ = reg.Entity(e.ID)
	if got.Availability != AvailabilityOnline {
		t.Errorf("availability = %q with every source online", got.Availability)
	}
}

func TestAvailabilityModeAnyNeedsOnlyOneSource(t *testing.T) {
	reg := NewRegistry()
	// "any" is what a device with a redundant bridge declares: one route being
	// down does not mean the device is unreachable.
	e, _ := upsert(t, reg, "sensor", "s2", entityConfig{
		"name": "S", "unique_id": "s2", "state_topic": "s/state",
		"availability": []any{
			map[string]any{"topic": "a/status"},
			map[string]any{"topic": "b/status"},
		},
		"availability_mode": "any",
	})

	reg.ApplyState("c1", "a/status", []byte("offline"))
	reg.ApplyState("c1", "b/status", []byte("online"))

	got, _ := reg.Entity(e.ID)
	if got.Availability != AvailabilityOnline {
		t.Errorf("availability = %q, want online: one source is up", got.Availability)
	}
}
