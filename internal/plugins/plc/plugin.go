// Package plc implements an mqttview plugin for a Beckhoff PLC that publishes
// its I/O over MQTT.
//
// Unlike Home Assistant discovery there is no announcement message to read: the
// PLC simply publishes each point under a numeric address, and the meaning of
// that address lives in a second stream keyed by the point's PLC name. Its
// job is to fold those two streams back together, so that
// "plc/digital/input/245" reads as a motion detector in the hallway rather than
// as address 245.
//
// It also keeps a journal of digital edges. Pressing a physical button and
// reading back which address moved is the fastest way to find out what a wall
// switch is wired to, and the journal is what the MCP server serves so that a
// PLC program can be written against "the button I just pressed".
//
// It observes only. The PLC accepts commands on its own topic, but a house is a
// poor place to discover a bug, so nothing here publishes.
package plc

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mqttview/mqttview/internal/httpx"
	"github.com/mqttview/mqttview/internal/mqttc"
	"github.com/mqttview/mqttview/internal/plugin"
)

// ID is the plugin's registry identifier.
const ID = "beckhoff-plc"

func init() {
	plugin.Register(ID, func() plugin.Plugin { return &Plugin{} })
}

// Plugin projects a Beckhoff PLC's MQTT traffic into a room-level view.
type Plugin struct {
	host     plugin.Host
	registry *Registry

	mu          sync.RWMutex
	prefix      string
	meterPrefix string

	dirty     bool
	dirtyMu   sync.Mutex
	flushStop chan struct{}
	closeOnce sync.Once
}

// Meta describes the plugin.
func (p *Plugin) Meta() plugin.Meta {
	return plugin.Meta{
		ID:          ID,
		Name:        "Beckhoff PLC",
		Version:     "1.0.0",
		Description: "Reads a Beckhoff PLC's MQTT feed and shows its inputs, outputs, temperatures, DALI lights, shades, power and meters by room and name. Observes only; it never publishes a command.",
		Author:      "mqttview",
		Panel:       "beckhoff-plc",
		SettingsSchema: []plugin.SettingField{
			{
				Key:         "topicPrefix",
				Label:       "PLC topic prefix",
				Type:        "string",
				Default:     "plc",
				Description: "The prefix the PLC publishes under, e.g. 'plc' for plc/digital/input/245.",
			},
			{
				Key:         "meterPrefix",
				Label:       "Meter topic prefix",
				Type:        "string",
				Default:     "mbus2mqtt",
				Description: "Prefix for M-Bus meters published alongside the PLC. Leave empty to ignore them.",
			},
			{
				Key:         "subscribeQos",
				Label:       "Subscription QoS",
				Type:        "select",
				Default:     "0",
				Description: "QoS used for the plugin's own subscriptions.",
				Options: []plugin.SettingOption{
					{Value: "0", Label: "0 - at most once"},
					{Value: "1", Label: "1 - at least once"},
					{Value: "2", Label: "2 - exactly once"},
				},
			},
			{
				Key:         "journalSize",
				Label:       "Signal log length",
				Type:        "number",
				Default:     defaultJournalSize,
				Description: "How many digital transitions to keep for the discovery view and the MCP server.",
			},
		},
	}
}

// Init reads settings and starts the UI batching loop.
func (p *Plugin) Init(_ context.Context, host plugin.Host) error {
	p.host = host
	settings := host.Settings()
	if p.registry == nil {
		p.registry = NewRegistry(settingInt(settings, "journalSize", defaultJournalSize))
	}

	p.mu.Lock()
	p.prefix = strings.Trim(settingString(settings, "topicPrefix", "plc"), "/")
	if p.prefix == "" {
		p.prefix = "plc"
	}
	p.meterPrefix = strings.Trim(settingString(settings, "meterPrefix", "mbus2mqtt"), "/")
	prefix, meterPrefix := p.prefix, p.meterPrefix
	p.mu.Unlock()

	p.flushStop = make(chan struct{})
	// The channel is passed in rather than read from the field, so Close can
	// never race with the loop over the same variable.
	go p.flushLoop(p.flushStop)

	host.Logger().Info("beckhoff plc plugin started", "prefix", prefix, "meterPrefix", meterPrefix)
	return nil
}

// Subscriptions asks for the PLC tree, and the meter tree when one is
// configured. Both are bounded branches of one broker, not a wildcard on the
// root, so this stays a few hundred topics rather than everything.
func (p *Plugin) Subscriptions() []mqttc.Subscription {
	p.mu.RLock()
	prefix, meterPrefix := p.prefix, p.meterPrefix
	p.mu.RUnlock()

	qos := p.qos()
	subs := []mqttc.Subscription{{Filter: prefix + "/#", QoS: qos}}
	if meterPrefix != "" {
		subs = append(subs, mqttc.Subscription{Filter: meterPrefix + "/#", QoS: qos})
	}
	return subs
}

func (p *Plugin) qos() byte {
	if p.host == nil {
		return 0
	}
	raw := settingString(p.host.Settings(), "subscribeQos", "0")
	q, err := strconv.Atoi(raw)
	if err != nil || q < 0 || q > 2 {
		return 0
	}
	return byte(q)
}

// HandleMessage folds one PLC or meter message into the registry.
func (p *Plugin) HandleMessage(_ context.Context, msg mqttc.Message) {
	p.mu.RLock()
	prefix, meterPrefix := p.prefix, p.meterPrefix
	p.mu.RUnlock()

	received := msg.ReceivedAt
	if received.IsZero() {
		received = time.Now()
	}
	if p.registry.Apply(prefix, meterPrefix, msg.ConnectionID, msg.Topic, msg.Payload, received) {
		p.markDirty()
	}
}

func (p *Plugin) markDirty() {
	p.dirtyMu.Lock()
	p.dirty = true
	p.dirtyMu.Unlock()
}

// flushLoop emits at most one update every 250ms. The electricity feed alone
// runs at 1Hz and the digital stream bursts far faster than that; the browser
// only needs the end state.
func (p *Plugin) flushLoop(stop <-chan struct{}) {
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()

	for {
		select {
		case <-stop:
			return
		case <-t.C:
			p.dirtyMu.Lock()
			dirty := p.dirty
			p.dirty = false
			p.dirtyMu.Unlock()
			if !dirty {
				continue
			}
			points, lights := p.registry.Stats()
			// The sequence number rides along so the panel knows whether to
			// refetch the edge log or only the state.
			p.host.Emit("changed", map[string]any{
				"points": points,
				"lights": lights,
				"seq":    p.registry.Journal().Latest(),
			})
		}
	}
}

// Close stops the batching goroutine. It is safe to call more than once.
func (p *Plugin) Close() error {
	p.closeOnce.Do(func() {
		if p.flushStop != nil {
			close(p.flushStop)
		}
	})
	return nil
}

// Routes exposes the plugin's HTTP API under /api/p/beckhoff-plc.
func (p *Plugin) Routes(r chi.Router) {
	r.Get("/status", p.handleStatus)
	r.Get("/state", p.handleState)
	r.Get("/edges", p.handleEdges)
}

// maxEdgeWait bounds a long poll. It sits below the usual sixty second proxy
// timeout so a caller gets an empty answer rather than a dropped connection.
const maxEdgeWait = 50 * time.Second

// handleEdges serves the digital edge journal, optionally blocking until
// something new happens. Blocking is what turns it into "tell me the next
// button pressed" for both the panel and the MCP server.
func (p *Plugin) handleEdges(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	since := parseUint(q.Get("since"))
	limit := parseInt(q.Get("limit"), 100)
	wait := time.Duration(parseInt(q.Get("waitMs"), 0)) * time.Millisecond
	if wait > maxEdgeWait {
		wait = maxEdgeWait
	}

	match := edgeMatch(q.Get("connectionId"), q.Get("kind"), q.Get("rising") == "true")

	journal := p.registry.Journal()
	var (
		edges  []Edge
		latest uint64
	)
	if wait > 0 {
		edges, latest = journal.Wait(r.Context(), since, limit, wait, match)
	} else {
		edges, latest = journal.Since(since, limit, match)
	}

	if edges == nil {
		edges = []Edge{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"edges": edges,
		"seq":   latest,
	})
}

// edgeMatch builds the filter for a journal read, or nil when nothing was
// asked for and every edge qualifies.
func edgeMatch(connID, kind string, risingOnly bool) Match {
	if connID == "" && kind == "" && !risingOnly {
		return nil
	}
	return func(e Edge) bool {
		if connID != "" && e.ConnectionID != connID {
			return false
		}
		if kind != "" && string(e.Kind) != kind {
			return false
		}
		if risingOnly && !e.Rising() {
			return false
		}
		return true
	}
}

func parseUint(s string) uint64 {
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func parseInt(s string, def int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func (p *Plugin) handleStatus(w http.ResponseWriter, _ *http.Request) {
	points, lights := p.registry.Stats()
	p.mu.RLock()
	prefix, meterPrefix := p.prefix, p.meterPrefix
	p.mu.RUnlock()

	journal := p.registry.Journal()
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"topicPrefix": prefix,
		"meterPrefix": meterPrefix,
		"points":      points,
		"lights":      lights,
		"edges":       journal.Len(),
		"seq":         journal.Latest(),
		// Stated rather than configurable: control is deliberately not built.
		"readOnly": true,
	})
}

func (p *Plugin) handleState(w http.ResponseWriter, r *http.Request) {
	connID := r.URL.Query().Get("connectionId")
	httpx.WriteJSON(w, http.StatusOK, p.registry.Snapshot(connID))
}

func settingString(settings map[string]any, key, def string) string {
	if v, ok := settings[key]; ok {
		switch s := v.(type) {
		case string:
			// An empty string is a deliberate "off" for meterPrefix, so it is
			// returned as-is rather than falling back to the default.
			return s
		case float64:
			return strconv.FormatFloat(s, 'f', -1, 64)
		}
	}
	return def
}

func settingInt(settings map[string]any, key string, def int) int {
	switch v := settings[key].(type) {
	case float64:
		return int(v)
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// compile-time check that the plugin satisfies the interface.
var _ plugin.Plugin = (*Plugin)(nil)
