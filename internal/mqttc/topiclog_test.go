package mqttc

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
)

func logMsg(topic, payload string, at time.Time) Message {
	return Message{Topic: topic, Payload: []byte(payload), ReceivedAt: at}
}

// The reason this type exists at all: a chatty topic must not be able to push
// a quiet topic's past out of reach, which is precisely what the shared
// History ring does.
func TestABusyTopicDoesNotEvictAQuietTopicsHistory(t *testing.T) {
	l := NewTopicLog(10, 1<<20)
	base := time.Now()

	l.Add(logMsg("quiet", "first", base))
	for i := range 500 {
		l.Add(logMsg("busy", fmt.Sprintf("n%d", i), base.Add(time.Duration(i)*time.Millisecond)))
	}

	got := l.Entries("quiet", 0)
	if len(got) != 1 {
		t.Fatalf("the quiet topic kept %d entries, want 1", len(got))
	}
	if string(got[0].Payload) != "first" {
		t.Errorf("the quiet topic's message is %q, want %q", got[0].Payload, "first")
	}

	// And for contrast, the same traffic through the shared ring loses it.
	h := NewHistory(10)
	h.Add(logMsg("quiet", "first", base))
	for i := range 500 {
		h.Add(logMsg("busy", fmt.Sprintf("n%d", i), base))
	}
	if kept := h.Recent(100, "quiet"); len(kept) != 0 {
		t.Errorf("the shared ring still has the quiet topic (%d entries), so this type is pointless", len(kept))
	}
}

func TestEntriesComeBackOldestFirst(t *testing.T) {
	l := NewTopicLog(10, 1<<20)
	base := time.Now()
	for i := range 5 {
		l.Add(logMsg("t", fmt.Sprintf("%d", i), base.Add(time.Duration(i)*time.Second)))
	}

	got := l.Entries("t", 0)
	if len(got) != 5 {
		t.Fatalf("got %d entries, want 5", len(got))
	}
	for i, m := range got {
		if want := fmt.Sprintf("%d", i); string(m.Payload) != want {
			t.Errorf("entry %d is %q, want %q — the order is not chronological", i, m.Payload, want)
		}
	}
}

func TestTheRingOverwritesTheOldestEntryForATopic(t *testing.T) {
	l := NewTopicLog(3, 1<<20)
	base := time.Now()
	for i := range 5 {
		l.Add(logMsg("t", fmt.Sprintf("%d", i), base.Add(time.Duration(i)*time.Second)))
	}

	got := l.Entries("t", 0)
	if len(got) != 3 {
		t.Fatalf("kept %d entries, want the ring size of 3", len(got))
	}
	want := []string{"2", "3", "4"}
	for i, m := range got {
		if string(m.Payload) != want[i] {
			t.Errorf("entry %d is %q, want %q", i, m.Payload, want[i])
		}
	}
}

func TestALimitTakesTheNewestEntriesNotTheOldest(t *testing.T) {
	l := NewTopicLog(10, 1<<20)
	base := time.Now()
	for i := range 6 {
		l.Add(logMsg("t", fmt.Sprintf("%d", i), base.Add(time.Duration(i)*time.Second)))
	}

	got := l.Entries("t", 2)
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	if string(got[0].Payload) != "4" || string(got[1].Payload) != "5" {
		t.Errorf("got %q and %q, want the two newest, 4 and 5", got[0].Payload, got[1].Payload)
	}
}

// The budget has to bound the number of topics, not just each ring: a hundred
// thousand topics each individually within their limit is still a hundred
// thousand rings.
func TestTheBudgetEvictsWholeTopicsLeastRecentlyUpdatedFirst(t *testing.T) {
	// Four topics of 100 bytes each fit; the fifth must push the oldest out.
	l := NewTopicLog(1, 400)
	base := time.Now()
	payload := strings.Repeat("x", 100)

	for i := range 4 {
		l.Add(logMsg(fmt.Sprintf("t%d", i), payload, base.Add(time.Duration(i)*time.Second)))
	}
	if got := l.Topics(); got != 4 {
		t.Fatalf("holding %d topics, want 4 before the budget is exceeded", got)
	}

	l.Add(logMsg("t4", payload, base.Add(5*time.Second)))

	if got := l.Entries("t0", 0); got != nil {
		t.Errorf("t0 was the least recently updated and should have been evicted, but has %d entries", len(got))
	}
	if got := l.Entries("t4", 0); len(got) != 1 {
		t.Errorf("the topic just written has %d entries, want 1", len(got))
	}
	if s := l.Stats(); s.TopicsEvicted != 1 {
		t.Errorf("reported %d evictions, want 1 — the UI shows this number", s.TopicsEvicted)
	}
}

// Reading a topic is a use of it. Without this, a topic being actively watched
// in the UI is evicted while the user is looking at it.
func TestReadingATopicKeepsItFromBeingEvicted(t *testing.T) {
	l := NewTopicLog(1, 300)
	base := time.Now()
	payload := strings.Repeat("x", 100)

	l.Add(logMsg("a", payload, base))
	l.Add(logMsg("b", payload, base.Add(time.Second)))
	l.Add(logMsg("c", payload, base.Add(2*time.Second)))

	// "a" is now the least recently updated and next to go. Reading it should
	// move it out of the firing line.
	l.Entries("a", 0)
	l.Add(logMsg("d", payload, base.Add(3*time.Second)))

	if got := l.Entries("a", 0); len(got) != 1 {
		t.Errorf("topic a was read and then evicted anyway")
	}
	if got := l.Entries("b", 0); got != nil {
		t.Errorf("topic b should have been evicted instead, but has %d entries", len(got))
	}
}

// A single topic bigger than the whole budget must not spin forever dropping
// and re-adding itself.
func TestATopicLargerThanTheWholeBudgetIsKeptRatherThanLoopingForever(t *testing.T) {
	l := NewTopicLog(4, 10)
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.Add(logMsg("t", strings.Repeat("x", 500), time.Now()))
		l.Add(logMsg("t", strings.Repeat("y", 500), time.Now()))
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Add did not return: eviction is looping on a topic it cannot drop")
	}
	if got := l.Entries("t", 0); len(got) == 0 {
		t.Error("the only topic was evicted, leaving the log empty and the budget still exceeded")
	}
}

// The byte accounting has to survive the ring wrapping, or the budget drifts
// upward until it means nothing.
func TestBytesStopCountingOnceAnEntryIsOverwritten(t *testing.T) {
	l := NewTopicLog(3, 1<<30)
	payload := strings.Repeat("x", 100)
	for range 30 {
		l.Add(logMsg("t", payload, time.Now()))
	}

	// Three slots of a hundred bytes, however many messages went through.
	if got := l.Stats().Bytes; got != 300 {
		t.Errorf("accounted %d bytes after 30 messages through a 3-slot ring, want 300", got)
	}
}

func TestAnEvictedTopicStopsCountingAgainstTheBudget(t *testing.T) {
	l := NewTopicLog(1, 250)
	payload := strings.Repeat("x", 100)
	for i := range 5 {
		l.Add(logMsg(fmt.Sprintf("t%d", i), payload, time.Now()))
	}

	s := l.Stats()
	if s.Bytes > s.Budget {
		t.Errorf("holding %d bytes against a budget of %d: eviction is not reclaiming", s.Bytes, s.Budget)
	}
	if s.Bytes != int64(s.Topics*100) {
		t.Errorf("%d bytes for %d topics: the accounting does not match what is held", s.Bytes, s.Topics)
	}
}

func TestALargePayloadIsTruncatedAndSaysSo(t *testing.T) {
	l := NewTopicLog(2, 1<<30)
	l.Add(logMsg("t", strings.Repeat("x", maxTopicLogPayload+500), time.Now()))

	got := l.Entries("t", 0)
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	if len(got[0].Payload) != maxTopicLogPayload {
		t.Errorf("kept %d bytes, want it clipped to %d", len(got[0].Payload), maxTopicLogPayload)
	}
	if !got[0].Truncated {
		t.Error("the payload was clipped but Truncated is false, so the UI would present half a document as the whole one")
	}
}

// The ring keeps overwriting behind whatever the caller is holding.
func TestACallerCannotMutateTheStoredHistory(t *testing.T) {
	l := NewTopicLog(4, 1<<30)
	original := []byte("hello")
	l.Add(Message{Topic: "t", Payload: original, ReceivedAt: time.Now()})

	original[0] = 'J' // the caller reusing its own buffer

	got := l.Entries("t", 0)
	if string(got[0].Payload) != "hello" {
		t.Errorf("stored payload is %q: Add kept the caller's slice", got[0].Payload)
	}

	got[0].Payload[0] = 'X' // the reader scribbling on what it was handed
	again := l.Entries("t", 0)
	if string(again[0].Payload) != "hello" {
		t.Errorf("stored payload is %q: Entries handed out the ring's own slice", again[0].Payload)
	}
}

func TestATopicSeenOnceHasNothingToDiffAgainst(t *testing.T) {
	l := NewTopicLog(4, 1<<30)
	l.Add(logMsg("t", "only", time.Now()))

	if _, ok := l.Previous("t"); ok {
		t.Error("Previous reported a message to diff against after a single message; the UI would render that as 'no changes'")
	}
}

func TestPreviousReturnsTheMessageBeforeTheCurrentOne(t *testing.T) {
	l := NewTopicLog(4, 1<<30)
	base := time.Now()
	l.Add(logMsg("t", "old", base))
	l.Add(logMsg("t", "new", base.Add(time.Second)))

	prev, ok := l.Previous("t")
	if !ok {
		t.Fatal("Previous found nothing to diff against after two messages")
	}
	if string(prev.Payload) != "old" {
		t.Errorf("Previous returned %q, want the earlier message %q", prev.Payload, "old")
	}
}

func TestAnUnseenTopicHasNoHistory(t *testing.T) {
	l := NewTopicLog(4, 1<<30)
	l.Add(logMsg("a", "x", time.Now()))

	if got := l.Entries("b", 0); got != nil {
		t.Errorf("an untouched topic returned %d entries", len(got))
	}
	if _, _, _, ok := l.Span("b"); ok {
		t.Error("Span reported a range for a topic never seen")
	}
}

func TestSpanReportsTheRangeTheHistoryActuallyCovers(t *testing.T) {
	l := NewTopicLog(10, 1<<30)
	base := time.Now().Truncate(time.Second)
	for i := range 4 {
		l.Add(logMsg("t", "x", base.Add(time.Duration(i)*time.Minute)))
	}

	first, last, count, ok := l.Span("t")
	if !ok {
		t.Fatal("Span found no history for a topic with four messages")
	}
	if !first.Equal(base) {
		t.Errorf("span starts at %v, want %v", first, base)
	}
	if !last.Equal(base.Add(3 * time.Minute)) {
		t.Errorf("span ends at %v, want %v", last, base.Add(3*time.Minute))
	}
	if count != 4 {
		t.Errorf("span counts %d messages, want 4", count)
	}
}

func TestClearDropsEverythingAndReleasesTheBudget(t *testing.T) {
	l := NewTopicLog(4, 1<<30)
	for i := range 10 {
		l.Add(logMsg(fmt.Sprintf("t%d", i), "payload", time.Now()))
	}

	l.Clear()

	if got := l.Topics(); got != 0 {
		t.Errorf("holding %d topics after Clear", got)
	}
	if got := l.Stats().Bytes; got != 0 {
		t.Errorf("accounting still shows %d bytes after Clear", got)
	}
	// And it still works afterwards.
	l.Add(logMsg("fresh", "x", time.Now()))
	if got := l.Entries("fresh", 0); len(got) != 1 {
		t.Error("the log does not record anything after Clear")
	}
}

func TestZeroValuesTakeTheDefaults(t *testing.T) {
	l := NewTopicLog(0, 0)
	s := l.Stats()
	if s.EntriesPer != DefaultTopicLogEntries {
		t.Errorf("entries per topic is %d, want the default %d", s.EntriesPer, DefaultTopicLogEntries)
	}
	if s.Budget != DefaultTopicLogBudget {
		t.Errorf("budget is %d, want the default %d", s.Budget, DefaultTopicLogBudget)
	}
}

func TestConcurrentWritersAndReadersDoNotRace(t *testing.T) {
	l := NewTopicLog(20, 1<<20)
	done := make(chan struct{})

	for w := range 4 {
		go func() {
			for i := range 200 {
				l.Add(logMsg(fmt.Sprintf("t%d", i%8), fmt.Sprintf("w%d-%d", w, i), time.Now()))
			}
			done <- struct{}{}
		}()
	}
	for range 2 {
		go func() {
			for i := range 200 {
				l.Entries(fmt.Sprintf("t%d", i%8), 5)
				l.Stats()
				l.Previous("t0")
			}
			done <- struct{}{}
		}()
	}
	for range 6 {
		<-done
	}
	close(done)
}

func TestPayloadsSurviveArbitraryBytes(t *testing.T) {
	l := NewTopicLog(4, 1<<30)
	raw := []byte{0x00, 0xff, 0xfe, 0x01, '\n', 0x80}
	l.Add(Message{Topic: "bin", Payload: raw, ReceivedAt: time.Now()})

	got := l.Entries("bin", 0)
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	if !bytes.Equal(got[0].Payload, raw) {
		t.Errorf("stored %v, want %v — binary payloads are being mangled", got[0].Payload, raw)
	}
}
