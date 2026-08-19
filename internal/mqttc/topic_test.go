package mqttc_test

import (
	"testing"

	"github.com/mqttview/mqttview/internal/mqttc"
)

func TestMatchFilter(t *testing.T) {
	cases := []struct {
		filter string
		topic  string
		want   bool
	}{
		{"sport/tennis/player1", "sport/tennis/player1", true},
		{"sport/tennis/#", "sport/tennis/player1", true},
		{"sport/tennis/#", "sport/tennis/player1/ranking", true},
		{"sport/tennis/#", "sport/tennis", true}, // '#' also matches the parent
		{"sport/#", "sport", true},
		{"sport/+", "sport/tennis", true},
		{"sport/+", "sport/tennis/player1", false},
		{"sport/+/player1", "sport/tennis/player1", true},
		{"+/+", "/finance", true},
		{"/+", "/finance", true},
		{"+", "/finance", false},
		// A leading wildcard must not reach the reserved $ namespace.
		{"#", "$SYS/broker/uptime", false},
		{"+/monitor/Clients", "$SYS/monitor/Clients", false},
		{"$SYS/#", "$SYS/broker/uptime", true},
		{"a/b", "a/b/c", false},
		{"a/b/c", "a/b", false},
	}

	for _, tc := range cases {
		if got := mqttc.MatchFilter(tc.filter, tc.topic); got != tc.want {
			t.Errorf("MatchFilter(%q, %q) = %v, want %v", tc.filter, tc.topic, got, tc.want)
		}
	}
}

func TestValidateFilter(t *testing.T) {
	valid := []string{"a/b", "a/+/b", "a/#", "#", "+", "$SYS/#", "a//b"}
	for _, f := range valid {
		if err := mqttc.ValidateFilter(f); err != nil {
			t.Errorf("ValidateFilter(%q) = %v, want nil", f, err)
		}
	}

	invalid := []string{"", "a/#/b", "sport+", "sport/tennis#", "a/b+c"}
	for _, f := range invalid {
		if err := mqttc.ValidateFilter(f); err == nil {
			t.Errorf("ValidateFilter(%q) = nil, want an error", f)
		}
	}
}

func TestValidateTopicRejectsWildcards(t *testing.T) {
	if err := mqttc.ValidateTopic("a/+/b"); err == nil {
		t.Error("publishing to a wildcard topic must be rejected")
	}
	if err := mqttc.ValidateTopic("a/b"); err != nil {
		t.Errorf("ValidateTopic(a/b) = %v, want nil", err)
	}
}

func TestNormalizeFillsDefaults(t *testing.T) {
	spec := mqttc.ConnectionSpec{Name: "broker", URL: "mqtt.example.com"}
	if err := spec.Normalize(); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if spec.URL != "tcp://mqtt.example.com:1883" {
		t.Errorf("URL = %q, want tcp://mqtt.example.com:1883", spec.URL)
	}
	if spec.Version != mqttc.V311 {
		t.Errorf("Version = %v, want 3.1.1", spec.Version)
	}
	if spec.ClientID == "" {
		t.Error("a client ID should have been generated")
	}
	if spec.KeepAlive != 60 {
		t.Errorf("KeepAlive = %d, want 60", spec.KeepAlive)
	}
}

func TestNormalizeMovesCredentialsOutOfURL(t *testing.T) {
	spec := mqttc.ConnectionSpec{Name: "broker", URL: "mqtts://user:secret@broker.local"}
	if err := spec.Normalize(); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if spec.Username != "user" || spec.Password != "secret" {
		t.Errorf("credentials = %q/%q, want user/secret", spec.Username, spec.Password)
	}
	if spec.URL != "ssl://broker.local:8883" {
		t.Errorf("URL = %q, want ssl://broker.local:8883 with credentials stripped", spec.URL)
	}
	if !spec.UsesTLS() {
		t.Error("mqtts:// should be reported as a TLS transport")
	}
}

func TestTreeChildrenAndSearch(t *testing.T) {
	tree := mqttc.NewTree()
	for _, topic := range []string{
		"home/kitchen/temperature",
		"home/kitchen/humidity",
		"home/bedroom/temperature",
	} {
		tree.Record(mqttc.Message{Topic: topic, Payload: []byte("1")})
	}

	roots, ok := tree.Children("")
	if !ok || len(roots) != 1 || roots[0].Name != "home" {
		t.Fatalf("roots = %+v", roots)
	}
	if roots[0].TopicCount != 3 {
		t.Errorf("home.TopicCount = %d, want 3", roots[0].TopicCount)
	}

	kids, ok := tree.Children("home")
	if !ok || len(kids) != 2 {
		t.Fatalf("children of home = %+v", kids)
	}

	matches := tree.Match("home/+/temperature", 10)
	if len(matches) != 2 {
		t.Errorf("Match returned %d topics, want 2", len(matches))
	}

	found := tree.Search("humid", 10)
	if len(found) != 1 || found[0].Topic != "home/kitchen/humidity" {
		t.Errorf("Search returned %+v", found)
	}
}

func TestHistoryIsANewestFirstRing(t *testing.T) {
	h := mqttc.NewHistory(3)
	for i, topic := range []string{"a", "b", "c", "d"} {
		h.Add(mqttc.Message{Topic: topic, Seq: uint64(i + 1)})
	}

	recent := h.Recent(10, "")
	if len(recent) != 3 {
		t.Fatalf("Recent returned %d messages, want 3", len(recent))
	}
	if recent[0].Topic != "d" || recent[2].Topic != "b" {
		t.Errorf("order = %s,%s,%s; want d,c,b", recent[0].Topic, recent[1].Topic, recent[2].Topic)
	}

	filtered := h.Recent(10, "c")
	if len(filtered) != 1 || filtered[0].Topic != "c" {
		t.Errorf("filtered = %+v", filtered)
	}
}
