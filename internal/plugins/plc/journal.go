package plc

import (
	"context"
	"sync"
	"time"
)

// Edge is one observed transition of a digital point.
//
// This is the record a PLC programmer works from: press a wall switch, read
// back which address moved and what it is called, and the specification for
// what should happen next can name the point rather than guess at it.
type Edge struct {
	// Seq is monotonic per process and is how a client resumes without gaps.
	Seq          uint64    `json:"seq"`
	ConnectionID string    `json:"connectionId"`
	Topic        string    `json:"topic"`
	Kind         Kind      `json:"kind"`
	Address      int       `json:"address"`
	Name         string    `json:"name"`
	Label        string    `json:"label,omitempty"`
	Location     string    `json:"location,omitempty"`
	SensorType   string    `json:"sensorType,omitempty"`
	AlarmZone    bool      `json:"alarmZone,omitempty"`
	From         bool      `json:"from"`
	To           bool      `json:"to"`
	At           time.Time `json:"at"`
}

// Rising reports whether the edge went from off to on, which is what a button
// press looks like.
func (e Edge) Rising() bool { return e.To && !e.From }

// defaultJournalSize is how many edges are kept. A house generates a handful
// per minute in normal use; five hundred covers a long commissioning session
// without letting an unattended instance grow without bound.
const defaultJournalSize = 500

// Journal is a bounded, sequenced log of digital edges with support for
// blocking until the next one arrives.
type Journal struct {
	mu      sync.Mutex
	entries []Edge
	seq     uint64
	size    int
	// changed is closed and replaced on every append, which wakes every
	// waiter at once without keeping a list of them.
	changed chan struct{}
}

// NewJournal returns an empty journal holding at most size entries.
func NewJournal(size int) *Journal {
	if size <= 0 {
		size = defaultJournalSize
	}
	return &Journal{size: size, changed: make(chan struct{})}
}

// Append records an edge and assigns it a sequence number.
func (j *Journal) Append(e Edge) Edge {
	j.mu.Lock()
	j.seq++
	e.Seq = j.seq
	j.entries = append(j.entries, e)
	if len(j.entries) > j.size {
		// Drop from the front, reusing the backing array by copying down so a
		// long-lived journal does not keep growing its allocation.
		copy(j.entries, j.entries[len(j.entries)-j.size:])
		j.entries = j.entries[:j.size]
	}
	close(j.changed)
	j.changed = make(chan struct{})
	j.mu.Unlock()
	return e
}

// Match selects which edges a reader cares about. A nil Match accepts all.
type Match func(Edge) bool

// Since returns edges newer than seq that satisfy match, oldest first, capped
// at limit, together with the newest sequence number in the journal. Passing
// seq 0 returns the most recent limit entries, which is what a fresh UI wants.
func (j *Journal) Since(seq uint64, limit int, match Match) ([]Edge, uint64) {
	j.mu.Lock()
	defer j.mu.Unlock()

	if limit <= 0 || limit > j.size {
		limit = j.size
	}

	var out []Edge
	for _, e := range j.entries {
		if seq > 0 && e.Seq <= seq {
			continue
		}
		if match != nil && !match(e) {
			continue
		}
		out = append(out, e)
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, j.seq
}

// Wait blocks until an edge newer than seq satisfies match, the context ends,
// or the deadline passes, then returns whatever is available. It never returns
// an error for the timeout case: an empty slice means "nothing happened yet",
// which is a normal answer to "tell me the next button pressed".
//
// The match runs inside the wait rather than on the result, so a poll filtered
// to rising edges keeps waiting through the falling ones instead of returning
// empty the moment any unrelated point moves.
func (j *Journal) Wait(ctx context.Context, seq uint64, limit int, timeout time.Duration, match Match) ([]Edge, uint64) {
	if edges, latest := j.Since(seq, limit, match); len(edges) > 0 {
		return edges, latest
	}
	if timeout <= 0 {
		return nil, j.Latest()
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		// The wake channel is taken before re-checking, so an append landing
		// between the check and the select still wakes this waiter.
		j.mu.Lock()
		wake := j.changed
		j.mu.Unlock()

		if edges, latest := j.Since(seq, limit, match); len(edges) > 0 {
			return edges, latest
		}

		select {
		case <-ctx.Done():
			return nil, j.Latest()
		case <-timer.C:
			return nil, j.Latest()
		case <-wake:
			// Re-check at the top; an append that did not match keeps us here.
		}
	}
}

// Latest returns the newest sequence number issued.
func (j *Journal) Latest() uint64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.seq
}

// Len returns how many edges are currently held.
func (j *Journal) Len() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.entries)
}
