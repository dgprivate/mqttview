package hass

import (
	"fmt"
	"testing"
)

// mqttview evaluates the value_template shapes that real devices actually use
// and refuses the rest rather than embedding a Jinja engine. These are the
// shapes, taken from Zigbee2MQTT, Tasmota, ESPHome and Shelly payloads.

func TestSupportedTemplateShapes(t *testing.T) {
	tests := []struct {
		name    string
		tpl     string
		payload string
		want    string
	}{
		{"the whole payload", "{{ value }}", "21.5", "21.5"},
		{"a top-level field", "{{ value_json.temp }}", `{"temp":21.5}`, "21.5"},
		{"a nested field", "{{ value_json.a.b.c }}", `{"a":{"b":{"c":7}}}`, "7"},
		{"bracket notation", `{{ value_json['odd key'] }}`, `{"odd key":3}`, "3"},
		{"double-quoted brackets", `{{ value_json["odd key"] }}`, `{"odd key":3}`, "3"},
		{"an array index", "{{ value_json.items[1] }}", `{"items":[10,20]}`, "20"},
		{"round with a precision", "{{ value_json.x | round(1) }}", `{"x":1.2345}`, "1.2"},
		{"round with no argument", "{{ value_json.x | round }}", `{"x":1.6}`, "2"},
		{"int", "{{ value_json.x | int }}", `{"x":"42"}`, "42"},
		{"float", "{{ value_json.x | float }}", `{"x":"1.5"}`, "1.5"},
		{"string", "{{ value_json.x | string }}", `{"x":42}`, "42"},
		{"upper", "{{ value_json.s | upper }}", `{"s":"on"}`, "ON"},
		{"lower", "{{ value_json.s | lower }}", `{"s":"ON"}`, "on"},
		{"whitespace is ignored", "{{value_json.temp}}", `{"temp":1}`, "1"},
		{"an empty template is the payload", "", "raw", "raw"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := renderTemplate(tt.tpl, []byte(tt.payload))
			if !res.Supported {
				t.Fatalf("%q was not supported", tt.tpl)
			}
			if got := fmt.Sprint(res.Value); got != tt.want {
				t.Errorf("value = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestUnsupportedTemplatesFallBackToTheRawPayload(t *testing.T) {
	// The rule is that mqttview will not guess. Anything it cannot evaluate
	// shows the payload untouched and is marked, so the UI can say so rather
	// than presenting a wrong number as a reading.
	for _, tpl := range []string{
		"{% if value %}on{% endif %}",
		"{{ value_json.a }} and {{ value_json.b }}",
		"{{ value_json.x | some_unknown_filter }}",
		"{{ states('sensor.other') }}",
		"{{",
		"{{ }}",
	} {
		res := renderTemplate(tpl, []byte(`{"a":1}`))
		if res.Supported {
			t.Errorf("%q was reported as evaluated", tpl)
		}
		if res.Value != `{"a":1}` {
			t.Errorf("%q gave %v, want the raw payload", tpl, res.Value)
		}
	}
}

func TestATemplateOverAPayloadThatIsNotJSON(t *testing.T) {
	// A device that advertises value_json but sends plain text: the template
	// cannot be applied, and the payload is shown as it arrived.
	res := renderTemplate("{{ value_json.temp }}", []byte("not json"))
	if res.Value != "not json" {
		t.Errorf("value = %v", res.Value)
	}
}

func TestAMissingFieldYieldsNothingRatherThanAWrongValue(t *testing.T) {
	res := renderTemplate("{{ value_json.absent }}", []byte(`{"present":1}`))
	// Whatever it decides, it must not report another field's value.
	if v := fmt.Sprint(res.Value); v == "1" {
		t.Fatalf("a missing field resolved to a different field's value: %v", res.Value)
	}
}

func TestFiltersOnValuesOfTheWrongType(t *testing.T) {
	// round on a word, int on an object: the filter cannot apply, and the
	// result must not be a made-up number.
	for _, tc := range []struct{ tpl, payload string }{
		{"{{ value_json.s | round(1) }}", `{"s":"text"}`},
		{"{{ value_json.o | int }}", `{"o":{"a":1}}`},
		{"{{ value_json.l | upper }}", `{"l":[1,2]}`},
	} {
		res := renderTemplate(tc.tpl, []byte(tc.payload))
		if res.Supported {
			if v := fmt.Sprint(res.Value); v == "0" {
				t.Errorf("%q on %s produced a fabricated %q", tc.tpl, tc.payload, v)
			}
		}
	}
}
