package hass

import (
	"errors"
	"net/http"
	"testing"
)

// What the control endpoint does when something other than the caller is at
// fault. Each of these answers a different question for whoever is looking at
// the browser's network tab, so each needs its own status.

func TestABrokerThatRefusesTheCommandIsNotReportedAsSuccess(t *testing.T) {
	p, h, srv := startPlugin(t, map[string]any{"allowControl": true})
	discover(t, p, "homeassistant/switch/x/y/config",
		`{"name":"S","uniq_id":"s1","command_topic":"home/s/set"}`)

	var entities []HassEntityLike
	getJSON(t, srv, "/entities", &entities)

	h.mu.Lock()
	h.publishErr = errors.New("not connected")
	h.mu.Unlock()

	code, body := postJSON(t, srv, "/command",
		map[string]string{"entityId": entities[0].ID, "action": "turn_on"}, "")
	// 502, not 500: the failure is between mqttview and the broker, and the
	// operator's next step is to look at the connection, not at the logs.
	if code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", code)
	}
	if body == "" {
		t.Error("the response says nothing about why")
	}
}

func TestCommandingSomethingThatIsNotThere(t *testing.T) {
	_, _, srv := startPlugin(t, map[string]any{"allowControl": true})

	if code, _ := postJSON(t, srv, "/command",
		map[string]string{"entityId": "nothing/like/this", "action": "turn_on"}, ""); code != http.StatusNotFound {
		t.Errorf("an unknown entity gave %d, want 404", code)
	}
}

func TestAnActionAnEntityCannotPerform(t *testing.T) {
	p, _, srv := startPlugin(t, map[string]any{"allowControl": true})
	// A sensor has no command topic, so there is nothing it can be told.
	discover(t, p, "homeassistant/sensor/x/y/config",
		`{"name":"T","uniq_id":"t1","state_topic":"home/t"}`)

	var entities []HassEntityLike
	getJSON(t, srv, "/entities", &entities)

	code, body := postJSON(t, srv, "/command",
		map[string]string{"entityId": entities[0].ID, "action": "turn_on"}, "")
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", code, body)
	}
}

func TestPinningSomethingThatIsNotThere(t *testing.T) {
	_, _, srv := startPlugin(t, nil)

	if code, _ := postJSON(t, srv, "/pin",
		map[string]any{"deviceKey": "absent", "connectionId": "c1", "pinned": true}, ""); code != http.StatusNotFound {
		t.Errorf("pinning an unknown device gave %d, want 404", code)
	}
}

func TestMalformedBodiesAreRejectedRatherThanGuessedAt(t *testing.T) {
	_, _, srv := startPlugin(t, map[string]any{"allowControl": true})

	for _, path := range []string{"/command", "/pin"} {
		code, _ := postRaw(t, srv, path, "{not json")
		if code != http.StatusBadRequest {
			t.Errorf("%s with a broken body gave %d, want 400", path, code)
		}
	}
}
