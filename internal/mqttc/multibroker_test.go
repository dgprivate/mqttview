package mqttc_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/dgprivate/mqttview/internal/mqttc"
)

// TestSeveralBrokersAtOnce is the multi-connection guarantee: one mqttview
// instance talks to several unrelated brokers at the same time, each with its
// own protocol version and credentials, and neither their state nor their
// messages bleed into one another.
func TestSeveralBrokersAtOnce(t *testing.T) {
	brokerA := startBroker(t)
	brokerB := startBroker(t)
	brokerC := startBroker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mgr := mqttc.NewManager(nil)
	defer mgr.Shutdown(context.Background())

	// Deliberately different protocol versions: the manager must not assume a
	// single client implementation.
	specs := []mqttc.ConnectionSpec{
		{ID: "a", Name: "site A", URL: brokerA, Version: mqttc.V5, CleanStart: true,
			Subscriptions: []mqttc.Subscription{{Filter: "site/#", QoS: 1}}},
		{ID: "b", Name: "site B", URL: brokerB, Version: mqttc.V311, CleanStart: true,
			Subscriptions: []mqttc.Subscription{{Filter: "site/#", QoS: 1}}},
		{ID: "c", Name: "site C", URL: brokerC, Version: mqttc.V31, CleanStart: true,
			Subscriptions: []mqttc.Subscription{{Filter: "site/#", QoS: 1}}},
	}

	var mu sync.Mutex
	seen := map[string][]string{}
	mgr.AddObserver(mqttc.Observer{OnMessage: func(m mqttc.Message) {
		mu.Lock()
		seen[m.ConnectionID] = append(seen[m.ConnectionID], m.Topic)
		mu.Unlock()
	}})

	for _, spec := range specs {
		if _, err := mgr.Upsert(ctx, spec); err != nil {
			t.Fatalf("upsert %s: %v", spec.ID, err)
		}
		if err := mgr.Connect(ctx, spec.ID); err != nil {
			t.Fatalf("connect %s: %v", spec.ID, err)
		}
	}

	if got := len(mgr.List()); got != 3 {
		t.Fatalf("manager holds %d connections, want 3", got)
	}
	for _, spec := range specs {
		conn, _ := mgr.Get(spec.ID)
		if st := conn.Status(); st.State != mqttc.StateConnected {
			t.Fatalf("connection %s is %q, want connected", spec.ID, st.State)
		}
	}

	time.Sleep(300 * time.Millisecond)

	// Each broker gets a distinct topic. Since the brokers are unrelated, a
	// message must only ever surface on the connection that published it.
	payloads := map[string]string{"a": "site/a/temp", "b": "site/b/temp", "c": "site/c/temp"}
	for id, topic := range payloads {
		conn, _ := mgr.Get(id)
		if err := conn.Publish(ctx, mqttc.PublishRequest{
			Topic: topic, Payload: []byte(id), QoS: 1, Retain: true,
		}); err != nil {
			t.Fatalf("publish on %s: %v", id, err)
		}
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := len(seen["a"]) > 0 && len(seen["b"]) > 0 && len(seen["c"]) > 0
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	for id, wantTopic := range payloads {
		topics := seen[id]
		if len(topics) == 0 {
			t.Fatalf("connection %s received nothing", id)
		}
		for _, topic := range topics {
			if topic != wantTopic {
				t.Errorf("connection %s received %q, which belongs to another broker", id, topic)
			}
		}
	}

	// The same isolation has to hold for the derived state each connection
	// keeps, since that is what the UI reads.
	for id, wantTopic := range payloads {
		conn, _ := mgr.Get(id)
		if _, ok := conn.Tree().Value(wantTopic); !ok {
			t.Errorf("connection %s has no tree entry for its own topic %q", id, wantTopic)
		}
		for otherID, otherTopic := range payloads {
			if otherID == id {
				continue
			}
			if _, ok := conn.Tree().Value(otherTopic); ok {
				t.Errorf("connection %s leaked topic %q from connection %s", id, otherTopic, otherID)
			}
		}
		if topics, _, _ := conn.Tree().Stats(); topics != 1 {
			t.Errorf("connection %s tracks %d topics, want exactly its own", id, topics)
		}
	}
}

// TestRemovingOneBrokerLeavesTheOthers checks that tearing down a connection
// does not disturb its neighbours — the manager's map and the observer list
// are shared, so this is worth pinning down.
func TestRemovingOneBrokerLeavesTheOthers(t *testing.T) {
	brokerA := startBroker(t)
	brokerB := startBroker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	mgr := mqttc.NewManager(nil)
	defer mgr.Shutdown(context.Background())

	for id, url := range map[string]string{"a": brokerA, "b": brokerB} {
		if _, err := mgr.Upsert(ctx, mqttc.ConnectionSpec{
			ID: id, Name: id, URL: url, Version: mqttc.V311, CleanStart: true,
			Subscriptions: []mqttc.Subscription{{Filter: "#", QoS: 0}},
		}); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
		if err := mgr.Connect(ctx, id); err != nil {
			t.Fatalf("connect %s: %v", id, err)
		}
	}

	if err := mgr.Remove(ctx, "a"); err != nil {
		t.Fatalf("remove a: %v", err)
	}
	if _, ok := mgr.Get("a"); ok {
		t.Error("connection a is still registered after removal")
	}

	conn, ok := mgr.Get("b")
	if !ok {
		t.Fatal("connection b disappeared when a was removed")
	}
	if st := conn.Status(); st.State != mqttc.StateConnected {
		t.Fatalf("connection b is %q after removing a, want connected", st.State)
	}

	// It must still work, not merely still exist.
	time.Sleep(200 * time.Millisecond)
	if err := conn.Publish(ctx, mqttc.PublishRequest{Topic: "still/alive", Payload: []byte("yes")}); err != nil {
		t.Fatalf("publish on the surviving connection: %v", err)
	}
}
