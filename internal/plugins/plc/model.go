package plc

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// Kind classifies one addressable point on the PLC.
type Kind string

const (
	KindInput       Kind = "input"
	KindOutput      Kind = "output"
	KindTemperature Kind = "temperature"
)

// Point is a single digital input, digital output or temperature channel.
// The PLC publishes these by numeric address under a name like "DI-31-5", and
// separately publishes human metadata keyed by that same name; Label, Location
// and the fields below it are filled in from that second stream.
type Point struct {
	ConnectionID string    `json:"connectionId"`
	Topic        string    `json:"topic"`
	Kind         Kind      `json:"kind"`
	Address      int       `json:"address"`
	Name         string    `json:"name"`
	Device       string    `json:"device,omitempty"`
	Bool         *bool     `json:"bool,omitempty"`
	Number       *float64  `json:"number,omitempty"`
	UpdatedAt    time.Time `json:"updatedAt"`

	Label      string `json:"label,omitempty"`
	Location   string `json:"location,omitempty"`
	SensorType string `json:"sensorType,omitempty"`
	AlarmZone  bool   `json:"alarmZone,omitempty"`
	State      string `json:"state,omitempty"`
}

// Light is one DALI ballast.
type Light struct {
	ConnectionID string    `json:"connectionId"`
	Topic        string    `json:"topic"`
	Address      int       `json:"address"`
	Name         string    `json:"name"`
	Device       string    `json:"device,omitempty"`
	Status       int       `json:"status"`
	ActualLevel  int       `json:"actualLevel"`
	MinLevel     int       `json:"minLevel"`
	MaxLevel     int       `json:"maxLevel"`
	FadeTime     int       `json:"fadeTime"`
	FadeRate     int       `json:"fadeRate"`
	LastCommand  string    `json:"lastCommand,omitempty"`
	Error        string    `json:"error,omitempty"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// Percent renders the DALI arc power level as a rough percentage. DALI's
// dimming curve is logarithmic, so this is for sorting and bar widths in the
// UI, not for anything that needs to be photometrically true.
func (l Light) Percent() int {
	if l.ActualLevel <= 0 {
		return 0
	}
	if l.ActualLevel >= daliMaxLevel {
		return 100
	}
	return int(float64(l.ActualLevel)/daliMaxLevel*100 + 0.5)
}

// daliMaxLevel is the highest arc power level DALI defines.
const daliMaxLevel = 254

// Shade is a blind or roller shutter, addressed by slug rather than number.
type Shade struct {
	ConnectionID string    `json:"connectionId"`
	Topic        string    `json:"topic"`
	Slug         string    `json:"slug"`
	Device       string    `json:"device,omitempty"`
	Position     int       `json:"position"`
	LastCommand  string    `json:"lastCommand,omitempty"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// Actuator covers the branches that each hold a handful of named devices —
// door locks, water valves, switched relays. Their payloads are not uniform,
// so the parsed state is best-effort and the raw JSON is kept for display.
type Actuator struct {
	ConnectionID string          `json:"connectionId"`
	Topic        string          `json:"topic"`
	Group        string          `json:"group"`
	Slug         string          `json:"slug"`
	State        string          `json:"state,omitempty"`
	Raw          json.RawMessage `json:"raw,omitempty"`
	UpdatedAt    time.Time       `json:"updatedAt"`
}

// Phase is one leg of the three-phase supply.
type Phase struct {
	Voltage float64 `json:"voltage"`
	Current float64 `json:"current"`
}

// Electricity is the derived three-phase measurement for one metering point.
type Electricity struct {
	ConnectionID     string    `json:"connectionId"`
	Topic            string    `json:"topic"`
	Name             string    `json:"name"`
	Phases           [3]Phase  `json:"phases"`
	Frequency        float64   `json:"frequency"`
	VoltageImbalance float64   `json:"voltageImbalance"`
	CurrentImbalance float64   `json:"currentImbalance"`
	AlarmActive      bool      `json:"alarmActive"`
	ActiveAlarms     []string  `json:"activeAlarms,omitempty"`
	UpdatedAt        time.Time `json:"updatedAt"`
}

// Stream is one of the PLC's internal publish counters, with the age of the
// most recent item. A stream whose age climbs is the earliest sign that part
// of the PLC has stopped talking while the rest carries on.
type Stream struct {
	Count int64 `json:"count"`
	AgeS  int64 `json:"ageS"`
}

// Watchdog is the PLC's own view of its health and alarm state.
type Watchdog struct {
	ConnectionID        string            `json:"connectionId"`
	Topic               string            `json:"topic"`
	UptimeS             int64             `json:"uptimeS"`
	Alive               bool              `json:"alive"`
	MQTTConnected       bool              `json:"mqttConnected"`
	BackupMQTTConnected bool              `json:"backupMqttConnected"`
	DirectHAMode        bool              `json:"directHaMode"`
	FallbackActive      bool              `json:"fallbackActive"`
	AlarmMode           string            `json:"alarmMode"`
	AlarmTriggered      bool              `json:"alarmTriggered"`
	Ready               bool              `json:"ready"`
	PersistentValid     bool              `json:"persistentValid"`
	Streams             map[string]Stream `json:"streams,omitempty"`
	UpdatedAt           time.Time         `json:"updatedAt"`
}

// Meter is an M-Bus meter: a flat set of numeric readings plus availability.
type Meter struct {
	ConnectionID string             `json:"connectionId"`
	Topic        string             `json:"topic"`
	Name         string             `json:"name"`
	Available    bool               `json:"available"`
	Readings     map[string]float64 `json:"readings,omitempty"`
	UpdatedAt    time.Time          `json:"updatedAt"`
}

// Bridge is the health of whatever gateway publishes on the PLC's behalf.
type Bridge struct {
	ConnectionID string    `json:"connectionId"`
	Topic        string    `json:"topic"`
	Source       string    `json:"source,omitempty"`
	Version      string    `json:"version,omitempty"`
	UptimeHours  float64   `json:"uptimeHours"`
	RSSMB        float64   `json:"rssMb"`
	FreeMemMB    float64   `json:"freeMemMb"`
	LoadAvg      []float64 `json:"loadAvg,omitempty"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// sensorMeta is the human description of a point, published on its own branch
// and keyed by the point's PLC name.
type sensorMeta struct {
	State     string `json:"state"`
	Type      string `json:"type"`
	Location  string `json:"location"`
	Name      string `json:"name"`
	AlarmZone bool   `json:"alarm_zone"`
	Timestamp int64  `json:"timestamp"`
}

// envelope is the header every addressed PLC payload shares.
type envelope struct {
	Timestamp int64           `json:"timestamp"`
	Device    string          `json:"device"`
	Address   int             `json:"address"`
	Type      string          `json:"type"`
	Name      string          `json:"name"`
	Value     json.RawMessage `json:"value"`
}

// route is a PLC topic broken into the parts the registry dispatches on.
type route struct {
	// Branch is the first segment after the prefix, e.g. "digital" or "dali".
	Branch string
	// Sub is the second segment where the branch has one, e.g. "input".
	Sub string
	// Leaf is the remainder: an address, a slug or a sensor name.
	Leaf string
}

// parseRoute splits a topic below the configured prefix. It returns false for
// anything outside the prefix, including the prefix itself.
func parseRoute(prefix, topic string) (route, bool) {
	rest, ok := trimPrefix(prefix, topic)
	if !ok {
		return route{}, false
	}
	parts := strings.Split(rest, "/")
	switch len(parts) {
	case 0:
		return route{}, false
	case 1:
		return route{Branch: parts[0]}, parts[0] != ""
	case 2:
		return route{Branch: parts[0], Leaf: parts[1]}, true
	default:
		return route{Branch: parts[0], Sub: parts[1], Leaf: strings.Join(parts[2:], "/")}, true
	}
}

// trimPrefix removes "<prefix>/" from a topic, reporting whether it was there.
func trimPrefix(prefix, topic string) (string, bool) {
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return topic, topic != ""
	}
	if !strings.HasPrefix(topic, prefix+"/") {
		return "", false
	}
	rest := strings.TrimPrefix(topic, prefix+"/")
	if rest == "" {
		return "", false
	}
	return rest, true
}

// msTime converts the PLC's epoch-millisecond stamps, falling back to the
// receive time when a payload carries none.
func msTime(ms int64, fallback time.Time) time.Time {
	if ms <= 0 {
		return fallback
	}
	return time.UnixMilli(ms)
}

// atoiSafe parses an address segment, returning -1 when it is not a number.
// Some branches are addressed by slug, and those must not become address 0.
func atoiSafe(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return -1
	}
	return n
}
