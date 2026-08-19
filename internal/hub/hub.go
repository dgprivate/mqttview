// Package hub pushes live MQTT traffic, connection status and plugin events
// to browsers over a single WebSocket per tab.
package hub

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/mqttview/mqttview/internal/mqttc"
)

// Frame is the envelope for everything sent to a browser.
type Frame struct {
	Type string `json:"type"`
	// Event names the plugin event when Type is "event".
	Event string `json:"event,omitempty"`
	Data  any    `json:"data,omitempty"`
}

// Frame types sent by the server.
const (
	FrameHello   = "hello"
	FrameMessage = "message"
	FrameStatus  = "status"
	FrameEvent   = "event"
	FrameStats   = "stats"
	FrameError   = "error"
)

// command is what a browser sends up the socket.
type command struct {
	Type string `json:"type"`
	// ConnectionID and Filter apply to watch/unwatch.
	ConnectionID string `json:"connectionId,omitempty"`
	Filter       string `json:"filter,omitempty"`
	// MaxRate caps messages per second for this client; 0 keeps the default.
	MaxRate int `json:"maxRate,omitempty"`
}

const (
	// sendBuffer is how many frames may queue for a slow browser before
	// messages start being dropped. A tab that cannot keep up loses live
	// updates rather than slowing the server down.
	sendBuffer = 512
	// defaultRate caps per-client message throughput; the topic tree still
	// records everything, so nothing is lost, only the live stream is thinned.
	defaultRate = 200
	// writeTimeout bounds a single frame write.
	writeTimeout = 10 * time.Second
	// pingInterval keeps intermediaries from closing an idle socket.
	pingInterval = 30 * time.Second
)

// Hub tracks connected browsers and fans events out to them.
type Hub struct {
	log            *slog.Logger
	originPatterns []string

	mu      sync.RWMutex
	clients map[*Client]struct{}
}

// New builds a hub. originPatterns is passed to the WebSocket accept check;
// same-origin requests are always allowed.
func New(log *slog.Logger, originPatterns []string) *Hub {
	if log == nil {
		log = slog.Default()
	}
	return &Hub{
		log:            log.With("component", "hub"),
		originPatterns: originPatterns,
		clients:        map[*Client]struct{}{},
	}
}

// Client is one browser socket.
type Client struct {
	hub  *Hub
	conn *websocket.Conn
	send chan Frame

	mu    sync.RWMutex
	watch map[string]string // connection ID -> topic filter
	rate  int

	dropped atomic.Uint64
	// tokens implements a one-second token bucket for message frames.
	tokenMu     sync.Mutex
	tokens      int
	windowStart time.Time
}

// Serve upgrades an HTTP request and runs the client until it disconnects.
func (h *Hub) Serve(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns:  h.originPatterns,
		CompressionMode: websocket.CompressionContextTakeover,
	})
	if err != nil {
		h.log.Debug("websocket upgrade rejected", "error", err)
		return
	}
	// Payloads can be large; the default 32 KiB read limit would kill the
	// socket on a big publish command.
	conn.SetReadLimit(8 << 20)

	c := &Client{
		hub:         h,
		conn:        conn,
		send:        make(chan Frame, sendBuffer),
		watch:       map[string]string{},
		rate:        defaultRate,
		tokens:      defaultRate,
		windowStart: time.Now(),
	}

	h.mu.Lock()
	h.clients[c] = struct{}{}
	count := len(h.clients)
	h.mu.Unlock()
	h.log.Debug("browser connected", "clients", count)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	go c.writeLoop(ctx)
	c.push(Frame{Type: FrameHello, Data: map[string]any{"maxRate": defaultRate}})
	c.readLoop(ctx)

	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
	_ = conn.CloseNow()
	h.log.Debug("browser disconnected")
}

// BroadcastMessage delivers an MQTT message to every client watching it.
func (h *Hub) BroadcastMessage(msg mqttc.Message) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for c := range h.clients {
		if !c.wants(msg) {
			continue
		}
		if !c.takeToken() {
			c.dropped.Add(1)
			continue
		}
		c.push(Frame{Type: FrameMessage, Data: msg})
	}
}

// BroadcastStatus delivers a connection status change to every client.
func (h *Hub) BroadcastStatus(st mqttc.Status) {
	h.broadcast(Frame{Type: FrameStatus, Data: st})
}

// BroadcastEvent delivers a named plugin event to every client.
func (h *Hub) BroadcastEvent(event string, payload any) {
	h.broadcast(Frame{Type: FrameEvent, Event: event, Data: payload})
}

func (h *Hub) broadcast(f Frame) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		c.push(f)
	}
}

// Clients reports how many browsers are connected.
func (h *Hub) Clients() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

func (c *Client) push(f Frame) {
	select {
	case c.send <- f:
	default:
		// The browser is not draining fast enough. Losing a frame is better
		// than blocking the MQTT goroutine that produced it.
		c.dropped.Add(1)
	}
}

func (c *Client) wants(msg mqttc.Message) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	filter, ok := c.watch[msg.ConnectionID]
	if !ok {
		return false
	}
	if filter == "" || filter == "#" {
		return true
	}
	return mqttc.MatchFilter(filter, msg.Topic)
}

// takeToken implements a fixed-window rate limit on message frames.
func (c *Client) takeToken() bool {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()

	if time.Since(c.windowStart) >= time.Second {
		c.windowStart = time.Now()
		c.tokens = c.rate
	}
	if c.tokens <= 0 {
		return false
	}
	c.tokens--
	return true
}

func (c *Client) readLoop(ctx context.Context) {
	for {
		typ, data, err := c.conn.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}

		var cmd command
		if err := json.Unmarshal(data, &cmd); err != nil {
			c.push(Frame{Type: FrameError, Data: "malformed command"})
			continue
		}

		switch cmd.Type {
		case "watch":
			if cmd.ConnectionID == "" {
				c.push(Frame{Type: FrameError, Data: "watch requires connectionId"})
				continue
			}
			if cmd.Filter != "" {
				if err := mqttc.ValidateFilter(cmd.Filter); err != nil {
					c.push(Frame{Type: FrameError, Data: err.Error()})
					continue
				}
			}
			c.mu.Lock()
			c.watch[cmd.ConnectionID] = cmd.Filter
			if cmd.MaxRate > 0 {
				c.rate = min(cmd.MaxRate, 5000)
			}
			c.mu.Unlock()

		case "unwatch":
			c.mu.Lock()
			delete(c.watch, cmd.ConnectionID)
			c.mu.Unlock()

		case "rate":
			if cmd.MaxRate > 0 {
				c.mu.Lock()
				c.rate = min(cmd.MaxRate, 5000)
				c.mu.Unlock()
			}

		case "ping":
			c.push(Frame{Type: "pong"})

		default:
			c.push(Frame{Type: FrameError, Data: "unknown command: " + cmd.Type})
		}
	}
}

func (c *Client) writeLoop(ctx context.Context) {
	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	stats := time.NewTicker(time.Second)
	defer stats.Stop()

	var lastDropped uint64

	for {
		select {
		case <-ctx.Done():
			return

		case f := <-c.send:
			if err := c.write(ctx, f); err != nil {
				return
			}

		case <-ping.C:
			pctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.conn.Ping(pctx)
			cancel()
			if err != nil {
				return
			}

		case <-stats.C:
			// Tell the UI when it is not seeing everything, rather than
			// letting it silently show a thinned stream.
			if d := c.dropped.Load(); d != lastDropped {
				lastDropped = d
				if err := c.write(ctx, Frame{Type: FrameStats, Data: map[string]any{"dropped": d}}); err != nil {
					return
				}
			}
		}
	}
}

func (c *Client) write(ctx context.Context, f Frame) error {
	raw, err := json.Marshal(f)
	if err != nil {
		c.hub.log.Warn("encoding frame failed", "type", f.Type, "error", err)
		return nil // a bad frame must not kill the socket
	}
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()

	if err := c.conn.Write(wctx, websocket.MessageText, raw); err != nil {
		if !errors.Is(err, context.Canceled) {
			c.hub.log.Debug("websocket write failed", "error", err)
		}
		return err
	}
	return nil
}
