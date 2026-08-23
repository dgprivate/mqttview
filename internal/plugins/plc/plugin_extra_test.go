package plc

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

// The plugin runs against more than one broker in a house that has a second
// PLC, and everything that acts on the world has to know which one it means.

func TestCommandsAndNamesNeedAConnectionWhenThereIsMoreThanOne(t *testing.T) {
	p, h, srv := startPlugin(t, map[string]any{"allowControl": true})
	_ = p

	h.mu.Lock()
	h.conns = twoConnections(t)
	h.mu.Unlock()

	code, body := post(t, srv, http.MethodPost, "/command",
		CommandRequest{Target: "dali", Command: "on", Address: 1}, "")
	if code != http.StatusBadRequest {
		t.Errorf("command with two brokers gave %d, want 400: %s", code, body)
	}
	// The message has to name the field to add, not just say no.
	if body == "" {
		t.Error("the refusal explains nothing")
	}

	code, _ = post(t, srv, http.MethodPut, "/mappings",
		map[string]any{"name": "DI-1-1", "label": "Kitchen switch"}, "")
	if code != http.StatusBadRequest {
		t.Errorf("naming a point with two brokers gave %d, want 400", code)
	}
}

func TestABrokerThatRefusesACommandIsReportedAsSuch(t *testing.T) {
	_, h, srv := startPlugin(t, map[string]any{"allowControl": true})

	h.mu.Lock()
	h.conns = twoConnections(t)[:1]
	h.publishErr = errors.New("not connected")
	h.mu.Unlock()

	code, body := post(t, srv, http.MethodPost, "/command",
		CommandRequest{Target: "dali", Command: "on", Address: 1}, "")
	if code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502: %s", code, body)
	}
}

func TestNamingAPointNeedsTheOperatorRole(t *testing.T) {
	_, _, srv := startPlugin(t, nil)

	// A name is shown to everyone and is what an electrician works from, so
	// changing one is not a viewer's decision.
	code, _ := post(t, srv, http.MethodPut, "/mappings",
		map[string]any{"name": "DI-1-1", "label": "Kitchen switch"}, "viewer")
	if code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", code)
	}
}

func TestAMalformedCommandOrMappingBody(t *testing.T) {
	_, _, srv := startPlugin(t, map[string]any{"allowControl": true})

	for _, path := range []string{"/command", "/mappings"} {
		method := http.MethodPost
		if path == "/mappings" {
			method = http.MethodPut
		}
		if code, _ := postRawPLC(t, srv, method, path, "{broken"); code != http.StatusBadRequest {
			t.Errorf("%s with a broken body gave %d, want 400", path, code)
		}
	}
}

func TestTheEdgeLogCanBeFilteredWhileWaiting(t *testing.T) {
	p, _, srv := startPlugin(t, nil)

	// Two points move; the caller is only interested in one of them. A long
	// poll that returns on the wrong edge would tell the electrician the wrong
	// input was pressed, which is the whole thing this endpoint exists to
	// avoid getting wrong.
	apply(t, p.registry, "plc/digital/input/1", `{"address":1,"name":"DI-1-1","value":false}`)
	apply(t, p.registry, "plc/digital/input/2", `{"address":2,"name":"DI-1-2","value":false}`)

	go func() {
		time.Sleep(200 * time.Millisecond)
		apply(t, p.registry, "plc/digital/input/1", `{"address":1,"name":"DI-1-1","value":true}`)
	}()

	var out struct {
		Edges []Edge `json:"edges"`
		Seq   uint64 `json:"seq"`
	}
	code := get(t, srv, "/edges?waitMs=5000&rising=true&kind=input", &out)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(out.Edges) != 1 || out.Edges[0].Name != "DI-1-1" {
		t.Fatalf("edges = %+v", out.Edges)
	}
	if !out.Edges[0].Rising() {
		t.Error("a rising-only poll returned a falling edge")
	}
}

func TestALongPollThatSeesNothingReturnsEmptyRatherThanHanging(t *testing.T) {
	_, _, srv := startPlugin(t, nil)

	var out struct {
		Edges []Edge `json:"edges"`
		Seq   uint64 `json:"seq"`
	}
	start := time.Now()
	if code := get(t, srv, "/edges?waitMs=300", &out); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(out.Edges) != 0 {
		t.Errorf("edges = %+v, want none", out.Edges)
	}
	// It waited, and it came back.
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("the poll took %v", elapsed)
	}
}

func TestSettingsAreReadFromStringsAsWellAsTypedValues(t *testing.T) {
	// The settings form posts strings; the API and the defaults use typed
	// values. Both have to mean the same thing, or a setting silently reverts
	// to its default the first time somebody edits it in the UI.
	if got := settingBool(map[string]any{"allowControl": "true"}, "allowControl", false); !got {
		t.Error(`the string "true" was not read as a boolean`)
	}
	if got := settingInt(map[string]any{"journalSize": "250"}, "journalSize", 10); got != 250 {
		t.Errorf("the string \"250\" was read as %d", got)
	}
	if got := settingString(map[string]any{"subscribeQos": float64(1)}, "subscribeQos", "0"); got != "1" {
		t.Errorf("a numeric setting rendered as %q", got)
	}
	// Anything unusable falls back rather than producing a zero value.
	if got := settingInt(map[string]any{"journalSize": "not a number"}, "journalSize", 10); got != 10 {
		t.Errorf("an unparseable size gave %d, want the default", got)
	}
	if got := settingBool(map[string]any{"allowControl": 3}, "allowControl", false); got {
		t.Error("a number was read as a boolean")
	}
	// An empty meter prefix is a deliberate "do not subscribe", not a default.
	if got := settingString(map[string]any{"meterPrefix": ""}, "meterPrefix", "mbus2mqtt"); got != "" {
		t.Errorf("an empty prefix became %q", got)
	}
}
