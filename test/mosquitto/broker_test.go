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
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
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

// --------------------------------------------------------------------------
// Which brokers the matrix runs against
// --------------------------------------------------------------------------

// One version is not a compatibility test. A broker people run is whatever
// their distribution shipped or their compose file pinned, and 2.0 and 2.1 are
// years apart: config directives have come and gone, and 2.1 rewrote the
// WebSocket support so it no longer needs libwebsockets. Everything in this
// package runs against each of these.
//
// Set MQTTVIEW_MOSQUITTO_IMAGES to change the list without editing this —
// comma-separated image references, or empty to test only the local broker.
var defaultImages = []string{
	"eclipse-mosquitto:2.0.22",
	// The 2.1 line publishes no plain tag; -alpine is the only one there is.
	"eclipse-mosquitto:2.1.2-alpine",
}

// startFunc starts a broker configured for one mode. Tests receive one per
// version rather than calling a package-level function, so a version cannot
// leak from one subtest into another.
type startFunc func(*testing.T, config) *broker

// runner knows how to run one version of Mosquitto.
type runner struct {
	name string
	// command runs mosquitto with a config file, both inside dir.
	command func(t *testing.T, dir, conf string) *exec.Cmd
	// cleanup runs after the process is killed, for anything the process
	// itself does not take with it.
	cleanup func()
	// version is what the broker should report on startup, or empty for the
	// local one, whose version is whatever the machine has.
	version string
}

// eachBroker runs fn once per broker version, as a subtest named after it.
func eachBroker(t *testing.T, fn func(t *testing.T, start startFunc)) {
	t.Helper()
	for _, r := range runners(t) {
		t.Run(r.name, func(t *testing.T) {
			fn(t, func(t *testing.T, cfg config) *broker {
				return startWith(t, r, cfg)
			})
		})
	}
}

func runners(t *testing.T) []runner {
	t.Helper()
	if testing.Short() {
		t.Skip("this package starts a real broker")
	}

	out := []runner{{
		name: "local",
		command: func(t *testing.T, _, conf string) *exec.Cmd {
			return exec.Command(mosquittoPath(t), "-c", conf, "-v")
		},
	}}

	images := defaultImages
	if v, ok := os.LookupEnv("MQTTVIEW_MOSQUITTO_IMAGES"); ok {
		images = nil
		for _, image := range strings.Split(v, ",") {
			if image = strings.TrimSpace(image); image != "" {
				images = append(images, image)
			}
		}
	}
	for _, image := range images {
		out = append(out, dockerRunner(t, image))
	}
	return out
}

var (
	dockerOnce sync.Once
	dockerOK   bool
	pulled     sync.Map // image → error from the pull
)

// dockerRunner runs a published Mosquitto image. Host networking and the
// temporary directory bind-mounted at the same path, so a config file naming
// /tmp/xxx/server.crt means the same thing on both sides and the tests do not
// have to know they are talking to a container.
func dockerRunner(t *testing.T, image string) runner {
	t.Helper()
	version := image
	if _, tag, ok := strings.Cut(image, ":"); ok {
		version = strings.TrimSuffix(tag, "-alpine")
	}

	return runner{
		name:    version,
		version: version,
		command: func(t *testing.T, dir, conf string) *exec.Cmd {
			requireDocker(t)
			pullOnce(t, image)

			name := fmt.Sprintf("mqttview-test-%d-%s", os.Getpid(), filepath.Base(dir))
			// Killing `docker run` kills the client, not the container, so it
			// is removed by name afterwards. --rm alone only covers a
			// container that exited on its own.
			t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
			// --user: the image runs as its own mosquitto user, which cannot
			// read a directory created by testing.T. Mosquitto needs no
			// privileges here, so it runs as whoever runs the test.
			return exec.Command("docker", "run", "--rm", "--name", name,
				"--network", "host",
				"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
				"-v", dir+":"+dir,
				image, "mosquitto", "-c", conf, "-v")
		},
	}
}

func requireDocker(t *testing.T) {
	t.Helper()
	dockerOnce.Do(func() {
		cmd := exec.Command("docker", "version", "--format", "{{.Server.Version}}")
		dockerOK = cmd.Run() == nil
	})
	if !dockerOK {
		t.Skip("docker is not available, so other Mosquitto versions cannot be run")
	}
}

// pullOnce fetches an image once per run. Without it every test pulls, and the
// first assertion in a suite becomes "is the network fast today".
func pullOnce(t *testing.T, image string) {
	t.Helper()
	v, _ := pulled.LoadOrStore(image, sync.OnceValue(func() error {
		cmd := exec.Command("docker", "pull", "--quiet", image)
		if raw, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("docker pull %s: %w\n%s", image, err, raw)
		}
		return nil
	}))
	if err := v.(func() error)(); err != nil {
		t.Skipf("%v", err)
	}
}

// broker is one running Mosquitto, configured for one mode.
type broker struct {
	host    string
	port    int
	dir     string
	log     *lockedBuffer
	cmd     *exec.Cmd
	version string // what it reported on startup
}

var brokerHosts atomic.Uint32

// brokerHost hands out a loopback address of its own to each broker.
//
// A free port is not enough. `go test ./...` runs each package as its own
// process, they all ask the kernel for a port the same way, and a port one of
// them tested and released is one another can be given moments later. That
// happened: a WebSocket test found its broker's listener missing, because the
// port had gone to a server in another package, and the log said so — "Unable
// to create websockets listener on port 38035" — after this harness had
// already decided the broker was up.
//
// All of 127.0.0.0/8 is loopback on Linux, so every broker gets an address
// nothing else in any process is using, and the port stops mattering.
func brokerHost() string {
	n := brokerHosts.Add(1)
	return fmt.Sprintf("127.%d.%d.%d", 30+(n>>16)%100, (n>>8)&0xFF, n&0xFF)
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

// certs is the small PKI these tests need: one CA, a client certificate signed
// by it, and per-broker server certificates issued from it on demand.
type certs struct {
	// The CA is kept, not just its certificate: each broker gets a server
	// certificate issued for the loopback address it is listening on.
	ca            *x509.Certificate
	caKey         *ecdsa.PrivateKey
	caPEM         string
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
			ca:            ca,
			caKey:         caKey,
			caPEM:         caPEM,
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

// startWith writes a mosquitto.conf for the mode and runs one version of the
// daemon on a free port until the test ends.
func startWith(t *testing.T, r runner, cfg config) *broker {
	t.Helper()
	// The directory is bind-mounted into a container for the image runners,
	// and 0700 from testing.T is not enough for the process inside it to walk
	// into. Throwaway certificates in /tmp are not a secret being spilled.
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	host := brokerHost()
	port := freePort(t)

	var conf strings.Builder
	// 2.0 refuses to listen on anything but localhost without a listener line,
	// and refuses anonymous clients unless told: both are deliberate, and both
	// are why a config written for 1.x silently stops working.
	switch {
	case cfg.unixSocket:
		fmt.Fprintf(&conf, "listener 0 %s\n", filepath.Join(dir, "mqtt.sock"))
	default:
		fmt.Fprintf(&conf, "listener %d %s\n", port, host)
	}
	if cfg.websockets {
		conf.WriteString("protocol websockets\n")
	}
	if cfg.tls {
		ca := testPKI(t)
		// The server certificate is issued for this broker's own address, so
		// verification is real rather than waved through.
		serverCert, serverKey, err := issue(ca.ca, ca.caKey, "localhost",
			[]string{"localhost"}, []net.IP{net.ParseIP(host)}, false)
		if err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(dir, "ca.crt"), ca.caPEM)
		write(t, filepath.Join(dir, "server.crt"), serverCert)
		write(t, filepath.Join(dir, "server.key"), serverKey)
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
	cmd := r.command(t, dir, confPath)
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	b := &broker{host: host, port: port, dir: dir, log: out, cmd: cmd, version: r.version}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		if r.cleanup != nil {
			r.cleanup()
		}
		if t.Failed() {
			t.Logf("mosquitto.conf:\n%s\nbroker log:\n%s", conf.String(), out.String())
		}
	})

	b.waitUntilListening(t, cfg)
	b.checkVersion(t)
	b.checkItIsOurs(t, cfg)
	return b
}

// checkItIsOurs proves the broker answering on the port is the one this test
// started. Something else holding it would make our own broker fail to bind
// and exit, leaving the test talking to a stranger and asserting nonsense
// about it.
func (b *broker) checkItIsOurs(t *testing.T, cfg config) {
	t.Helper()
	if cfg.unixSocket {
		return // a path it created is a path nobody else has
	}
	want := fmt.Sprintf("listen socket on port %d", b.port)
	deadline := time.Now().Add(10 * time.Second)
	for {
		log := b.log.String()
		if strings.Contains(log, want) {
			return
		}
		// Any error line, not a list of known ones. Mosquitto reported "Unable
		// to create websockets listener on port N" right after the line this
		// waits for, and a check that knew about "Unable to bind" and nothing
		// else declared the broker healthy anyway.
		if i := strings.Index(log, "Error: "); i >= 0 {
			t.Fatalf("the broker failed to start what it was asked to:\n%s", log[i:])
		}
		if time.Now().After(deadline) {
			t.Fatalf("the broker never said it bound port %d:\n%s", b.port, log)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// checkVersion reads the version out of the startup banner. Without it a
// matrix that silently ran one broker three times would look like broad
// coverage: an image tag can be wrong, and a runner can hand back something
// cached under a name that no longer means what it says.
func (b *broker) checkVersion(t *testing.T) {
	t.Helper()
	banner := regexp.MustCompile(`mosquitto version (\S+)`)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if m := banner.FindStringSubmatch(b.log.String()); m != nil {
			if b.version != "" && m[1] != b.version {
				t.Fatalf("asked for Mosquitto %s and got %s", b.version, m[1])
			}
			b.version = m[1]
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the broker never said which version it is:\n%s", b.log.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (b *broker) waitUntilListening(t *testing.T, cfg config) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cfg.unixSocket {
			if _, err := os.Stat(b.socketPath()); err == nil {
				return
			}
		} else if conn, err := net.DialTimeout("tcp", b.addr(), time.Second); err == nil {
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

func (b *broker) addr() string { return fmt.Sprintf("%s:%d", b.host, b.port) }

func (b *broker) url(scheme string) string {
	if scheme == "unix" {
		return "unix://" + b.socketPath()
	}
	return fmt.Sprintf("%s://%s", scheme, b.addr())
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
	// Waited for rather than read. Connect returns when the session is up and
	// the status is published from the client's own callback, so the two are
	// ordered but not simultaneous — reading it here caught "connecting"
	// occasionally, which is a race in the assertion and not in the client.
	deadline := time.Now().Add(15 * time.Second)
	for conn.Status().State != mqttc.StateConnected {
		if time.Now().After(deadline) {
			t.Fatalf("connected to %s, but the state stayed %q", spec.URL, conn.Status().State)
		}
		time.Sleep(20 * time.Millisecond)
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

var (
	portMu    sync.Mutex
	portsUsed = map[int]bool{}
)

// freePort asks the operating system for a port and then refuses to hand out
// one this process has already given away.
//
// The bind-and-close trick alone is not enough: a port closed a moment ago is
// a port the kernel will happily offer again, and with several brokers being
// started at once that produced two tests sharing one number. The symptom was
// worth the fix — a plain-TCP test reached another test's TLS listener and
// reported "the broker closed the connection immediately", which is true,
// unhelpful, and about a broker it was never supposed to be talking to.
func freePort(t *testing.T) int {
	t.Helper()
	portMu.Lock()
	defer portMu.Unlock()

	for range 100 {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := l.Addr().(*net.TCPAddr).Port
		l.Close()
		if !portsUsed[port] {
			portsUsed[port] = true
			return port
		}
	}
	t.Fatal("no unused port after a hundred tries")
	return 0
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

// waitFor retries a condition until it holds or the time runs out. Used where
// the thing being waited on is a message that may be published again in the
// meantime, which a plain sleep-and-check cannot express.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}
