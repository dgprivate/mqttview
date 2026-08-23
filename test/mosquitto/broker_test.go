// Package mosquitto_test connects mqttview's client to a real Mosquitto,
// once per transport and protocol version it claims to support.
//
// The unit tests use an in-process Go broker, which is fast, deterministic and
// written by the same kind of person who wrote the client — so the two can
// agree on a misreading of the specification and both be wrong together.
// Mosquitto is the broker almost every install actually points at, and it is
// strict where the specification is strict: a client id that is too long, a
// TLS handshake without a client certificate, a v3.1.1 client sending a v5
// property. Everything here is the real daemon, configured per mode, over a
// real socket.
//
// `go test -short` skips it, and so does any machine without mosquitto
// installed.
package mosquitto_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dgprivate/mqttview/internal/mqttc"
)

// daemon is where the package looks for Mosquitto. It is in sbin on Debian and
// Ubuntu, which is not on a normal user's PATH, so the absolute path is tried
// as well as the name.
var daemonPaths = []string{"mosquitto", "/usr/sbin/mosquitto", "/usr/local/sbin/mosquitto"}

func mosquittoPath(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("this package starts a real broker")
	}
	for _, p := range daemonPaths {
		if found, err := exec.LookPath(p); err == nil {
			return found
		}
	}
	t.Skip("mosquitto is not installed; apt-get install mosquitto")
	return ""
}

// broker is one running Mosquitto, configured for one mode.
type broker struct {
	port int
	dir  string
	log  *lockedBuffer
	cmd  *exec.Cmd
}

// config describes what to write into mosquitto.conf. Every field maps to one
// line of it, so a test reads as a description of the broker it is talking to.
type config struct {
	tls         bool
	requireCert bool // mutual TLS
	websockets  bool
	unixSocket  bool
	users       map[string]string // username → password, empty means anonymous
	extra       []string
}

// certs is the small PKI these tests need: one CA, a server certificate for
// 127.0.0.1 and a client certificate signed by the same CA.
type certs struct {
	caPEM         string
	serverCert    string
	serverKey     string
	clientCertPEM string
	clientKeyPEM  string
	// otherCAPEM is a second, unrelated CA, used to check that verification
	// actually verifies rather than accepting any certificate that parses.
	otherCAPEM string
}

var (
	pkiOnce sync.Once
	pki     certs
	pkiErr  error
)

// testPKI builds the certificates once per run. Generating a key per test is
// most of the runtime of a suite this size, and none of these are secret.
func testPKI(t *testing.T) certs {
	t.Helper()
	pkiOnce.Do(func() {
		ca, caKey, caPEM, err := newCA("mqttview test CA")
		if err != nil {
			pkiErr = err
			return
		}
		serverCert, serverKey, err := issue(ca, caKey, "localhost", []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")}, false)
		if err != nil {
			pkiErr = err
			return
		}
		clientCert, clientKey, err := issue(ca, caKey, "mqttview", nil, nil, true)
		if err != nil {
			pkiErr = err
			return
		}
		_, _, otherCAPEM, err := newCA("somebody else's CA")
		if err != nil {
			pkiErr = err
			return
		}
		pki = certs{
			caPEM:         caPEM,
			serverCert:    serverCert,
			serverKey:     serverKey,
			clientCertPEM: clientCert,
			clientKeyPEM:  clientKey,
			otherCAPEM:    otherCAPEM,
		}
	})
	if pkiErr != nil {
		t.Fatalf("building the test PKI: %v", pkiErr)
	}
	return pki
}

func newCA(name string) (*x509.Certificate, *ecdsa.PrivateKey, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, "", err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, "", err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, "", err
	}
	return cert, key, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), nil
}

func issue(ca *x509.Certificate, caKey *ecdsa.PrivateKey, cn string, dns []string, ips []net.IP, client bool) (certPEM, keyPEM string, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	usage := x509.ExtKeyUsageServerAuth
	if client {
		usage = x509.ExtKeyUsageClientAuth
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		DNSNames:     dns,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return "", "", err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})), nil
}

// start writes a mosquitto.conf for the mode and runs the daemon on a free
// port until the test ends.
func start(t *testing.T, cfg config) *broker {
	t.Helper()
	bin := mosquittoPath(t)
	dir := t.TempDir()
	port := freePort(t)

	var conf strings.Builder
	// 2.0 refuses to listen on anything but localhost without a listener line,
	// and refuses anonymous clients unless told: both are deliberate, and both
	// are why a config written for 1.x silently stops working.
	switch {
	case cfg.unixSocket:
		fmt.Fprintf(&conf, "listener 0 %s\n", filepath.Join(dir, "mqtt.sock"))
	default:
		fmt.Fprintf(&conf, "listener %d 127.0.0.1\n", port)
	}
	if cfg.websockets {
		conf.WriteString("protocol websockets\n")
	}
	if cfg.tls {
		ca := testPKI(t)
		write(t, filepath.Join(dir, "ca.crt"), ca.caPEM)
		write(t, filepath.Join(dir, "server.crt"), ca.serverCert)
		write(t, filepath.Join(dir, "server.key"), ca.serverKey)
		fmt.Fprintf(&conf, "cafile %s\n", filepath.Join(dir, "ca.crt"))
		fmt.Fprintf(&conf, "certfile %s\n", filepath.Join(dir, "server.crt"))
		fmt.Fprintf(&conf, "keyfile %s\n", filepath.Join(dir, "server.key"))
		fmt.Fprintf(&conf, "require_certificate %t\n", cfg.requireCert)
	}
	if len(cfg.users) == 0 {
		conf.WriteString("allow_anonymous true\n")
	} else {
		conf.WriteString("allow_anonymous false\n")
		pwfile := filepath.Join(dir, "passwd")
		write(t, pwfile, "")
		if err := os.Chmod(pwfile, 0o600); err != nil {
			t.Fatal(err)
		}
		for user, pass := range cfg.users {
			cmd := exec.Command("mosquitto_passwd", "-b", pwfile, user, pass)
			if raw, err := cmd.CombinedOutput(); err != nil {
				t.Skipf("mosquitto_passwd is unavailable: %v\n%s", err, raw)
			}
		}
		fmt.Fprintf(&conf, "password_file %s\n", pwfile)
	}
	for _, line := range cfg.extra {
		conf.WriteString(line + "\n")
	}

	confPath := filepath.Join(dir, "mosquitto.conf")
	write(t, confPath, conf.String())

	out := &lockedBuffer{}
	cmd := exec.Command(bin, "-c", confPath, "-v")
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	b := &broker{port: port, dir: dir, log: out, cmd: cmd}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		if t.Failed() {
			t.Logf("mosquitto.conf:\n%s\nbroker log:\n%s", conf.String(), out.String())
		}
	})

	b.waitUntilListening(t, cfg)
	return b
}

func (b *broker) waitUntilListening(t *testing.T, cfg config) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cfg.unixSocket {
			if _, err := os.Stat(b.socketPath()); err == nil {
				return
			}
		} else if conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", b.port), time.Second); err == nil {
			conn.Close()
			return
		}
		if b.cmd.ProcessState != nil {
			t.Fatalf("mosquitto exited before it listened:\n%s", b.log.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("mosquitto never listened:\n%s", b.log.String())
}

func (b *broker) socketPath() string { return filepath.Join(b.dir, "mqtt.sock") }

func (b *broker) url(scheme string) string {
	if scheme == "unix" {
		return "unix://" + b.socketPath()
	}
	return fmt.Sprintf("%s://127.0.0.1:%d", scheme, b.port)
}

// --------------------------------------------------------------------------
// Talking to it with mqttview's own client
// --------------------------------------------------------------------------

// session is a connected mqttview client plus the messages it has received.
type session struct {
	mgr      *mqttc.Manager
	conn     *mqttc.Conn
	messages chan mqttc.Message

	// states is every status the connection has passed through, in order.
	// Recorded rather than sampled: a client that loses its link and gets it
	// back inside a millisecond is working perfectly and invisible to any
	// amount of polling.
	statesMu sync.Mutex
	states   []mqttc.State
}

func (s *session) record(st mqttc.Status) {
	s.statesMu.Lock()
	defer s.statesMu.Unlock()
	if n := len(s.states); n == 0 || s.states[n-1] != st.State {
		s.states = append(s.states, st.State)
	}
}

func (s *session) seenStates() []mqttc.State {
	s.statesMu.Lock()
	defer s.statesMu.Unlock()
	return append([]mqttc.State(nil), s.states...)
}

// awaitStates waits for the connection to have passed through a sequence, in
// order, with anything else allowed in between.
func (s *session) awaitStates(t *testing.T, want []mqttc.State, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		seen := s.seenStates()
		i := 0
		for _, st := range seen {
			if st == want[i] {
				if i++; i == len(want) {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the connection went %v, wanted it to pass through %v", seen, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// connect brings up one connection through the manager the server uses, and
// fails the test if it does not reach the broker. The spec is passed in whole
// so each test states exactly what it is asking for.
func connect(t *testing.T, spec mqttc.ConnectionSpec) *session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mgr := mqttc.NewManager(nil)
	messages := make(chan mqttc.Message, 64)
	s := &session{mgr: mgr, messages: messages}
	mgr.AddObserver(mqttc.Observer{
		OnMessage: func(m mqttc.Message) {
			select {
			case messages <- m:
			default: // a test that stopped reading is not a reason to block the client
			}
		},
		OnStatus: s.record,
	})
	t.Cleanup(func() { mgr.Shutdown(context.Background()) })

	if spec.ID == "" {
		spec.ID = "c1"
	}
	if spec.Name == "" {
		spec.Name = "mosquitto"
	}
	conn, err := mgr.Upsert(ctx, spec)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := mgr.Connect(ctx, conn.Spec().ID); err != nil {
		t.Fatalf("connect to %s: %v", spec.URL, err)
	}
	if got := conn.Status().State; got != mqttc.StateConnected {
		t.Fatalf("connected, but the state is %q", got)
	}
	s.conn = conn
	return s
}

// tryConnect is connect for the cases that are supposed to fail.
func tryConnect(t *testing.T, spec mqttc.ConnectionSpec) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	mgr := mqttc.NewManager(nil)
	t.Cleanup(func() { mgr.Shutdown(context.Background()) })

	if spec.ID == "" {
		spec.ID = "c1"
	}
	if spec.Name == "" {
		spec.Name = "mosquitto"
	}
	if _, err := mgr.Upsert(ctx, spec); err != nil {
		return err
	}
	return mgr.Connect(ctx, spec.ID)
}

func (s *session) publish(t *testing.T, req mqttc.PublishRequest) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.conn.Publish(ctx, req); err != nil {
		t.Fatalf("publish to %s: %v", req.Topic, err)
	}
}

// await waits for a message on a topic. Anything else that arrives first is
// kept, because a test asserting on one topic should not be broken by another
// message overtaking it.
func (s *session) await(t *testing.T, topic string, timeout time.Duration) mqttc.Message {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case m := <-s.messages:
			if m.Topic == topic {
				return m
			}
		case <-deadline:
			t.Fatalf("no message on %q within %s", topic, timeout)
		}
	}
}

func (s *session) awaitNothing(t *testing.T, topic string, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case m := <-s.messages:
			if m.Topic == topic {
				t.Fatalf("a message arrived on %q that should not have: %q", topic, m.Payload)
			}
		case <-deadline:
			return
		}
	}
}

// --------------------------------------------------------------------------

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
