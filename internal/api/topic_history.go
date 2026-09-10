package api

import (
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dgprivate/mqttview/internal/httpx"
	"github.com/dgprivate/mqttview/internal/mqttc"
)

// historyEntry is one message as the timeline renders it. The payload is sent
// as text when it is valid UTF-8 and as base64 otherwise, rather than letting
// a JSON encoder turn arbitrary bytes into replacement characters and then
// showing that to somebody debugging a binary protocol.
type historyEntry struct {
	Seq        uint64    `json:"seq"`
	ReceivedAt time.Time `json:"receivedAt"`
	Payload    string    `json:"payload"`
	Base64     bool      `json:"base64,omitempty"`
	Size       int       `json:"size"`
	QoS        byte      `json:"qos"`
	Retain     bool      `json:"retain"`
	Truncated  bool      `json:"truncated,omitempty"`
}

func toHistoryEntry(m mqttc.Message) historyEntry {
	e := historyEntry{
		Seq:        m.Seq,
		ReceivedAt: m.ReceivedAt,
		Size:       len(m.Payload),
		QoS:        m.QoS,
		Retain:     m.Retain,
		Truncated:  m.Truncated,
	}
	if utf8.Valid(m.Payload) {
		e.Payload = string(m.Payload)
	} else {
		e.Payload = base64.StdEncoding.EncodeToString(m.Payload)
		e.Base64 = true
	}
	return e
}

// requireTopic pulls the topic parameter, which every handler here needs.
func requireTopic(w http.ResponseWriter, r *http.Request) (string, bool) {
	topic := r.URL.Query().Get("topic")
	if topic == "" {
		httpx.WriteError(w, http.StatusBadRequest, "the topic parameter is required")
		return "", false
	}
	return topic, true
}

// handleTopicHistory returns one topic's recent messages, oldest first.
func (s *Server) handleTopicHistory(w http.ResponseWriter, r *http.Request) {
	c, ok := s.conn(w, r)
	if !ok {
		return
	}
	topic, ok := requireTopic(w, r)
	if !ok {
		return
	}

	entries := c.TopicLog().Entries(topic, intParam(r, "limit", 0))
	out := make([]historyEntry, len(entries))
	for i, m := range entries {
		out[i] = toHistoryEntry(m)
	}

	resp := map[string]any{"topic": topic, "entries": out}
	if first, last, count, ok := c.TopicLog().Span(topic); ok {
		resp["span"] = map[string]any{"first": first, "last": last, "count": count}
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// handleTopicDiff returns the current message and the one before it. The
// comparison itself is the browser's: rendering a diff is a display decision,
// and inventing a diff format here would fix it for every future client.
func (s *Server) handleTopicDiff(w http.ResponseWriter, r *http.Request) {
	c, ok := s.conn(w, r)
	if !ok {
		return
	}
	topic, ok := requireTopic(w, r)
	if !ok {
		return
	}

	entries := c.TopicLog().Entries(topic, 2)
	if len(entries) == 0 {
		httpx.WriteErrorf(w, http.StatusNotFound, "no history for topic %q", topic)
		return
	}

	resp := map[string]any{
		"topic":   topic,
		"current": toHistoryEntry(entries[len(entries)-1]),
	}
	// One message means there is nothing to compare against. Saying so beats
	// returning the same message twice, which renders as "no changes" and
	// reads as "nothing happened".
	if len(entries) == 2 {
		resp["previous"] = toHistoryEntry(entries[0])
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// handleTopicExport writes a topic's history as a file. CSV for a spreadsheet,
// JSON for anything that will be parsed again.
func (s *Server) handleTopicExport(w http.ResponseWriter, r *http.Request) {
	c, ok := s.conn(w, r)
	if !ok {
		return
	}
	topic, ok := requireTopic(w, r)
	if !ok {
		return
	}

	format := strings.ToLower(r.URL.Query().Get("format"))
	if format == "" {
		format = "csv"
	}
	if format != "csv" && format != "json" {
		httpx.WriteError(w, http.StatusBadRequest, `format must be "csv" or "json"`)
		return
	}

	entries := c.TopicLog().Entries(topic, intParam(r, "limit", 0))
	if len(entries) == 0 {
		httpx.WriteErrorf(w, http.StatusNotFound, "no history for topic %q", topic)
		return
	}

	w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(exportFilename(topic, format)))

	if format == "json" {
		out := make([]historyEntry, len(entries))
		for i, m := range entries {
			out[i] = toHistoryEntry(m)
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"topic": topic, "entries": out})
		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	cw := csv.NewWriter(w)
	// A header row, because the first thing anybody does with this is open it
	// in a spreadsheet and wonder which column is which.
	_ = cw.Write([]string{"received_at", "topic", "qos", "retain", "size", "encoding", "payload"})
	for _, m := range entries {
		e := toHistoryEntry(m)
		encoding := "text"
		if e.Base64 {
			encoding = "base64"
		}
		if e.Truncated {
			encoding += "; truncated"
		}
		_ = cw.Write([]string{
			m.ReceivedAt.Format(time.RFC3339Nano),
			topic,
			strconv.Itoa(int(m.QoS)),
			strconv.FormatBool(m.Retain),
			strconv.Itoa(e.Size),
			encoding,
			e.Payload,
		})
	}
	cw.Flush()
}

// exportFilename turns a topic into something a filesystem will accept, since
// a topic may contain slashes and very little else is guaranteed about it.
func exportFilename(topic, format string) string {
	safe := strings.Map(func(rune_ rune) rune {
		switch {
		case rune_ >= 'a' && rune_ <= 'z', rune_ >= 'A' && rune_ <= 'Z',
			rune_ >= '0' && rune_ <= '9', rune_ == '-', rune_ == '_':
			return rune_
		default:
			return '-'
		}
	}, topic)
	safe = strings.Trim(safe, "-")
	if safe == "" {
		safe = "topic"
	}
	if len(safe) > 80 {
		safe = safe[:80]
	}
	return fmt.Sprintf("%s-%s.%s", safe, time.Now().UTC().Format("20060102-150405"), format)
}

// seriesPoint is one sample on a chart.
type seriesPoint struct {
	At    time.Time `json:"t"`
	Value float64   `json:"v"`
}

// handleTopicSeries extracts a numeric series from a topic's history so the UI
// can chart it. The field is a dotted path into the JSON payload; an empty
// field means the payload is itself the number.
func (s *Server) handleTopicSeries(w http.ResponseWriter, r *http.Request) {
	c, ok := s.conn(w, r)
	if !ok {
		return
	}
	topic, ok := requireTopic(w, r)
	if !ok {
		return
	}
	field := r.URL.Query().Get("field")

	var since time.Time
	if raw := r.URL.Query().Get("since"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, `since must be a duration such as "15m" or "24h"`)
			return
		}
		since = time.Now().Add(-d)
	}

	entries := c.TopicLog().Entries(topic, 0)
	points := make([]seriesPoint, 0, len(entries))
	var skipped int
	for _, m := range entries {
		if !since.IsZero() && m.ReceivedAt.Before(since) {
			continue
		}
		v, ok := numericAt(m.Payload, field)
		if !ok {
			// A payload that is not a number at that path is not an error:
			// a topic can carry a status string between readings.
			skipped++
			continue
		}
		points = append(points, seriesPoint{At: m.ReceivedAt, Value: v})
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"topic":   topic,
		"field":   field,
		"points":  points,
		"skipped": skipped,
	})
}

// handleTopicFields lists the numeric paths found in a topic's recent
// payloads, so the UI can offer a chooser rather than asking somebody to type
// a JSON path from memory.
func (s *Server) handleTopicFields(w http.ResponseWriter, r *http.Request) {
	c, ok := s.conn(w, r)
	if !ok {
		return
	}
	topic, ok := requireTopic(w, r)
	if !ok {
		return
	}

	// Only the most recent payloads are inspected: a field that stopped
	// appearing a hundred messages ago is not one to offer a chart of.
	const inspect = 20
	seen := map[string]bool{}
	for _, m := range c.TopicLog().Entries(topic, inspect) {
		collectNumericPaths(m.Payload, seen)
	}

	fields := make([]string, 0, len(seen))
	for path := range seen {
		fields = append(fields, path)
	}
	sort.Strings(fields)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"topic": topic, "fields": fields})
}

// numericAt reads a number out of a payload at a dotted path. An empty path
// means the payload is the number itself, which is how most sensors publish.
func numericAt(payload []byte, field string) (float64, bool) {
	text := strings.TrimSpace(string(payload))
	if field == "" {
		return parseNumber(text)
	}

	var doc any
	if err := json.Unmarshal(payload, &doc); err != nil {
		return 0, false
	}
	for _, key := range strings.Split(field, ".") {
		obj, ok := doc.(map[string]any)
		if !ok {
			return 0, false
		}
		doc, ok = obj[key]
		if !ok {
			return 0, false
		}
	}
	switch v := doc.(type) {
	case float64:
		return finite(v)
	case string:
		return parseNumber(v)
	case bool:
		// A boolean charts as 0 or 1, which is what makes a switch's history
		// legible on the same axis as everything else.
		if v {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// parseNumber also accepts the on/off spellings that MQTT devices publish
// instead of a number, so a relay's history charts alongside a temperature.
func parseNumber(text string) (float64, bool) {
	if v, err := strconv.ParseFloat(text, 64); err == nil {
		return finite(v)
	}
	switch strings.ToUpper(strings.TrimSpace(text)) {
	case "ON", "TRUE", "OPEN", "OPENING":
		return 1, true
	case "OFF", "FALSE", "CLOSED", "CLOSING":
		return 0, true
	}
	return 0, false
}

// finite rejects NaN and the infinities: they are not JSON numbers, and
// encoding one would fail the whole response rather than the one sample.
func finite(v float64) (float64, bool) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

// collectNumericPaths walks a JSON payload and records every dotted path that
// holds something chartable.
func collectNumericPaths(payload []byte, into map[string]bool) {
	text := strings.TrimSpace(string(payload))
	if _, ok := parseNumber(text); ok {
		// The empty path: the payload is itself the value.
		into[""] = true
		return
	}
	var doc any
	if err := json.Unmarshal(payload, &doc); err != nil {
		return
	}
	walkNumericPaths("", doc, into, 0)
}

// walkNumericPaths recurses into objects only. Arrays are skipped on purpose:
// an index is not a stable name, and "readings.3.value" means something
// different on the next message.
func walkNumericPaths(prefix string, doc any, into map[string]bool, depth int) {
	const maxDepth = 6
	if depth > maxDepth {
		return
	}
	obj, ok := doc.(map[string]any)
	if !ok {
		return
	}
	for key, value := range obj {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		switch v := value.(type) {
		case float64, bool:
			if _, ok := numericValue(v); ok {
				into[path] = true
			}
		case string:
			if _, ok := parseNumber(v); ok {
				into[path] = true
			}
		case map[string]any:
			walkNumericPaths(path, v, into, depth+1)
		}
	}
}

func numericValue(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return finite(t)
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// handleBrokerStats returns what the broker publishes about itself under $SYS.
//
// It reports whether the namespace is being collected at all as well as what
// arrived, because "this broker has no clients" and "this broker will not say"
// are different facts and a page of zeroes cannot tell them apart. A broker
// may publish nothing there, or deny the reserved namespace outright.
func (s *Server) handleBrokerStats(w http.ResponseWriter, r *http.Request) {
	c, ok := s.conn(w, r)
	if !ok {
		return
	}

	stats := c.BrokerStats()
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"enabled":   c.Spec().SysStats,
		"connected": c.Status().State == mqttc.StateConnected,
		"stats":     stats,
	})
}
