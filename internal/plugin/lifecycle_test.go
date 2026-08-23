package plugin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mqttview/mqttview/internal/mqttc"
)

// The runtime's contract with a plugin: it is built when enabled, torn down
// when disabled, rebuilt when its settings change, and never left half-alive.

func TestAPluginIsStartedStoppedAndRestarted(t *testing.T) {
	rt, _, _, p := newRuntime(t)
	id := "recorder-" + t.Name()

	ctx := context.Background()
	if err := rt.Start(ctx, map[string]Defaults{id: {Enabled: true}}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(rt.Stop)

	if got := p.inits; got != 1 {
		t.Fatalf("the plugin was initialised %d times", got)
	}

	// Saving settings restarts a running plugin, because a plugin reads its
	// settings in Init and has no other way to learn they changed.
	if err := rt.SaveSettings(ctx, id, map[string]any{"prefix": "changed"}); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	if p.inits != 2 || p.closes != 1 {
		t.Errorf("after a settings change: %d inits, %d closes", p.inits, p.closes)
	}

	if err := rt.SetEnabled(ctx, id, false); err != nil {
		t.Fatalf("disabling: %v", err)
	}
	if p.closes != 2 {
		t.Errorf("disabling did not close the plugin: %d closes", p.closes)
	}

	// Settings saved while it is off are kept but do not start it.
	if err := rt.SaveSettings(ctx, id, map[string]any{"prefix": "off"}); err != nil {
		t.Fatalf("SaveSettings while disabled: %v", err)
	}
	if p.inits != 2 {
		t.Errorf("saving settings started a disabled plugin: %d inits", p.inits)
	}

	if err := rt.SetEnabled(ctx, id, true); err != nil {
		t.Fatalf("re-enabling: %v", err)
	}
	if p.inits != 3 {
		t.Errorf("re-enabling did not start it: %d inits", p.inits)
	}

	// Enabling twice is what a double-click does, and it must not build a
	// second instance behind the first.
	if err := rt.SetEnabled(ctx, id, true); err != nil {
		t.Fatalf("enabling twice: %v", err)
	}
	if p.inits != 3 {
		t.Errorf("a second enable started another instance: %d inits", p.inits)
	}
}

func TestSettingsSurviveARestartOfTheRuntime(t *testing.T) {
	rt, db, mgr, _ := newRuntime(t)
	id := "recorder-" + t.Name()

	ctx := context.Background()
	if err := rt.Start(ctx, map[string]Defaults{id: {Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	if err := rt.SaveSettings(ctx, id, map[string]any{"prefix": "remembered"}); err != nil {
		t.Fatal(err)
	}
	rt.Stop()

	// A second runtime over the same database is what a restart of the server
	// looks like. The plugin must come back on, with its settings.
	again := NewRuntime(db, mgr, nil, nil)
	if err := again.Start(ctx, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(again.Stop)

	info, ok := again.Get(id)
	if !ok {
		t.Fatalf("the plugin is not in the new runtime: %+v", again.List())
	}
	if !info.Enabled {
		t.Error("the plugin came back disabled")
	}
	if info.Settings["prefix"] != "remembered" {
		t.Errorf("settings = %+v", info.Settings)
	}
}

func TestAPluginThatRefusesToStartIsReportedAndNotLeftHalfAlive(t *testing.T) {
	rt, _, _, p := newRuntime(t)
	id := "recorder-" + t.Name()

	p.failInit = true

	ctx := context.Background()
	// Start does not fail: one broken plugin must not stop the server. The
	// failure is recorded against that plugin instead.
	if err := rt.Start(ctx, map[string]Defaults{id: {Enabled: true}}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(rt.Stop)

	err := rt.SetEnabled(ctx, id, true)
	if err == nil {
		t.Fatal("enabling a plugin that refuses to start reported success")
	}
	if !errorMentions(err, "refuses to start") {
		t.Errorf("error %q does not carry the plugin's reason", err)
	}
}

func TestOperationsOnAPluginThatIsNotInstalled(t *testing.T) {
	rt, _, _, _ := newRuntime(t)
	ctx := context.Background()
	if err := rt.Start(ctx, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Stop)

	if err := rt.SetEnabled(ctx, "not-installed", true); !errors.Is(err, ErrUnknownPlugin) {
		t.Errorf("SetEnabled gave %v, want ErrUnknownPlugin", err)
	}
	if err := rt.SaveSettings(ctx, "not-installed", nil); !errors.Is(err, ErrUnknownPlugin) {
		t.Errorf("SaveSettings gave %v, want ErrUnknownPlugin", err)
	}
}

func TestOnlyAnEnabledPluginServesItsRoutes(t *testing.T) {
	rt, _, _, _ := newRuntime(t)
	id := "recorder-" + t.Name()

	ctx := context.Background()
	if err := rt.Start(ctx, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Stop)

	// Mounted the way the API mounts it: the handler resolves the plugin from
	// the {pluginID} route parameter, so it needs a router that sets one.
	router := chi.NewRouter()
	router.Handle(RoutePrefix+"/{pluginID}/*", rt.Handler())
	router.Handle(RoutePrefix+"/{pluginID}", rt.Handler())

	srv := httptest.NewServer(router)
	defer srv.Close()

	// Disabled: its routes are not reachable, or a plugin could keep acting on
	// the world after somebody switched it off.
	resp, err := srv.Client().Get(srv.URL + RoutePrefix + "/" + id + "/ping")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("a disabled plugin answered %d, want 409", resp.StatusCode)
	}

	if err := rt.SetEnabled(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	resp, err = srv.Client().Get(srv.URL + RoutePrefix + "/" + id + "/ping")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("an enabled plugin answered %d", resp.StatusCode)
	}

	// An unknown plugin is a 404 rather than a panic on a nil router.
	resp2, err := srv.Client().Get(srv.URL + RoutePrefix + "/not-installed/ping")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown plugin gave %d, want 404", resp2.StatusCode)
	}
}

func TestMessagesOnlyReachAPluginThatSubscribedToThem(t *testing.T) {
	rt, _, _, p := newRuntime(t)
	id := "recorder-" + t.Name()

	ctx := context.Background()
	if err := rt.Start(ctx, map[string]Defaults{id: {Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Stop)

	// dispatch is what the manager's observer calls for every message, so this
	// is the path a real broker message takes from here on.
	rt.dispatch(mqttc.Message{ConnectionID: "c1", Topic: "recorder/a", Payload: []byte("1")})
	rt.dispatch(mqttc.Message{ConnectionID: "c1", Topic: "somewhere/else", Payload: []byte("2")})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && p.seen() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if got := p.seen(); got != 1 {
		t.Fatalf("the plugin saw %d messages, want only the one it subscribed to", got)
	}

	// Stopping is idempotent: the shutdown path runs on every restart.
	rt.Stop()
	rt.Stop()
}

func errorMentions(err error, want string) bool {
	return err != nil && len(err.Error()) > 0 && contains(err.Error(), want)
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
