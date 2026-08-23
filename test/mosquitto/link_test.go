package mosquitto_test

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// link is a TCP relay in front of the broker that can be cut mid-session.
//
// Two things nobody can test against a broker directly need it. A Last Will is
// only published when a client vanishes without saying goodbye, and the client
// API deliberately has no "vanish" — closing it sends DISCONNECT, which
// discards the will. And reconnection can only be observed if the connection
// breaks while both ends are still running. Killing the broker breaks both at
// once and proves neither.
//
// So the client connects to this instead, and the test pulls the cable.
type link struct {
	listener net.Listener
	target   string

	mu     sync.Mutex
	active []net.Conn
	closed bool
}

func newLink(t *testing.T, target string) *link {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	lk := &link{listener: l, target: target}
	t.Cleanup(lk.close)

	go lk.serve()
	return lk
}

func (l *link) addr() string { return l.listener.Addr().String() }

func (l *link) url() string { return "mqtt://" + l.addr() }

func (l *link) serve() {
	for {
		client, err := l.listener.Accept()
		if err != nil {
			return
		}
		broker, err := net.DialTimeout("tcp", l.target, 5*time.Second)
		if err != nil {
			client.Close()
			continue
		}

		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			client.Close()
			broker.Close()
			return
		}
		l.active = append(l.active, client, broker)
		l.mu.Unlock()

		go func() { _, _ = io.Copy(broker, client) }()
		go func() { _, _ = io.Copy(client, broker) }()
	}
}

// cut drops every connection currently running through the link, without
// closing the listener: the client can reconnect, and the broker sees the old
// session end the way a power cut ends it.
func (l *link) cut() {
	l.mu.Lock()
	conns := l.active
	l.active = nil
	l.mu.Unlock()

	for _, c := range conns {
		// A TCP reset rather than a FIN, so the broker treats it as a client
		// that disappeared rather than one that hung up politely. A FIN is
		// still an unclean MQTT disconnect and would publish the will too, but
		// a reset is what a lost network actually looks like.
		if tcp, ok := c.(*net.TCPConn); ok {
			_ = tcp.SetLinger(0)
		}
		_ = c.Close()
	}
}

func (l *link) close() {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	_ = l.listener.Close()
	l.cut()
}
