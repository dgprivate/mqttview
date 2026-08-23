package mqttc

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"

	"github.com/coder/websocket"
	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/packets"
)

// MQTT 5 over WebSockets is dialled here rather than by autopaho, for one
// reason: autopaho turns every write into a WebSocket frame, and the library
// writes a packet in pieces — one of which, the empty property section of a
// SUBSCRIBE, is zero bytes long. That produces a zero-length binary frame in
// the middle of the packet.
//
// The frame is legal. RFC 6455 allows an empty payload, and the MQTT
// specification is explicit that a receiver "MUST NOT assume that MQTT Control
// Packets are aligned on WebSocket frame boundaries" — the frames are a
// transport for a byte stream, and a frame carrying no bytes contributes
// nothing to it.
//
// Mosquitto 2.1 disagrees. It ends the packet at the empty frame and
// disconnects the client with "malformed packet", so a WebSocket connection
// against 2.1 connects, subscribes to nothing and delivers nothing. 2.0
// accepts it, as does every other broker tested. Reported by
// test/mosquitto, which runs the matrix against both.
//
// Sending the frame gains nothing, so mqttview stops sending it. That is a
// smaller change than asking every user with 2.1 to switch transport, and it
// is not a workaround that hides anything: the bytes on the wire are identical
// either way.
func dialWebSocket(ctx context.Context, cfg autopaho.ClientConfig, u *url.URL) (net.Conn, error) {
	opts := &websocket.DialOptions{
		// Required by the MQTT specification, and brokers refuse the handshake
		// without it.
		Subprotocols: []string{"mqtt"},
	}
	if cfg.TlsCfg != nil {
		opts.HTTPClient = &http.Client{
			Transport: &http.Transport{TLSClientConfig: cfg.TlsCfg},
		}
	}

	// The handshake response is not a body to drain: on success the library has
	// already taken it apart, and on failure it keeps a copy for the error
	// message. Closed anyway when there is one, because a linter cannot know
	// that and neither can the next reader.
	conn, resp, err := websocket.Dial(ctx, u.String(), opts)
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("websocket connection failed: %w", err)
	}
	// Not ctx: that one covers the connection attempt and is cancelled once it
	// has succeeded, which would take the connection down with it.
	stream := websocket.NetConn(context.Background(), conn, websocket.MessageBinary)

	// NewThreadSafeConn is how paho keeps the several writes making up one
	// packet from interleaving with another goroutine's.
	return packets.NewThreadSafeConn(noEmptyFrames{stream}), nil
}

// noEmptyFrames drops writes with nothing in them, so they never become a
// frame. A write of zero bytes has written all zero of its bytes, which is
// what it reports.
type noEmptyFrames struct {
	net.Conn
}

func (c noEmptyFrames) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return c.Conn.Write(p)
}

// tlsDial is the plain and TLS half of AttemptConnection. Setting the hook at
// all means autopaho stops dialling for every scheme, not just the ones this
// file exists for, so the others have to keep working.
func tlsDial(ctx context.Context, cfg autopaho.ClientConfig, u *url.URL) (net.Conn, error) {
	switch u.Scheme {
	case "ws", "wss":
		return dialWebSocket(ctx, cfg, u)
	case "ssl", "tls", "mqtts", "mqtt+ssl", "tcps":
		d := tls.Dialer{Config: cfg.TlsCfg}
		conn, err := d.DialContext(ctx, "tcp", u.Host)
		if err != nil {
			return nil, err
		}
		return packets.NewThreadSafeConn(conn), nil
	case "unix":
		var d net.Dialer
		conn, err := d.DialContext(ctx, "unix", u.Path)
		if err != nil {
			return nil, err
		}
		return packets.NewThreadSafeConn(conn), nil
	default:
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", u.Host)
		if err != nil {
			return nil, err
		}
		return packets.NewThreadSafeConn(conn), nil
	}
}
