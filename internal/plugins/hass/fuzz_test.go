package hass

import (
	"strings"
	"testing"
)

// Discovery payloads are written by other people's firmware — Tasmota, ESPHome,
// Shelly, Zigbee2MQTT and a long tail of hand-rolled devices. The parser has to
// take whatever any of them publishes without bringing down the registry.

func FuzzParseConfig(f *testing.F) {
	for _, s := range []string{
		`{"name":"Lamp","stat_t":"~/state","~":"home/lamp","uniq_id":"a1"}`,
		`{"name":"T","state_topic":"a/b","value_template":"{{ value_json.temp | round(1) }}"}`,
		`{"dev":{"ids":["x"],"name":"D"},"cmps":{"c1":{"platform":"switch"}}}`,
		`{"availability":[{"topic":"a/av","payload_available":"on"}]}`,
		`{}`, `[]`, `null`, `not json`, `{"name":`, `{"~":"a","stat_t":"~~~"}`,
		`{"qos":"not a number"}`, `{"name":"` + strings.Repeat("x", 5000) + `"}`,
	} {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, payload []byte) {
		cfg, err := parseConfig(payload)
		if err != nil {
			return
		}
		// Anything the parser accepted is then read by the registry, so the
		// accessors it uses must not panic on it.
		cfg.str("name")
		cfg.str("~")
		cfg.str("state_topic")

		reg := NewRegistry()
		dt := discoveryTopic{Prefix: "homeassistant", Component: "sensor", ObjectID: "o"}
		_, topics := reg.Upsert("c1", dt, cfg)
		for _, topic := range topics {
			// A device names its own topics. If one of those reaches SUBSCRIBE
			// unvalidated, a malformed config becomes a broker-side error.
			if topic == "" {
				t.Fatal("registry produced an empty subscription topic")
			}
		}
		reg.Devices("c1")
		reg.Entities("c1")
	})
}

func FuzzParseDiscoveryTopic(f *testing.F) {
	for _, s := range []string{
		"homeassistant/sensor/x/config", "homeassistant/sensor/node/x/config",
		"homeassistant/device/x/config", "homeassistant/config",
		"homeassistant//config", "homeassistant/sensor/x/state",
		"", "/", strings.Repeat("a/", 400) + "config",
	} {
		f.Add("homeassistant", s)
	}

	f.Fuzz(func(t *testing.T, prefix, topic string) {
		dt, ok := parseDiscoveryTopic(prefix, topic)
		if !ok {
			return
		}
		// A topic it claimed to understand has to round-trip, or the registry
		// will store an entity under a key it can never look up again.
		if got := dt.topicOf(); got != topic {
			t.Fatalf("parsed %q but rebuilt it as %q", topic, got)
		}
	})
}

func FuzzRenderTemplate(f *testing.F) {
	seeds := []struct{ tpl, payload string }{
		{"{{ value }}", `21.5`},
		{"{{ value_json.temp }}", `{"temp":21.5}`},
		{"{{ value_json.a.b.c }}", `{"a":{"b":{"c":1}}}`},
		{"{{ value_json.x | round(1) }}", `{"x":1.2345}`},
		{"{{ value_json['odd key'] }}", `{"odd key":1}`},
		{"{% if value %}on{% endif %}", `1`},
		{"", `{}`},
		{"{{", `{}`},
		{"{{ value_json." + strings.Repeat("a.", 500) + "b }}", `{}`},
	}
	for _, s := range seeds {
		f.Add(s.tpl, []byte(s.payload))
	}

	// mqttview evaluates the common Jinja shapes and refuses the rest rather
	// than embedding a template engine. The invariant is that refusing shows
	// the payload untouched: a template it could not evaluate must never
	// produce a computed-looking value, which the UI would present as a
	// reading.
	f.Fuzz(func(t *testing.T, tpl string, payload []byte) {
		res := renderTemplate(tpl, payload)
		if res.Supported {
			return
		}
		if got, ok := res.Value.(string); !ok || got != string(payload) {
			t.Fatalf("template %q was unsupported but returned %#v instead of the raw payload", tpl, res.Value)
		}
	})
}
