package mqttc

import "sync"

// DefaultHistorySize is how many recent messages a connection keeps when the
// spec does not say otherwise.
const DefaultHistorySize = 2000

// maxHistoryPayload caps the payload kept per history entry. The full payload
// is still available from the topic tree's last-known value.
const maxHistoryPayload = 64 * 1024

// History is a fixed-size ring of the most recent messages on a connection.
// It is the backing store for the live message stream in the UI.
type History struct {
	mu     sync.RWMutex
	buf    []Message
	next   int
	filled bool
}

// NewHistory returns a ring holding at most size messages.
func NewHistory(size int) *History {
	if size <= 0 {
		size = DefaultHistorySize
	}
	return &History{buf: make([]Message, size)}
}

// Add appends a message, overwriting the oldest entry once full.
func (h *History) Add(m Message) {
	if len(m.Payload) > maxHistoryPayload {
		trimmed := make([]byte, maxHistoryPayload)
		copy(trimmed, m.Payload)
		m.Payload = trimmed
	} else {
		m.Payload = append([]byte(nil), m.Payload...)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	h.buf[h.next] = m
	h.next = (h.next + 1) % len(h.buf)
	if h.next == 0 {
		h.filled = true
	}
}

// Recent returns up to limit messages, newest first. When filter is a
// non-empty MQTT topic filter, only matching messages are returned.
func (h *History) Recent(limit int, filter string) []Message {
	if limit <= 0 {
		limit = 200
	}
	h.mu.RLock()
	defer h.mu.RUnlock()

	count := h.next
	if h.filled {
		count = len(h.buf)
	}
	out := make([]Message, 0, min(limit, count))
	for i := 0; i < count && len(out) < limit; i++ {
		// Walk backwards from the most recently written slot.
		idx := (h.next - 1 - i + len(h.buf)*2) % len(h.buf)
		m := h.buf[idx]
		if m.Topic == "" {
			continue
		}
		if filter != "" && !MatchFilter(filter, m.Topic) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// Len reports how many messages are currently retained.
func (h *History) Len() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.filled {
		return len(h.buf)
	}
	return h.next
}

// Clear drops all retained messages.
func (h *History) Clear() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.buf = make([]Message, len(h.buf))
	h.next = 0
	h.filled = false
}
