package plugin

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/dgprivate/mqttview/internal/mqttc"
	"github.com/dgprivate/mqttview/internal/secrets"
	"github.com/dgprivate/mqttview/internal/store"
)

// recorder is a plugin that remembers what the runtime did to it, which is the
// only way to check that the lifecycle is what the contract says.
type recorder struct {
	mu       sync.Mutex
	host     Host
	inits    int
	closes   int
	messages []mqttc.Message
	failInit bool
}

func (p *recorder) Meta() Meta {
	return Meta{
		ID: "recorder", Name: "Recorder", Version: "1.0.0",
		Description: "A plugin that records what happens to it.",
		SettingsSchema: []SettingField{
			{Key: "prefix", Label: "Prefix", Type: "string", Default: "test"},
			{Key: "flag", Label: "Flag", Type: "bool", Default: false},
		},
	}
}

func (p *recorder) Init(_ context.Context, host Host) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failInit {
		return errors.New("this plugin refuses to start")
	}
	p.host = host
	p.inits++
	return nil
}

func (p *recorder) Subscriptions() []mqttc.Subscription {
	return []mqttc.Subscription{{Filter: "recorder/#", QoS: 0}}
}

func (p *recorder) HandleMessage(_ context.Context, m mqttc.Message) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.messages = append(p.messages, m)
}

func (p *recorder) Routes(r chi.Router) {
	r.Get("/ping", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("pong")) })
}

func (p *recorder) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closes++
	return nil
}

func (p *recorder) seen() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.messages)
}

func newRuntime(t *testing.T) (*Runtime, *store.Store, *mqttc.Manager, *recorder) {
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

	mgr := mqttc.NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Registered under a unique id per test, because the registry is global —
	// and removed again afterwards, because a test name is only unique within
	// one run. Under -count=5 the second run registered the same id and
	// Register panics on a duplicate, by design: outside a test that is a
	// build mistake.
	p := &recorder{}
	id := "recorder-" + t.Name()
	Register(id, func() Plugin { return p })
	unregisterAfter(t, id)

	rt := NewRuntime(db, mgr, slog.New(slog.NewTextHandler(io.Discard, nil)), func(string, any) {})
	return rt, db, mgr, p
}

// hostFor builds the host the runtime would hand a plugin. The runtime builds
// it inside start(), which needs a registered plugin and a database row; this
// is the same object without that ceremony.
func hostFor(rt *Runtime, id string) Host {
	return &pluginHost{
		runtime:  rt,
		pluginID: id,
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		kv:       &kvStore{db: rt.db, pluginID: id},
	}
}

func TestHostPublishAndSubscribeReachTheManager(t *testing.T) {
	rt, db, mgr, _ := newRuntime(t)

	spec := mqttc.ConnectionSpec{
		ID: "c1", Name: "c1", URL: "mqtt://127.0.0.1:1", Version: mqttc.V311,
	}
	if err := spec.Normalize(); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Upsert(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	_ = db

	h := hostFor(rt, "recorder")

	// The host sees the manager's connections, which is how a plugin knows
	// which brokers it is running against.
	if len(h.Connections()) != 1 {
		t.Fatalf("the host sees %d connections", len(h.Connections()))
	}
	if _, ok := h.Connection("c1"); !ok {
		t.Error("the host cannot look up a connection by id")
	}
	if _, ok := h.Connection("nope"); ok {
		t.Error("an unknown id resolved")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Nothing is connected, so both of these fail — the property under test is
	// that they reach the manager and report rather than panicking.
	if err := h.Publish(ctx, "c1", mqttc.PublishRequest{Topic: "a", Payload: []byte("b")}); err == nil {
		t.Error("publishing to a disconnected broker succeeded")
	}
	if err := h.Publish(ctx, "nope", mqttc.PublishRequest{Topic: "a"}); err == nil {
		t.Error("publishing to an unknown connection succeeded")
	}
	// Subscribe and Unsubscribe with an empty connection id fan out to every
	// connection, which is how a plugin says "wherever I am enabled".
	_ = h.Subscribe(ctx, "", []mqttc.Subscription{{Filter: "x/#"}})
	_ = h.Unsubscribe(ctx, "", []string{"x/#"})
	_ = h.Unsubscribe(ctx, "c1", []string{"x/#"})
}

func TestHostKVIsNamespacedAndPersistent(t *testing.T) {
	rt, _, _, _ := newRuntime(t)

	a := hostFor(rt, "plugin-a").Store()
	b := hostFor(rt, "plugin-b").Store()

	if err := a.Set("k", "value-a"); err != nil {
		t.Fatal(err)
	}
	if err := b.Set("k", "value-b"); err != nil {
		t.Fatal(err)
	}

	got, ok, err := a.Get("k")
	if err != nil || !ok || got != "value-a" {
		t.Fatalf("Get = %q %v %v", got, ok, err)
	}
	// Two plugins using the same key name must not collide.
	got, _, _ = b.Get("k")
	if got != "value-b" {
		t.Errorf("plugin b read %q", got)
	}

	all, err := a.All()
	if err != nil || len(all) != 1 {
		t.Fatalf("All = %+v %v", all, err)
	}

	if err := a.Delete("k"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := a.Get("k"); ok {
		t.Error("the key survived deletion")
	}
	// A key that was never set reports absent rather than empty-and-present.
	if _, ok, _ := a.Get("never-set"); ok {
		t.Error("an unset key reported as present")
	}
}

func TestRegisteredReportsWhatIsCompiledIn(t *testing.T) {
	// The plugins themselves live in subpackages, so nothing is registered in
	// this one unless a test registers it. This used to read the registry and
	// require it to be non-empty, which passed only because some other test had
	// run first and left its plugin behind — a shuffled order failed it, and
	// tidying up after those tests failed it every time.
	for _, id := range []string{"zzz-last", "aaa-first"} {
		Register(id, func() Plugin { return &recorder{} })
		unregisterAfter(t, id)
	}

	ids := Registered()
	if !slices.Contains(ids, "aaa-first") || !slices.Contains(ids, "zzz-last") {
		t.Fatalf("Registered() = %v, missing what was just registered", ids)
	}
	// Sorted, so a list rendered from it does not shuffle.
	if !slices.IsSorted(ids) {
		t.Fatalf("Registered() is not sorted: %v", ids)
	}
}

func TestLookupOfAnUnknownPlugin(t *testing.T) {
	if _, err := lookup("definitely-not-registered"); !errors.Is(err, ErrUnknownPlugin) {
		t.Fatalf("lookup gave %v, want ErrUnknownPlugin", err)
	}
}

func TestDuplicateRegistrationPanics(t *testing.T) {
	// A build-time mistake, so it fails loudly rather than silently replacing
	// somebody else's plugin.
	//
	// The first registration is undone afterwards. Left in place, a second run
	// of this test in the same binary panicked on that line instead of the one
	// under test, and recovered from it — passing while asserting nothing.
	Register("duplicate-test", func() Plugin { return &recorder{} })
	unregisterAfter(t, "duplicate-test")

	defer func() {
		if recover() == nil {
			t.Fatal("registering the same id twice did not panic")
		}
	}()
	Register("duplicate-test", func() Plugin { return &recorder{} })
}

// unregisterAfter removes a test's registration when it ends. The registry is
// global and a test name is only unique within one run, so anything a test puts
// in it has to come back out — otherwise -count=2 is a duplicate registration
// and the surviving entries leak into whatever runs next.
func unregisterAfter(t *testing.T, id string) {
	t.Helper()
	t.Cleanup(func() {
		registryMu.Lock()
		delete(registry, id)
		registryMu.Unlock()
	})
}
