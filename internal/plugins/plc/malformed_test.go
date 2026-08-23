package plc

import (
	"testing"
)

// Anything can publish to a topic. The PLC's own publisher is well behaved,
// but a mistyped mosquitto_pub, a half-written retained message or another
// program using the same prefix are all things that happen in a real house —
// and none of them may become state the panel shows as though the PLC said it.

func TestGarbageOnEveryTopicIsRefused(t *testing.T) {
	topics := []string{
		"plc/digital/input/1",
		"plc/digital/output/1",
		"plc/digital/temperature/1",
		"plc/dali/light/1",
		"plc/shade/kitchen",
		"plc/electricity/processed/house",
		"plc/sensor/DI-1-1",
		"plc/watchdog",
		"plc/debug/health",
		"mbus2mqtt/water/state",
	}
	// The actuator groups — access, safety, relay — are deliberately absent.
	// They have no shared schema, and a device publishing a bare word like
	// "locked" or "OFF" is normal MQTT, so anything that is not JSON is taken
	// as the state verbatim. Refusing unfamiliar text there would mean
	// refusing half the devices that use those topics. Empty payloads and
	// broken JSON are still refused, which the next test covers.
	payloads := map[string]string{
		"truncated":       `{"address":`,
		"not json at all": `<xml/>`,
		"an empty string": ``,
		"a bare bracket":  `[`,
	}

	for _, topic := range topics {
		for name, payload := range payloads {
			r := NewRegistry(0)
			if r.Apply(prefix, meterPrefix, conn, topic, []byte(payload), now) {
				t.Errorf("%s on %s was taken as state", name, topic)
			}
			st := r.Snapshot(conn)
			if len(st.Points) > 0 || len(st.Lights) > 0 || len(st.Shades) > 0 ||
				len(st.Electricity) > 0 || len(st.Actuators) > 0 {
				t.Errorf("%s on %s produced state: %+v", name, topic, st)
			}
		}
	}
}

func TestAPayloadWithNoIdentityIsRefused(t *testing.T) {
	// Well-formed JSON that names nothing. There is no address to file it
	// under, and inventing one — 0, say — would put a phantom device in the
	// panel that no wire corresponds to.
	for _, tc := range []struct{ topic, payload string }{
		{"plc/shade/", `{"position":50}`},
		{"plc/sensor/", `{"unit":"°C"}`},
		{"plc/access/", `{"state":"locked"}`},
	} {
		r := NewRegistry(0)
		if r.Apply(prefix, meterPrefix, conn, tc.topic, []byte(tc.payload), now) {
			t.Errorf("%s with no name was accepted", tc.topic)
		}
	}
}

func TestAnAddressTakenFromTheTopicWhenThePayloadOmitsIt(t *testing.T) {
	// The PLC usually includes the address in the payload, but the topic
	// carries it too. Falling back to the topic is what keeps a light visible
	// when a firmware update changes the payload shape.
	r := NewRegistry(0)
	apply(t, r, "plc/dali/light/7", `{"name":"LI-7","actual_level":128}`)

	lights := r.Snapshot(conn).Lights
	if len(lights) != 1 {
		t.Fatalf("got %d lights", len(lights))
	}
	if lights[0].Address != 7 {
		t.Errorf("address = %d, want 7 from the topic", lights[0].Address)
	}
}

func TestAShadeNamedOnlyByItsTopic(t *testing.T) {
	r := NewRegistry(0)
	apply(t, r, "plc/shade/kitchen", `{"position":40}`)

	shades := r.Snapshot(conn).Shades
	if len(shades) != 1 || shades[0].Slug != "kitchen" {
		t.Fatalf("shades = %+v", shades)
	}
}

func TestElectricityNeedsAKnownSubBranch(t *testing.T) {
	r := NewRegistry(0)

	// "processed" carries the derived values and "status" the raw feed. A
	// third sub-branch is something this version does not understand, and
	// guessing at it would show numbers under the wrong labels.
	if r.Apply(prefix, meterPrefix, conn, "plc/electricity/raw/house",
		[]byte(`{"frequency":50}`), now) {
		t.Error("an unknown electricity sub-branch was accepted")
	}
	if !r.Apply(prefix, meterPrefix, conn, "plc/electricity/processed/house",
		[]byte(`{"frequency":50}`), now) {
		t.Fatal("the processed sub-branch was refused")
	}

	elec := r.Snapshot(conn).Electricity
	if len(elec) != 1 || elec[0].Name != "house" {
		t.Fatalf("electricity = %+v", elec)
	}
}

func TestMetadataAttachesToAPoint(t *testing.T) {
	r := NewRegistry(0)
	apply(t, r, "plc/sensor/DI-31-5", `{"name":"Kitchen probe","location":"Kitchen","type":"temperature"}`)
	apply(t, r, "plc/digital/temperature/5", `{"address":5,"name":"DI-31-5","value":21.5}`)

	points := r.Snapshot(conn).Points
	if len(points) != 1 {
		t.Fatalf("got %d points", len(points))
	}
	// Metadata that arrived first still has to reach the point it describes;
	// the PLC publishes them in whichever order it likes.
	// The PLC publishes metadata and readings in whichever order it likes, so
	// metadata that arrived first still has to reach the point it describes.
	if points[0].Label != "Kitchen probe" || points[0].SensorType != "temperature" ||
		points[0].Location != "Kitchen" {
		t.Errorf("point = %+v, want the metadata attached", points[0])
	}
}

func TestAnActuatorRefusesWhatCannotBeAState(t *testing.T) {
	for name, payload := range map[string]string{
		// An empty retained payload is MQTT for "forget this". Creating a
		// device from it puts something in the panel at the moment the broker
		// said to remove it.
		"an empty payload": "",
		// Something that opens like an object and does not parse was meant to
		// be JSON. `{"state":` is not a state anybody wants to read.
		"broken JSON":         `{"state":`,
		"a truncated object":  `{`,
		"an unterminated key": `{"a"`,
	} {
		r := NewRegistry(0)
		if r.Apply(prefix, meterPrefix, conn, "plc/access/front_door", []byte(payload), now) {
			t.Errorf("%s was taken as an actuator state", name)
		}
		if len(r.Snapshot(conn).Actuators) != 0 {
			t.Errorf("%s produced an actuator", name)
		}
	}

	// And what it does accept: a bare word, which is what a lock publishes.
	r := NewRegistry(0)
	if !r.Apply(prefix, meterPrefix, conn, "plc/access/front_door", []byte("locked"), now) {
		t.Fatal("a bare word was refused")
	}
	if got := r.Snapshot(conn).Actuators[0].State; got != "locked" {
		t.Errorf("state = %q", got)
	}
}
