package mosquitto_test

import (
	"testing"
	"time"

	"github.com/dgprivate/mqttview/internal/mqttc"
)

// Clearing a retained message is a zero-length retained publish, and whether
// that works is the broker's decision rather than ours. The property worth
// asserting is not that mqttview sent the right packet: it is that a client
// connecting afterwards no longer receives the value.
func TestClearingARetainedMessageStopsLaterClientsSeeingIt(t *testing.T) {
	eachBroker(t, func(t *testing.T, start startFunc) {
		b := start(t, config{})
		topic := "mqttview/clear/temperature"

		publisher := connect(t, mqttc.ConnectionSpec{
			ID: "pub", URL: b.url("mqtt"), Version: mqttc.V311, CleanStart: true,
		})
		publisher.publish(t, mqttc.PublishRequest{
			Topic: topic, Payload: []byte("21.5"), QoS: 1, Retain: true,
		})

		// It really is retained: a client connecting now gets it.
		before := connect(t, mqttc.ConnectionSpec{
			ID: "before", URL: b.url("mqtt"), Version: mqttc.V311, CleanStart: true,
			Subscriptions: []mqttc.Subscription{{Filter: topic, QoS: 1}},
		})
		if msg := before.await(t, topic, 15*time.Second); string(msg.Payload) != "21.5" {
			t.Fatalf("payload = %q, want the retained value", msg.Payload)
		}

		// The clear itself: zero bytes, retain set.
		publisher.publish(t, mqttc.PublishRequest{
			Topic: topic, Payload: []byte{}, QoS: 1, Retain: true,
		})

		// And now a client connecting afterwards must receive nothing at all.
		after := connect(t, mqttc.ConnectionSpec{
			ID: "after", URL: b.url("mqtt"), Version: mqttc.V311, CleanStart: true,
			Subscriptions: []mqttc.Subscription{{Filter: topic, QoS: 1}},
		})
		after.awaitNothing(t, topic, 3*time.Second)
	})
}
