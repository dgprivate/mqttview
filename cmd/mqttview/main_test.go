package main

import (
	"context"
	"flag"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mqttview/mqttview/internal/auth"
	"github.com/mqttview/mqttview/internal/config"
	"github.com/mqttview/mqttview/internal/mqttc"
	"github.com/mqttview/mqttview/internal/secrets"
	"github.com/mqttview/mqttview/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()

	dir := t.TempDir()
	key, err := secrets.LoadOrCreateKey("", dir)
	if err != nil {
		t.Fatal(err)
	}
	box, err := secrets.New(key)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(dir, "test.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// resetFlags gives run() a clean FlagSet. It registers its flags on the
// default one, which would panic on a redefinition the second time a test
// calls it.
func resetFlags() {
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	flag.CommandLine.SetOutput(io.Discard)
}

func TestEnvOr(t *testing.T) {
	if got := envOr("MQTTVIEW_TEST_UNSET", "fallback"); got != "fallback" {
		t.Errorf("envOr = %q", got)
	}
	t.Setenv("MQTTVIEW_TEST_SET", "value")
	if got := envOr("MQTTVIEW_TEST_SET", "fallback"); got != "value" {
		t.Errorf("envOr = %q", got)
	}
	// An empty variable is treated as unset, so an accidental export= does not
	// blank a configured value.
	t.Setenv("MQTTVIEW_TEST_EMPTY", "")
	if got := envOr("MQTTVIEW_TEST_EMPTY", "fallback"); got != "fallback" {
		t.Errorf("envOr = %q", got)
	}
}

func TestNewLoggerLevels(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"error", slog.LevelError},
		{"WARN", slog.LevelWarn},
		{"nonsense", slog.LevelInfo},
		{"", slog.LevelInfo},
	} {
		log := newLogger(tc.in)
		if log.Enabled(context.Background(), tc.want-1) {
			t.Errorf("%q logs below %v", tc.in, tc.want)
		}
		if !log.Enabled(context.Background(), tc.want) {
			t.Errorf("%q does not log at %v", tc.in, tc.want)
		}
	}
}

func TestPluginDefaultsAreCarriedFromTheConfig(t *testing.T) {
	cfg := config.Config{Plugins: map[string]config.PluginConfig{
		"a": {Enabled: true, Settings: map[string]any{"k": "v"}},
		"b": {Enabled: false},
	}}

	got := pluginDefaults(cfg)
	if len(got) != 2 {
		t.Fatalf("got %d defaults", len(got))
	}
	if !got["a"].Enabled || got["a"].Settings["k"] != "v" {
		t.Errorf("plugin a = %+v", got["a"])
	}
	if got["b"].Enabled {
		t.Error("plugin b should be disabled")
	}
}

func TestLoadConnectionsSkipsTheUnusableOnes(t *testing.T) {
	db := testStore(t)
	mgr := mqttc.NewManager(quiet())

	good := mqttc.ConnectionSpec{
		ID: "good", Name: "good", URL: "mqtt://broker:1883", Version: mqttc.V311,
	}
	if err := db.SaveConnection(store.ConnectionRecord{Spec: good}); err != nil {
		t.Fatal(err)
	}

	// A row that no longer normalises — a URL scheme dropped in an upgrade,
	// say — must not stop the server booting.
	if _, err := db.DB().Exec(
		`UPDATE connections SET url = 'gopher://h:70' WHERE id = 'good'`); err != nil {
		t.Fatal(err)
	}

	if err := loadConnections(context.Background(), db, mgr, quiet()); err != nil {
		t.Fatalf("a bad row stopped startup: %v", err)
	}
	if len(mgr.List()) != 0 {
		t.Errorf("an unusable connection was registered: %+v", mgr.List())
	}
}

func TestLoadConnectionsRegistersTheGoodOnes(t *testing.T) {
	db := testStore(t)
	mgr := mqttc.NewManager(quiet())

	for _, id := range []string{"a", "b"} {
		if err := db.SaveConnection(store.ConnectionRecord{Spec: mqttc.ConnectionSpec{
			ID: id, Name: id, URL: "mqtt://broker:1883", Version: mqttc.V311,
		}}); err != nil {
			t.Fatal(err)
		}
	}

	if err := loadConnections(context.Background(), db, mgr, quiet()); err != nil {
		t.Fatal(err)
	}
	if len(mgr.List()) != 2 {
		t.Fatalf("registered %d connections", len(mgr.List()))
	}
}

func TestProbeHealth(t *testing.T) {
	// Answering, so the check passes.
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/health" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ok.Close()

	addr := strings.TrimPrefix(ok.URL, "http://")
	if err := probeHealth(addr); err != nil {
		t.Fatalf("a healthy server was reported unhealthy: %v", err)
	}

	// A bind address is not something to dial, so it is rewritten to loopback
	// before connecting — this is what the container HEALTHCHECK passes.
	_, port, _ := strings.Cut(addr, ":")
	if err := probeHealth("0.0.0.0:" + port); err != nil {
		t.Errorf("a 0.0.0.0 bind address was not rewritten: %v", err)
	}

	unhealthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer unhealthy.Close()
	if err := probeHealth(strings.TrimPrefix(unhealthy.URL, "http://")); err == nil {
		t.Error("a 503 was reported as healthy")
	}

	// Nothing listening is a failure, and the message says so.
	err := probeHealth("127.0.0.1:1")
	if err == nil || !strings.Contains(err.Error(), "health check") {
		t.Errorf("error = %v", err)
	}
}

func TestProbeHealthUsesTheEnvironmentWhenNoAddressIsGiven(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	t.Setenv("MQTTVIEW_ADDR", strings.TrimPrefix(srv.URL, "http://"))
	if err := probeHealth(""); err != nil {
		t.Fatalf("the address from the environment was not used: %v", err)
	}
}

func TestSessionSweeperStopsWithItsContext(t *testing.T) {
	// It runs for the life of the process, so the only thing to assert is that
	// it returns when told to rather than leaking a goroutine.
	db := testStore(t)
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	key, err := secrets.LoadOrCreateKey("", cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	box, err := secrets.New(key)
	if err != nil {
		t.Fatal(err)
	}
	svc := auth.New(db, cfg, box, quiet())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sessionSweeper(ctx, svc)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the sweeper did not stop with its context")
	}
}

func TestVersionFlagPrintsAndExits(t *testing.T) {
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs; resetFlags() })

	os.Args = []string{"mqttview", "-version"}
	resetFlags()
	if err := run(); err != nil {
		t.Fatalf("-version returned %v", err)
	}
}

func TestHealthCheckFlagUsesTheProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs; resetFlags() })

	os.Args = []string{"mqttview", "-health-check", "-addr", strings.TrimPrefix(srv.URL, "http://")}
	resetFlags()
	if err := run(); err != nil {
		t.Fatalf("-health-check returned %v", err)
	}
}

func TestPluginDefaultsOnAnEmptyConfig(t *testing.T) {
	if got := pluginDefaults(config.Config{}); len(got) != 0 {
		t.Errorf("got %d defaults from an empty config", len(got))
	}
}

// TestRunBootsAndShutsDownCleanly starts the whole server the way main does,
// waits for it to answer, and then stops it the way a container would.
//
// It is the only test that covers the wiring in run(): the parts it joins
// together — config, store, auth, plugins, HTTP — are each tested on their own,
// but nothing else proves they fit.
func TestRunBootsAndShutsDownCleanly(t *testing.T) {
	dir := t.TempDir()

	// A port the kernel picked, so parallel packages cannot collide.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs; resetFlags() })
	os.Args = []string{
		"mqttview",
		"-config", filepath.Join(dir, "absent.yaml"),
		"-addr", addr,
		"-data", dir,
		"-log-level", "error",
		"-bootstrap-email", "boot@example.com",
		"-bootstrap-password", "a-long-enough-password",
	}
	resetFlags()

	errCh := make(chan error, 1)
	go func() { errCh <- run() }()

	// Waiting for health proves the signal handler is installed, which is what
	// makes the interrupt below safe to send.
	healthy := false
	for i := 0; i < 100; i++ {
		if err := probeHealth(addr); err == nil {
			healthy = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !healthy {
		t.Fatal("the server never became healthy")
	}

	// The bootstrap account exists and the database was created where it was
	// told to.
	if _, err := os.Stat(filepath.Join(dir, "mqttview.db")); err != nil {
		t.Errorf("the database was not created in the data directory: %v", err)
	}

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("run returned %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the server did not shut down on SIGTERM")
	}
}

func TestRunRefusesAnUnusableConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mqttview.yaml")
	// Local login off with no provider: nobody could sign in, so it is refused
	// at startup rather than after somebody has deployed it.
	if err := os.WriteFile(path, []byte("auth:\n  allow_local: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs; resetFlags() })
	os.Args = []string{"mqttview", "-config", path, "-data", dir}
	resetFlags()

	if err := run(); err == nil {
		t.Fatal("a configuration nobody could sign in to was accepted")
	}
}
