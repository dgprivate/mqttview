package mqttc

import (
	"fmt"
	"testing"
	"time"
)

// The ingest path, measured rather than argued about.
//
// Every message a broker delivers goes through Tree.Record and History.Add,
// under one lock each, on the client's read goroutine. A house on a busy
// broker is a few hundred messages a second; a badly behaved device or a
// `#` subscription on a large installation is thousands. These say what that
// costs before anybody optimises anything.

func benchMessage(topic string, payload []byte) Message {
	return Message{
		ConnectionID: "c1",
		Topic:        topic,
		Payload:      payload,
		QoS:          1,
		ReceivedAt:   time.Now(),
	}
}

func BenchmarkTreeRecordSameTopic(b *testing.B) {
	// The common case: a sensor publishing to the topic it always publishes
	// to, so every message updates a node that already exists.
	tree := NewTree()
	msg := benchMessage("house/kitchen/sensor/temperature", []byte("21.5"))

	b.ReportAllocs()
	for b.Loop() {
		tree.Record(msg)
	}
}

func BenchmarkTreeRecordManyTopics(b *testing.B) {
	// A tree that keeps growing, which is what a wildcard subscription on a
	// large installation looks like for the first few minutes.
	tree := NewTree()
	topics := make([]string, 1024)
	for i := range topics {
		topics[i] = fmt.Sprintf("house/room%d/device%d/state", i%32, i)
	}
	payload := []byte(`{"state":"on","brightness":128}`)

	b.ReportAllocs()
	i := 0
	for b.Loop() {
		tree.Record(benchMessage(topics[i%len(topics)], payload))
		i++
	}
}

func BenchmarkHistoryAdd(b *testing.B) {
	h := NewHistory(1000)
	msg := benchMessage("house/kitchen/sensor/temperature", []byte("21.5"))

	b.ReportAllocs()
	for b.Loop() {
		h.Add(msg)
	}
}

func BenchmarkHistoryAddLargePayload(b *testing.B) {
	// A camera snapshot or a firmware blob: the ring buffer copies what it
	// keeps, so the payload size is the cost.
	h := NewHistory(1000)
	msg := benchMessage("house/camera/snapshot", make([]byte, 64*1024))

	b.ReportAllocs()
	for b.Loop() {
		h.Add(msg)
	}
}
