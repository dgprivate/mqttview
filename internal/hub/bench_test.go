package hub

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/dgprivate/mqttview/internal/mqttc"
)

// What a message costs on its way to the browsers.
//
// Every connected browser gets the same frame, and each one encodes it again:
// the writer goroutine marshals what it is handed. With one browser open that
// is invisible. With a phone, a tablet and two desktops on a busy broker it is
// the same JSON produced four times a message.

func benchClient(h *Hub, rate int) *Client {
	c := &Client{
		hub:   h,
		send:  make(chan []byte, 1024),
		watch: map[string]string{"c1": "#"},
		rate:  rate,
	}
	// Drain, so the send buffer is never the thing being measured.
	go func() {
		for range c.send {
		}
	}()
	return c
}

func benchHub(t testing.TB, clients int) *Hub {
	h := New(nil, nil)
	for range clients {
		c := benchClient(h, 1_000_000)
		h.mu.Lock()
		h.clients[c] = struct{}{}
		h.mu.Unlock()
	}
	return h
}

func benchMsg() mqttc.Message {
	return mqttc.Message{
		ConnectionID: "c1",
		Topic:        "house/kitchen/sensor/temperature",
		Payload:      []byte(`{"temperature":21.5,"humidity":48,"battery":97}`),
		QoS:          1,
		ReceivedAt:   time.Now(),
	}
}

func BenchmarkBroadcastMessage(b *testing.B) {
	for _, clients := range []int{1, 4, 16} {
		b.Run(name(clients), func(b *testing.B) {
			h := benchHub(b, clients)
			msg := benchMsg()

			b.ReportAllocs()
			for b.Loop() {
				h.BroadcastMessage(msg)
			}
		})
	}
}

// BenchmarkFrameEncode is the part of the cost that is per-client today and
// need not be: the same frame encoded once per browser.
func BenchmarkFrameEncode(b *testing.B) {
	f := Frame{Type: FrameMessage, Data: benchMsg()}

	b.ReportAllocs()
	for b.Loop() {
		if _, err := json.Marshal(f); err != nil {
			b.Fatal(err)
		}
	}
}

func name(n int) string {
	switch n {
	case 1:
		return "1 browser"
	case 4:
		return "4 browsers"
	default:
		return "16 browsers"
	}
}
