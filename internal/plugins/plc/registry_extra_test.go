package plc

import (
	"testing"
	"time"
)

// The branches the happy path does not reach: the gateway health topic, the
// actuator groups whose payloads have no shared shape, and the projection's
// sorting and teardown.

func TestBridgeHealthIsRecorded(t *testing.T) {
	r := NewRegistry(0)
	apply(t, r, "plc/debug/health", `{
		"timestamp":1787323213256,"source":"nodered_internal","nodered_version":"4.1.8",
		"uptime_hours":121.4,"memory":{"rss_mb":297},"os":{"freeMem_mb":1978,"loadAvg":[0.06,0.07,0.08]}}`)

	st := r.Snapshot(conn)
	if st.Bridge == nil {
		t.Fatal("the gateway health topic produced nothing")
	}
	if st.Bridge.Source != "nodered_internal" || st.Bridge.Version != "4.1.8" {
		t.Errorf("bridge = %+v", st.Bridge)
	}
	if st.Bridge.UptimeHours != 121.4 || st.Bridge.RSSMB != 297 || st.Bridge.FreeMemMB != 1978 {
		t.Errorf("bridge numbers = %+v", st.Bridge)
	}
	if len(st.Bridge.LoadAvg) != 3 {
		t.Errorf("load average = %v", st.Bridge.LoadAvg)
	}

	// A different debug topic is not the health one.
	if r.Apply(prefix, meterPrefix, conn, "plc/debug/something-else", []byte(`{}`), now) {
		t.Error("an unrelated debug topic was taken as health")
	}
}

func TestActuatorGroupsTakeWhateverShapeArrives(t *testing.T) {
	r := NewRegistry(0)

	// These branches have no shared schema, so an object is kept whole and a
	// bare scalar is read as the state.
	apply(t, r, "plc/access/front_door_lock", `{"state":"locked","by":"keypad"}`)
	apply(t, r, "plc/safety/main_water_valve", `{"value":true}`)
	apply(t, r, "plc/relay/induction_cooktop", `"OFF"`)

	st := r.Snapshot(conn)
	if len(st.Actuators) != 3 {
		t.Fatalf("got %d actuators: %+v", len(st.Actuators), st.Actuators)
	}

	byGroup := map[string]*Actuator{}
	for _, a := range st.Actuators {
		byGroup[a.Group] = a
	}
	if byGroup["access"].State != "locked" {
		t.Errorf("access state = %q", byGroup["access"].State)
	}
	// A boolean is rendered as ON/OFF, because that is what a panel shows.
	if byGroup["safety"].State != "ON" {
		t.Errorf("safety state = %q", byGroup["safety"].State)
	}
	if byGroup["relay"].State != "OFF" {
		t.Errorf("relay state = %q", byGroup["relay"].State)
	}
	// The whole object is kept for the ones that carry more than a state.
	if len(byGroup["access"].Raw) == 0 {
		t.Error("the raw payload was not kept")
	}
	if byGroup["relay"].Raw != nil {
		t.Error("a scalar payload should not be kept as raw JSON")
	}
}

func TestActuatorWithNoSlugIsIgnored(t *testing.T) {
	r := NewRegistry(0)
	if r.Apply(prefix, meterPrefix, conn, "plc/access", []byte(`{"state":"x"}`), now) {
		t.Error("a group topic with no device name became an actuator")
	}
}

func TestNumericStatesAreRendered(t *testing.T) {
	r := NewRegistry(0)
	apply(t, r, "plc/relay/dimmer", `{"position":42}`)

	st := r.Snapshot(conn)
	if len(st.Actuators) != 1 || st.Actuators[0].State != "42" {
		t.Fatalf("actuator = %+v", st.Actuators)
	}
}

func TestSnapshotSortsEverythingStably(t *testing.T) {
	r := NewRegistry(0)

	// Inserted out of order; a panel that reshuffles on every poll is unusable.
	for _, addr := range []int{9, 1, 5} {
		apply(t, r, "plc/digital/input/"+itoa(addr),
			`{"address":`+itoa(addr)+`,"name":"DI-`+itoa(addr)+`","value":false}`)
		apply(t, r, "plc/dali/light/"+itoa(addr),
			`{"address":`+itoa(addr)+`,"name":"LI-`+itoa(addr)+`"}`)
	}
	apply(t, r, "plc/shade/zeta", `{"slug":"zeta","position":1}`)
	apply(t, r, "plc/shade/alpha", `{"slug":"alpha","position":2}`)
	apply(t, r, "plc/relay/zulu", `"ON"`)
	apply(t, r, "plc/access/alpha", `"LOCKED"`)

	st := r.Snapshot(conn)
	for i := 1; i < len(st.Points); i++ {
		if st.Points[i-1].Kind == st.Points[i].Kind && st.Points[i-1].Address > st.Points[i].Address {
			t.Errorf("points are not sorted by address: %+v", st.Points)
			break
		}
	}
	for i := 1; i < len(st.Lights); i++ {
		if st.Lights[i-1].Address > st.Lights[i].Address {
			t.Errorf("lights are not sorted: %+v", st.Lights)
			break
		}
	}
	if len(st.Shades) == 2 && st.Shades[0].Slug != "alpha" {
		t.Errorf("shades are not sorted: %+v", st.Shades)
	}
	// Actuators sort by group then slug, so the groups stay together.
	if len(st.Actuators) == 2 && st.Actuators[0].Group != "access" {
		t.Errorf("actuators are not sorted by group: %+v", st.Actuators)
	}
}

func TestForgetDropsOneConnection(t *testing.T) {
	r := NewRegistry(0)
	r.Apply(prefix, meterPrefix, "c1", "plc/digital/input/1", []byte(`{"address":1,"name":"A","value":true}`), now)
	r.Apply(prefix, meterPrefix, "c2", "plc/digital/input/1", []byte(`{"address":1,"name":"B","value":true}`), now)

	if points, _ := r.Stats(); points != 2 {
		t.Fatalf("Stats = %d points", points)
	}

	r.Forget("c1")
	if len(r.Snapshot("c1").Points) != 0 {
		t.Error("the forgotten connection still has state")
	}
	if len(r.Snapshot("c2").Points) != 1 {
		t.Error("forgetting one connection took another's state")
	}
}

func TestSnapshotWithNoConnectionFilterReturnsEverything(t *testing.T) {
	r := NewRegistry(0)
	r.Apply(prefix, meterPrefix, "c1", "plc/digital/input/1", []byte(`{"address":1,"name":"A","value":true}`), now)
	r.Apply(prefix, meterPrefix, "c2", "plc/digital/input/2", []byte(`{"address":2,"name":"B","value":true}`), now)

	if got := len(r.Snapshot("").Points); got != 2 {
		t.Fatalf("an empty filter returned %d points, want both", got)
	}
}

func TestUnknownSubBranchesAreIgnored(t *testing.T) {
	r := NewRegistry(0)

	for _, topic := range []string{
		"plc/digital/unknown/1", // not input, output or temperature
		"plc/dali/group/1",      // only "light" is understood
		"plc/electricity/raw/house",
		"plc/nonsense/thing",
	} {
		if r.Apply(prefix, meterPrefix, conn, topic, []byte(`{"value":1}`), now) {
			t.Errorf("%s was taken as state", topic)
		}
	}
}

func TestMeterAvailabilityAndUnknownLeaf(t *testing.T) {
	r := NewRegistry(0)

	apply(t, r, "mbus2mqtt/water/availability", `offline`)
	st := r.Snapshot(conn)
	if len(st.Meters) != 1 || st.Meters[0].Available {
		t.Fatalf("meter = %+v", st.Meters)
	}

	apply(t, r, "mbus2mqtt/water/availability", `"online"`)
	if !r.Snapshot(conn).Meters[0].Available {
		t.Error("a quoted online payload was not understood")
	}

	// Any other leaf under a meter is not something this understands.
	if r.Apply(prefix, meterPrefix, conn, "mbus2mqtt/water/config", []byte(`{}`), now) {
		t.Error("an unknown meter leaf was taken as state")
	}
}

func TestJournalSizeSettingIsHonoured(t *testing.T) {
	r := NewRegistry(3)

	apply(t, r, "plc/digital/input/1", `{"address":1,"name":"A","value":false}`)
	for i := 0; i < 6; i++ {
		v := "true"
		if i%2 == 1 {
			v = "false"
		}
		apply(t, r, "plc/digital/input/1", `{"address":1,"name":"A","value":`+v+`}`)
	}

	if got := r.Journal().Len(); got != 3 {
		t.Fatalf("the journal holds %d, want the configured 3", got)
	}
}

func TestTimestampsFallBackToArrivalTime(t *testing.T) {
	r := NewRegistry(0)
	// A payload with no timestamp still needs one, or the UI shows the zero
	// time as "56 years ago".
	apply(t, r, "plc/digital/input/1", `{"address":1,"name":"A","value":true}`)

	got := r.Snapshot(conn).Points[0].UpdatedAt
	if got.IsZero() || got.Before(now.Add(-time.Minute)) {
		t.Fatalf("UpdatedAt = %v, want the receive time", got)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
