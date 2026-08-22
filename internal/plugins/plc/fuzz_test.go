package plc

import (
	"testing"
	"time"
)

// Everything this plugin parses comes off a broker: the PLC's own payloads, and
// anything else that happens to publish under the same prefix. The registry has
// to survive all of it, because one bad message must not take out the panel for
// every other point.

func FuzzRegistryApply(f *testing.F) {
	seeds := []struct{ topic, payload string }{
		{"plc/digital/input/245", `{"timestamp":1787323248151,"device":"plc1","address":245,"type":"digital","name":"DI-31-5","value":false}`},
		{"plc/digital/temperature/9", `{"address":9,"name":"T-2-1","value":28.7}`},
		{"plc/dali/light/1", `{"address":1,"name":"LI-1","status":254,"actual_level":0,"max_level":0,"min_level":50,"error":"x"}`},
		{"plc/sensor/DI-31-5", `{"state":"OFF","type":"motion","location":"hallway","name":"Hodnik","alarm_zone":true}`},
		{"plc/electricity/processed/house", `{"L1_voltage":234.2,"imbalance":{"voltage":1.06,"current":108.1},"alarm_active":true,"active_alarms":["x"]}`},
		{"plc/watchdog/state", `{"uptime_s":1,"alive":true,"streams":{"di":{"count":1,"age_s":0}}}`},
		{"plc/shade/bathroom_top", `{"slug":"bathroom_top","position":8}`},
		{"plc/access/front_door_lock", `{"state":"locked"}`},
		{"mbus2mqtt/meter/state", `{"water_volume_total":53.19}`},
		{"mbus2mqtt/meter/availability", `online`},
		// The shapes that are not the happy path.
		{"plc/digital/input/245", `not json`},
		{"plc/digital/input/notanumber", `{"value":true}`},
		{"plc/dali/light/1", `{"address":-2147483648,"actual_level":-1}`},
		{"plc/", ``},
		{"plc/digital/input/1", `{"value":1e400}`},
		{"plc/watchdog/state", `{"streams":null}`},
	}
	for _, s := range seeds {
		f.Add(s.topic, []byte(s.payload))
	}

	now := time.Unix(1787323248, 0)

	f.Fuzz(func(t *testing.T, topic string, payload []byte) {
		r := NewRegistry(8)
		r.Apply("plc", "mbus2mqtt", "c1", topic, payload, now)

		// Whatever it made of that, the projection has to be renderable: the
		// UI calls this on every change, and a panic here is the whole page.
		st := r.Snapshot("c1")
		for _, p := range st.Points {
			if p.Bool != nil && p.Number != nil {
				t.Fatalf("point %q decoded as both a bool and a number", p.Name)
			}
		}
		for _, l := range st.Lights {
			if pct := l.Percent(); pct < 0 || pct > 100 {
				t.Fatalf("light %d reported %d%%", l.Address, pct)
			}
		}
		r.Journal().Since(0, 10, nil)
		r.Stats()
	})
}

func FuzzBuildCommand(f *testing.F) {
	seeds := []struct {
		target, command string
		address         int
		param           string
	}{
		{"dali", "on", 1, ""},
		{"dali", "arc", 64, "254"},
		{"digital", "set", 80, "true"},
		{"system", "refresh", 0, ""},
		{"safety", "reset_water_valve", 0, ""},
		{"alarm", "panic", 0, ""},
		{"dali", "arc", 1, "999999999999999999999"},
		{"", "", 0, ""},
	}
	for _, s := range seeds {
		f.Add(s.target, s.command, s.address, s.param)
	}

	// This is the one place the plugin can act on a house, so the property
	// under test is not "does not panic" but "never emits anything outside the
	// catalogue". A validator that can be talked past is the whole risk.
	f.Fuzz(func(t *testing.T, target, command string, address int, param string) {
		req := CommandRequest{Target: target, Command: command, Address: address}
		if param != "" {
			req.Params = []string{param}
		}

		spec, payload, err := buildCommand(req)
		if err != nil {
			return
		}
		if _, ok := lookupCommand(spec.Target, spec.Command); !ok {
			t.Fatalf("accepted a command outside the catalogue: %s/%s", spec.Target, spec.Command)
		}
		if len(payload) == 0 {
			t.Fatal("accepted a command but produced no payload")
		}
		if spec.MaxAddress > 0 && (req.Address < spec.MinAddress || req.Address > spec.MaxAddress) {
			t.Fatalf("accepted address %d for %s/%s", req.Address, spec.Target, spec.Command)
		}
		if spec.Param == "" && len(req.Params) > 0 {
			t.Fatalf("accepted a parameter for %s/%s, which takes none", spec.Target, spec.Command)
		}
	})
}

func FuzzMappingKey(f *testing.F) {
	for _, s := range []string{"c1|DI-1-1", "|", "a|b|c", "", "map:", "\x00|\x00"} {
		f.Add(s, "DI-1-1")
	}

	// Mapping keys are built from a connection ID and a PLC name and parsed
	// back on restart. A pair that does not survive the round trip loses
	// somebody's names.
	f.Fuzz(func(t *testing.T, connID, name string) {
		key := mappingKey(connID, name)
		gotConn, gotName, ok := parseMappingKey(key)
		if !ok {
			return
		}
		if gotConn+"|"+gotName != connID+"|"+name {
			t.Fatalf("round trip changed %q|%q into %q|%q", connID, name, gotConn, gotName)
		}
	})
}
