package hub

import (
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/mqttview/mqttview/internal/mqttc"
)

// A browser cannot keep up with a busy broker, and the hub's job is to be
// honest about that rather than to pretend the stream is complete.

func TestAClientThatCannotKeepUpIsToldWhatItMissed(t *testing.T) {
	h, conn, ctx := dial(t)

	// A rate of one frame a second, then a burst: everything after the first
	// message in the window is dropped by design.
	send(t, ctx, conn, map[string]any{
		"type": "watch", "connectionId": "c1", "filter": "#", "maxRate": 1,
	})
	// The requested rate takes effect when the current one-second window rolls
	// over, so the burst has to come after that and not before.
	time.Sleep(1100 * time.Millisecond)

	for i := 0; i < 50; i++ {
		h.BroadcastMessage(mqttc.Message{ConnectionID: "c1", Topic: "a/b", Seq: uint64(i)})
	}

	// The first frame gets through; the rest are counted and reported in a
	// stats frame, so the UI can say "you are not seeing everything".
	stats := readUntil(t, ctx, conn, FrameStats)
	data, ok := stats.Data.(map[string]any)
	if !ok {
		t.Fatalf("stats frame carried %T", stats.Data)
	}
	dropped, ok := data["dropped"].(float64)
	if !ok || dropped == 0 {
		t.Fatalf("stats reported %v dropped, want a positive count", data["dropped"])
	}
}

func TestARateAboveTheCeilingIsClamped(t *testing.T) {
	h, conn, ctx := dial(t)

	// Asking for a million frames a second is a way to defeat the limit; the
	// hub caps it rather than refusing, because the intent is legitimate.
	send(t, ctx, conn, map[string]any{
		"type": "watch", "connectionId": "c1", "filter": "#", "maxRate": 1_000_000,
	})
	time.Sleep(100 * time.Millisecond)

	h.BroadcastMessage(mqttc.Message{ConnectionID: "c1", Topic: "a/b"})
	if f := readUntil(t, ctx, conn, FrameMessage); f.Type != FrameMessage {
		t.Fatal("no message arrived after a clamped rate")
	}
}

func TestTheHubSurvivesAClientDisappearingMidBroadcast(t *testing.T) {
	h, conn, _ := dial(t)

	// Closing without a handshake is what a phone going into a tunnel does.
	_ = conn.CloseNow()

	for i := 0; i < 100; i++ {
		h.BroadcastMessage(mqttc.Message{ConnectionID: "c1", Topic: "a/b"})
		h.BroadcastStatus(mqttc.Status{ConnectionID: "c1", State: mqttc.StateConnected})
		h.BroadcastEvent("plugin", map[string]string{"k": "v"})
	}

	for i := 0; i < 50 && h.Clients() != 0; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if got := h.Clients(); got != 0 {
		t.Errorf("Clients() = %d after the socket went away", got)
	}
}

func TestANonTextFrameIsIgnoredRatherThanClosingTheSocket(t *testing.T) {
	h, conn, ctx := dial(t)

	if err := conn.Write(ctx, websocket.MessageBinary, []byte{0x00, 0x01}); err != nil {
		t.Fatalf("write: %v", err)
	}

	// The socket is still usable afterwards.
	send(t, ctx, conn, map[string]any{"type": "watch", "connectionId": "c1", "filter": "#"})
	time.Sleep(100 * time.Millisecond)
	h.BroadcastMessage(mqttc.Message{ConnectionID: "c1", Topic: "a/b"})

	if f := readUntil(t, ctx, conn, FrameMessage); f.Type != FrameMessage {
		t.Fatal("the socket stopped working after a binary frame")
	}
}

func TestWatchingWithoutAFilterMeansEverythingOnThatConnection(t *testing.T) {
	h, conn, ctx := dial(t)

	send(t, ctx, conn, map[string]any{"type": "watch", "connectionId": "c1"})
	time.Sleep(100 * time.Millisecond)

	h.BroadcastMessage(mqttc.Message{ConnectionID: "c1", Topic: "anything/at/all"})
	f := readUntil(t, ctx, conn, FrameMessage)

	data, ok := f.Data.(map[string]any)
	if !ok {
		t.Fatalf("message frame carried %T", f.Data)
	}
	if data["topic"] != "anything/at/all" {
		t.Errorf("topic = %v", data["topic"])
	}

	// A message on another connection is still not delivered: an empty filter
	// widens the topic, not the connection.
	h.BroadcastMessage(mqttc.Message{ConnectionID: "c2", Topic: "anything/at/all"})
	h.BroadcastStatus(mqttc.Status{ConnectionID: "c2", State: mqttc.StateConnected})
	if f := readUntil(t, ctx, conn, FrameStatus); f.Type != FrameStatus {
		t.Fatal("the status frame that follows was not delivered")
	}
}

func TestPingKeepsTheConnectionAlive(t *testing.T) {
	_, conn, ctx := dial(t)

	send(t, ctx, conn, map[string]any{"type": "ping"})
	if f := readUntil(t, ctx, conn, "pong"); f.Type != "pong" {
		t.Fatal("a ping was not answered")
	}
}
