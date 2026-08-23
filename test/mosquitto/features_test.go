package mosquitto_test

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dgprivate/mqttview/internal/mqttc"
)

// What the panel shows and the plugins depend on, against the real broker:
// retained values, quality of service, wildcards, wills, MQTT 5 properties and
// a session that survives losing the network.

func TestRetainedMessagesArriveOnSubscribe(t *testing.T) {
	// The topic tree is mostly retained values. A broker that has them and a
	// client that does not ask for them is an empty panel on a busy house.
	b := start(t, config{})
	topic := "mqttview/retained/temperature"

	publisher := connect(t, mqttc.ConnectionSpec{
		ID: "pub", URL: b.url("mqtt"), Version: mqttc.V311, CleanStart: true,
	})
	publisher.publish(t, mqttc.PublishRequest{
		Topic: topic, Payload: []byte("21.5"), QoS: 1, Retain: true,
	})

	// A second client, connecting afterwards, must see it.
	subscriber := connect(t, mqttc.ConnectionSpec{
		ID: "sub", URL: b.url("mqtt"), Version: mqttc.V311, CleanStart: true,
		Subscriptions: []mqttc.Subscription{{Filter: "mqttview/retained/#", QoS: 1}},
	})

	msg := subscriber.await(t, topic, 15*time.Second)
	if string(msg.Payload) != "21.5" {
		t.Errorf("payload = %q", msg.Payload)
	}
	if !msg.Retain {
		t.Error("the message did not arrive marked as retained, so the UI cannot tell it apart from a live one")
	}
}

func TestAnEmptyRetainedMessageClearsTheValue(t *testing.T) {
	// How a retained value is deleted, and the reason an empty payload is not
	// the same as no payload.
	b := start(t, config{})
	topic := "mqttview/retained/clearing"

	pub := connect(t, mqttc.ConnectionSpec{
		ID: "pub", URL: b.url("mqtt"), Version: mqttc.V311, CleanStart: true,
	})
	pub.publish(t, mqttc.PublishRequest{Topic: topic, Payload: []byte("stale"), QoS: 1, Retain: true})
	pub.publish(t, mqttc.PublishRequest{Topic: topic, Payload: nil, QoS: 1, Retain: true})

	sub := connect(t, mqttc.ConnectionSpec{
		ID: "sub", URL: b.url("mqtt"), Version: mqttc.V311, CleanStart: true,
		Subscriptions: []mqttc.Subscription{{Filter: topic, QoS: 1}},
	})
	sub.awaitNothing(t, topic, 2*time.Second)
}

func TestEveryQualityOfService(t *testing.T) {
	b := start(t, config{})

	for qos := byte(0); qos <= 2; qos++ {
		t.Run(fmt.Sprintf("QoS %d", qos), func(t *testing.T) {
			topic := fmt.Sprintf("mqttview/qos/%d", qos)
			s := connect(t, mqttc.ConnectionSpec{
				ID: fmt.Sprintf("q%d", qos), URL: b.url("mqtt"), Version: mqttc.V5, CleanStart: true,
				Subscriptions: []mqttc.Subscription{{Filter: topic, QoS: qos}},
			})
			s.publish(t, mqttc.PublishRequest{Topic: topic, Payload: []byte("x"), QoS: qos})

			msg := s.await(t, topic, 15*time.Second)
			if msg.QoS != qos {
				t.Errorf("published at QoS %d and it arrived at %d", qos, msg.QoS)
			}
		})
	}
}

func TestWildcardSubscriptions(t *testing.T) {
	b := start(t, config{})
	s := connect(t, mqttc.ConnectionSpec{
		URL: b.url("mqtt"), Version: mqttc.V311, CleanStart: true,
		Subscriptions: []mqttc.Subscription{
			{Filter: "house/+/temperature", QoS: 1},
			{Filter: "house/kitchen/#", QoS: 1},
		},
	})

	for _, topic := range []string{
		"house/bedroom/temperature", // the single-level wildcard
		"house/kitchen/light/state", // the multi-level one
	} {
		s.publish(t, mqttc.PublishRequest{Topic: topic, Payload: []byte("on"), QoS: 1})
		if got := string(s.await(t, topic, 15*time.Second).Payload); got != "on" {
			t.Errorf("%s: payload = %q", topic, got)
		}
	}

	// And nothing outside them, or a filter is not a filter.
	s.publish(t, mqttc.PublishRequest{Topic: "garage/door", Payload: []byte("open"), QoS: 1})
	s.awaitNothing(t, "garage/door", 2*time.Second)
}

func TestBinaryAndAwkwardPayloads(t *testing.T) {
	// The panel renders whatever the house sends, and a house sends protobuf,
	// a 200 KB camera snapshot and a topic with a space in it.
	b := start(t, config{})

	big := bytes.Repeat([]byte{0xDE, 0xAD, 0xBE, 0xEF}, 64*1024) // 256 KB
	cases := []struct {
		name    string
		topic   string
		payload []byte
	}{
		{"binary", "mqttview/awkward/binary", []byte{0x00, 0xFF, 0x1B, 0x00, 0x7F}},
		{"large", "mqttview/awkward/large", big},
		{"unicode topic", "mqttview/awkward/temperatura/dnevna soba", []byte("21,5 °C")},
		{"empty payload", "mqttview/awkward/empty", []byte{}},
	}

	s := connect(t, mqttc.ConnectionSpec{
		URL: b.url("mqtt"), Version: mqttc.V5, CleanStart: true,
		Subscriptions: []mqttc.Subscription{{Filter: "mqttview/awkward/#", QoS: 1}},
	})

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s.publish(t, mqttc.PublishRequest{Topic: tc.topic, Payload: tc.payload, QoS: 1})
			msg := s.await(t, tc.topic, 20*time.Second)
			if !bytes.Equal(msg.Payload, tc.payload) {
				t.Errorf("payload came back changed: %d bytes out, %d back", len(tc.payload), len(msg.Payload))
			}
		})
	}
}

func TestMQTT5PropertiesSurviveTheBroker(t *testing.T) {
	// The properties the plugins read. Mosquitto forwards them untouched, so
	// this asserts mqttview sends and parses them rather than that the broker
	// stores them.
	b := start(t, config{})
	topic := "mqttview/props/command"

	s := connect(t, mqttc.ConnectionSpec{
		URL: b.url("mqtt"), Version: mqttc.V5, CleanStart: true,
		Subscriptions: []mqttc.Subscription{{Filter: topic, QoS: 1}},
	})

	s.publish(t, mqttc.PublishRequest{
		Topic: topic, Payload: []byte(`{"state":"on"}`), QoS: 1,
		Props: &mqttc.MessageProps{
			ContentType:     "application/json",
			ResponseTopic:   "mqttview/props/reply",
			CorrelationData: []byte("req-42"),
			User:            map[string]string{"source": "mqttview"},
		},
	})

	msg := s.await(t, topic, 15*time.Second)
	if msg.Props == nil {
		t.Fatal("the properties did not come back at all")
	}
	if msg.Props.ContentType != "application/json" {
		t.Errorf("content type = %q", msg.Props.ContentType)
	}
	if msg.Props.ResponseTopic != "mqttview/props/reply" {
		t.Errorf("response topic = %q", msg.Props.ResponseTopic)
	}
	if string(msg.Props.CorrelationData) != "req-42" {
		t.Errorf("correlation data = %q", msg.Props.CorrelationData)
	}
	if msg.Props.User["source"] != "mqttview" {
		t.Errorf("user properties = %v", msg.Props.User)
	}
}

func TestPropertiesAreNotSentOnAThreePointOneOneConnection(t *testing.T) {
	// MQTT 3.1.1 has no properties. A client that sends them anyway produces a
	// malformed packet and the broker hangs up, which is a connection that
	// works until somebody uses a plugin.
	b := start(t, config{})
	topic := "mqttview/props/three"

	s := connect(t, mqttc.ConnectionSpec{
		URL: b.url("mqtt"), Version: mqttc.V311, CleanStart: true,
		Subscriptions: []mqttc.Subscription{{Filter: topic, QoS: 1}},
	})
	s.publish(t, mqttc.PublishRequest{
		Topic: topic, Payload: []byte("x"), QoS: 1,
		Props: &mqttc.MessageProps{ContentType: "application/json"},
	})

	msg := s.await(t, topic, 15*time.Second)
	if msg.Props != nil {
		t.Errorf("a 3.1.1 message arrived carrying properties: %+v", msg.Props)
	}
	if s.conn.Status().State != mqttc.StateConnected {
		t.Error("the connection did not survive publishing with properties on 3.1.1")
	}
}

func TestALastWillIsPublishedWhenTheClientVanishes(t *testing.T) {
	// The point of a will: the house learns that something stopped, without
	// that thing being well enough to say so.
	b := start(t, config{})
	willTopic := "mqttview/status/gateway"

	watcher := connect(t, mqttc.ConnectionSpec{
		ID: "watcher", URL: b.url("mqtt"), Version: mqttc.V311, CleanStart: true,
		Subscriptions: []mqttc.Subscription{{Filter: willTopic, QoS: 1}},
	})

	cable := newLink(t, fmt.Sprintf("127.0.0.1:%d", b.port))
	doomed := connect(t, mqttc.ConnectionSpec{
		ID: "doomed", URL: cable.url(), Version: mqttc.V311, CleanStart: true,
		Will: &mqttc.Will{Topic: willTopic, Payload: "offline", QoS: 1},
		// The broker declares a client dead after one and a half keep-alive
		// periods, so this decides how long the test waits.
		KeepAlive: 2,
	})
	doomed.publish(t, mqttc.PublishRequest{Topic: willTopic, Payload: []byte("online"), QoS: 1, Retain: true})
	if got := string(watcher.await(t, willTopic, 15*time.Second).Payload); got != "online" {
		t.Fatalf("the doomed client never announced itself: %q", got)
	}

	cable.cut()

	msg := watcher.await(t, willTopic, 30*time.Second)
	if string(msg.Payload) != "offline" {
		t.Errorf("the will payload was %q", msg.Payload)
	}
}

func TestAConnectionComesBackByItselfAfterTheNetworkDrops(t *testing.T) {
	// A house is not a datacentre: wifi drops, the broker is restarted, the
	// switch reboots. A panel that needs a human to press reconnect is a panel
	// showing yesterday's values.
	b := start(t, config{})
	cable := newLink(t, fmt.Sprintf("127.0.0.1:%d", b.port))
	topic := "mqttview/reconnect/probe"

	s := connect(t, mqttc.ConnectionSpec{
		URL: cable.url(), Version: mqttc.V311, CleanStart: true, KeepAlive: 2,
		AutoConnect:   true,
		Subscriptions: []mqttc.Subscription{{Filter: topic, QoS: 1}},
	})
	s.mgr.StartAutoConnect(t.Context())

	cable.cut()

	// The link has to be seen to break and then come back, or "still
	// connected" passes this test without anything having reconnected. The
	// gap can be under a millisecond, so it is read from the status stream
	// the UI subscribes to rather than by sampling the state.
	s.awaitStates(t, []mqttc.State{
		mqttc.StateConnected, mqttc.StateError, mqttc.StateConnected,
	}, 60*time.Second)

	// And the subscription has to have come back with it: a reconnect that
	// leaves the client deaf is worse than staying down, because it looks fine.
	s.publish(t, mqttc.PublishRequest{Topic: topic, Payload: []byte("back"), QoS: 1})
	if got := string(s.await(t, topic, 20*time.Second).Payload); got != "back" {
		t.Errorf("payload after the reconnect = %q", got)
	}
}

func TestASessionSurvivesAReconnectWhenCleanStartIsOff(t *testing.T) {
	// The other half of the clean-start switch in the connection form: the
	// broker keeps the subscription and queues QoS 1 messages while the client
	// is away.
	b := start(t, config{})
	topic := "mqttview/session/queued"
	clientID := "mqttview-session-test"

	first := connect(t, mqttc.ConnectionSpec{
		ID: "first", URL: b.url("mqtt"), Version: mqttc.V311,
		ClientID: clientID, CleanStart: false,
		Subscriptions: []mqttc.Subscription{{Filter: topic, QoS: 1}},
	})
	// Give the subscription time to be registered before dropping it.
	first.publish(t, mqttc.PublishRequest{Topic: topic, Payload: []byte("first"), QoS: 1})
	first.await(t, topic, 15*time.Second)
	first.mgr.Shutdown(t.Context())

	// Published while nobody is listening.
	other := connect(t, mqttc.ConnectionSpec{
		ID: "other", URL: b.url("mqtt"), Version: mqttc.V311, CleanStart: true,
	})
	other.publish(t, mqttc.PublishRequest{Topic: topic, Payload: []byte("while away"), QoS: 1})

	// The same client id, resuming rather than starting fresh, and without
	// re-subscribing: if the session was kept, the queued message arrives.
	resumed := connect(t, mqttc.ConnectionSpec{
		ID: "resumed", URL: b.url("mqtt"), Version: mqttc.V311,
		ClientID: clientID, CleanStart: false,
	})
	if got := string(resumed.await(t, topic, 20*time.Second).Payload); got != "while away" {
		t.Errorf("the message queued for the offline session = %q", got)
	}
}

func TestTopicsWithTrailingAndDoubleSlashesRoundTrip(t *testing.T) {
	// Legal MQTT, produced by real devices, and the kind of thing a topic tree
	// gets wrong by normalising it away.
	b := start(t, config{})
	s := connect(t, mqttc.ConnectionSpec{
		URL: b.url("mqtt"), Version: mqttc.V311, CleanStart: true,
		Subscriptions: []mqttc.Subscription{{Filter: "mqttview/slashes/#", QoS: 1}},
	})

	for _, topic := range []string{
		"mqttview/slashes/trailing/",
		"mqttview/slashes//double",
	} {
		s.publish(t, mqttc.PublishRequest{Topic: topic, Payload: []byte("x"), QoS: 1})
		msg := s.await(t, topic, 15*time.Second)
		if msg.Topic != topic {
			t.Errorf("topic came back as %q rather than %q", msg.Topic, topic)
		}
	}
}

func TestTheTopicTreeMatchesWhatTheBrokerSent(t *testing.T) {
	// The tree is what the panel draws, and it is built from the same messages
	// asserted above. This checks the two agree.
	b := start(t, config{})
	s := connect(t, mqttc.ConnectionSpec{
		URL: b.url("mqtt"), Version: mqttc.V311, CleanStart: true,
		Subscriptions: []mqttc.Subscription{{Filter: "house/#", QoS: 1}},
	})

	for topic, payload := range map[string]string{
		"house/kitchen/temperature": "21.5",
		"house/kitchen/humidity":    "48",
		"house/garage/door":         "closed",
	} {
		s.publish(t, mqttc.PublishRequest{Topic: topic, Payload: []byte(payload), QoS: 1, Retain: true})
		s.await(t, topic, 15*time.Second)
	}

	value, ok := s.conn.Tree().Value("house/kitchen/temperature")
	if !ok {
		t.Fatalf("the tree has no house/kitchen/temperature:\n%s", treeSummary(s))
	}
	if string(value.Payload) != "21.5" {
		t.Errorf("the tree holds %q", value.Payload)
	}
	// Deliberately not asserting Retain here. The broker clears the flag when
	// it delivers to a subscription that already existed — it is only set when
	// a value is replayed to a new subscriber, which is what
	// TestRetainedMessagesArriveOnSubscribe covers.

	// The branch above it exists too, because the panel walks down to a value
	// rather than being handed one.
	rooms, ok := s.conn.Tree().Children("house")
	if !ok || len(rooms) != 2 {
		t.Errorf("house has %d children, want kitchen and garage:\n%s", len(rooms), treeSummary(s))
	}
}

func treeSummary(s *session) string {
	var b strings.Builder
	roots, _ := s.conn.Tree().Children("")
	for _, root := range roots {
		fmt.Fprintf(&b, "  %s\n", root.Topic)
		children, _ := s.conn.Tree().Children(root.Topic)
		for _, c := range children {
			fmt.Fprintf(&b, "    %s\n", c.Topic)
		}
	}
	return b.String()
}
