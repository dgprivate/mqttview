// Package recorder writes MQTT messages to the database for the connections
// that ask for it.
//
// The whole design is about staying off the message path. A message arriving
// from the broker is delivered to observers synchronously, so anything slow
// done there is back-pressure on the MQTT client itself — and a disk write is
// slow. So the observer does one channel send and returns, and everything
// else happens on a goroutine of its own.
package recorder

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dgprivate/mqttview/internal/mqttc"
	"github.com/dgprivate/mqttview/internal/store"
)

const (
	// queueDepth is how many messages may be waiting to be written. Deep
	// enough to absorb a burst, shallow enough that a stalled disk is noticed
	// as dropped messages rather than as memory disappearing.
	queueDepth = 4096
	// batchSize and flushEvery decide when a batch is written: whichever comes
	// first. A busy broker fills the batch; a quiet one is written on the
	// timer rather than being held indefinitely.
	batchSize  = 500
	flushEvery = time.Second
	// pruneEvery is how often retention is enforced. On a timer rather than
	// after each batch: the delete is a scan, and doing it per batch would
	// spend more time pruning than recording.
	pruneEvery = 5 * time.Minute
	// maxPayload caps one recorded payload. A camera publishing frames would
	// otherwise fill a disk in an afternoon.
	maxPayload = 64 * 1024
)

// Recorder writes messages to disk for the connections that want it.
type Recorder struct {
	db  *store.Store
	log *slog.Logger

	queue chan store.RecordedMessage

	// wanted is the set of connections recording, and how much each keeps.
	// Read on the message path, so it is behind its own lock rather than
	// being consulted through the store.
	mu     sync.RWMutex
	wanted map[string]int

	dropped atomic.Uint64
	written atomic.Uint64

	stop chan struct{}
	done chan struct{}
}

// New returns a recorder. Start must be called before it writes anything.
func New(db *store.Store, log *slog.Logger) *Recorder {
	if log == nil {
		log = slog.Default()
	}
	return &Recorder{
		db:     db,
		log:    log,
		queue:  make(chan store.RecordedMessage, queueDepth),
		wanted: map[string]int{},
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
}

// Configure sets which connections record and how much each keeps. Calling it
// again replaces the whole set, which is what happens when a connection is
// saved.
func (r *Recorder) Configure(connectionID string, record bool, keep int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if record {
		r.wanted[connectionID] = keep
	} else {
		delete(r.wanted, connectionID)
	}
}

// Recording reports whether a connection is being recorded.
func (r *Recorder) Recording(connectionID string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.wanted[connectionID]
	return ok
}

// Observe is the message-path half. It must not block and must not be slow:
// the manager delivers to observers synchronously, so time spent here is time
// the MQTT client is not reading its socket.
func (r *Recorder) Observe(m mqttc.Message) {
	r.mu.RLock()
	_, want := r.wanted[m.ConnectionID]
	r.mu.RUnlock()
	if !want {
		return
	}

	payload := m.Payload
	if len(payload) > maxPayload {
		payload = payload[:maxPayload]
	}
	rec := store.RecordedMessage{
		ConnectionID: m.ConnectionID,
		Topic:        m.Topic,
		// Copied: the manager hands the same slice to every observer and the
		// writer goroutine reads it long after this returns.
		Payload:    append([]byte(nil), payload...),
		QoS:        m.QoS,
		Retain:     m.Retain,
		ReceivedAt: m.ReceivedAt,
	}

	select {
	case r.queue <- rec:
	default:
		// Dropping is the right failure. Blocking here would stall the MQTT
		// client, so a slow disk would stop mqttview reading its socket and
		// the broker would start disconnecting it — a recording feature
		// taking the live view down with it.
		r.dropped.Add(1)
	}
}

// Start begins writing. It returns immediately; the work is on its own
// goroutine.
func (r *Recorder) Start(ctx context.Context) {
	go r.run(ctx)
}

func (r *Recorder) run(ctx context.Context) {
	defer close(r.done)

	batch := make([]store.RecordedMessage, 0, batchSize)
	flush := time.NewTicker(flushEvery)
	defer flush.Stop()
	prune := time.NewTicker(pruneEvery)
	defer prune.Stop()

	write := func() {
		if len(batch) == 0 {
			return
		}
		if err := r.db.AppendRecorded(batch); err != nil {
			// Recording is not what mqttview is for; failing to write is
			// worth saying and not worth stopping over.
			r.log.Error("recording messages failed", "messages", len(batch), "error", err)
		} else {
			r.written.Add(uint64(len(batch)))
		}
		batch = batch[:0]
	}

	for {
		select {
		case <-ctx.Done():
			r.drain(&batch, write)
			return
		case <-r.stop:
			r.drain(&batch, write)
			return
		case rec := <-r.queue:
			batch = append(batch, rec)
			if len(batch) >= batchSize {
				write()
			}
		case <-flush.C:
			write()
		case <-prune.C:
			r.prune()
		}
	}
}

// drain writes what is queued before stopping, so a clean shutdown does not
// throw away a second of messages.
func (r *Recorder) drain(batch *[]store.RecordedMessage, write func()) {
	for {
		select {
		case rec := <-r.queue:
			*batch = append(*batch, rec)
			if len(*batch) >= batchSize {
				write()
			}
		default:
			write()
			return
		}
	}
}

func (r *Recorder) prune() {
	r.mu.RLock()
	wanted := make(map[string]int, len(r.wanted))
	for id, keep := range r.wanted {
		wanted[id] = keep
	}
	r.mu.RUnlock()

	for id, keep := range wanted {
		removed, err := r.db.PruneRecorded(id, keep)
		if err != nil {
			r.log.Error("pruning recordings failed", "connection", id, "error", err)
			continue
		}
		if removed > 0 {
			r.log.Debug("pruned recordings", "connection", id, "removed", removed)
		}
	}
}

// Stop flushes and shuts down.
func (r *Recorder) Stop() {
	select {
	case <-r.stop:
		return // already stopping
	default:
		close(r.stop)
	}
	<-r.done
}

// Stats reports what the recorder has done, including what it could not keep.
//
// Dropped is reported rather than hidden: a recording with a hole in it that
// nobody is told about is worse than no recording, because it will be read as
// evidence that nothing happened.
type Stats struct {
	Written uint64 `json:"written"`
	Dropped uint64 `json:"dropped"`
}

// Stats returns the counters.
func (r *Recorder) Stats() Stats {
	return Stats{Written: r.written.Load(), Dropped: r.dropped.Load()}
}
