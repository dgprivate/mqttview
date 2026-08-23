package mqttc_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/dgprivate/mqttview/internal/mqttc"
)

// TestRoundTripEveryVersion is the load-bearing test for the client layer:
// each supported protocol version must connect, subscribe, publish and
// receive against a real broker.
func TestRoundTripEveryVersion(t *testing.T) {
	url := startBroker(t)

	for _, tc := range []struct {
		name    string
		version mqttc.Version
	}{
		{"MQTT 3.1", mqttc.V31},
		{"MQTT 3.1.1", mqttc.V311},
		{"MQTT 5.0", mqttc.V5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr := mqttc.NewManager(nil)

			received := make(chan mqttc.Message, 8)
			mgr.AddObserver(mqttc.Observer{
				OnMessage: func(m mqttc.Message) { received <- m },
			})

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			conn, err := mgr.Upsert(ctx, mqttc.ConnectionSpec{
				ID:            "c-" + tc.name,
				Name:          tc.name,
				URL:           url,
				Version:       tc.version,
				CleanStart:    true,
				Subscriptions: []mqttc.Subscription{{Filter: "test/#", QoS: 1}},
			})
			if err != nil {
				t.Fatalf("upsert: %v", err)
			}
			t.Cleanup(func() { mgr.Shutdown(context.Background()) })

			if err := mgr.Connect(ctx, conn.Spec().ID); err != nil {
				t.Fatalf("connect: %v", err)
			}
			if got := conn.Status().State; got != mqttc.StateConnected {
				t.Fatalf("state = %q, want connected", got)
			}

			// The broker only routes to an established subscription, so give
			// the SUBSCRIBE a moment to land before publishing.
			time.Sleep(200 * time.Millisecond)

			if err := conn.Publish(ctx, mqttc.PublishRequest{
				Topic:   "test/room/temperature",
				Payload: []byte("21.5"),
				QoS:     1,
			}); err != nil {
				t.Fatalf("publish: %v", err)
			}

			select {
			case msg := <-received:
				if msg.Topic != "test/room/temperature" {
					t.Errorf("topic = %q", msg.Topic)
				}
				if string(msg.Payload) != "21.5" {
					t.Errorf("payload = %q", msg.Payload)
				}
				if msg.Seq == 0 {
					t.Error("message has no sequence number")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("did not receive the published message")
			}

			// The topic tree must have recorded the same value.
			value, ok := conn.Tree().Value("test/room/temperature")
			if !ok {
				t.Fatal("topic tree has no entry for the published topic")
			}
			if string(value.Payload) != "21.5" {
				t.Errorf("tree payload = %q", value.Payload)
			}

			if n := conn.History().Len(); n != 1 {
				t.Errorf("history holds %d messages, want 1", n)
			}
		})
	}
}

// TestRetainedMessagesPopulateTree checks that a client subscribing after the
// fact still sees retained state, which is what makes the tree useful on a
// broker that has been running for weeks.
func TestRetainedMessagesPopulateTree(t *testing.T) {
	url := startBroker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	publisher := mqttc.NewManager(nil)
	pub, err := publisher.Upsert(ctx, mqttc.ConnectionSpec{
		ID: "pub", Name: "publisher", URL: url, Version: mqttc.V311, CleanStart: true,
	})
	if err != nil {
		t.Fatalf("upsert publisher: %v", err)
	}
	if err := publisher.Connect(ctx, "pub"); err != nil {
		t.Fatalf("connect publisher: %v", err)
	}
	if err := pub.Publish(ctx, mqttc.PublishRequest{
		Topic: "retained/value", Payload: []byte("kept"), QoS: 1, Retain: true,
	}); err != nil {
		t.Fatalf("publish retained: %v", err)
	}
	publisher.Shutdown(ctx)

	subscriber := mqttc.NewManager(nil)
	defer subscriber.Shutdown(context.Background())

	sub, err := subscriber.Upsert(ctx, mqttc.ConnectionSpec{
		ID: "sub", Name: "subscriber", URL: url, Version: mqttc.V311, CleanStart: true,
		Subscriptions: []mqttc.Subscription{{Filter: "retained/#", QoS: 1}},
	})
	if err != nil {
		t.Fatalf("upsert subscriber: %v", err)
	}
	if err := subscriber.Connect(ctx, "sub"); err != nil {
		t.Fatalf("connect subscriber: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if v, ok := sub.Tree().Value("retained/value"); ok {
			if string(v.Payload) != "kept" {
				t.Fatalf("retained payload = %q", v.Payload)
			}
			if !v.Retain {
				t.Error("retained flag was not preserved")
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("retained message never arrived")
}

// TestEphemeralSubscriptionsStayOutOfTheSpec guards the boundary that lets a
// plugin subscribe without editing the user's saved connection.
func TestEphemeralSubscriptionsStayOutOfTheSpec(t *testing.T) {
	url := startBroker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	mgr := mqttc.NewManager(nil)
	defer mgr.Shutdown(context.Background())

	conn, err := mgr.Upsert(ctx, mqttc.ConnectionSpec{
		ID: "e1", Name: "ephemeral", URL: url, Version: mqttc.V311, CleanStart: true,
		Subscriptions: []mqttc.Subscription{{Filter: "user/#", QoS: 0}},
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := mgr.Connect(ctx, "e1"); err != nil {
		t.Fatalf("connect: %v", err)
	}

	if err := conn.SubscribeEphemeral(ctx, []mqttc.Subscription{{Filter: "plugin/#", QoS: 0}}); err != nil {
		t.Fatalf("ephemeral subscribe: %v", err)
	}

	subs := conn.Spec().Subscriptions
	if len(subs) != 1 || subs[0].Filter != "user/#" {
		t.Fatalf("ephemeral subscription leaked into the spec: %+v", subs)
	}

	var mu sync.Mutex
	var got []string
	mgr.AddObserver(mqttc.Observer{OnMessage: func(m mqttc.Message) {
		mu.Lock()
		got = append(got, m.Topic)
		mu.Unlock()
	}})

	time.Sleep(200 * time.Millisecond)
	if err := conn.Publish(ctx, mqttc.PublishRequest{Topic: "plugin/hello", Payload: []byte("x")}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("ephemeral subscription did not deliver a message")
}
