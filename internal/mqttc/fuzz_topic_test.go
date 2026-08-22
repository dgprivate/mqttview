package mqttc

import (
	"strings"
	"testing"
)

// The topic layer reads bytes chosen by whoever can publish to the broker. A
// filter comes from a user, but a topic does not, and the two are matched
// against each other on every message. These targets look for the input that
// makes that code panic or contradict itself.

func FuzzValidateFilter(f *testing.F) {
	for _, s := range []string{
		"", "#", "+", "a/#", "a/+/b", "a/#/b", "sport/tennis/#", "+/+/#",
		"a//b", "/", "//", strings.Repeat("a/", 200) + "#", "a/#+", "$SYS/#",
		"\x00", "\xff\xfe", strings.Repeat("+/", 500),
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, filter string) {
		err := ValidateFilter(filter)
		if err != nil {
			return
		}
		// A filter the validator accepted must be usable: matching it against
		// anything at all has to return rather than panic, and a filter must
		// always match a topic built from its own literal segments.
		MatchFilter(filter, "a/b/c")
		MatchFilter(filter, filter)

		if !strings.ContainsAny(filter, "+#") && !MatchFilter(filter, filter) {
			t.Fatalf("literal filter %q does not match itself", filter)
		}
	})
}

func FuzzMatchFilter(f *testing.F) {
	seeds := []struct{ filter, topic string }{
		{"#", "a/b"}, {"a/#", "a/b/c"}, {"a/+/c", "a/b/c"}, {"+", "a"},
		{"a/b", "a/b"}, {"a/b", "a/b/c"}, {"#", ""}, {"+/#", "a"},
		{"$SYS/#", "$SYS/broker/uptime"}, {"#", "$SYS/broker/uptime"},
	}
	for _, s := range seeds {
		f.Add(s.filter, s.topic)
	}

	f.Fuzz(func(t *testing.T, filter, topic string) {
		// Matching an invalid filter is allowed to be false, but never a panic.
		if err := ValidateFilter(filter); err != nil {
			MatchFilter(filter, topic)
			return
		}
		got := MatchFilter(filter, topic)

		// "#" is the whole tree, with the one carve-out every broker makes: it
		// does not reach topics starting with "$".
		if filter == "#" && !strings.HasPrefix(topic, "$") && !got {
			t.Fatalf("# should match %q", topic)
		}
	})
}

func FuzzTreeRecord(f *testing.F) {
	for _, s := range []string{
		"a/b/c", "", "/", "//", "a//b", strings.Repeat("deep/", 300) + "leaf",
		"$SYS/broker/uptime", "\x00/\xff", strings.Repeat("x", 70000),
	} {
		f.Add(s, []byte("payload"))
	}

	// The tree is built entirely from topics a broker sends. A topic that
	// corrupts it takes the whole live view down, not one message.
	f.Fuzz(func(t *testing.T, topic string, payload []byte) {
		tree := NewTree()
		tree.Record(Message{Topic: topic, Payload: payload})

		topics, messages, _ := tree.Stats()
		if messages > 0 && topics == 0 {
			t.Fatalf("recorded a message under no topic: %q", topic)
		}
		// Whatever went in must be findable or deliberately absent, never a
		// panic on the way back out.
		tree.Value(topic)
		tree.Node(topic)
		tree.Children("")
		tree.Match("#", 10)
		tree.Search(topic, 10)
	})
}
