package mqttc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/eclipse/paho.golang/autopaho"
)

// frameRecorder is a WebSocket endpoint that reports every frame it receives,
// one per channel send, so a test can see the framing rather than only the
// reassembled byte stream a broker would see.
type frameRecorder struct {
	frames      chan []byte
	subprotocol chan string
}

func newFrameRecorder() *frameRecorder {
	return &frameRecorder{frames: make(chan []byte, 16), subprotocol: make(chan string, 1)}
}

func (f *frameRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"mqtt"}})
	if err != nil {
		return
	}
	defer c.CloseNow()
	f.subprotocol <- c.Subprotocol()
	for {
		typ, data, err := c.Read(r.Context())
		if err != nil {
			close(f.frames)
			return
		}
		if typ != websocket.MessageBinary {
			data = append([]byte("non-binary:"), data...)
		}
		f.frames <- data
	}
}

func wsURL(t *testing.T, server *httptest.Server) *url.URL {
	t.Helper()
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	u.Scheme = strings.Replace(u.Scheme, "http", "ws", 1)
	return u
}

func dialCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func nextFrame(t *testing.T, frames <-chan []byte) []byte {
	t.Helper()
	select {
	case f, ok := <-frames:
		if !ok {
			t.Fatal("the connection closed before the frame arrived")
		}
		return f
	case <-time.After(5 * time.Second):
		t.Fatal("no frame arrived")
	}
	return nil
}

// The reason the dialer exists: Mosquitto 2.1 ends a packet at a zero-length
// frame and disconnects the client as malformed. A write of nothing must not
// become a frame, and the writes around it must arrive whole and in order.
func TestAnEmptyWriteNeverBecomesAWebSocketFrame(t *testing.T) {
	rec := newFrameRecorder()
	server := httptest.NewServer(rec)
	t.Cleanup(server.Close)

	conn, err := tlsDial(dialCtx(t), autopaho.ClientConfig{}, wsURL(t, server))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	for _, w := range []string{"head", "", "tail"} {
		n, err := conn.Write([]byte(w))
		if err != nil || n != len(w) {
			t.Fatalf("write %q = %d, %v; want %d, nil", w, n, err, len(w))
		}
	}

	if got := string(nextFrame(t, rec.frames)); got != "head" {
		t.Fatalf("first frame = %q, want %q", got, "head")
	}
	// Had the empty write become a frame, it would be this one.
	if got := string(nextFrame(t, rec.frames)); got != "tail" {
		t.Fatalf("second frame = %q, want %q: the empty write reached the wire", got, "tail")
	}
}

// Brokers refuse a WebSocket handshake that does not ask for "mqtt", so the
// dialer has to ask for it whatever else changes.
func TestTheWebSocketHandshakeAsksForTheMQTTSubprotocol(t *testing.T) {
	rec := newFrameRecorder()
	server := httptest.NewServer(rec)
	t.Cleanup(server.Close)

	conn, err := tlsDial(dialCtx(t), autopaho.ClientConfig{}, wsURL(t, server))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	select {
	case got := <-rec.subprotocol:
		if got != "mqtt" {
			t.Errorf("negotiated subprotocol = %q, want mqtt", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the server never saw the handshake")
	}
}

// Bytes from the broker reach the reader as a stream, whatever frames they
// arrived in.
func TestBytesFromTheBrokerArriveOverWebSockets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"mqtt"}})
		if err != nil {
			return
		}
		defer c.CloseNow()
		_ = c.Write(r.Context(), websocket.MessageBinary, []byte("con"))
		_ = c.Write(r.Context(), websocket.MessageBinary, []byte("nack"))
		_, _, _ = c.Read(r.Context()) // hold the connection open until the client goes
	}))
	t.Cleanup(server.Close)

	conn, err := tlsDial(dialCtx(t), autopaho.ClientConfig{}, wsURL(t, server))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	got := make([]byte, len("connack"))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "connack" {
		t.Errorf("read %q, want %q", got, "connack")
	}
}

// Secure WebSockets go through the connection's own TLS settings — its CA, its
// client certificate — and not the system's defaults. A broker with a private
// CA is the ordinary case for this, not an edge case.
func TestSecureWebSocketsUseTheConnectionsTLSSettings(t *testing.T) {
	rec := newFrameRecorder()
	server := httptest.NewTLSServer(rec)
	t.Cleanup(server.Close)
	u := wsURL(t, server)

	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	trusting := autopaho.ClientConfig{TlsCfg: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}

	conn, err := tlsDial(dialCtx(t), trusting, u)
	if err != nil {
		t.Fatalf("wss with the server's CA trusted: %v", err)
	}
	_ = conn.Close()

	// The same server with the defaults: its certificate is self-signed, so
	// succeeding here would mean the configured CA was never the one consulted.
	empty := autopaho.ClientConfig{TlsCfg: &tls.Config{RootCAs: x509.NewCertPool(), MinVersion: tls.VersionTLS12}}
	if conn, err := tlsDial(dialCtx(t), empty, u); err == nil {
		_ = conn.Close()
		t.Fatal("wss to an untrusted certificate succeeded")
	}
}

// A refused handshake is reported as a WebSocket failure, with the cause kept,
// so the diagnosis can say it was the upgrade that failed rather than a
// timeout somewhere later.
func TestARefusedWebSocketHandshakeSaysSo(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no websockets here", http.StatusForbidden)
	}))
	t.Cleanup(server.Close)

	_, err := tlsDial(dialCtx(t), autopaho.ClientConfig{}, wsURL(t, server))
	if err == nil {
		t.Fatal("a refused handshake produced a connection")
	}
	if !strings.Contains(err.Error(), "websocket") || !strings.Contains(err.Error(), "403") {
		t.Errorf("error = %q, want it to name the WebSocket upgrade and the status", err)
	}
}

// echoListener echoes back whatever each connection sends, so a dial can be
// shown to reach the right endpoint end to end.
func echoListener(t *testing.T, ln net.Listener) {
	t.Helper()
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
}

func assertEchoes(t *testing.T, conn net.Conn) {
	t.Helper()
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "ping" {
		t.Errorf("echo = %q, want ping", got)
	}
}

// Installing the dial hook makes it the only dialler for every scheme, not
// just WebSockets. Each of the others has to reach its endpoint, or the fix
// for one transport has broken the rest.
func TestEveryOtherSchemeStillReachesItsEndpoint(t *testing.T) {
	t.Run("tcp", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		echoListener(t, ln)

		conn, err := tlsDial(dialCtx(t), autopaho.ClientConfig{}, &url.URL{Scheme: "mqtt", Host: ln.Addr().String()})
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		assertEchoes(t, conn)
	})

	t.Run("tls", func(t *testing.T) {
		// httptest's certificate, served from a plain TLS listener: the
		// broker side of mqtts is TLS over TCP, not HTTPS.
		server := httptest.NewTLSServer(http.NotFoundHandler())
		serverCfg := server.TLS.Clone()
		pool := x509.NewCertPool()
		pool.AddCert(server.Certificate())
		server.Close()

		ln, err := tls.Listen("tcp", "127.0.0.1:0", serverCfg)
		if err != nil {
			t.Fatal(err)
		}
		echoListener(t, ln)
		cfg := autopaho.ClientConfig{TlsCfg: &tls.Config{RootCAs: pool, ServerName: "example.com", MinVersion: tls.VersionTLS12}}

		for _, scheme := range []string{"ssl", "tls", "mqtts", "mqtt+ssl", "tcps"} {
			conn, err := tlsDial(dialCtx(t), cfg, &url.URL{Scheme: scheme, Host: ln.Addr().String()})
			if err != nil {
				t.Fatalf("%s: dial: %v", scheme, err)
			}
			assertEchoes(t, conn)
		}
	})

	t.Run("unix", func(t *testing.T) {
		// Short, because a socket path has a length limit well below what
		// t.TempDir produces on some systems.
		dir, err := os.MkdirTemp("", "mq")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		path := filepath.Join(dir, "s")

		ln, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		echoListener(t, ln)

		conn, err := tlsDial(dialCtx(t), autopaho.ClientConfig{}, &url.URL{Scheme: "unix", Path: path})
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		assertEchoes(t, conn)
	})
}

// A dial that fails returns the error and no connection, for every scheme. A
// non-nil connection alongside an error would be used by the caller.
func TestAFailedDialReturnsTheErrorForEveryScheme(t *testing.T) {
	// A port that was open a moment ago and is now closed refuses at once,
	// rather than waiting out a timeout.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := ln.Addr().String()
	_ = ln.Close()

	for _, u := range []*url.URL{
		{Scheme: "mqtt", Host: closed},
		{Scheme: "mqtts", Host: closed},
		{Scheme: "unix", Path: filepath.Join(t.TempDir(), "nothing-listens-here")},
		{Scheme: "ws", Host: closed},
	} {
		conn, err := tlsDial(dialCtx(t), autopaho.ClientConfig{TlsCfg: &tls.Config{MinVersion: tls.VersionTLS12}}, u)
		if err == nil {
			_ = conn.Close()
			t.Errorf("%s: dial to nothing succeeded", u.Scheme)
			continue
		}
		if conn != nil {
			t.Errorf("%s: a connection came back alongside %v", u.Scheme, err)
		}
		var opErr *net.OpError
		if u.Scheme != "ws" && !errors.As(err, &opErr) {
			t.Errorf("%s: error %v lost the network cause", u.Scheme, err)
		}
	}
}
