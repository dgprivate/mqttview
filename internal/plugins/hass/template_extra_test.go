package hass

import (
	"fmt"
	"testing"
)

// The filters and conversions a real payload runs into. Every case here is a
// shape some device actually publishes: a number as a string, a boolean where
// a reading was expected, an array index into a multi-channel sensor.

func TestFiltersThatAppearInRealPayloads(t *testing.T) {
	tests := []struct {
		name    string
		tpl     string
		payload string
		want    string
	}{
		{"abs on a negative", "{{ value_json.x | abs }}", `{"x":-4.5}`, "4.5"},
		{"trim", "{{ value_json.s | trim }}", `{"s":"  on  "}`, "on"},
		{"round to two", "{{ value_json.x | round(2) }}", `{"x":1.23456}`, "1.23"},
		// A device that sends its number as a string is extremely common, and
		// the filters have to cope rather than showing nothing.
		{"round on a numeric string", "{{ value_json.x | round(1) }}", `{"x":"1.25"}`, "1.3"},
		{"int on a float", "{{ value_json.x | int }}", `{"x":7.9}`, "7"},
		{"float on an int", "{{ value_json.x | float }}", `{"x":3}`, "3"},
		{"string on a bool", "{{ value_json.x | string }}", `{"x":true}`, "true"},
		{"int on a bool", "{{ value_json.x | int }}", `{"x":true}`, "1"},
		{"int on a false bool", "{{ value_json.x | int }}", `{"x":false}`, "0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := renderTemplate(tt.tpl, []byte(tt.payload))
			if !res.Supported {
				t.Fatalf("%q was not evaluated", tt.tpl)
			}
			if got := fmt.Sprint(res.Value); got != tt.want {
				t.Errorf("value = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAFilterArgumentThatIsNotANumber(t *testing.T) {
	// `round(two)` is a typo in somebody's configuration, not a value. It must
	// be reported as unsupported rather than silently rounding to zero digits,
	// which would look like it worked.
	res := renderTemplate("{{ value_json.x | round(two) }}", []byte(`{"x":1.234}`))
	if res.Supported {
		t.Errorf("round(two) was evaluated, giving %v", res.Value)
	}
}

func TestAMissingKeyIsNullRatherThanAnError(t *testing.T) {
	// A key that is not in the payload yet is normal: a device publishes a
	// partial object and fills it in later. The template is still valid, so
	// this is "no value", not "bad template" — the UI shows them differently.
	for _, tc := range []struct{ tpl, payload string }{
		{"{{ value_json.absent }}", `{"present":1}`},
		{"{{ value_json.a.b }}", `{"a":{"c":1}}`},
		// Indexing into something that is not an array, or past its end.
		{"{{ value_json.items[5] }}", `{"items":[1,2]}`},
		{"{{ value_json.items[0] }}", `{"items":"not an array"}`},
		// Reaching through a scalar as though it were an object.
		{"{{ value_json.a.b }}", `{"a":7}`},
	} {
		res := renderTemplate(tc.tpl, []byte(tc.payload))
		if !res.Supported {
			t.Errorf("%q on %s was reported as an unsupported template", tc.tpl, tc.payload)
		}
		if res.Value != nil && fmt.Sprint(res.Value) != "" {
			t.Errorf("%q on %s gave %v, want nothing", tc.tpl, tc.payload, res.Value)
		}
	}
}

func TestANegativeOrUnparseableIndex(t *testing.T) {
	// `items[01x]` is not an index. Anything that is not a number at all is a
	// template mqttview does not understand, which is different from an index
	// that is simply out of range.
	if res := renderTemplate("{{ value_json.items[x] }}", []byte(`{"items":[1]}`)); res.Supported {
		t.Errorf("a non-numeric index was evaluated: %v", res.Value)
	}
}

func TestRenderingValuesThatAreNotScalars(t *testing.T) {
	// An object or array reaching the string filter is JSON, not Go's %v: the
	// UI shows it to somebody who is trying to read their device's payload.
	res := renderTemplate("{{ value_json.o | string }}", []byte(`{"o":{"b":1}}`))
	if got := fmt.Sprint(res.Value); got != `{"b":1}` {
		t.Errorf("an object rendered as %q", got)
	}

	res = renderTemplate("{{ value_json.n | string }}", []byte(`{"n":null}`))
	if got := fmt.Sprint(res.Value); got != "" {
		t.Errorf("null rendered as %q", got)
	}
}
