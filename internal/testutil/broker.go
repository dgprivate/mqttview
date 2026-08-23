// Package testutil provides an in-process MQTT broker for tests. It lives in a
// normal package rather than a _test file so that every package's tests can
// share one implementation.
package testutil

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
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
	t.Cleanup(func() { stopBroker(t, server) })

	waitForListener(t, addr)
	return &Broker{URL: "tcp://" + addr, Server: server}
}

// stopBroker shuts the broker down.
//
// Clients are disconnected first. Server.Close does that itself, but only
// after taking the listeners down, and the listener teardown waits on a
// WaitGroup that an attached client keeps held — so a test that leaves a
// client connected hangs there rather than failing.
//
// This used to be the site of an intermittent data race, and closing in a
// different order only moved it. The actual cause was elsewhere: a manager
// whose auto-connect loop was still dialling after Shutdown returned, landing
// on a port that FreePort had since handed to another test's broker. Manager
// now waits for those loops, and this is an ordinary close again.
func stopBroker(t *testing.T, server *mqtt.Server) {
	t.Helper()

	// Order matters, and each step is here because leaving it out breaks
	// something specific:
	//
	//  1. Stop the clients. The listener teardown waits on a WaitGroup an
	//     attached client holds, so skipping this hangs instead of failing.
	//  2. Close the listeners, and wait for their accept loops to return.
	//     After this nothing can be in EstablishConnection.
	//  3. Only then Close the server, which writes the field that
	//     EstablishConnection reads without synchronisation. Doing it while an
	//     accept was in flight is a data race the detector catches about one
	//     run in ten, reported against whichever test was running at the time.
	for _, cl := range server.Clients.GetAll() {
		cl.Stop(errors.New("testutil: broker stopping"))
	}
	server.Listeners.CloseAll(func(string) {})
	_ = server.Close()
}

// Publish sends a message straight from the broker, which is the easiest way
// for a test to simulate a device.
func (b *Broker) Publish(t *testing.T, topic string, payload []byte, retain bool) {
	t.Helper()
	if err := b.Server.Publish(topic, payload, retain, 0); err != nil {
		t.Fatalf("testutil: publish to %s: %v", topic, err)
	}
}

// DropClients disconnects every connected client without stopping the broker.
//
// From a client's side this is indistinguishable from the broker restarting,
// which is the event a reconnect loop exists for and the one that is otherwise
// impossible to provoke in a test.
func (b *Broker) DropClients() {
	for _, cl := range b.Server.Clients.GetAll() {
		cl.Stop(errors.New("testutil: connection dropped"))
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

// waitForListener blocks until the broker has bound its port.
//
// It proves that by failing to bind the port itself, rather than by dialling
// it. Dialling looks more natural and is what this used to do, but it hands
// the broker a connection to establish and immediately tear down — and a test
// that finishes quickly then closes the broker while that connection is still
// being established, which is a data race inside mochi and cost a long evening
// to track down. The probe that does not connect cannot cause it.
func waitForListener(t *testing.T, addr string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return // already bound, which is what we are waiting for
		}
		_ = l.Close()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("testutil: broker at %s did not bind its port", addr)
}

// SelfSignedPEM returns a self-signed certificate and its private key, both
// PEM encoded.
//
// For tests that need TLS material that parses rather than TLS material that
// is trusted: building a tls.Config, checking that a bad key is refused, and
// so on.
func SelfSignedPEM(commonName string) (certPEM, keyPEM string, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", "", err
	}

	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{commonName},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return "", "", err
	}

	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyPEM = string(pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
	return certPEM, keyPEM, nil
}

// HasSubscription reports whether any connected client is subscribed to the
// exact filter.
//
// It is the difference between "the plugin says it subscribed" and "the broker
// will deliver to it", and the tests that publish a retained message need the
// second one: a message published before the subscription lands is simply
// never seen.
func (b *Broker) HasSubscription(filter string) bool {
	for _, cl := range b.Server.Clients.GetAll() {
		for _, sub := range cl.State.Subscriptions.GetAll() {
			if sub.Filter == filter {
				return true
			}
		}
	}
	return false
}

// WaitFor blocks until cond returns true, and fails the test if it never does.
//
// It replaces `time.Sleep(100ms)` followed by an assertion. The sleep version
// has two failure modes and both are bad: on a fast machine it wastes the
// difference, and on a slow one it asserts before the thing it is waiting for
// has happened. When that assertion happens to pass anyway — because the test
// checks something weaker than it claims — the result is a green test that
// exercises nothing, which is exactly what made this project's coverage drop
// two points on a CI runner while staying green.
//
// desc is what the test is waiting for, phrased so the failure reads as a
// sentence: "timed out waiting for the broker to report connected".
func WaitFor(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	if cond() {
		return
	}
	t.Fatalf("timed out after %v waiting for %s", timeout, desc)
}
