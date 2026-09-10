package mqttc

import (
	"strconv"
	"strings"
	"time"
)

// SysPrefix is the reserved namespace brokers publish their own statistics
// under. A subscription to "#" does not match it — that is in the spec, and
// internal/mqttc/topic.go enforces it — so reading these means asking for them
// by name.
const SysPrefix = "$SYS/"

// SysFilter is the subscription that brings the broker's statistics in.
const SysFilter = "$SYS/#"

// BrokerStats is what a broker says about itself. Every number here is the
// broker's own: mqttview does not compute rates or totals of its own to sit
// beside them, because the broker is the authority on its own load and two
// disagreeing numbers on one page is worse than one number.
//
// Brokers differ on what they publish and what they call it. The typed fields
// cover the widely-implemented Mosquitto layout; Raw carries everything that
// arrived, so a broker with its own names still shows something rather than
// an empty page.
type BrokerStats struct {
	// Available is false when nothing has arrived under $SYS at all, which
	// means either the broker publishes none or the subscription was refused.
	// It is reported rather than shown as zeroes, because a broker with no
	// clients and a broker that will not say are different facts.
	Available bool `json:"available"`

	Version string `json:"version,omitempty"`
	// Uptime is reported in seconds, as the broker publishes it.
	Uptime      int64  `json:"uptime,omitempty"`
	HasUptime   bool   `json:"hasUptime,omitempty"`
	Description string `json:"description,omitempty"`

	Clients       SysClients  `json:"clients"`
	Messages      SysMessages `json:"messages"`
	Bytes         SysBytes    `json:"bytes"`
	Subscriptions *int64      `json:"subscriptions,omitempty"`
	Retained      *int64      `json:"retained,omitempty"`
	Heap          SysHeap     `json:"heap"`

	// Load holds the broker's own moving averages, keyed as the broker names
	// them below $SYS/broker/load — "messages/received/1min" and so on.
	Load map[string]float64 `json:"load,omitempty"`

	// Raw is every $SYS topic that arrived, with the prefix stripped, exactly
	// as published. Nothing is hidden behind the typed fields above.
	Raw map[string]string `json:"raw,omitempty"`

	// UpdatedAt is the most recent timestamp across every $SYS value, so the
	// UI can say how stale the page is rather than implying it is live.
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
}

// SysClients counts connections as the broker sees them.
type SysClients struct {
	Connected    *int64 `json:"connected,omitempty"`
	Total        *int64 `json:"total,omitempty"`
	Maximum      *int64 `json:"maximum,omitempty"`
	Disconnected *int64 `json:"disconnected,omitempty"`
	Expired      *int64 `json:"expired,omitempty"`
}

// SysMessages counts messages as the broker sees them.
type SysMessages struct {
	Received *int64 `json:"received,omitempty"`
	Sent     *int64 `json:"sent,omitempty"`
	Stored   *int64 `json:"stored,omitempty"`
	Dropped  *int64 `json:"dropped,omitempty"`
}

// SysBytes counts traffic as the broker sees it.
type SysBytes struct {
	Received *int64 `json:"received,omitempty"`
	Sent     *int64 `json:"sent,omitempty"`
}

// SysHeap reports the broker's own memory use where it publishes it.
type SysHeap struct {
	Current *int64 `json:"current,omitempty"`
	Maximum *int64 `json:"maximum,omitempty"`
}

// BrokerStats reads the connection's $SYS values out of the topic tree. There
// is no second store: the tree already holds the last value of every topic,
// and a $SYS topic is a topic.
func (c *Conn) BrokerStats() BrokerStats {
	// An explicit ceiling rather than Match's default: brokers publish a few
	// dozen $SYS topics, and a page that silently dropped some of them would
	// be worse than one that admitted it could not fit them.
	const maxSysTopics = 2000
	return brokerStatsFrom(c.tree.Match(SysFilter, maxSysTopics))
}

func brokerStatsFrom(values []TopicValue) BrokerStats {
	stats := BrokerStats{
		Load: map[string]float64{},
		Raw:  map[string]string{},
	}
	if len(values) == 0 {
		return stats
	}
	stats.Available = true

	// First pass: everything as published, with nothing interpreted yet.
	for _, v := range values {
		key := strings.TrimPrefix(v.Topic, SysPrefix)
		text := strings.TrimSpace(string(v.Payload))
		stats.Raw[key] = text
		if v.UpdatedAt.After(stats.UpdatedAt) {
			stats.UpdatedAt = v.UpdatedAt
		}
		if rest, ok := strings.CutPrefix(strings.TrimPrefix(key, "broker/"), "load/"); ok {
			if f, err := strconv.ParseFloat(text, 64); err == nil {
				stats.Load[rest] = f
			}
		}
	}

	// Second pass: resolve each typed field from the raw values, taking the
	// first name that is present.
	//
	// The order matters and the alternatives are not synonyms arriving at the
	// same moment. Mosquitto publishes clients/connected and clients/active as
	// separate topics whose values differ for an instant around a connection,
	// so reading whichever happened to be walked last reported nought clients
	// while this process held a connection open. The canonical name wins; the
	// alternative is a fallback for brokers that publish only that one.
	stats.Version = stats.first("broker/version")
	stats.Description = stats.first("broker/sysdescr", "broker/description")
	if n, ok := leadingInt(stats.first("broker/uptime")); ok {
		stats.Uptime, stats.HasUptime = n, true
	}

	stats.Clients.Connected = stats.firstInt("broker/clients/connected", "broker/clients/active")
	stats.Clients.Total = stats.firstInt("broker/clients/total")
	stats.Clients.Maximum = stats.firstInt("broker/clients/maximum")
	stats.Clients.Disconnected = stats.firstInt("broker/clients/disconnected", "broker/clients/inactive")
	stats.Clients.Expired = stats.firstInt("broker/clients/expired")

	stats.Messages.Received = stats.firstInt("broker/messages/received")
	stats.Messages.Sent = stats.firstInt("broker/messages/sent")
	stats.Messages.Stored = stats.firstInt("broker/messages/stored", "broker/store/messages/count")
	stats.Messages.Dropped = stats.firstInt("broker/messages/dropped", "broker/publish/messages/dropped")

	stats.Bytes.Received = stats.firstInt("broker/bytes/received")
	stats.Bytes.Sent = stats.firstInt("broker/bytes/sent")

	stats.Subscriptions = stats.firstInt("broker/subscriptions/count")
	stats.Retained = stats.firstInt("broker/retained messages/count")
	stats.Heap.Current = stats.firstInt("broker/heap/current", "broker/heap/current size")
	stats.Heap.Maximum = stats.firstInt("broker/heap/maximum", "broker/heap/maximum size")

	if len(stats.Load) == 0 {
		stats.Load = nil
	}
	return stats
}

// first returns the value of the first key that was published.
func (b BrokerStats) first(keys ...string) string {
	for _, k := range keys {
		if v, ok := b.Raw[k]; ok {
			return v
		}
	}
	return ""
}

// firstInt returns the first key that was published *and* holds a number. A
// key present but unparseable falls through to the next candidate rather than
// shadowing it, and a value none of them can supply stays absent — a confident
// zero is a claim the broker did not make.
func (b BrokerStats) firstInt(keys ...string) *int64 {
	for _, k := range keys {
		v, ok := b.Raw[k]
		if !ok {
			continue
		}
		if n, ok := parseCounter(v); ok {
			return &n
		}
	}
	return nil
}

// parseCounter reads a counter, tolerating the brokers that publish one as a
// float.
func parseCounter(text string) (int64, bool) {
	trimmed := strings.TrimSpace(text)
	if n, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
		return n, true
	}
	if f, err := strconv.ParseFloat(trimmed, 64); err == nil {
		return int64(f), true
	}
	return 0, false
}

// leadingInt reads the number at the start of a string such as "1234 seconds".
func leadingInt(text string) (int64, bool) {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}
