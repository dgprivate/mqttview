package main

import (
	"bytes"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dgprivate/mqttview/internal/auth"
	"github.com/dgprivate/mqttview/internal/config"
	"github.com/dgprivate/mqttview/internal/secrets"
	"github.com/dgprivate/mqttview/internal/store"
	"github.com/dgprivate/mqttview/internal/testutil"
)

// runWith calls run() with these arguments, as main would.
func runWith(t *testing.T, args ...string) error {
	t.Helper()
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs; resetFlags() })
	os.Args = append([]string{"mqttview"}, args...)
	resetFlags()
	return run()
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// serve starts run() in the background, waits until ready says it is up, and
// returns a function that stops it the way a container would and reports what
// run returned.
func serve(t *testing.T, ready func() bool, args ...string) (stop func() error) {
	t.Helper()
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs; resetFlags() })
	os.Args = append([]string{"mqttview"}, args...)
	resetFlags()

	errCh := make(chan error, 1)
	go func() { errCh <- run() }()

	// Up means the signal handler is installed, which is what makes SIGTERM
	// below safe to send to the test process.
	deadline := time.Now().Add(10 * time.Second)
	for !ready() {
		select {
		case err := <-errCh:
			t.Fatalf("run returned before it was up: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the server never came up")
		}
		time.Sleep(50 * time.Millisecond)
	}

	return func() error {
		if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-errCh:
			return err
		case <-time.After(30 * time.Second):
			t.Fatal("the server did not shut down on SIGTERM")
			return nil
		}
	}
}

// reopen opens the data directory a run left behind, with the key it wrote.
func reopen(t *testing.T, dir string) *store.Store {
	t.Helper()
	key, err := secrets.LoadOrCreateKey("", dir)
	if err != nil {
		t.Fatal(err)
	}
	box, err := secrets.New(key)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(dir, "mqttview.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// captureStdout returns whatever fn printed to standard output.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan []byte)
	go func() { b, _ := io.ReadAll(r); done <- b }()

	defer func() { os.Stdout = old }()
	fn()
	_ = w.Close()
	return string(<-done)
}

func writeConfig(t *testing.T, dir, yaml string) string {
	t.Helper()
	path := filepath.Join(dir, "mqttview.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A failure before the server is up is read by an operator from a container
// log, once. Each one has to say which thing on disk is wrong.

func TestADataDirectoryThatIsAFileStopsTheBootAndSaysSo(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "data")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	err := runWith(t, "-config", filepath.Join(t.TempDir(), "absent.yaml"), "-data", blocker)
	if err == nil {
		t.Fatal("a data directory that is a file was accepted")
	}
	if !strings.Contains(err.Error(), "data directory") || !strings.Contains(err.Error(), blocker) {
		t.Errorf("error %q does not name the data directory", err)
	}
}

func TestAnUnusableKeyFileStopsTheBootAndIsNamed(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "secret.key")
	if err := os.WriteFile(keyFile, []byte("not a key\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := runWith(t, "-config", filepath.Join(dir, "absent.yaml"), "-data", dir)
	if err == nil {
		t.Fatal("a corrupt key file was accepted")
	}
	if !strings.Contains(err.Error(), keyFile) {
		t.Errorf("error %q does not name the key file", err)
	}

	// Replacing it would make every stored credential unreadable for good.
	raw, _ := os.ReadFile(keyFile)
	if string(raw) != "not a key\n" {
		t.Error("the unusable key file was overwritten")
	}
}

func TestADatabaseThatCannotBeOpenedStopsTheBoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "mqttview.db"), 0o700); err != nil {
		t.Fatal(err)
	}

	err := runWith(t, "-config", filepath.Join(dir, "absent.yaml"), "-data", dir)
	if err == nil {
		t.Fatal("a database path that is a directory was accepted")
	}
	if !strings.Contains(err.Error(), "store") {
		t.Errorf("error %q does not say it was the database", err)
	}
}

// A first administrator with a weak password is the weakest account on the
// install, and the only one nobody chose interactively.
func TestAWeakBootstrapPasswordStopsTheBoot(t *testing.T) {
	dir := t.TempDir()

	err := runWith(t, "-config", filepath.Join(dir, "absent.yaml"), "-data", dir,
		"-addr", freeAddr(t), "-bootstrap-password", "short")
	if err == nil {
		t.Fatal("a short bootstrap password was accepted")
	}
	if !strings.Contains(err.Error(), "bootstrap admin") ||
		!strings.Contains(err.Error(), "at least") {
		t.Errorf("error %q does not say what the password needs", err)
	}
	if n, err := reopen(t, dir).CountUsers(); err != nil || n != 0 {
		t.Errorf("an account exists after the refusal: %d, %v", n, err)
	}
}

// With no password given, one is generated. It is printed exactly once, so
// what is printed has to be the password that works.
func TestAGeneratedFirstPasswordIsPrintedAndSignsIn(t *testing.T) {
	dir := t.TempDir()
	addr := freeAddr(t)

	var runErr error
	out := captureStdout(t, func() {
		stop := serve(t, func() bool { return probeHealth(addr) == nil },
			"-config", filepath.Join(dir, "absent.yaml"), "-data", dir,
			"-addr", addr, "-log-level", "error")
		runErr = stop()
	})
	if runErr != nil {
		t.Fatal(runErr)
	}

	m := regexp.MustCompile(`email:\s+(\S+)\s+password:\s+(\S+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no credentials were printed:\n%s", out)
	}
	if m[1] != "admin@localhost" {
		t.Errorf("printed email %q, want the default admin@localhost", m[1])
	}

	u, err := reopen(t, dir).GetUserByEmail(m[1])
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := auth.VerifyPassword(u.PasswordHash, m[2]); err != nil || !ok {
		t.Errorf("the printed password does not sign in: %v, %v", ok, err)
	}
	if u.Role != store.RoleAdmin {
		t.Errorf("the first account is %q, not an administrator", u.Role)
	}
}

// In Home Assistant mode there is no login form, so a bootstrap account would
// be a credential for a door that does not exist. The broker the app was
// pointed at must still be there on the first start.
func TestHomeAssistantModeCreatesNoAccountButKeepsItsBroker(t *testing.T) {
	dir := t.TempDir()
	addr := freeAddr(t)
	cfg := writeConfig(t, dir, `
auth:
  mode: ingress
connections:
  - name: Home Assistant
    url: mqtt://127.0.0.1:1
`)

	stop := serve(t, func() bool { return probeHealth(addr) == nil },
		"-config", cfg, "-data", dir, "-addr", addr, "-log-level", "error",
		"-bootstrap-password", "a-long-enough-password")
	if err := stop(); err != nil {
		t.Fatal(err)
	}

	db := reopen(t, dir)
	if n, err := db.CountUsers(); err != nil || n != 0 {
		t.Errorf("Home Assistant mode created %d accounts (%v)", n, err)
	}
	conns, err := db.ListConnections()
	if err != nil {
		t.Fatal(err)
	}
	if len(conns) != 1 || conns[0].Spec.Name != "Home Assistant" {
		t.Errorf("connections after the first start: %+v", conns)
	}
}

func TestWithTLSOnTheServerSpeaksOnlyHTTPS(t *testing.T) {
	dir := t.TempDir()
	addr := freeAddr(t)

	certPEM, keyPEM, err := testutil.SelfSignedPEM("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, []byte(certPEM), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, []byte(keyPEM), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := writeConfig(t, dir, "tls:\n  enabled: true\n  cert_file: "+certFile+"\n  key_file: "+keyFile+"\n")

	client := &http.Client{
		Timeout: 2 * time.Second,
		// The certificate is the one the test just made up.
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec
	}
	healthy := func() bool {
		resp, err := client.Get("https://" + addr + "/api/health")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}

	stop := serve(t, healthy, "-config", cfg, "-data", dir, "-addr", addr, "-log-level", "error",
		"-bootstrap-password", "a-long-enough-password")
	defer func() {
		if err := stop(); err != nil {
			t.Error(err)
		}
	}()

	// Plain HTTP to a TLS listener gets the standard 400, never the API.
	resp, err := http.Get("http://" + addr + "/api/health")
	if err == nil {
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Error("the health endpoint answered over plain HTTP with TLS on")
		}
	}
}

// A port somebody else holds is a fatal error, not a server that logs a line
// and then sits there answering nothing.
func TestAPortThatIsTakenIsReported(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	dir := t.TempDir()
	done := make(chan error, 1)
	go func() {
		done <- runWith(t, "-config", filepath.Join(dir, "absent.yaml"), "-data", dir,
			"-addr", ln.Addr().String(), "-log-level", "error",
			"-bootstrap-password", "a-long-enough-password")
	}()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "address already in use") {
			t.Errorf("run returned %v, want the bind failure", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("run kept going on a port it could not bind")
	}
}

// A database that cannot be read at boot must stop it. Starting anyway would
// show an install with no connections, and the next save would start writing
// over what is really there.
func TestAnUnreadableDatabaseStopsTheBootRatherThanLookingEmpty(t *testing.T) {
	db, mgr, log := seedHarness(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()

	if err := loadConnections(ctx, db, mgr, log); err == nil {
		t.Error("loading connections from an unreadable database succeeded")
	}
	seeds := []config.ConnectionSeed{{Name: "b", URL: "mqtt://127.0.0.1:1"}}
	if err := seedConnections(ctx, db, mgr, seeds, log); err == nil {
		t.Error("seeding into an unreadable database succeeded")
	}
	if len(mgr.List()) != 0 {
		t.Error("a connection was started from a database that could not be read")
	}
}

// -check-config prints what a file means. Two things in it matter beyond
// accuracy: a broker password never appears, and a shared fallback account is
// called out, because that is everybody using one identity.
func TestCheckConfigOutputShowsWhoButNeverTheBrokerPassword(t *testing.T) {
	local := config.Config{
		Addr: ":8114", DataDir: "/data",
		Connections: []config.ConnectionSeed{
			{Name: "home", URL: "mqtt://broker:1883", Username: "addons", Password: "hunter2"},
			{Name: "anon", URL: "mqtt://other:1883"},
		},
		FrameAncestors: []string{"https://ha.example"},
	}
	local.Auth.AllowLocal = true

	out := captureStdout(t, func() {
		if err := describeConfig(local, ""); err != nil {
			t.Error(err)
		}
	})
	for _, want := range []string{"no file", "password login:  true", "2 declared", "as addons", "https://ha.example"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not mention %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "hunter2") {
		t.Error("the broker password was printed")
	}

	ingress := config.Config{}
	ingress.Auth.Mode = config.ModeIngress
	ingress.Auth.Ingress.FallbackUser = "household"
	out = captureStdout(t, func() {
		if err := describeConfig(ingress, "/config/mqttview.yaml"); err != nil {
			t.Error(err)
		}
	})
	if !strings.Contains(out, "household") || !strings.Contains(out, "everybody shares") {
		t.Errorf("a shared fallback account was not called out:\n%s", out)
	}
	if bytes.Contains([]byte(out), []byte("password login")) {
		t.Error("ingress mode described a password login it does not have")
	}
}
