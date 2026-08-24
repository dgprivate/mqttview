package hass

import (
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The registry decides which topics an entity listens on, how availability
// from several sources combines, and how devices are keyed. Each is a place
// where getting it wrong shows up as a device that looks offline or an entity
// that never updates.

func upsert(t *testing.T, reg *Registry, component, object string, cfg entityConfig) (*Entity, []string) {
	t.Helper()
	dt := discoveryTopic{Prefix: "homeassistant", Component: component, ObjectID: object}
	return reg.Upsert("c1", dt, cfg)
}

func TestSubscribedTopicsCoverEveryStreamAnEntityNames(t *testing.T) {
	reg := NewRegistry()

	_, topics := upsert(t, reg, "sensor", "s1", entityConfig{
		"name":                  "T",
		"state_topic":           "home/state",
		"availability_topic":    "home/available",
		"json_attributes_topic": "home/attrs",
	})

	want := map[string]bool{"home/state": false, "home/available": false, "home/attrs": false}
	for _, tp := range topics {
		if _, ok := want[tp]; ok {
			want[tp] = true
		}
	}
	for topic, found := range want {
		if !found {
			t.Errorf("%s was not subscribed, so that stream would never arrive", topic)
		}
	}
}

func TestAvailabilityListIsUsedAsWellAsTheSingleTopic(t *testing.T) {
	reg := NewRegistry()

	_, topics := upsert(t, reg, "sensor", "s1", entityConfig{
		"name":        "T",
		"state_topic": "home/state",
		"availability": []any{
			map[string]any{"topic": "a/one", "payload_available": "up", "payload_not_available": "down"},
			map[string]any{"topic": "a/two"},
		},
	})

	joined := strings.Join(topics, " ")
	if !strings.Contains(joined, "a/one") || !strings.Contains(joined, "a/two") {
		t.Fatalf("the availability list was not subscribed: %v", topics)
	}
}

func TestAvailabilityModeDecidesHowSourcesCombine(t *testing.T) {
	// "all" means every source has to say up; "any" means one is enough. A
	// device with a bridge and a radio uses this to say which it means.
	tests := []struct {
		mode   string
		states []bool
		want   bool
	}{
		{"all", []bool{true, true}, true},
		{"all", []bool{true, false}, false},
		{"any", []bool{false, true}, true},
		{"any", []bool{false, false}, false},
		{"latest", []bool{false, true}, true},
	}

	for _, tt := range tests {
		t.Run(tt.mode+"", func(t *testing.T) {
			reg := NewRegistry()
			e, _ := upsert(t, reg, "sensor", "s1", entityConfig{
				"name": "T", "state_topic": "home/state",
				"availability_mode": tt.mode,
				"availability": []any{
					map[string]any{"topic": "a/one"},
					map[string]any{"topic": "a/two"},
				},
			})

			topics := []string{"a/one", "a/two"}
			for i, up := range tt.states {
				payload := "offline"
				if up {
					payload = "online"
				}
				reg.ApplyState("c1", topics[i], []byte(payload))
			}

			got, _ := reg.Entity(e.ID)
			want := AvailabilityOffline
			if tt.want {
				want = AvailabilityOnline
			}
			if got.Availability != want {
				t.Errorf("mode %q with %v gave %q, want %q",
					tt.mode, tt.states, got.Availability, want)
			}
		})
	}
}

func TestDeviceKeyPrefersTheIdentifierThenTheConnection(t *testing.T) {
	reg := NewRegistry()

	// Two entities from the same device identifier group together.
	upsert(t, reg, "sensor", "a", entityConfig{
		"name": "A", "state_topic": "a", "unique_id": "a",
		"device": map[string]any{"identifiers": []any{"dev-1"}, "name": "Device"},
	})
	upsert(t, reg, "sensor", "b", entityConfig{
		"name": "B", "state_topic": "b", "unique_id": "b",
		"device": map[string]any{"identifiers": []any{"dev-1"}, "name": "Device"},
	})

	devices := reg.Devices("c1")
	if len(devices) != 1 || len(devices[0].Entities) != 2 {
		t.Fatalf("entities did not group by identifier: %+v", devices)
	}

	// A MAC address works when there is no identifier: it is what Zigbee and
	// ESPHome devices tend to send.
	upsert(t, reg, "sensor", "c", entityConfig{
		"name": "C", "state_topic": "c", "unique_id": "c",
		"device": map[string]any{"connections": []any{[]any{"mac", "aa:bb:cc"}}, "name": "MacDevice"},
	})
	if len(reg.Devices("c1")) != 2 {
		t.Fatalf("a MAC-identified device did not group: %+v", reg.Devices("c1"))
	}

	// With neither, the entity still has to appear somewhere.
	upsert(t, reg, "sensor", "d", entityConfig{"name": "D", "state_topic": "d", "unique_id": "d"})
	total := 0
	for _, d := range reg.Devices("c1") {
		total += len(d.Entities)
	}
	if total != 4 {
		t.Errorf("an entity with no device block was lost: %d of 4", total)
	}
}

func TestEntitiesAreSortedStably(t *testing.T) {
	reg := NewRegistry()
	for _, name := range []string{"zebra", "apple", "mango"} {
		upsert(t, reg, "sensor", name, entityConfig{
			"name": name, "state_topic": name, "unique_id": name,
			"device": map[string]any{"identifiers": []any{"dev"}},
		})
	}

	devices := reg.Devices("c1")
	if len(devices) != 1 {
		t.Fatalf("devices = %+v", devices)
	}
	names := make([]string, 0, len(devices[0].Entities))
	for _, e := range devices[0].Entities {
		names = append(names, e.Name)
	}
	// A list that reorders between polls is unusable, so the order is fixed.
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Fatalf("entities are not sorted: %v", names)
		}
	}
}

func TestSecondaryTopicsRouteToTheRightTemplate(t *testing.T) {
	reg := NewRegistry()
	e, _ := upsert(t, reg, "sensor", "s1", entityConfig{
		"name": "T", "state_topic": "home/state", "unique_id": "t",
		"json_attributes_topic":    "home/attrs",
		"json_attributes_template": "{{ value_json.inner }}",
	})

	// Attributes arrive on their own topic and are read with their own
	// template, not the state one.
	reg.ApplyState("c1", "home/attrs", []byte(`{"inner":{"rssi":-42}}`))

	got, _ := reg.Entity(e.ID)
	if got.Attributes == nil || got.Attributes["rssi"] != float64(-42) {
		t.Fatalf("attributes = %+v", got.Attributes)
	}
}

func TestStateOnAnUnknownTopicChangesNothing(t *testing.T) {
	reg := NewRegistry()
	upsert(t, reg, "sensor", "s1", entityConfig{"name": "T", "state_topic": "home/state"})

	if changed := reg.ApplyState("c1", "somewhere/else", []byte("1")); len(changed) != 0 {
		t.Fatalf("an unrelated topic changed %d entities", len(changed))
	}
}

func TestPinnedStateIsRestored(t *testing.T) {
	reg := NewRegistry()
	upsert(t, reg, "sensor", "s1", entityConfig{
		"name": "T", "state_topic": "t", "unique_id": "t",
		"device": map[string]any{"identifiers": []any{"dev-1"}},
	})

	devices := reg.Devices("c1")
	key := devices[0].Key

	storeKey, ok := reg.SetPinned("c1", key, true)
	if !ok || storeKey == "" {
		t.Fatalf("SetPinned = %q %v", storeKey, ok)
	}

	// A fresh registry, as after a restart, with the pins read back from the
	// key-value store.
	fresh := NewRegistry()
	fresh.LoadPinned(map[string]bool{storeKey: true})
	upsert(t, fresh, "sensor", "s1", entityConfig{
		"name": "T", "state_topic": "t", "unique_id": "t",
		"device": map[string]any{"identifiers": []any{"dev-1"}},
	})
	if !fresh.Devices("c1")[0].Pinned {
		t.Error("the pin did not survive a restart")
	}

	if _, ok := reg.SetPinned("c1", "no-such-device", true); ok {
		t.Error("pinning an unknown device reported success")
	}
}

func TestStatsCountDevicesAndEntities(t *testing.T) {
	reg := NewRegistry()
	if d, e := reg.Stats(); d != 0 || e != 0 {
		t.Fatalf("an empty registry reports %d devices, %d entities", d, e)
	}

	upsert(t, reg, "sensor", "a", entityConfig{
		"name": "A", "state_topic": "a", "unique_id": "a",
		"device": map[string]any{"identifiers": []any{"dev"}},
	})
	upsert(t, reg, "sensor", "b", entityConfig{
		"name": "B", "state_topic": "b", "unique_id": "b",
		"device": map[string]any{"identifiers": []any{"dev"}},
	})

	d, e := reg.Stats()
	if d != 1 || e != 2 {
		t.Errorf("Stats = %d devices, %d entities", d, e)
	}
}

func TestRemoveReportsTheTopicsNoLongerNeeded(t *testing.T) {
	reg := NewRegistry()
	dt := discoveryTopic{Prefix: "homeassistant", Component: "sensor", ObjectID: "s1"}
	reg.Upsert("c1", dt, entityConfig{"name": "T", "state_topic": "home/only-mine"})

	orphaned := reg.Remove("c1", dt.topicOf())
	found := false
	for _, tp := range orphaned {
		if tp == "home/only-mine" {
			found = true
		}
	}
	if !found {
		// Leaving it subscribed means paying for a device that is gone.
		t.Fatalf("removing an entity did not release its topics: %v", orphaned)
	}

	// A topic another entity still uses must not be released.
	reg2 := NewRegistry()
	dtA := discoveryTopic{Prefix: "homeassistant", Component: "sensor", ObjectID: "a"}
	dtB := discoveryTopic{Prefix: "homeassistant", Component: "sensor", ObjectID: "b"}
	reg2.Upsert("c1", dtA, entityConfig{"name": "A", "state_topic": "shared"})
	reg2.Upsert("c1", dtB, entityConfig{"name": "B", "state_topic": "shared"})

	for _, tp := range reg2.Remove("c1", dtA.topicOf()) {
		if tp == "shared" {
			t.Error("a topic another entity still listens on was released")
		}
	}
}

func TestSettingBoolReadsWhatTheUISends(t *testing.T) {
	// The settings map comes back from JSON, so a boolean may arrive as a
	// string.
	for _, tc := range []struct {
		value any
		want  bool
	}{
		{true, true}, {false, false}, {"true", true}, {"false", false},
		{"1", true}, {"0", false}, {nil, true}, {"nonsense", true},
	} {
		if got := settingBool(map[string]any{"k": tc.value}, "k", true); got != tc.want {
			t.Errorf("settingBool(%v) = %v, want %v", tc.value, got, tc.want)
		}
	}
}

func TestReadingTheRegistryWhileItUpdatesIsSafe(t *testing.T) {
	// The panel polls /devices while messages arrive, so the registry is read
	// and written at the same time by definition. It used to hand out pointers
	// to its live entities, and the race detector caught an HTTP handler
	// serialising one while a state message rewrote it — from the plugin's
	// end-to-end test, once in a hundred runs, on a loaded machine.
	//
	// Run this with -race; without it the test proves nothing.
	reg := NewRegistry()
	upsert(t, reg, "sensor", "s1", entityConfig{
		"name":                  "T",
		"state_topic":           "home/state",
		"json_attributes_topic": "home/attrs",
		"availability_topic":    "home/available",
	})

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			reg.ApplyState("c1", "home/state", []byte(strconv.Itoa(i)))
			reg.ApplyState("c1", "home/attrs", []byte(`{"battery":42}`))
			reg.ApplyState("c1", "home/available", []byte("online"))
		}
	}()

	// What the handlers do: take what the registry gives them and read every
	// field of it, which is what marshalling to JSON amounts to.
	for range 200 {
		for _, d := range reg.Devices("") {
			for _, e := range d.Entities {
				readEverything(e)
			}
		}
		for _, e := range reg.Entities("") {
			readEverything(e)
		}
		if e, ok := reg.Entity("c1|homeassistant/sensor/s1/config"); ok {
			readEverything(e)
		}
	}

	close(stop)
	wg.Wait()
}

// readEverything touches the fields a JSON encoder would, so the race detector
// sees the same reads a real request makes.
func readEverything(e *Entity) {
	if e == nil {
		return
	}
	_ = e.Name + e.ConnectionID + string(e.Availability)
	_ = e.UpdatedAt
	if e.State != nil {
		_ = e.State.Raw + toString(e.State.Value)
	}
	for k, v := range e.Attributes {
		_ = k
		_ = v
	}
}
