package mqttc

import (
	"strings"
	"testing"
)

// Normalize is the gate every connection passes through, from the API, from
// the database on boot and from a plugin. What it accepts and what it rewrites
// decides whether a saved connection still works after an upgrade.

func TestNormalizeRewritesSchemesAndFillsInPorts(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"mqtt://broker", "tcp://broker:1883"},
		{"mqtt://broker:1884", "tcp://broker:1884"},
		{"tcp://broker", "tcp://broker:1883"},
		{"mqtts://broker", "ssl://broker:8883"},
		{"ssl://broker", "ssl://broker:8883"},
		{"tls://broker", "ssl://broker:8883"},
		{"tcps://broker", "ssl://broker:8883"},
		{"mqtt+ssl://broker", "ssl://broker:8883"},
		// 8083/8084, not 80/443: those are the ports brokers actually listen
		// on for MQTT over WebSocket.
		{"ws://broker", "ws://broker:8083"},
		{"wss://broker", "wss://broker:8084"},
		{"ws://broker:9001/mqtt", "ws://broker:9001/mqtt"},
		{"MQTT://Broker:1883", "tcp://Broker:1883"},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			spec := ConnectionSpec{Name: "n", URL: tt.in, Version: V311}
			if err := spec.Normalize(); err != nil {
				t.Fatalf("Normalize: %v", err)
			}
			if spec.URL != tt.want {
				t.Errorf("URL = %q, want %q", spec.URL, tt.want)
			}
		})
	}
}

func TestNormalizeRefusesWhatCannotBeConnectedTo(t *testing.T) {
	for _, tt := range []struct {
		name string
		spec ConnectionSpec
		want string
	}{
		{"no name", ConnectionSpec{URL: "mqtt://h:1883"}, "name"},
		{"no url", ConnectionSpec{Name: "n"}, "url"},
		{"unparseable url", ConnectionSpec{Name: "n", URL: "://"}, "url"},
		{"no host", ConnectionSpec{Name: "n", URL: "mqtt://"}, "host"},
		{"unknown scheme", ConnectionSpec{Name: "n", URL: "gopher://h:70"}, "scheme"},
		{"unknown version", ConnectionSpec{Name: "n", URL: "mqtt://h", Version: Version(9)}, "version"},
		{"keep alive too large", ConnectionSpec{Name: "n", URL: "mqtt://h", Version: V311, KeepAlive: 70000}, "keepAlive"},
		{"bad subscription qos", ConnectionSpec{
			Name: "n", URL: "mqtt://h", Version: V311,
			Subscriptions: []Subscription{{Filter: "a/#", QoS: 9}},
		}, "qos"},
		{"bad retain handling", ConnectionSpec{
			Name: "n", URL: "mqtt://h", Version: V311,
			Subscriptions: []Subscription{{Filter: "a/#", RetainHandling: 9}},
		}, "retainHandling"},
		{"a will with a bad qos", ConnectionSpec{
			Name: "n", URL: "mqtt://h", Version: V311,
			Will: &Will{Topic: "t", QoS: 9},
		}, "qos"},
		{"tls min version", ConnectionSpec{
			Name: "n", URL: "mqtts://h", Version: V311,
			TLS: TLSSpec{MinVersion: "1.1"},
		}, "minVersion"},
		{"a client cert with no key", ConnectionSpec{
			Name: "n", URL: "mqtts://h", Version: V311,
			TLS: TLSSpec{ClientCertPEM: "x"},
		}, "together"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.spec.Normalize()
			if err == nil {
				t.Fatal("this spec was accepted")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

func TestNormalizeFillsInDefaults(t *testing.T) {
	spec := ConnectionSpec{Name: "n", URL: "mqtt://h", Version: V311}
	if err := spec.Normalize(); err != nil {
		t.Fatal(err)
	}

	if spec.ClientID == "" || !strings.HasPrefix(spec.ClientID, "mqttview-") {
		t.Errorf("client id = %q, want a generated one", spec.ClientID)
	}
	if spec.KeepAlive == 0 || spec.ConnectTimeout == 0 {
		t.Errorf("defaults were not filled in: %+v", spec)
	}
	// HistorySize is deliberately left at zero, which NewHistory reads as "use
	// the default"; writing the number into the spec would freeze today's
	// default into every saved connection.
	if spec.HistorySize != 0 {
		t.Errorf("HistorySize = %d, want it left for NewHistory to decide", spec.HistorySize)
	}
	if h := NewHistory(spec.HistorySize); h == nil {
		t.Error("a zero history size did not produce a ring")
	}
}

func TestUnixSocketsAreMQTT3Only(t *testing.T) {
	spec := ConnectionSpec{Name: "n", URL: "unix:///tmp/mqtt.sock", Version: V5}
	if err := spec.Normalize(); err == nil {
		t.Fatal("a unix socket on MQTT 5 was accepted")
	}

	spec.Version = V311
	if err := spec.Normalize(); err != nil {
		t.Fatalf("a unix socket on MQTT 3.1.1 was refused: %v", err)
	}
}

func TestUsesTLS(t *testing.T) {
	for url, want := range map[string]bool{
		"tcp://h:1883": false, "ws://h:80": false, "unix:///s": false,
		"ssl://h:8883": true, "wss://h:443": true,
	} {
		spec := ConnectionSpec{URL: url}
		if got := spec.UsesTLS(); got != want {
			t.Errorf("%s: UsesTLS = %v, want %v", url, got, want)
		}
	}
}

func TestParseVersion(t *testing.T) {
	for in, want := range map[string]Version{
		"3.1": V31, "v3": V31, "3.1.1": V311, "4": V311, "v4": V311, "5": V5, "5.0": V5, "v5": V5,
		// An omitted version means the default rather than an error: it is
		// what a config file that does not mention it should get.
		"": V311,
	} {
		got, err := ParseVersion(in)
		if err != nil || got != want {
			t.Errorf("ParseVersion(%q) = %v %v, want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"2", "6", "3.11", "nonsense"} {
		if _, err := ParseVersion(in); err == nil {
			t.Errorf("ParseVersion(%q) was accepted", in)
		}
	}
}

func TestVersionStringsRoundTrip(t *testing.T) {
	for _, v := range []Version{V31, V311, V5} {
		got, err := ParseVersion(v.String())
		if err != nil || got != v {
			t.Errorf("%v round-tripped to %v %v", v, got, err)
		}
	}
	if !V5.Valid() || Version(9).Valid() {
		t.Error("Valid is wrong")
	}
}

func TestValidateTopic(t *testing.T) {
	// A topic is not a filter: publishing to a wildcard is a mistake the
	// broker cannot correct.
	for _, bad := range []string{"", "a/#", "a/+", "#", "+", strings.Repeat("x", 70000)} {
		if err := ValidateTopic(bad); err == nil {
			t.Errorf("ValidateTopic(%q) accepted it", bad)
		}
	}
	for _, good := range []string{"a", "a/b/c", "a//b", "/", "$SYS/broker/uptime"} {
		if err := ValidateTopic(good); err != nil {
			t.Errorf("ValidateTopic(%q) = %v", good, err)
		}
	}
}

func TestTreePreviewsAndSearches(t *testing.T) {
	tree := NewTree()

	tree.Record(Message{Topic: "home/kitchen/temp", Payload: []byte(`{"temp":21.5}`)})
	tree.Record(Message{Topic: "home/kitchen/humidity", Payload: []byte("55")})
	tree.Record(Message{Topic: "home/garage/door", Payload: []byte("closed")})
	// Binary, which must be previewed as hex rather than as mojibake.
	tree.Record(Message{Topic: "home/binary", Payload: []byte{0x00, 0xff, 0xfe}})
	// Long, which must be truncated rather than sent whole to a browser.
	tree.Record(Message{Topic: "home/long", Payload: []byte(strings.Repeat("x", 5000))})

	children, ok := tree.Children("home")
	if !ok || len(children) != 4 {
		t.Fatalf("children of home = %d %v", len(children), ok)
	}

	if node, ok := tree.Node("home/kitchen"); !ok || node.TopicCount != 2 {
		t.Errorf("home/kitchen = %+v %v", node, ok)
	}
	if v, ok := tree.Value("home/garage/door"); !ok || string(v.Payload) != "closed" {
		t.Errorf("value = %+v %v", v, ok)
	}
	if _, ok := tree.Value("home/never"); ok {
		t.Error("an unpublished topic had a value")
	}

	if got := tree.Match("home/kitchen/#", 10); len(got) != 2 {
		t.Errorf("Match returned %d", len(got))
	}
	if got := tree.Match("home/#", 1); len(got) != 1 {
		t.Errorf("Match ignored its limit: %d", len(got))
	}

	if got := tree.Search("kitchen", 10); len(got) != 2 {
		t.Errorf("Search found %d", len(got))
	}
	if got := tree.Search("nothing-matches-this", 10); len(got) != 0 {
		t.Errorf("Search invented %d results", len(got))
	}

	topics, messages, _ := tree.Stats()
	if topics != 5 || messages != 5 {
		t.Errorf("Stats = %d topics, %d messages", topics, messages)
	}
}

func TestHistoryFiltersByTopic(t *testing.T) {
	h := NewHistory(10)
	h.Add(Message{Topic: "a/one", Seq: 1})
	h.Add(Message{Topic: "b/two", Seq: 2})
	h.Add(Message{Topic: "a/three", Seq: 3})

	if got := h.Recent(10, "a/#"); len(got) != 2 {
		t.Errorf("filtered history returned %d", len(got))
	}
	if got := h.Recent(10, ""); len(got) != 3 {
		t.Errorf("unfiltered history returned %d", len(got))
	}
	if got := h.Recent(1, ""); len(got) != 1 {
		t.Errorf("the limit was ignored: %d", len(got))
	}
	if got := h.Recent(10, "nothing/#"); len(got) != 0 {
		t.Errorf("a filter matching nothing returned %d", len(got))
	}
}
