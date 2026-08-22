// Command mqttview runs the mqttview server: an MQTT browser with realtime
// updates, authentication and plugins.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mqttview/mqttview/internal/api"
	"github.com/mqttview/mqttview/internal/auth"
	"github.com/mqttview/mqttview/internal/config"
	"github.com/mqttview/mqttview/internal/hub"
	"github.com/mqttview/mqttview/internal/mqttc"
	"github.com/mqttview/mqttview/internal/plugin"
	"github.com/mqttview/mqttview/internal/secrets"
	"github.com/mqttview/mqttview/internal/store"
	webui "github.com/mqttview/mqttview/web"

	// Bundled plugins register themselves on import. Adding a plugin to a
	// build is one import line.
	_ "github.com/mqttview/mqttview/internal/plugins/hass"
	_ "github.com/mqttview/mqttview/internal/plugins/plc"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mqttview:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath     = flag.String("config", envOr("MQTTVIEW_CONFIG", "mqttview.yaml"), "path to the YAML config file")
		addr           = flag.String("addr", "", "listen address, overrides the config file")
		dataDir        = flag.String("data", "", "data directory, overrides the config file")
		logLevel       = flag.String("log-level", envOr("MQTTVIEW_LOG_LEVEL", "info"), "debug, info, warn or error")
		bootstrapEmail = flag.String("bootstrap-email", envOr("MQTTVIEW_BOOTSTRAP_EMAIL", ""), "email for the admin account created on first run")
		bootstrapPass  = flag.String("bootstrap-password", os.Getenv("MQTTVIEW_BOOTSTRAP_PASSWORD"), "password for that account; generated if empty")
		showVersion    = flag.Bool("version", false, "print the version and exit")
		healthCheck    = flag.Bool("health-check", false, "query the health endpoint of a running instance and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("mqttview", version)
		return nil
	}

	// The container's HEALTHCHECK runs this rather than curl or wget, so the
	// image needs no shell utilities to report whether it is up.
	if *healthCheck {
		return probeHealth(*addr)
	}

	log := newLogger(*logLevel)
	slog.SetDefault(log)

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *addr != "" {
		cfg.Addr = *addr
	}
	if *dataDir != "" {
		cfg.DataDir = *dataDir
	}

	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	key, err := secrets.LoadOrCreateKey(cfg.SecretKey, cfg.DataDir)
	if err != nil {
		return err
	}
	box, err := secrets.New(key)
	if err != nil {
		return err
	}

	db, err := store.Open(cfg.DataDir+"/mqttview.db", box)
	if err != nil {
		return err
	}
	defer db.Close()

	authSvc := auth.New(db, cfg, box, log)
	created, generated, err := authSvc.BootstrapAdmin(*bootstrapEmail, *bootstrapPass)
	if err != nil {
		return fmt.Errorf("bootstrap admin: %w", err)
	}
	if created {
		email := *bootstrapEmail
		if email == "" {
			email = "admin@localhost"
		}
		if generated != "" {
			// Printed once, to stdout rather than the log, because it is a
			// credential and not an operational event.
			fmt.Printf("\n  mqttview created the first administrator account:\n"+
				"    email:    %s\n    password: %s\n"+
				"  Change it after signing in.\n\n", email, generated)
		} else {
			log.Info("created the first administrator account", "email", email)
		}
	}

	mgr := mqttc.NewManager(log)
	h := hub.New(log, api.OriginPatterns(cfg.BaseURL))

	// Every MQTT message and status change goes straight to the browsers.
	mgr.AddObserver(mqttc.Observer{
		OnMessage: h.BroadcastMessage,
		OnStatus:  h.BroadcastStatus,
	})

	plugins := plugin.NewRuntime(db, mgr, log, h.BroadcastEvent)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := loadConnections(ctx, db, mgr, log); err != nil {
		return err
	}
	if err := plugins.Start(ctx, pluginDefaults(cfg)); err != nil {
		return err
	}
	defer plugins.Stop()

	mgr.StartAutoConnect(ctx)

	webFS, err := webui.FS()
	if err != nil {
		return fmt.Errorf("load embedded frontend: %w", err)
	}

	srv := api.New(api.Options{
		Config:  cfg,
		Log:     log,
		Store:   db,
		Auth:    authSvc,
		MQTT:    mgr,
		Hub:     h,
		Plugins: plugins,
		Web:     webFS,
		Version: version,
	})

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: it would cut off long-lived WebSockets.
		IdleTimeout: 120 * time.Second,
	}

	go sessionSweeper(ctx, authSvc)

	errCh := make(chan error, 1)
	go func() {
		scheme := "http"
		if cfg.TLS.Enabled {
			scheme = "https"
		}
		log.Info("mqttview listening",
			"addr", cfg.Addr, "scheme", scheme, "version", version, "baseUrl", cfg.BaseURL)

		var err error
		if cfg.TLS.Enabled {
			err = httpSrv.ListenAndServeTLS(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		} else {
			err = httpSrv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("HTTP shutdown was not clean", "error", err)
	}
	mgr.Shutdown(shutdownCtx)
	return nil
}

// loadConnections restores saved broker definitions into the manager.
func loadConnections(ctx context.Context, db *store.Store, mgr *mqttc.Manager, log *slog.Logger) error {
	records, err := db.ListConnections()
	if err != nil {
		return fmt.Errorf("load connections: %w", err)
	}
	for _, rec := range records {
		if _, err := mgr.Upsert(ctx, rec.Spec); err != nil {
			// One malformed definition should not stop the server booting.
			log.Error("skipping unusable connection", "name", rec.Spec.Name, "error", err)
		}
	}
	log.Info("loaded connections", "count", len(records))
	return nil
}

// pluginDefaults converts the config file's plugin section into the runtime's
// first-run defaults.
func pluginDefaults(cfg config.Config) map[string]plugin.Defaults {
	out := make(map[string]plugin.Defaults, len(cfg.Plugins))
	for id, pc := range cfg.Plugins {
		out[id] = plugin.Defaults{Enabled: pc.Enabled, Settings: pc.Settings}
	}
	return out
}

// sessionSweeper deletes expired sessions hourly.
func sessionSweeper(ctx context.Context, a *auth.Service) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	a.PurgeSessions()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.PurgeSessions()
		}
	}
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// probeHealth asks a running instance whether it is healthy and turns the
// answer into an exit status, which is all a container HEALTHCHECK reads.
//
// It talks to the loopback address rather than to whatever the server binds,
// because the check runs inside the same network namespace and the endpoint is
// not meant to be reachable from outside it.
func probeHealth(addr string) error {
	if addr == "" {
		addr = envOr("MQTTVIEW_ADDR", "127.0.0.1:8114")
	}
	// A bind address of 0.0.0.0 or :port is not something to connect to.
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		addr = "127.0.0.1" + addr[i:]
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/api/health", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("health check: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check: %s", resp.Status)
	}
	return nil
}
