package mqttc_test

import (
	"fmt"
	"net"
	"testing"
	"time"

	mqtt "github.com/mochi-mqtt/server/v2"
	mochiauth "github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"
)

// startBroker runs an in-process MQTT broker for the duration of a test. Using
// a real broker rather than a mock means the tests exercise actual CONNECT,
// SUBSCRIBE and PUBLISH packets for both protocol versions.
func startBroker(t *testing.T) string {
	t.Helper()

	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	server := mqtt.New(&mqtt.Options{InlineClient: true})
	if err := server.AddHook(new(mochiauth.AllowHook), nil); err != nil {
		t.Fatalf("add auth hook: %v", err)
	}
	if err := server.AddListener(listeners.NewTCP(listeners.Config{
		ID:      "test",
		Address: addr,
	})); err != nil {
		t.Fatalf("add listener: %v", err)
	}

	go func() {
		if err := server.Serve(); err != nil {
			t.Logf("broker stopped: %v", err)
		}
	}()
	t.Cleanup(func() { _ = server.Close() })

	waitForListener(t, addr)
	return "tcp://" + addr
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
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
	t.Fatalf("broker at %s did not start", addr)
}
