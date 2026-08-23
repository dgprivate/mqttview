package plc

import (
	"context"
	"testing"
	"time"
)

const (
	prefix      = "plc"
	meterPrefix = "mbus2mqtt"
	conn        = "c1"
)

var now = time.Date(2026, 8, 21, 15, 0, 0, 0, time.UTC)

func apply(t *testing.T, r *Registry, topic, payload string) bool {
	t.Helper()
	return r.Apply(prefix, meterPrefix, conn, topic, []byte(payload), now)
}

func TestPointsJoinSensorMetadata(t *testing.T) {
	r := NewRegistry(0)

	// Metadata deliberately arrives after the value, because retained messages
	// come back in no guaranteed order.
	apply(t, r, "plc/digital/input/245",
		`{"timestamp":1787323248151,"device":"plc1","address":245,"type":"digital","name":"DI-31-5","value":false}`)
	apply(t, r, "plc/sensor/DI-31-5",
		`{"state":"OFF","type":"motion","location":"hallway","name":"Spodnji hodnik","alarm_zone":true,"timestamp":1787323248151}`)

	st := r.Snapshot(conn)
	if len(st.Points) != 1 {
		t.Fatalf("expected 1 point, got %d", len(st.Points))
	}
	p := st.Points[0]
	if p.Label != "Spodnji hodnik" || p.Location != "hallway" || p.SensorType != "motion" || !p.AlarmZone {
		t.Errorf("metadata not joined: %+v", p)
	}
	if p.Kind != KindInput || p.Address != 245 || p.Bool == nil || *p.Bool {
		t.Errorf("point decoded wrongly: %+v", p)
	}
	if st.Summary.Described != 1 || st.Summary.Inputs != 1 {
		t.Errorf("unexpected summary: %+v", st.Summary)
	}
}

func TestTemperatureDecodesAsNumber(t *testing.T) {
	r := NewRegistry(0)
	apply(t, r, "plc/digital/temperature/9",
		`{"timestamp":1787323249261,"device":"plc1","address":9,"type":"temp","name":"T-2-1","value":28.72999952}`)

	st := r.Snapshot(conn)
	p := st.Points[0]
	if p.Number == nil || *p.Number < 28.7 || *p.Number > 28.8 {
		t.Fatalf("temperature not decoded: %+v", p)
	}
	if p.Bool != nil {
		t.Error("a temperature must not decode as a bool")
	}
}

func TestJournalRecordsOnlyTransitions(t *testing.T) {
	r := NewRegistry(0)
	apply(t, r, "plc/sensor/DI-31-5",
		`{"state":"OFF","type":"motion","location":"hallway","name":"Spodnji hodnik","alarm_zone":true}`)

	// First value: the retained flood on connect, not a button press.
	apply(t, r, "plc/digital/input/245", `{"address":245,"name":"DI-31-5","value":false}`)
	if got := r.Journal().Len(); got != 0 {
		t.Fatalf("first observation must not be journalled, got %d entries", got)
	}

	apply(t, r, "plc/digital/input/245", `{"address":245,"name":"DI-31-5","value":true}`)
	apply(t, r, "plc/digital/input/245", `{"address":245,"name":"DI-31-5","value":true}`) // repeat, no edge
	apply(t, r, "plc/digital/input/245", `{"address":245,"name":"DI-31-5","value":false}`)

	edges, seq := r.Journal().Since(0, 10, nil)
	if len(edges) != 2 || seq != 2 {
		t.Fatalf("expected 2 edges and seq 2, got %d edges seq %d", len(edges), seq)
	}
	if !edges[0].Rising() || edges[0].Label != "Spodnji hodnik" || edges[0].Address != 245 {
		t.Errorf("rising edge wrong: %+v", edges[0])
	}
	if edges[1].Rising() {
		t.Errorf("second edge should be falling: %+v", edges[1])
	}
}

func TestJournalSinceAndRingBuffer(t *testing.T) {
	j := NewJournal(3)
	for i := 0; i < 5; i++ {
		j.Append(Edge{Name: "DI-1", To: i%2 == 0})
	}
	if j.Len() != 3 {
		t.Fatalf("ring buffer should hold 3, holds %d", j.Len())
	}
	edges, seq := j.Since(0, 10, nil)
	if seq != 5 || len(edges) != 3 || edges[0].Seq != 3 || edges[2].Seq != 5 {
		t.Fatalf("unexpected window: seq=%d edges=%+v", seq, edges)
	}
	// Resuming from a sequence returns only what is newer.
	edges, _ = j.Since(4, 10, nil)
	if len(edges) != 1 || edges[0].Seq != 5 {
		t.Fatalf("resume from seq 4 wrong: %+v", edges)
	}
}

func TestJournalWaitReturnsNextMatchingEdge(t *testing.T) {
	j := NewJournal(10)
	j.Append(Edge{Name: "DI-1", From: true, To: false}) // falling, must not satisfy a rising wait

	done := make(chan []Edge, 1)
	go func() {
		edges, _ := j.Wait(context.Background(), 0, 10, 2*time.Second, func(e Edge) bool { return e.Rising() })
		done <- edges
	}()

	// Falling edges keep the waiter blocked; the rising one releases it.
	//
	// No sleep between them: Append takes the journal's lock and the waiter
	// re-checks under the same lock, so the ordering is the journal's rather
	// than the scheduler's. Spacing them out only made the test slower and hid
	// the case where both land before the waiter looks.
	j.Append(Edge{Name: "DI-2", From: true, To: false})
	j.Append(Edge{Name: "DI-3", From: false, To: true})

	select {
	case edges := <-done:
		if len(edges) != 1 || edges[0].Name != "DI-3" {
			t.Fatalf("expected only the rising edge, got %+v", edges)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Wait did not return")
	}
}

func TestJournalWaitTimesOutEmpty(t *testing.T) {
	j := NewJournal(10)
	start := time.Now()
	edges, seq := j.Wait(context.Background(), 0, 10, 80*time.Millisecond, nil)
	if len(edges) != 0 || seq != 0 {
		t.Fatalf("expected an empty answer, got %d edges seq %d", len(edges), seq)
	}
	if time.Since(start) < 50*time.Millisecond {
		t.Error("Wait returned before its timeout")
	}
}

func TestDaliLightsAndErrorCount(t *testing.T) {
	r := NewRegistry(0)
	apply(t, r, "plc/dali/light/1",
		`{"timestamp":1787322845931,"device":"plc1","address":1,"type":"dali","name":"LI-1","status":254,`+
			`"actual_level":0,"max_level":0,"min_level":50,"fade_time":0,"fade_rate":45,`+
			`"last_command":"query","error":"ParameterAddressIsAShortAddress"}`)
	apply(t, r, "plc/dali/light/5",
		`{"address":5,"name":"LI-5","status":254,"actual_level":254,"min_level":50}`)

	st := r.Snapshot(conn)
	if st.Summary.Lights != 2 || st.Summary.LightsWithError != 1 || st.Summary.LightsOn != 1 {
		t.Fatalf("unexpected light summary: %+v", st.Summary)
	}
	if st.Lights[0].Address != 1 || st.Lights[1].Percent() != 100 {
		t.Errorf("lights wrong: %+v %+v", st.Lights[0], st.Lights[1])
	}
}

func TestElectricityKeepsProcessedFieldsAcrossRawUpdates(t *testing.T) {
	r := NewRegistry(0)
	apply(t, r, "plc/electricity/processed/house",
		`{"L1_voltage":234.2,"L2_voltage":235.2,"L3_voltage":236.7,"L1_current":1.4,"L2_current":3.6,`+
			`"L3_current":5.1,"frequency":50.002,"imbalance":{"voltage":1.06,"current":108.1},`+
			`"alarm_active":true,"active_alarms":["current_imbalance"]}`)
	// The raw feed carries no imbalance and must not erase it.
	apply(t, r, "plc/electricity/status/house",
		`{"timestamp":1787323248341,"device":"plc1","type":"electricity","name":"EL","L1_voltage":233.9,"frequency":50.0}`)

	st := r.Snapshot(conn)
	if len(st.Electricity) != 1 {
		t.Fatalf("expected one metering point, got %d", len(st.Electricity))
	}
	e := st.Electricity[0]
	if e.CurrentImbalance < 108 || e.VoltageImbalance < 1 {
		t.Errorf("imbalance lost by the raw update: %+v", e)
	}
	if !e.AlarmActive || len(e.ActiveAlarms) != 1 {
		t.Errorf("alarm state lost: %+v", e)
	}
	if e.Phases[0].Voltage != 233.9 {
		t.Errorf("raw update should still refresh voltages: %+v", e.Phases)
	}
}

func TestWatchdogAndMeters(t *testing.T) {
	r := NewRegistry(0)
	apply(t, r, "plc/watchdog/state",
		`{"uptime_s":1388667,"alive":true,"mqtt_connected":true,"backup_mqtt_connected":true,`+
			`"alarm_mode":"disarmed","alarm_triggered":false,"ready":true,"persistent_valid":true,`+
			`"streams":{"di":{"count":931115,"age_s":0},"do":{"count":986,"age_s":5224}}}`)
	apply(t, r, "mbus2mqtt/mbus_water_meter/state",
		`{"water_volume_total":53.19,"water_flow_rate":0.001,"error_code":0}`)
	apply(t, r, "mbus2mqtt/mbus_water_meter/availability", `online`)

	st := r.Snapshot(conn)
	if st.Watchdog == nil || st.Watchdog.AlarmMode != "disarmed" || st.Watchdog.Streams["do"].AgeS != 5224 {
		t.Fatalf("watchdog wrong: %+v", st.Watchdog)
	}
	if len(st.Meters) != 1 {
		t.Fatalf("expected one meter, got %d", len(st.Meters))
	}
	m := st.Meters[0]
	if !m.Available || m.Readings["water_volume_total"] != 53.19 {
		t.Errorf("meter wrong: %+v", m)
	}
}

func TestMalformedPayloadKeepsLastGoodValue(t *testing.T) {
	r := NewRegistry(0)
	apply(t, r, "plc/digital/input/245", `{"address":245,"name":"DI-31-5","value":true}`)
	if changed := apply(t, r, "plc/digital/input/245", `not json at all`); changed {
		t.Error("a malformed payload must not count as a change")
	}

	st := r.Snapshot(conn)
	if st.Points[0].Bool == nil || !*st.Points[0].Bool {
		t.Error("last good value was lost to a malformed publish")
	}
}

func TestTopicsOutsideThePrefixAreIgnored(t *testing.T) {
	r := NewRegistry(0)
	for _, topic := range []string{"homeassistant/sensor/x/state", "plc", "plcx/digital/input/1", ""} {
		if apply(t, r, topic, `{"value":true}`) {
			t.Errorf("topic %q should not have matched", topic)
		}
	}
	if len(r.Snapshot("").Points) != 0 {
		t.Error("registry took on state from an unrelated topic")
	}
}

func TestParseRoute(t *testing.T) {
	tests := []struct {
		topic  string
		branch string
		sub    string
		leaf   string
		ok     bool
	}{
		{"plc/digital/input/245", "digital", "input", "245", true},
		{"plc/dali/light/64", "dali", "light", "64", true},
		{"plc/sensor/DI-31-5", "sensor", "", "DI-31-5", true},
		{"plc/watchdog/state", "watchdog", "", "state", true},
		{"plc/electricity/processed/house", "electricity", "processed", "house", true},
		{"plc/", "", "", "", false},
		{"other/thing", "", "", "", false},
	}
	for _, tt := range tests {
		rt, ok := parseRoute("plc", tt.topic)
		if ok != tt.ok {
			t.Errorf("%s: ok=%v want %v", tt.topic, ok, tt.ok)
			continue
		}
		if ok && (rt.Branch != tt.branch || rt.Sub != tt.sub || rt.Leaf != tt.leaf) {
			t.Errorf("%s: got %+v", tt.topic, rt)
		}
	}
}

func TestBuildCommandAcceptsTheCatalogue(t *testing.T) {
	tests := []struct {
		name string
		req  CommandRequest
		want string
		tier commandTier
	}{
		{
			name: "dali on",
			req:  CommandRequest{Target: "dali", Command: "on", Address: 53},
			want: `{"target":"dali","command":"on","address":53}`,
			tier: tierLight,
		},
		{
			name: "dali arc level",
			req:  CommandRequest{Target: "dali", Command: "arc", Address: 5, Params: []string{"128"}},
			want: `{"target":"dali","command":"arc","address":5,"params":["128"]}`,
			tier: tierLight,
		},
		{
			name: "dali refresh takes no address",
			req:  CommandRequest{Target: "dali", Command: "refresh"},
			want: `{"target":"dali","command":"refresh"}`,
			tier: tierLight,
		},
		{
			name: "digital set normalises the value",
			req:  CommandRequest{Target: "digital", Command: "set", Address: 12, Params: []string{"ON"}},
			want: `{"target":"digital","command":"set","address":12,"params":["true"]}`,
			tier: tierOutput,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, payload, err := buildCommand(tt.req)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(payload) != tt.want {
				t.Errorf("payload:\n got %s\nwant %s", payload, tt.want)
			}
			if spec.Tier != tt.tier {
				t.Errorf("tier %q, want %q", spec.Tier, tt.tier)
			}
		})
	}
}

// The PLC understands a far larger vocabulary than the plugin sends. These are
// the ones that must never become reachable through it.
func TestBuildCommandRefusesEverythingOutsideTheCatalogue(t *testing.T) {
	forbidden := []CommandRequest{
		{Target: "system", Command: "wipe_persistent_magic"},
		{Target: "safety", Command: "reset_water_valve"},
		{Target: "safety", Command: "set_valve_closed", Params: []string{"true"}},
		{Target: "alarm", Command: "panic"},
		{Target: "alarm", Command: "set_mode", Params: []string{"armed"}},
		{Target: "mode", Command: "set_direct_ha", Params: []string{"true"}},
		{Target: "mode", Command: "set_backup_enabled", Params: []string{"false"}},
		{Target: "config", Command: "set_short_press_restore_last", Params: []string{"true"}},
		{Target: "shade", Command: "tilt_pulse_raw", Address: 1},
		{Target: "dali", Command: "dim", Address: 1},
	}

	for _, req := range forbidden {
		if _, _, err := buildCommand(req); err == nil {
			t.Errorf("%s/%s was accepted and must not be", req.Target, req.Command)
		}
	}
}

func TestBuildCommandValidatesAddressesAndParams(t *testing.T) {
	bad := []struct {
		name string
		req  CommandRequest
	}{
		{"dali address zero", CommandRequest{Target: "dali", Command: "on", Address: 0}},
		{"dali address past the bus", CommandRequest{Target: "dali", Command: "on", Address: 65}},
		{"digital address past the rack", CommandRequest{Target: "digital", Command: "set", Address: 81, Params: []string{"true"}}},
		{"arc level too high", CommandRequest{Target: "dali", Command: "arc", Address: 1, Params: []string{"255"}}},
		{"arc level negative", CommandRequest{Target: "dali", Command: "arc", Address: 1, Params: []string{"-1"}}},
		{"arc level not a number", CommandRequest{Target: "dali", Command: "arc", Address: 1, Params: []string{"bright"}}},
		{"arc without a level", CommandRequest{Target: "dali", Command: "arc", Address: 1}},
		{"digital set with a nonsense value", CommandRequest{Target: "digital", Command: "set", Address: 1, Params: []string{"maybe"}}},
		{"refresh with an address", CommandRequest{Target: "system", Command: "refresh", Address: 3}},
		{"on with a stray parameter", CommandRequest{Target: "dali", Command: "on", Address: 1, Params: []string{"x"}}},
	}

	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := buildCommand(tt.req); err == nil {
				t.Error("expected a validation error")
			}
		})
	}
}

func TestMappingOverridesPlcMetadata(t *testing.T) {
	r := NewRegistry(0)
	apply(t, r, "plc/sensor/DI-31-5",
		`{"state":"OFF","type":"motion","location":"hallway","name":"Spodnji hodnik","alarm_zone":true}`)
	apply(t, r, "plc/digital/input/245", `{"address":245,"name":"DI-31-5","value":false}`)

	r.SetMapping(conn, "DI-31-5", Mapping{Label: "Hodnik - senzor pri stopnicah", Notes: "naj prizge LI-5"})

	p := r.Snapshot(conn).Points[0]
	if p.Label != "Hodnik - senzor pri stopnicah" {
		t.Errorf("the person's name should win, got %q", p.Label)
	}
	// Fields the person left empty keep whatever the PLC published.
	if p.Location != "hallway" || p.SensorType != "motion" || !p.AlarmZone {
		t.Errorf("PLC metadata should fill the gaps: %+v", p)
	}
}

func TestMappingNamesAPointThePlcNeverDescribed(t *testing.T) {
	r := NewRegistry(0)
	apply(t, r, "plc/digital/input/99", `{"address":99,"name":"DI-9-9","value":false}`)
	if got := r.Snapshot(conn).Summary.Described; got != 0 {
		t.Fatalf("point should start undescribed, described=%d", got)
	}

	r.SetMapping(conn, "DI-9-9", Mapping{Label: "Kuhinja - stikalo pri vratih", Location: "kitchen"})
	st := r.Snapshot(conn)
	if st.Summary.Described != 1 || st.Points[0].Label != "Kuhinja - stikalo pri vratih" {
		t.Errorf("naming did not take: %+v", st.Points[0])
	}

	// Clearing every field removes the name again.
	r.SetMapping(conn, "DI-9-9", Mapping{})
	if got := r.Snapshot(conn).Summary.Described; got != 0 {
		t.Errorf("cleared mapping should delete, described=%d", got)
	}
}

func TestMappingReachesTheSignalLog(t *testing.T) {
	r := NewRegistry(0)
	r.SetMapping(conn, "DI-9-9", Mapping{Label: "Kuhinja - stikalo", Location: "kitchen"})
	apply(t, r, "plc/digital/input/99", `{"address":99,"name":"DI-9-9","value":false}`)
	apply(t, r, "plc/digital/input/99", `{"address":99,"name":"DI-9-9","value":true}`)

	edges, _ := r.Journal().Since(0, 10, nil)
	if len(edges) != 1 || edges[0].Label != "Kuhinja - stikalo" || edges[0].Location != "kitchen" {
		t.Fatalf("the log should carry the name: %+v", edges)
	}
}

func TestMappingsSurviveAReload(t *testing.T) {
	r := NewRegistry(0)
	r.SetMapping(conn, "DI-9-9", Mapping{Label: "Kuhinja", Notes: "prizge LI-5"})

	// What the plugin would have written to its key-value store.
	stored := map[string]string{
		mappingKey(conn, "DI-9-9"): `{"name":"DI-9-9","label":"Kuhinja","notes":"prizge LI-5"}`,
		"unrelated:key":            `{}`,
		mappingKey(conn, "DI-1-1"): `not json`,
	}

	fresh := NewRegistry(0)
	if err := fresh.LoadMappings(stored); err == nil {
		t.Error("a corrupt entry should be reported")
	}
	got := fresh.Mappings(conn)
	if len(got) != 1 || got[0].Name != "DI-9-9" || got[0].Notes != "prizge LI-5" {
		t.Fatalf("reload lost the mapping: %+v", got)
	}
}
