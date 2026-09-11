package recorder

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/dgprivate/mqttview/internal/mqttc"
	"github.com/dgprivate/mqttview/internal/secrets"
	"github.com/dgprivate/mqttview/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()

	dir := t.TempDir()
	key, err := secrets.LoadOrCreateKey("", dir)
	if err != nil {
		t.Fatal(err)
	}
	box, err := secrets.New(key)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(dir, "test.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// A connection row, because recorded_messages references it.
	if err := db.SaveConnection(store.ConnectionRecord{Spec: mqttc.ConnectionSpec{
		ID: "c1", Name: "test", URL: "mqtt://localhost:1883", Version: mqttc.V311,
	}}); err != nil {
		t.Fatal(err)
	}
	return db
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func msg(topic, body string) mqttc.Message {
	return mqttc.Message{
		ConnectionID: "c1", Topic: topic, Payload: []byte(body), ReceivedAt: time.Now(),
	}
}

func TestOnlyConnectionsThatAskedForItAreRecorded(t *testing.T) {
	db := newTestStore(t)
	r := New(db, quietLogger())
	r.Configure("c1", true, 0)
	r.Start(context.Background())

	r.Observe(msg("wanted/topic", "yes"))
	r.Observe(mqttc.Message{ConnectionID: "other", Topic: "unwanted", Payload: []byte("no"), ReceivedAt: time.Now()})
	r.Stop()

	got, err := db.Recorded(store.RecordedQuery{ConnectionID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("recorded %d messages, want only the one from a recording connection", len(got))
	}
	if got[0].Topic != "wanted/topic" {
		t.Errorf("recorded %q", got[0].Topic)
	}
}

func TestTurningRecordingOffStopsIt(t *testing.T) {
	db := newTestStore(t)
	r := New(db, quietLogger())
	r.Configure("c1", true, 0)
	r.Start(context.Background())

	r.Observe(msg("a/b", "before"))
	r.Configure("c1", false, 0)
	r.Observe(msg("a/b", "after"))
	r.Stop()

	got, _ := db.Recorded(store.RecordedQuery{ConnectionID: "c1"})
	if len(got) != 1 || string(got[0].Payload) != "before" {
		t.Errorf("recorded %d messages; want only the one from while it was on", len(got))
	}
}

// A clean shutdown must not throw away the second of messages that has not
// reached the batch size yet.
func TestStoppingWritesWhatIsStillQueued(t *testing.T) {
	db := newTestStore(t)
	r := New(db, quietLogger())
	r.Configure("c1", true, 0)
	r.Start(context.Background())

	for i := range 10 {
		r.Observe(msg("a/b", string(rune('a'+i))))
	}
	r.Stop() // immediately, well inside the flush interval

	got, _ := db.Recorded(store.RecordedQuery{ConnectionID: "c1"})
	if len(got) != 10 {
		t.Errorf("recorded %d of 10 messages; the rest were dropped on shutdown", len(got))
	}
}

// Blocking here would stall the MQTT client, so a slow disk would stop
// mqttview reading its socket and the broker would disconnect it — a recording
// feature taking the live view down with it.
func TestAFullQueueDropsRatherThanBlocking(t *testing.T) {
	db := newTestStore(t)
	r := New(db, quietLogger())
	r.Configure("c1", true, 0)
	// Deliberately not started: nothing drains the queue.

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range queueDepth + 100 {
			r.Observe(msg("a/b", "x"))
		}
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Observe blocked on a full queue, which would stall the MQTT client")
	}

	if got := r.Stats().Dropped; got == 0 {
		t.Error("the queue overflowed but nothing was counted as dropped")
	}
}

// A recording with a hole nobody is told about gets read as evidence that
// nothing happened.
func TestWhatCouldNotBeKeptIsCounted(t *testing.T) {
	db := newTestStore(t)
	r := New(db, quietLogger())
	r.Configure("c1", true, 0)

	for range queueDepth + 50 {
		r.Observe(msg("a/b", "x"))
	}

	stats := r.Stats()
	if stats.Dropped < 50 {
		t.Errorf("dropped = %d, want at least the 50 that could not fit", stats.Dropped)
	}
}

func TestARetentionLimitDropsTheOldestRows(t *testing.T) {
	db := newTestStore(t)
	r := New(db, quietLogger())
	r.Configure("c1", true, 5)
	r.Start(context.Background())

	for i := range 20 {
		r.Observe(msg("a/b", string(rune('a'+i))))
	}
	r.Stop()

	// Pruning is on a timer, so it is called directly here rather than the
	// test waiting five minutes for the ticker.
	if _, err := db.PruneRecorded("c1", 5); err != nil {
		t.Fatal(err)
	}

	got, _ := db.Recorded(store.RecordedQuery{ConnectionID: "c1"})
	if len(got) != 5 {
		t.Fatalf("kept %d rows, want the retention limit of 5", len(got))
	}
	// Newest first, and the newest is the last one written.
	if string(got[0].Payload) != string(rune('a'+19)) {
		t.Errorf("newest kept row is %q, want the last one written", got[0].Payload)
	}
}

// A camera publishing frames would otherwise fill a disk in an afternoon.
func TestAnEnormousPayloadIsClipped(t *testing.T) {
	db := newTestStore(t)
	r := New(db, quietLogger())
	r.Configure("c1", true, 0)
	r.Start(context.Background())

	big := make([]byte, maxPayload+5000)
	r.Observe(mqttc.Message{ConnectionID: "c1", Topic: "camera/frame", Payload: big, ReceivedAt: time.Now()})
	r.Stop()

	got, _ := db.Recorded(store.RecordedQuery{ConnectionID: "c1"})
	if len(got) != 1 {
		t.Fatalf("recorded %d messages, want 1", len(got))
	}
	if len(got[0].Payload) != maxPayload {
		t.Errorf("stored %d bytes, want it clipped to %d", len(got[0].Payload), maxPayload)
	}
}

// The manager hands the same slice to every observer and the writer reads it
// long after Observe returns.
func TestTheCallersBufferIsNotKept(t *testing.T) {
	db := newTestStore(t)
	r := New(db, quietLogger())
	r.Configure("c1", true, 0)
	r.Start(context.Background())

	buf := []byte("original")
	r.Observe(mqttc.Message{ConnectionID: "c1", Topic: "a/b", Payload: buf, ReceivedAt: time.Now()})
	copy(buf, "OVERWRIT")
	r.Stop()

	got, _ := db.Recorded(store.RecordedQuery{ConnectionID: "c1"})
	if len(got) != 1 || string(got[0].Payload) != "original" {
		t.Errorf("stored %q, want the bytes as they were when observed", got[0].Payload)
	}
}

func TestReadingBackFiltersByTopicAndWindow(t *testing.T) {
	db := newTestStore(t)
	base := time.Now().Truncate(time.Second)

	if err := db.AppendRecorded([]store.RecordedMessage{
		{ConnectionID: "c1", Topic: "a/one", Payload: []byte("1"), ReceivedAt: base},
		{ConnectionID: "c1", Topic: "a/two", Payload: []byte("2"), ReceivedAt: base.Add(time.Minute)},
		{ConnectionID: "c1", Topic: "a/one", Payload: []byte("3"), ReceivedAt: base.Add(2 * time.Minute)},
	}); err != nil {
		t.Fatal(err)
	}

	byTopic, _ := db.Recorded(store.RecordedQuery{ConnectionID: "c1", Topic: "a/one"})
	if len(byTopic) != 2 {
		t.Errorf("filtering by topic gave %d rows, want 2", len(byTopic))
	}

	inWindow, _ := db.Recorded(store.RecordedQuery{
		ConnectionID: "c1",
		Since:        base.Add(30 * time.Second),
		Until:        base.Add(90 * time.Second),
	})
	if len(inWindow) != 1 || inWindow[0].Topic != "a/two" {
		t.Errorf("the window gave %+v, want only a/two", inWindow)
	}
}

func TestStatsDescribeWhatIsOnDisk(t *testing.T) {
	db := newTestStore(t)
	base := time.Now().Truncate(time.Second)

	if err := db.AppendRecorded([]store.RecordedMessage{
		{ConnectionID: "c1", Topic: "a", Payload: []byte("12345"), ReceivedAt: base},
		{ConnectionID: "c1", Topic: "b", Payload: []byte("123"), ReceivedAt: base.Add(time.Hour)},
	}); err != nil {
		t.Fatal(err)
	}

	stats, err := db.RecordingStats("c1")
	if err != nil {
		t.Fatal(err)
	}
	if stats.Rows != 2 {
		t.Errorf("rows = %d, want 2", stats.Rows)
	}
	if stats.Bytes != 8 {
		t.Errorf("payload bytes = %d, want 8", stats.Bytes)
	}
	if stats.Oldest == nil || !stats.Oldest.Equal(base) {
		t.Errorf("oldest = %v, want %v", stats.Oldest, base)
	}
}

func TestStatsOnAnEmptyRecordingAreZeroRatherThanAnError(t *testing.T) {
	db := newTestStore(t)

	stats, err := db.RecordingStats("c1")
	if err != nil {
		t.Fatalf("stats on an empty recording failed: %v", err)
	}
	if stats.Rows != 0 || stats.Oldest != nil {
		t.Errorf("stats = %+v, want empty", stats)
	}
}

func TestStoppingTwiceDoesNotPanic(t *testing.T) {
	r := New(newTestStore(t), quietLogger())
	r.Start(context.Background())
	r.Stop()
	r.Stop()
}

func TestRecordingReportsWhatWasConfigured(t *testing.T) {
	r := New(newTestStore(t), quietLogger())

	if r.Recording("c1") {
		t.Error("a connection nobody configured reports itself as recording")
	}
	r.Configure("c1", true, 0)
	if !r.Recording("c1") {
		t.Error("a connection configured to record does not report itself as recording")
	}
	r.Configure("c1", false, 0)
	if r.Recording("c1") {
		t.Error("a connection switched off still reports itself as recording")
	}
}

// Retention runs on a timer in the background. The timer is minutes long, so
// this exercises the work it does rather than waiting for it.
func TestThePruneSweepEnforcesEachConnectionsRetention(t *testing.T) {
	db := newTestStore(t)
	r := New(db, quietLogger())
	r.Configure("c1", true, 3)

	batch := make([]store.RecordedMessage, 0, 20)
	for i := range 20 {
		batch = append(batch, store.RecordedMessage{
			ConnectionID: "c1", Topic: "a/b", Payload: []byte{byte(i)}, ReceivedAt: time.Now(),
		})
	}
	if err := db.AppendRecorded(batch); err != nil {
		t.Fatal(err)
	}

	r.prune()

	got, _ := db.Recorded(store.RecordedQuery{ConnectionID: "c1"})
	if len(got) != 3 {
		t.Errorf("kept %d rows after the sweep, want the retention limit of 3", len(got))
	}
}

// A connection that is not recording is not swept, so turning recording off
// does not quietly delete what was already captured.
func TestTheSweepLeavesAConnectionThatStoppedRecordingAlone(t *testing.T) {
	db := newTestStore(t)
	r := New(db, quietLogger())

	if err := db.AppendRecorded([]store.RecordedMessage{
		{ConnectionID: "c1", Topic: "a/b", Payload: []byte("x"), ReceivedAt: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}

	// Never configured, so never swept.
	r.prune()

	got, _ := db.Recorded(store.RecordedQuery{ConnectionID: "c1"})
	if len(got) != 1 {
		t.Errorf("the sweep removed %d rows from a connection it does not record", 1-len(got))
	}
}

func TestANilLoggerIsAcceptedRatherThanPanicking(t *testing.T) {
	r := New(newTestStore(t), nil)
	r.Configure("c1", true, 0)
	r.Start(context.Background())
	r.Observe(msg("a/b", "x"))
	r.Stop()
}
