package mqttc

import "testing"

// A connection spec arrives as a request body, and the ring it asks for is
// allocated whole the moment the connection starts. Without a ceiling the
// number in that body is an allocation instruction: at roughly a hundred bytes
// per Message, a hundred million entries is ten gigabytes before a single
// message has been received.
func TestAHistorySpecCannotAllocateMoreThanTheCap(t *testing.T) {
	h := NewHistory(100_000_000)
	if got := len(h.buf); got != MaxHistorySize {
		t.Fatalf("ring holds %d entries, want it clamped to %d", got, MaxHistorySize)
	}
}

// The clamp has to bite only at the top. A cap that quietly rewrote every
// request would be its own defect, so the sizes people actually ask for are
// the ones they get.
func TestAHistorySpecBelowTheCapIsHonoured(t *testing.T) {
	for _, size := range []int{1, DefaultHistorySize, MaxHistorySize} {
		if got := len(NewHistory(size).buf); got != size {
			t.Errorf("asked for %d entries, got %d", size, got)
		}
	}
	if got := len(NewHistory(0).buf); got != DefaultHistorySize {
		t.Errorf("zero should take the default %d, got %d", DefaultHistorySize, got)
	}
}

// A clamped ring is still a working ring: it wraps, and it reports what it
// holds. Capping the allocation must not cost the behaviour.
func TestAClampedHistoryStillWrapsAndReports(t *testing.T) {
	h := NewHistory(3)
	for i := range 5 {
		h.Add(Message{Topic: "a/b", Seq: uint64(i)})
	}
	if got := h.Len(); got != 3 {
		t.Fatalf("Len() = %d, want 3", got)
	}
	recent := h.Recent(10, "")
	if len(recent) != 3 {
		t.Fatalf("Recent returned %d messages, want 3", len(recent))
	}
	if recent[0].Seq != 4 {
		t.Errorf("newest message has Seq %d, want 4", recent[0].Seq)
	}
}

// The per-topic ring is the sharper version of the same problem: it is
// allocated once per topic the broker carries, so the spec's number is
// multiplied by however many topics turn up.
func TestATopicLogSpecCannotAllocateMoreThanTheCap(t *testing.T) {
	l := NewTopicLog(100_000_000, 0)
	if l.perTopic != MaxTopicLogEntries {
		t.Fatalf("per-topic ring is %d entries, want it clamped to %d", l.perTopic, MaxTopicLogEntries)
	}

	// And the clamp is what actually gets allocated, not just what is recorded.
	l.Add(Message{Topic: "a/b"})
	el, ok := l.rings["a/b"]
	if !ok {
		t.Fatal("no ring was created for the topic")
	}
	if got := len(el.Value.(*topicRing).buf); got != MaxTopicLogEntries {
		t.Errorf("allocated ring holds %d entries, want %d", got, MaxTopicLogEntries)
	}
}

func TestATopicLogSpecBelowTheCapIsHonoured(t *testing.T) {
	if got := NewTopicLog(50, 0).perTopic; got != 50 {
		t.Errorf("asked for 50 entries per topic, got %d", got)
	}
	if got := NewTopicLog(0, 0).perTopic; got != DefaultTopicLogEntries {
		t.Errorf("zero should take the default %d, got %d", DefaultTopicLogEntries, got)
	}
}
