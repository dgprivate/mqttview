// Package testutil provides an in-process MQTT broker for tests. It lives in a
// normal package rather than a _test file so that every package's tests can
// share one implementation.
package testutil

import (
	"fmt"
	"net"
	"testing"
	"time"

	mqtt "github.com/mochi-mqtt/server/v2"
	mochiauth "github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"
)

// Broker is a running test broker.
type Broker struct {
	// URL is the address clients should connect to, e.g. tcp://127.0.0.1:1883.
	URL string
	// Server is the underlying broker, exposed so tests can publish inline.
	Server *mqtt.Server
}

// StartBroker runs an MQTT broker that accepts any client, and stops it when
// the test finishes.
func StartBroker(t *testing.T) *Broker {
	t.Helper()

	addr := fmt.Sprintf("127.0.0.1:%d", FreePort(t))
	server := mqtt.New(&mqtt.Options{InlineClient: true})
	if err := server.AddHook(new(mochiauth.AllowHook), nil); err != nil {
		t.Fatalf("testutil: add auth hook: %v", err)
	}
	if err := server.AddListener(listeners.NewTCP(listeners.Config{ID: "test", Address: addr})); err != nil {
		t.Fatalf("testutil: add listener: %v", err)
	}

	go func() {
		if err := server.Serve(); err != nil {
			t.Logf("testutil: broker stopped: %v", err)
		}
	}()
	t.Cleanup(func() { _ = server.Close() })

	waitForListener(t, addr)
	return &Broker{URL: "tcp://" + addr, Server: server}
}

// Publish sends a message straight from the broker, which is the easiest way
// for a test to simulate a device.
func (b *Broker) Publish(t *testing.T, topic string, payload []byte, retain bool) {
	t.Helper()
	if err := b.Server.Publish(topic, payload, retain, 0); err != nil {
		t.Fatalf("testutil: publish to %s: %v", topic, err)
	}
}

// FreePort reserves and releases a port, returning its number.
func FreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("testutil: reserve port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func waitForListener(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("testutil: broker at %s did not start", addr)
}
