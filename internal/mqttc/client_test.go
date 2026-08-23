package mqttc

import (
	"context"
	"crypto/tls"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dgprivate/mqttview/internal/testutil"
)

func TestBuildTLSConfig(t *testing.T) {
	// A plaintext scheme gets no TLS config at all, which is what tells the
	// client not to wrap the connection.
	plain := ConnectionSpec{URL: "tcp://broker:1883"}
	cfg, err := buildTLSConfig(plain)
	if err != nil || cfg != nil {
		t.Fatalf("a plaintext spec produced %v %v", cfg, err)
	}

	spec := ConnectionSpec{
		URL: "ssl://broker:8883",
		TLS: TLSSpec{
			InsecureSkipVerify: true,
			ServerName:         "broker.example.com",
			ALPN:               []string{"mqtt"},
			MinVersion:         "1.3",
		},
	}
	cfg, err = buildTLSConfig(spec)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.InsecureSkipVerify || cfg.ServerName != "broker.example.com" {
		t.Errorf("config = %+v", cfg)
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("min version = %x, want TLS 1.3", cfg.MinVersion)
	}
	if len(cfg.NextProtos) != 1 || cfg.NextProtos[0] != "mqtt" {
		t.Errorf("ALPN = %v", cfg.NextProtos)
	}

	// The default floor is 1.2; nothing below that is offered.
	spec.TLS.MinVersion = ""
	cfg, _ = buildTLSConfig(spec)
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("default min version = %x, want TLS 1.2", cfg.MinVersion)
	}
}

func TestBuildTLSConfigWithCertificates(t *testing.T) {
	cert, key := testKeypair(t)

	spec := ConnectionSpec{
		URL: "ssl://broker:8883",
		TLS: TLSSpec{CAPEM: cert, ClientCertPEM: cert, ClientKeyPEM: key},
	}
	cfg, err := buildTLSConfig(spec)
	if err != nil {
		t.Fatalf("a valid keypair was refused: %v", err)
	}
	if cfg.RootCAs == nil {
		t.Error("the CA bundle was not loaded")
	}
	if len(cfg.Certificates) != 1 {
		t.Error("the client certificate was not loaded")
	}

	// Rubbish must be reported rather than silently ignored: a connection that
	// quietly drops its client certificate fails much later and confusingly.
	bad := spec
	bad.TLS.CAPEM = "not a certificate"
	if _, err := buildTLSConfig(bad); err == nil {
		t.Error("an unusable CA bundle was accepted")
	}

	bad = spec
	bad.TLS.ClientCertPEM = "not a certificate"
	if _, err := buildTLSConfig(bad); err == nil {
		t.Error("an unusable client certificate was accepted")
	}
}

// testKeypair returns a self-signed certificate and its key in PEM form.
func testKeypair(t *testing.T) (certPEM, keyPEM string) {
	t.Helper()

	// Generated once per test run; the content does not matter, only that it
	// parses as a real keypair.
	c, k, err := testutil.SelfSignedPEM("mqttview-test")
	if err != nil {
		t.Fatal(err)
	}
	return c, k
}

func TestNewClientRefusesAnUnsupportedVersion(t *testing.T) {
	if _, err := NewClient(ConnectionSpec{URL: "tcp://h:1883", Version: Version(99)}, Events{}); err == nil {
		t.Fatal("an unknown protocol version was accepted")
	}
}

func TestConnectAndPublishOverMQTT5(t *testing.T) {
	broker := testutil.StartBroker(t)
	m := NewManager(nil)

	spec := ConnectionSpec{
		ID: "v5", Name: "v5", URL: broker.URL, Version: V5, CleanStart: true,
		Subscriptions: []Subscription{{Filter: "test/#", QoS: 1}},
	}
	if err := spec.Normalize(); err != nil {
		t.Fatal(err)
	}
	c, err := m.Upsert(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}

	var got Message
	done := make(chan struct{})
	remove := m.AddObserver(Observer{OnMessage: func(msg Message) {
		select {
		case <-done:
		default:
			got = msg
			close(done)
		}
	}})
	defer remove()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := m.Connect(ctx, "v5"); err != nil {
		t.Fatalf("connecting over MQTT 5 failed: %v", err)
	}
	if state := c.Status().State; state != StateConnected {
		t.Fatalf("state = %q", state)
	}

	if err := m.Publish(ctx, "v5", PublishRequest{
		Topic: "test/a", Payload: []byte("hello"), QoS: 1,
		Props: &MessageProps{ContentType: "text/plain"},
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the message never came back")
	}
	if got.Topic != "test/a" || string(got.Payload) != "hello" {
		t.Fatalf("received %q %q", got.Topic, got.Payload)
	}

	// Subscribing and unsubscribing at runtime, on a live session.
	if err := c.Subscribe(ctx, []Subscription{{Filter: "other/#", QoS: 0}}); err != nil {
		t.Errorf("subscribe: %v", err)
	}
	if err := c.Unsubscribe(ctx, []string{"other/#"}); err != nil {
		t.Errorf("unsubscribe: %v", err)
	}

	m.Shutdown(ctx)
	if state := c.Status().State; state == StateConnected {
		t.Error("the connection is still up after Shutdown")
	}
}

func TestConnectOverMQTT311AndResubscribeAfterReconnect(t *testing.T) {
	broker := testutil.StartBroker(t)
	m := NewManager(nil)

	spec := ConnectionSpec{
		ID: "v3", Name: "v3", URL: broker.URL, Version: V311, CleanStart: true,
		Subscriptions: []Subscription{{Filter: "test/#", QoS: 0}},
	}
	if err := spec.Normalize(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Upsert(context.Background(), spec); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := m.Connect(ctx, "v3"); err != nil {
		t.Fatal(err)
	}

	// Disconnecting and connecting again is what an operator does after
	// changing a setting; the subscriptions have to come back with it.
	if err := m.Disconnect(ctx, "v3"); err != nil {
		t.Fatal(err)
	}
	if err := m.Connect(ctx, "v3"); err != nil {
		t.Fatalf("reconnecting: %v", err)
	}

	c, _ := m.Get("v3")
	if len(c.Spec().Subscriptions) != 1 {
		t.Error("the subscription did not survive the reconnect")
	}
	m.Shutdown(ctx)
}

func TestConnectingToNothingReportsWhy(t *testing.T) {
	m := NewManager(nil)
	spec := ConnectionSpec{ID: "dead", Name: "dead", URL: "mqtt://127.0.0.1:1", Version: V311}
	if err := spec.Normalize(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Upsert(context.Background(), spec); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := m.Connect(ctx, "dead")
	if err == nil {
		t.Fatal("connecting to a closed port succeeded")
	}
	// The hint is the whole point of the diagnosis layer.
	if !strings.Contains(err.Error(), "mqtt://127.0.0.1:1") && !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Errorf("error does not name the broker: %v", err)
	}

	c, _ := m.Get("dead")
	if st := c.Status(); st.State != StateError || st.LastError == "" {
		t.Errorf("status after a failed connect = %+v", st)
	}
}

func TestAutoConnectRetriesUntilItIsToldToStop(t *testing.T) {
	m := NewManager(nil)
	spec := ConnectionSpec{
		ID: "auto", Name: "auto", URL: "mqtt://127.0.0.1:1", Version: V311, AutoConnect: true,
	}
	if err := spec.Normalize(); err != nil {
		t.Fatal(err)
	}
	c, err := m.Upsert(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}

	before := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	m.StartAutoConnect(ctx)

	// Waiting on the state rather than on the intent. The intent is recorded
	// when the attempt starts and the state when it fails, so waiting for the
	// first and asserting the second is a race the test lost about once in
	// fifty shuffled runs — it caught the supervisor mid-dial, in
	// "connecting", which is not a bug in anything but the assertion.
	testutil.WaitFor(t, 10*time.Second, "the first attempt to fail", func() bool {
		return c.Status().State == StateError
	})
	if !c.wantsConnection() {
		t.Error("the supervisor gave up wanting the connection after one refusal")
	}
	if st := c.Status(); !strings.Contains(st.LastError, "127.0.0.1:1") {
		t.Errorf("the recorded error does not name what was unreachable: %q", st.LastError)
	}

	// Cancelling the context is how shutdown stops it — and Shutdown waits for
	// the loops to have stopped, so this is a statement about what has already
	// happened rather than a sleep long enough to probably be true.
	cancel()
	m.Shutdown(context.Background())
	if runtime.NumGoroutine() > before+2 {
		t.Errorf("a retry loop outlived Shutdown: %d goroutines, started from %d",
			runtime.NumGoroutine(), before)
	}
}

func TestConnackHintsCoverTheCommonRefusals(t *testing.T) {
	spec := ConnectionSpec{URL: "tcp://broker:1883"}

	for _, tc := range []struct{ err, want string }{
		{"Connection Refused: Bad user name or password", "username and password"},
		{"Connection Refused: Not Authorized", "ACL"},
		{"Connection Refused: identifier rejected", "client ID"},
		{"Connection Refused: Server Unavailable", "not accepting connections"},
	} {
		got := explainConnectError(spec, errors.New(tc.err)).Error()
		if !strings.Contains(got, tc.want) {
			t.Errorf("%q did not produce a hint about %q: %s", tc.err, tc.want, got)
		}
	}
}
