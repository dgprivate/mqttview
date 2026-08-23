package mqttc

import (
	"context"
	"testing"
	"time"

	"github.com/dgprivate/mqttview/internal/testutil"
)

// MQTT 5 carries properties alongside a payload, and mqttview's whole reason
// for supporting version 5 is showing them. A property that is set on the way
// out and missing on the way back is worse than not supporting them at all,
// because the panel then asserts the device did not send one.

func TestMQTT5PropertiesSurviveTheRoundTrip(t *testing.T) {
	broker := testutil.StartBroker(t)
	m := NewManager(nil)

	spec := ConnectionSpec{
		ID: "props", Name: "props", URL: broker.URL, Version: V5, CleanStart: true,
		Subscriptions: []Subscription{{Filter: "props/#", QoS: 1}},
	}
	if err := spec.Normalize(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Upsert(context.Background(), spec); err != nil {
		t.Fatal(err)
	}

	received := make(chan Message, 1)
	remove := m.AddObserver(Observer{OnMessage: func(msg Message) {
		select {
		case received <- msg:
		default:
		}
	}})
	defer remove()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := m.Connect(ctx, "props"); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer m.Shutdown(context.Background())

	format := byte(1)
	expiry := uint32(60)
	if err := m.Publish(ctx, "props", PublishRequest{
		Topic: "props/a", Payload: []byte("value"), QoS: 1,
		Props: &MessageProps{
			ContentType:     "application/json",
			ResponseTopic:   "props/reply",
			CorrelationData: []byte("abc"),
			PayloadFormat:   &format,
			MessageExpiry:   &expiry,
			User:            map[string]string{"origin": "test"},
		},
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	var got Message
	select {
	case got = <-received:
	case <-time.After(15 * time.Second):
		t.Fatal("the message never came back")
	}

	if got.Props == nil {
		t.Fatal("the properties were dropped between publish and receive")
	}
	if got.Props.ContentType != "application/json" {
		t.Errorf("content type = %q", got.Props.ContentType)
	}
	if got.Props.ResponseTopic != "props/reply" {
		t.Errorf("response topic = %q", got.Props.ResponseTopic)
	}
	if string(got.Props.CorrelationData) != "abc" {
		t.Errorf("correlation data = %q", got.Props.CorrelationData)
	}
	if got.Props.User["origin"] != "test" {
		t.Errorf("user properties = %v", got.Props.User)
	}
}

func TestAnMQTT5ConnectionCanCarryAWill(t *testing.T) {
	broker := testutil.StartBroker(t)
	m := NewManager(nil)
	defer m.Shutdown(context.Background())

	// A will with a delay exercises the WillProperties branch, which is not
	// reached by a plain will.
	spec := ConnectionSpec{
		ID: "will", Name: "will", URL: broker.URL, Version: V5, CleanStart: true,
		Will: &Will{
			Topic: "status/will", Payload: "gone", QoS: 1, Retain: true,
			DelayInterval: 5,
		},
	}
	if err := spec.Normalize(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Upsert(context.Background(), spec); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := m.Connect(ctx, "will"); err != nil {
		t.Fatalf("a connection with a delayed will was refused: %v", err)
	}
	if err := m.Disconnect(ctx, "will"); err != nil {
		t.Errorf("disconnect: %v", err)
	}
	// Disconnecting twice is what a double-click on the button does.
	if err := m.Disconnect(ctx, "will"); err != nil {
		t.Errorf("a second disconnect reported %v, want it to be a no-op", err)
	}
}

func TestPublishingOverMQTT5WhileDisconnected(t *testing.T) {
	broker := testutil.StartBroker(t)
	m := NewManager(nil)
	defer m.Shutdown(context.Background())

	spec := ConnectionSpec{
		ID: "offline", Name: "offline", URL: broker.URL, Version: V5, CleanStart: true,
	}
	if err := spec.Normalize(); err != nil {
		t.Fatal(err)
	}
	c, err := m.Upsert(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Never connected: the publish has to be refused rather than queued
	// silently, or an operator believes a command was sent.
	if err := c.Publish(ctx, PublishRequest{Topic: "a/b", Payload: []byte("x")}); err == nil {
		t.Error("publishing over a connection that was never up succeeded")
	}
	// Subscriptions, by contrast, are part of the definition and are applied
	// when the session comes up.
	if err := c.Subscribe(ctx, []Subscription{{Filter: "a/#"}}); err != nil {
		t.Errorf("subscribing while down: %v", err)
	}
	if err := c.Unsubscribe(ctx, []string{"a/#"}); err != nil {
		t.Errorf("unsubscribing while down: %v", err)
	}
	// An empty list is a no-op rather than an error, because that is what the
	// UI sends when nothing is selected.
	if err := c.Subscribe(ctx, nil); err != nil {
		t.Errorf("an empty subscribe: %v", err)
	}
	if err := c.Unsubscribe(ctx, nil); err != nil {
		t.Errorf("an empty unsubscribe: %v", err)
	}
}

func TestAnMQTT5SessionRecoversAfterTheBrokerRestarts(t *testing.T) {
	broker := testutil.StartBroker(t)
	m := NewManager(nil)
	defer m.Shutdown(context.Background())

	spec := ConnectionSpec{
		ID: "recover", Name: "recover", URL: broker.URL, Version: V5, CleanStart: true,
		Subscriptions: []Subscription{{Filter: "recover/#", QoS: 1}},
	}
	if err := spec.Normalize(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Upsert(context.Background(), spec); err != nil {
		t.Fatal(err)
	}

	received := make(chan Message, 8)
	remove := m.AddObserver(Observer{OnMessage: func(msg Message) {
		select {
		case received <- msg:
		default:
		}
	}})
	defer remove()

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	if err := m.Connect(ctx, "recover"); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// Dropping every client is what a broker restart looks like from here.
	broker.DropClients()

	// Recovery is not "the status flag came back": it is that a message
	// published afterwards is still delivered, which means the session and its
	// subscriptions were both re-established.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if err := m.Publish(ctx, "recover", PublishRequest{
			Topic: "recover/a", Payload: []byte("after"), QoS: 1,
		}); err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		select {
		case msg := <-received:
			if string(msg.Payload) == "after" {
				return
			}
		case <-time.After(time.Second):
		}
	}
	c, _ := m.Get("recover")
	t.Fatalf("no message was delivered after the broker dropped the session: %+v", c.Status())
}
