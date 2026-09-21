package mqttc

import (
	"container/list"
	"sync"
	"time"
)

// Defaults for the per-topic log. They are deliberately modest: the log exists
// to answer "what did this one topic do recently", not to be a database.
const (
	// DefaultTopicLogEntries is how many messages are kept per topic.
	DefaultTopicLogEntries = 100
	// MaxTopicLogEntries caps what a spec may ask to keep per topic, for the
	// same reason MaxHistorySize exists and more sharply: this ring is
	// allocated once per topic seen, so the cost is this number multiplied by
	// however many topics the broker carries. The byte budget below bounds
	// payloads held, not the rings themselves.
	MaxTopicLogEntries = 10_000
	// DefaultTopicLogBudget is the total payload budget across every topic.
	// Reaching it evicts whole topics, least-recently-updated first.
	DefaultTopicLogBudget = 32 << 20
)

// maxTopicLogPayload caps one entry. The global History keeps 64 KiB because
// it holds one ring; this holds one ring per topic, so a large payload
// arriving on a thousand topics would otherwise be the whole budget by itself.
const maxTopicLogPayload = 8 << 10

// TopicLog keeps a bounded ring of recent messages for each topic separately,
// which is what a timeline, a diff against the previous message and a chart of
// a numeric field all need. The global History cannot answer those: it is one
// ring shared by every topic, so a busy neighbour evicts a quiet topic's past
// long before that topic has changed twice.
//
// Memory is bounded two ways at once, because either alone is escapable. The
// per-topic ring bounds a single chatty topic, and the byte budget bounds the
// number of topics — a broker with two hundred thousand topics would otherwise
// hold two hundred thousand rings, each individually within its limit.
type TopicLog struct {
	mu       sync.Mutex
	perTopic int
	budget   int64
	bytes    int64

	rings map[string]*list.Element
	// order is most-recently-updated first, so eviction takes from the back.
	order *list.List

	evicted uint64
}

type topicRing struct {
	topic  string
	buf    []Message
	next   int
	filled bool
	bytes  int64
}

// NewTopicLog returns a log keeping at most entries messages per topic within
// a total payload budget in bytes. Zero or negative values take the defaults.
func NewTopicLog(entries int, budget int64) *TopicLog {
	if entries <= 0 {
		entries = DefaultTopicLogEntries
	}
	if entries > MaxTopicLogEntries {
		entries = MaxTopicLogEntries
	}
	if budget <= 0 {
		budget = DefaultTopicLogBudget
	}
	return &TopicLog{
		perTopic: entries,
		budget:   budget,
		rings:    make(map[string]*list.Element),
		order:    list.New(),
	}
}

// Add records a message against its topic.
func (l *TopicLog) Add(m Message) {
	if len(m.Payload) > maxTopicLogPayload {
		trimmed := make([]byte, maxTopicLogPayload)
		copy(trimmed, m.Payload)
		m.Payload = trimmed
		m.Truncated = true
	} else {
		m.Payload = append([]byte(nil), m.Payload...)
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	el, ok := l.rings[m.Topic]
	if !ok {
		r := &topicRing{topic: m.Topic, buf: make([]Message, l.perTopic)}
		el = l.order.PushFront(r)
		l.rings[m.Topic] = el
	} else {
		l.order.MoveToFront(el)
	}

	r := el.Value.(*topicRing)
	// Once the ring has wrapped every slot holds a message, and the one about
	// to be overwritten stops counting against the budget. Before it has
	// wrapped, the slot at next has never been written and there is nothing to
	// subtract — which is exactly what filled records.
	if r.filled {
		size := int64(len(r.buf[r.next].Payload))
		r.bytes -= size
		l.bytes -= size
	}
	r.buf[r.next] = m
	size := int64(len(m.Payload))
	r.bytes += size
	l.bytes += size

	r.next = (r.next + 1) % len(r.buf)
	if r.next == 0 {
		r.filled = true
	}

	l.evictLocked()
}

// evictLocked drops whole topics from the back of the order list until the
// budget is met. A topic is the unit of eviction rather than a message: half a
// topic's history is a timeline with an invisible hole in it, which is worse
// than an honest absence.
func (l *TopicLog) evictLocked() {
	for l.bytes > l.budget {
		back := l.order.Back()
		if back == nil {
			return
		}
		// Never evict the topic just written, or a single topic larger than
		// the whole budget would loop forever dropping and re-adding itself.
		if l.order.Len() == 1 {
			return
		}
		r := back.Value.(*topicRing)
		l.bytes -= r.bytes
		l.order.Remove(back)
		delete(l.rings, r.topic)
		l.evicted++
	}
}

// Entries returns up to limit messages for a topic, oldest first, which is the
// order a timeline and a chart both read in. A limit of zero returns them all.
func (l *TopicLog) Entries(topic string, limit int) []Message {
	l.mu.Lock()
	defer l.mu.Unlock()

	el, ok := l.rings[topic]
	if !ok {
		return nil
	}
	l.order.MoveToFront(el)
	r := el.Value.(*topicRing)

	out := r.chronological()
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	// The caller gets its own payloads: these are handed to JSON encoders and
	// to plugins, and the ring keeps overwriting behind them.
	cp := make([]Message, len(out))
	for i, m := range out {
		m.Payload = append([]byte(nil), m.Payload...)
		cp[i] = m
	}
	return cp
}

// chronological returns the ring's contents oldest first. Caller holds the lock.
func (r *topicRing) chronological() []Message {
	if !r.filled {
		return r.buf[:r.next]
	}
	out := make([]Message, 0, len(r.buf))
	out = append(out, r.buf[r.next:]...)
	out = append(out, r.buf[:r.next]...)
	return out
}

// Previous returns the message before the most recent one on a topic, which is
// what a diff compares against. It reports false when the topic has been seen
// only once — there is nothing to diff against, and returning the current
// message would render as "no changes" and be read as "nothing happened".
func (l *TopicLog) Previous(topic string) (Message, bool) {
	entries := l.Entries(topic, 2)
	if len(entries) < 2 {
		return Message{}, false
	}
	return entries[0], true
}

// Topics returns how many topics currently hold history.
func (l *TopicLog) Topics() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.rings)
}

// TopicLogStats is the memory accounting the UI shows so that a user setting a
// budget can see what it is doing.
type TopicLogStats struct {
	Topics        int    `json:"topics"`
	Bytes         int64  `json:"bytes"`
	Budget        int64  `json:"budget"`
	EntriesPer    int    `json:"entriesPerTopic"`
	TopicsEvicted uint64 `json:"topicsEvicted"`
}

// Stats reports current usage against the budget.
func (l *TopicLog) Stats() TopicLogStats {
	l.mu.Lock()
	defer l.mu.Unlock()
	return TopicLogStats{
		Topics:        len(l.rings),
		Bytes:         l.bytes,
		Budget:        l.budget,
		EntriesPer:    l.perTopic,
		TopicsEvicted: l.evicted,
	}
}

// Clear drops every topic's history.
func (l *TopicLog) Clear() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rings = make(map[string]*list.Element)
	l.order.Init()
	l.bytes = 0
}

// Span reports the time range a topic's history covers. It is what the UI uses
// to size a timeline before fetching any entries.
func (l *TopicLog) Span(topic string) (first, last time.Time, count int, ok bool) {
	entries := l.Entries(topic, 0)
	if len(entries) == 0 {
		return time.Time{}, time.Time{}, 0, false
	}
	return entries[0].ReceivedAt, entries[len(entries)-1].ReceivedAt, len(entries), true
}
