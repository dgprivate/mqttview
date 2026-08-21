// Command mqttview-mcp exposes a Beckhoff PLC's live signals to an AI agent
// over the Model Context Protocol.
//
// It exists for one workflow: writing PLC logic against physical inputs whose
// addresses nobody remembers. Rather than reading a wiring list, an agent can
// wait for the next signal, have a person press the button, and be told exactly
// which point moved and what it is called. The specification that comes out of
// that names a real point instead of a guess.
//
// The server is read-only, because the plugin it reads from is. It observes the
// house; it never actuates it.
//
// Configuration, by flag or environment variable:
//
//	-url       MQTTVIEW_URL           base URL, default http://127.0.0.1:8114
//	-email     MQTTVIEW_EMAIL         mqttview login
//	-password  MQTTVIEW_PASSWORD      mqttview password
//	-connection MQTTVIEW_CONNECTION_ID  restrict to one broker connection
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mqttview/mqttview/internal/plugins/plc"
)

// version is the MCP server's own version, reported at initialize.
const version = "1.0.0"

// maxWait bounds plc_wait_for_signal. It is a little under the plugin's own
// long-poll ceiling so the request returns rather than being cut off.
const maxWait = 45 * time.Second

func main() {
	log.SetFlags(0)
	// Logs must go to stderr: stdout is the MCP transport.
	log.SetOutput(os.Stderr)

	var (
		base       = flag.String("url", env("MQTTVIEW_URL", "http://127.0.0.1:8114"), "mqttview base URL")
		email      = flag.String("email", env("MQTTVIEW_EMAIL", ""), "mqttview login email")
		password   = flag.String("password", env("MQTTVIEW_PASSWORD", ""), "mqttview password")
		connection = flag.String("connection", env("MQTTVIEW_CONNECTION_ID", ""), "restrict to one broker connection ID")
	)
	flag.Parse()

	if *email == "" || *password == "" {
		log.Fatal("set MQTTVIEW_EMAIL and MQTTVIEW_PASSWORD (or pass -email and -password)")
	}

	client, err := NewClient(*base, *email, *password)
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Fail at startup rather than on the first tool call: a broken login is far
	// easier to diagnose here than inside an agent's transcript.
	loginCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	err = client.Login(loginCtx)
	cancel()
	if err != nil {
		log.Fatal(err)
	}

	server := newServer(client, *connection)
	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func newServer(client *Client, connectionID string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{
		Name:    "mqttview-plc",
		Version: version,
	}, nil)

	t := &tools{client: client, connectionID: connectionID}

	mcp.AddTool(s, &mcp.Tool{
		Name: "plc_wait_for_signal",
		Description: "Block until a digital input or output on the PLC changes state, then report which one. " +
			"This is how you identify a physical control: call this tool, ask the person to press the button or " +
			"trigger the sensor, and the point that moves is the answer. Returns the point's address, PLC name " +
			"(such as DI-31-5), human label, location and direction of travel. Times out with an empty list if " +
			"nothing happens, which is not an error - call it again if the person was not ready.",
	}, t.waitForSignal)

	mcp.AddTool(s, &mcp.Tool{
		Name: "plc_recent_signals",
		Description: "List digital transitions that already happened, newest last. Use it to see what a person " +
			"did a moment ago, or to check whether an output responded to an input. Only real transitions are " +
			"recorded; the retained values that arrive when the broker connects are not.",
	}, t.recentSignals)

	mcp.AddTool(s, &mcp.Tool{
		Name: "plc_find_points",
		Description: "Search the PLC's inputs, outputs and temperature channels by PLC name, human label, " +
			"location or address, and return their current values. Use it to turn a description such as " +
			"'hallway motion' into the address a PLC program must reference.",
	}, t.findPoints)

	mcp.AddTool(s, &mcp.Tool{
		Name: "plc_lights",
		Description: "List the DALI lights with their current level, configured minimum and maximum, and any " +
			"error the ballast reports. Set only_errors to see just the ones that are not answering properly.",
	}, t.lights)

	mcp.AddTool(s, &mcp.Tool{
		Name: "plc_name_point",
		Description: "Give a PLC point a human name, location and free-text note. Use it right after " +
			"plc_wait_for_signal, so the button somebody just pressed stops being 'DI-31-5' and becomes what they " +
			"called it. The notes field is the place to record what the signal should do, for whoever writes the " +
			"PLC logic. This writes to mqttview only: it changes what a point is called, never the PLC or anything " +
			"in the house. Sending every field empty removes the name.",
	}, t.namePoint)

	mcp.AddTool(s, &mcp.Tool{
		Name: "plc_overview",
		Description: "Summarise the PLC: how many points, lights and shades are known, the watchdog's alarm " +
			"mode and stream ages, three-phase electricity readings and any M-Bus meters. Start here to find " +
			"out what the installation contains.",
	}, t.overview)

	return s
}

type tools struct {
	client       *Client
	connectionID string
}

// waitInput is the argument set for plc_wait_for_signal.
type waitInput struct {
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"how long to wait, 1-45 seconds, default 30"`
	RisingOnly     bool   `json:"rising_only,omitempty" jsonschema:"only report off-to-on transitions, which is what a button press looks like"`
	Kind           string `json:"kind,omitempty" jsonschema:"restrict to 'input' or 'output'; empty means both"`
	Since          uint64 `json:"since,omitempty" jsonschema:"only report signals newer than this sequence number, from an earlier call"`
}

// signalOutput is returned by both signal tools.
type signalOutput struct {
	Signals []signalView `json:"signals"`
	// Seq is the newest sequence number; pass it back as 'since' to continue
	// without seeing the same transition twice.
	Seq      uint64 `json:"seq"`
	TimedOut bool   `json:"timed_out"`
	Note     string `json:"note,omitempty"`
}

// signalView is one transition, flattened for readability in a transcript.
type signalView struct {
	Seq        uint64 `json:"seq"`
	Address    int    `json:"address"`
	Name       string `json:"name"`
	Label      string `json:"label,omitempty"`
	Location   string `json:"location,omitempty"`
	SensorType string `json:"sensor_type,omitempty"`
	AlarmZone  bool   `json:"alarm_zone,omitempty"`
	Kind       string `json:"kind"`
	Topic      string `json:"topic"`
	Transition string `json:"transition"`
	At         string `json:"at"`
}

func toSignal(e plc.Edge) signalView {
	transition := "off -> on"
	if !e.To {
		transition = "on -> off"
	}
	return signalView{
		Seq:        e.Seq,
		Address:    e.Address,
		Name:       e.Name,
		Label:      e.Label,
		Location:   e.Location,
		SensorType: e.SensorType,
		AlarmZone:  e.AlarmZone,
		Kind:       string(e.Kind),
		Topic:      e.Topic,
		Transition: transition,
		At:         e.At.Format(time.RFC3339Nano),
	}
}

func (t *tools) waitForSignal(ctx context.Context, _ *mcp.CallToolRequest, in waitInput) (*mcp.CallToolResult, signalOutput, error) {
	wait := time.Duration(in.TimeoutSeconds) * time.Second
	if in.TimeoutSeconds <= 0 {
		wait = 30 * time.Second
	}
	if wait > maxWait {
		wait = maxWait
	}

	// The HTTP call must outlive the long poll, with room for the round trip.
	reqCtx, cancel := context.WithTimeout(ctx, wait+15*time.Second)
	defer cancel()

	since := in.Since
	if since == 0 {
		// Without a starting point the journal would hand back history, and
		// "the next button pressed" would be answered by an old one. Anchor on
		// the current end of the log first.
		status, err := t.client.Status(reqCtx)
		if err != nil {
			return nil, signalOutput{}, err
		}
		since = status.Seq
	}

	page, err := t.client.Edges(reqCtx, t.connectionID, since, 20, in.RisingOnly, in.Kind, wait)
	if err != nil {
		return nil, signalOutput{}, err
	}

	out := signalOutput{Seq: page.Seq, Signals: make([]signalView, 0, len(page.Edges))}
	for _, e := range page.Edges {
		out.Signals = append(out.Signals, toSignal(e))
	}
	if len(out.Signals) == 0 {
		out.TimedOut = true
		out.Note = fmt.Sprintf("nothing moved within %s; ask the person to press the control, then call again with since=%d", wait, out.Seq)
	}
	return nil, out, nil
}

// recentInput is the argument set for plc_recent_signals.
type recentInput struct {
	Limit      int    `json:"limit,omitempty" jsonschema:"how many transitions to return, default 20"`
	RisingOnly bool   `json:"rising_only,omitempty" jsonschema:"only off-to-on transitions"`
	Kind       string `json:"kind,omitempty" jsonschema:"restrict to 'input' or 'output'; empty means both"`
	Since      uint64 `json:"since,omitempty" jsonschema:"only transitions newer than this sequence number"`
}

func (t *tools) recentSignals(ctx context.Context, _ *mcp.CallToolRequest, in recentInput) (*mcp.CallToolResult, signalOutput, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}
	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	page, err := t.client.Edges(reqCtx, t.connectionID, in.Since, limit, in.RisingOnly, in.Kind, 0)
	if err != nil {
		return nil, signalOutput{}, err
	}

	out := signalOutput{Seq: page.Seq, Signals: make([]signalView, 0, len(page.Edges))}
	for _, e := range page.Edges {
		out.Signals = append(out.Signals, toSignal(e))
	}
	if len(out.Signals) == 0 {
		out.Note = "no transitions recorded yet; the log only fills once something actually changes"
	}
	return nil, out, nil
}

// findInput is the argument set for plc_find_points.
type findInput struct {
	Query string `json:"query,omitempty" jsonschema:"text matched against PLC name, human label, location and sensor type; empty returns everything"`
	Kind  string `json:"kind,omitempty" jsonschema:"restrict to 'input', 'output' or 'temperature'"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum results, default 50"`
}

type pointView struct {
	Address    int     `json:"address"`
	Name       string  `json:"name"`
	Label      string  `json:"label,omitempty"`
	Location   string  `json:"location,omitempty"`
	SensorType string  `json:"sensor_type,omitempty"`
	AlarmZone  bool    `json:"alarm_zone,omitempty"`
	Kind       string  `json:"kind"`
	Topic      string  `json:"topic"`
	Value      string  `json:"value"`
	Number     float64 `json:"number,omitempty"`
	UpdatedAt  string  `json:"updated_at"`
}

type findOutput struct {
	Points  []pointView `json:"points"`
	Total   int         `json:"total"`
	Note    string      `json:"note,omitempty"`
	Skipped int         `json:"skipped,omitempty"`
}

func (t *tools) findPoints(ctx context.Context, _ *mcp.CallToolRequest, in findInput) (*mcp.CallToolResult, findOutput, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = 50
	}
	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	state, err := t.client.State(reqCtx, t.connectionID)
	if err != nil {
		return nil, findOutput{}, err
	}

	needle := strings.ToLower(strings.TrimSpace(in.Query))
	var matched []pointView
	for _, p := range state.Points {
		if in.Kind != "" && string(p.Kind) != in.Kind {
			continue
		}
		if needle != "" && !pointMatches(p, needle) {
			continue
		}
		matched = append(matched, toPointView(p))
	}

	sort.Slice(matched, func(i, j int) bool {
		if matched[i].Kind != matched[j].Kind {
			return matched[i].Kind < matched[j].Kind
		}
		return matched[i].Address < matched[j].Address
	})

	out := findOutput{Total: len(matched)}
	if len(matched) > limit {
		// Say so rather than silently truncating: a partial list that looks
		// complete is how a point gets missed.
		out.Skipped = len(matched) - limit
		out.Note = fmt.Sprintf("showing %d of %d matches; narrow the query or raise the limit", limit, len(matched))
		matched = matched[:limit]
	}
	out.Points = matched
	if out.Points == nil {
		out.Points = []pointView{}
	}
	return nil, out, nil
}

func pointMatches(p *plc.Point, needle string) bool {
	for _, field := range []string{p.Name, p.Label, p.Location, p.SensorType, p.Topic} {
		if field != "" && strings.Contains(strings.ToLower(field), needle) {
			return true
		}
	}
	return strings.Contains(fmt.Sprint(p.Address), needle)
}

func toPointView(p *plc.Point) pointView {
	v := pointView{
		Address:    p.Address,
		Name:       p.Name,
		Label:      p.Label,
		Location:   p.Location,
		SensorType: p.SensorType,
		AlarmZone:  p.AlarmZone,
		Kind:       string(p.Kind),
		Topic:      p.Topic,
		UpdatedAt:  p.UpdatedAt.Format(time.RFC3339),
	}
	switch {
	case p.Bool != nil && *p.Bool:
		v.Value = "on"
	case p.Bool != nil:
		v.Value = "off"
	case p.Number != nil:
		v.Number = *p.Number
		v.Value = fmt.Sprintf("%.2f", *p.Number)
	default:
		v.Value = "unknown"
	}
	return v
}

// nameInput is the argument set for plc_name_point.
type nameInput struct {
	Name     string `json:"name" jsonschema:"the PLC's own name for the point, such as DI-31-5, exactly as a signal or search reported it"`
	Label    string `json:"label,omitempty" jsonschema:"what a person calls it, e.g. 'Kuhinja - stikalo pri vratih'"`
	Location string `json:"location,omitempty" jsonschema:"room or area, e.g. 'kitchen'"`
	Type     string `json:"type,omitempty" jsonschema:"what kind of thing it is: button, motion, door, water_leak and so on"`
	Notes    string `json:"notes,omitempty" jsonschema:"free text, typically what this signal should make the PLC do"`
}

type nameOutput struct {
	Saved   plc.Mapping `json:"saved"`
	Removed bool        `json:"removed"`
	Note    string      `json:"note"`
}

func (t *tools) namePoint(ctx context.Context, _ *mcp.CallToolRequest, in nameInput) (*mcp.CallToolResult, nameOutput, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, nameOutput{}, fmt.Errorf("name is required: it is the PLC's own name for the point, such as DI-31-5")
	}

	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	m := plc.Mapping{
		Name:     name,
		Label:    strings.TrimSpace(in.Label),
		Location: strings.TrimSpace(in.Location),
		Type:     strings.TrimSpace(in.Type),
		Notes:    strings.TrimSpace(in.Notes),
	}
	saved, err := t.client.SetMapping(reqCtx, t.connectionID, m)
	if err != nil {
		return nil, nameOutput{}, err
	}

	removed := saved.Label == "" && saved.Location == "" && saved.Type == "" && saved.Notes == ""
	note := fmt.Sprintf("%s is now called %q; it will show up under that name in mqttview and in future signals", name, saved.Label)
	if removed {
		note = fmt.Sprintf("the name for %s was removed; it falls back to whatever the PLC publishes about it", name)
	}
	return nil, nameOutput{Saved: saved, Removed: removed, Note: note}, nil
}

// lightsInput is the argument set for plc_lights.
type lightsInput struct {
	OnlyErrors bool `json:"only_errors,omitempty" jsonschema:"return only ballasts reporting an error"`
}

type lightView struct {
	Address     int    `json:"address"`
	Name        string `json:"name"`
	Level       int    `json:"level"`
	Percent     int    `json:"percent"`
	MinLevel    int    `json:"min_level"`
	MaxLevel    int    `json:"max_level"`
	Status      int    `json:"status"`
	LastCommand string `json:"last_command,omitempty"`
	Error       string `json:"error,omitempty"`
}

type lightsOutput struct {
	Lights     []lightView `json:"lights"`
	Total      int         `json:"total"`
	WithErrors int         `json:"with_errors"`
	On         int         `json:"on"`
}

func (t *tools) lights(ctx context.Context, _ *mcp.CallToolRequest, in lightsInput) (*mcp.CallToolResult, lightsOutput, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	state, err := t.client.State(reqCtx, t.connectionID)
	if err != nil {
		return nil, lightsOutput{}, err
	}

	out := lightsOutput{
		Total:      state.Summary.Lights,
		WithErrors: state.Summary.LightsWithError,
		On:         state.Summary.LightsOn,
		Lights:     []lightView{},
	}
	for _, l := range state.Lights {
		if in.OnlyErrors && l.Error == "" {
			continue
		}
		out.Lights = append(out.Lights, lightView{
			Address:     l.Address,
			Name:        l.Name,
			Level:       l.ActualLevel,
			Percent:     l.Percent(),
			MinLevel:    l.MinLevel,
			MaxLevel:    l.MaxLevel,
			Status:      l.Status,
			LastCommand: l.LastCommand,
			Error:       l.Error,
		})
	}
	return nil, out, nil
}

type overviewOutput struct {
	TopicPrefix string            `json:"topic_prefix"`
	ReadOnly    bool              `json:"read_only"`
	Counts      plc.Summary       `json:"counts"`
	Watchdog    *watchdogView     `json:"watchdog,omitempty"`
	Electricity []electricityView `json:"electricity,omitempty"`
	Meters      []meterView       `json:"meters,omitempty"`
	Shades      int               `json:"shades"`
	SignalsSeq  uint64            `json:"signals_seq"`
	Note        string            `json:"note,omitempty"`
}

type watchdogView struct {
	Alive          bool             `json:"alive"`
	UptimeHours    float64          `json:"uptime_hours"`
	AlarmMode      string           `json:"alarm_mode"`
	AlarmTriggered bool             `json:"alarm_triggered"`
	FallbackActive bool             `json:"fallback_active"`
	Ready          bool             `json:"ready"`
	Streams        map[string]int64 `json:"stream_age_seconds,omitempty"`
}

type electricityView struct {
	Name             string    `json:"name"`
	Voltages         []float64 `json:"voltages"`
	Currents         []float64 `json:"currents"`
	Frequency        float64   `json:"frequency"`
	VoltageImbalance float64   `json:"voltage_imbalance_percent"`
	CurrentImbalance float64   `json:"current_imbalance_percent"`
	AlarmActive      bool      `json:"alarm_active"`
	ActiveAlarms     []string  `json:"active_alarms,omitempty"`
}

type meterView struct {
	Name      string             `json:"name"`
	Available bool               `json:"available"`
	Readings  map[string]float64 `json:"readings,omitempty"`
}

func (t *tools) overview(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, overviewOutput, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	status, err := t.client.Status(reqCtx)
	if err != nil {
		return nil, overviewOutput{}, err
	}
	state, err := t.client.State(reqCtx, t.connectionID)
	if err != nil {
		return nil, overviewOutput{}, err
	}

	out := overviewOutput{
		TopicPrefix: status.TopicPrefix,
		ReadOnly:    status.ReadOnly,
		Counts:      state.Summary,
		Shades:      state.Summary.Shades,
		SignalsSeq:  status.Seq,
	}
	if status.Points == 0 {
		out.Note = "no PLC points seen yet; check the plugin's topic prefix and that its broker connection is up"
	}

	if w := state.Watchdog; w != nil {
		view := &watchdogView{
			Alive:          w.Alive,
			UptimeHours:    float64(w.UptimeS) / 3600,
			AlarmMode:      w.AlarmMode,
			AlarmTriggered: w.AlarmTriggered,
			FallbackActive: w.FallbackActive,
			Ready:          w.Ready,
		}
		if len(w.Streams) > 0 {
			view.Streams = make(map[string]int64, len(w.Streams))
			for k, s := range w.Streams {
				view.Streams[k] = s.AgeS
			}
		}
		out.Watchdog = view
	}

	for _, e := range state.Electricity {
		out.Electricity = append(out.Electricity, electricityView{
			Name:             e.Name,
			Voltages:         []float64{e.Phases[0].Voltage, e.Phases[1].Voltage, e.Phases[2].Voltage},
			Currents:         []float64{e.Phases[0].Current, e.Phases[1].Current, e.Phases[2].Current},
			Frequency:        e.Frequency,
			VoltageImbalance: e.VoltageImbalance,
			CurrentImbalance: e.CurrentImbalance,
			AlarmActive:      e.AlarmActive,
			ActiveAlarms:     e.ActiveAlarms,
		})
	}
	for _, m := range state.Meters {
		out.Meters = append(out.Meters, meterView{Name: m.Name, Available: m.Available, Readings: m.Readings})
	}
	return nil, out, nil
}
