package hub

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dgprivate/mqttview/internal/mqttc"
	"github.com/dgprivate/mqttview/internal/testutil"
)

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// dial starts a hub behind an httptest server and returns a connected client.
func dial(t *testing.T) (*Hub, *websocket.Conn, context.Context) {
	t.Helper()

	h := New(quietLog(), nil)
	srv := httptest.NewServer(http.HandlerFunc(h.Serve))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })

	// Every client gets a hello frame first; read it so tests start from a
	// known point.
	if f := readFrame(t, ctx, conn); f.Type != FrameHello {
		t.Fatalf("first frame was %q, want hello", f.Type)
	}
	return h, conn, ctx
}

func readFrame(t *testing.T, ctx context.Context, conn *websocket.Conn) Frame {
	t.Helper()
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	_, data, err := conn.Read(readCtx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var f Frame
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("frame is not JSON: %v", err)
	}
	return f
}

// readUntil skips the periodic stats and pong frames a test does not care
// about, so a timing coincidence cannot fail an unrelated assertion.
func readUntil(t *testing.T, ctx context.Context, conn *websocket.Conn, want string) Frame {
	t.Helper()
	for i := 0; i < 20; i++ {
		f := readFrame(t, ctx, conn)
		if f.Type == want {
			return f
		}
	}
	t.Fatalf("no %q frame arrived", want)
	return Frame{}
}

func send(t *testing.T, ctx context.Context, conn *websocket.Conn, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestClientCountTracksConnections(t *testing.T) {
	h, conn, _ := dial(t)

	if got := h.Clients(); got != 1 {
		t.Fatalf("Clients() = %d, want 1", got)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")

	// The hub removes the client when its read loop ends.
	testutil.WaitFor(t, 5*time.Second, "the hub to drop the disconnected client", func() bool {
		return h.Clients() == 0
	})
}

func TestAMessageOnlyReachesAClientWatchingIt(t *testing.T) {
	h, conn, ctx := dial(t)

	// Nothing is watched yet, so this must not arrive.
	h.BroadcastMessage(mqttc.Message{ConnectionID: "c1", Topic: "a/b", Payload: []byte("1")})

	send(t, ctx, conn, map[string]any{"type": "watch", "connectionId": "c1", "filter": "a/#"})
	// A ping is a round trip, so once the pong comes back the watch has been
	// applied and the ordering is not a guess.
	send(t, ctx, conn, map[string]any{"type": "ping"})
	readUntil(t, ctx, conn, "pong")

	h.BroadcastMessage(mqttc.Message{ConnectionID: "c1", Topic: "a/b", Payload: []byte("2")})
	f := readUntil(t, ctx, conn, FrameMessage)

	raw, _ := json.Marshal(f.Data)
	var got mqttc.Message
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("message frame: %v", err)
	}
	if got.Topic != "a/b" || string(got.Payload) != "2" {
		t.Fatalf("got %q %q — the first, unwatched message may have leaked", got.Topic, got.Payload)
	}
}

func TestAFilterExcludesTopicsOutsideIt(t *testing.T) {
	h, conn, ctx := dial(t)

	send(t, ctx, conn, map[string]any{"type": "watch", "connectionId": "c1", "filter": "kitchen/#"})
	send(t, ctx, conn, map[string]any{"type": "ping"})
	readUntil(t, ctx, conn, "pong")

	h.BroadcastMessage(mqttc.Message{ConnectionID: "c1", Topic: "garage/door", Payload: []byte("x")})
	h.BroadcastMessage(mqttc.Message{ConnectionID: "c1", Topic: "kitchen/light", Payload: []byte("y")})

	f := readUntil(t, ctx, conn, FrameMessage)
	raw, _ := json.Marshal(f.Data)
	var got mqttc.Message
	_ = json.Unmarshal(raw, &got)
	if got.Topic != "kitchen/light" {
		t.Fatalf("got %q, want the garage message to have been filtered out", got.Topic)
	}
}

func TestUnwatchStopsDelivery(t *testing.T) {
	h, conn, ctx := dial(t)

	send(t, ctx, conn, map[string]any{"type": "watch", "connectionId": "c1", "filter": "#"})
	send(t, ctx, conn, map[string]any{"type": "unwatch", "connectionId": "c1"})
	send(t, ctx, conn, map[string]any{"type": "ping"})
	readUntil(t, ctx, conn, "pong")

	h.BroadcastMessage(mqttc.Message{ConnectionID: "c1", Topic: "a/b"})
	// Nothing should come back but the periodic frames; a message frame here
	// would mean unwatch did not take.
	send(t, ctx, conn, map[string]any{"type": "ping"})
	for i := 0; i < 6; i++ {
		f := readFrame(t, ctx, conn)
		if f.Type == FrameMessage {
			t.Fatal("a message arrived after unwatch")
		}
		if f.Type == "pong" {
			return
		}
	}
}

func TestStatusAndEventFramesReachEveryClient(t *testing.T) {
	h, conn, ctx := dial(t)

	// Status and plugin events are not filtered by topic: they are about the
	// connection, so every browser gets them.
	h.BroadcastStatus(mqttc.Status{ConnectionID: "c1", State: mqttc.StateConnected})
	if f := readUntil(t, ctx, conn, FrameStatus); f.Data == nil {
		t.Error("status frame carried no data")
	}

	h.BroadcastEvent("plugin:x:changed", map[string]any{"n": 1})
	f := readUntil(t, ctx, conn, FrameEvent)
	if f.Event != "plugin:x:changed" {
		t.Errorf("event = %q", f.Event)
	}
}

func TestMalformedAndUnknownCommandsAreReported(t *testing.T) {
	_, conn, ctx := dial(t)

	if err := conn.Write(ctx, websocket.MessageText, []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	if f := readUntil(t, ctx, conn, FrameError); !strings.Contains(toString(f.Data), "malformed") {
		t.Errorf("error frame said %q", toString(f.Data))
	}

	send(t, ctx, conn, map[string]any{"type": "nonsense"})
	if f := readUntil(t, ctx, conn, FrameError); !strings.Contains(toString(f.Data), "unknown command") {
		t.Errorf("error frame said %q", toString(f.Data))
	}
}

func TestWatchIsValidated(t *testing.T) {
	_, conn, ctx := dial(t)

	send(t, ctx, conn, map[string]any{"type": "watch"})
	if f := readUntil(t, ctx, conn, FrameError); !strings.Contains(toString(f.Data), "connectionId") {
		t.Errorf("error frame said %q", toString(f.Data))
	}

	// An invalid filter must be refused here rather than sent to the broker.
	send(t, ctx, conn, map[string]any{"type": "watch", "connectionId": "c1", "filter": "a/#/b"})
	if f := readUntil(t, ctx, conn, FrameError); f.Data == nil {
		t.Error("an invalid filter was accepted")
	}
}

func TestRateCommandIsAccepted(t *testing.T) {
	_, conn, ctx := dial(t)

	send(t, ctx, conn, map[string]any{"type": "rate", "maxRate": 10})
	send(t, ctx, conn, map[string]any{"type": "ping"})
	readUntil(t, ctx, conn, "pong")
}

func TestBroadcastingToNobodyIsHarmless(t *testing.T) {
	h := New(quietLog(), []string{"example.com"})

	h.BroadcastMessage(mqttc.Message{Topic: "a"})
	h.BroadcastStatus(mqttc.Status{})
	h.BroadcastEvent("x", nil)

	if h.Clients() != 0 {
		t.Error("a hub with no clients reported some")
	}
}

func TestServeRejectsAForeignOrigin(t *testing.T) {
	// The origin check is what stops a page on another site opening a socket
	// to this one with the browser's cookies attached.
	h := New(quietLog(), []string{"mqttview.example.com"})
	srv := httptest.NewServer(http.HandlerFunc(h.Serve))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	req.Header.Set("Origin", "https://evil.example.net")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusSwitchingProtocols {
		t.Fatal("a socket from a foreign origin was upgraded")
	}
	if h.Clients() != 0 {
		t.Error("a rejected upgrade still registered a client")
	}
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	raw, _ := json.Marshal(v)
	return string(raw)
}
