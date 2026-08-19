package plugin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/go-chi/chi/v5"

	"github.com/mqttview/mqttview/internal/httpx"
	"github.com/mqttview/mqttview/internal/mqttc"
	"github.com/mqttview/mqttview/internal/store"
)

// queueSize bounds how far behind a plugin may fall before messages are
// dropped. Dropping is preferable to blocking: a wedged plugin must never
// stall the MQTT read loop or the live view.
const queueSize = 4096

// Defaults are the config-file values for a plugin, used the first time it is
// seen. Once a plugin's settings exist in the database, the database wins.
type Defaults struct {
	Enabled  bool
	Settings map[string]any
}

// Info is a plugin's state as shown in the UI.
type Info struct {
	Meta     Meta           `json:"meta"`
	Enabled  bool           `json:"enabled"`
	Error    string         `json:"error,omitempty"`
	Settings map[string]any `json:"settings"`
	// Dropped counts messages discarded because the plugin fell behind.
	Dropped uint64 `json:"dropped"`
}

// Runtime hosts every compiled-in plugin.
type Runtime struct {
	log   *slog.Logger
	db    *store.Store
	mqtt  *mqttc.Manager
	emit  func(event string, payload any)
	rctx  context.Context
	stop  context.CancelFunc
	unobs func()

	mu        sync.RWMutex
	instances map[string]*instance
}

type instance struct {
	meta     Meta
	plugin   Plugin
	settings map[string]any
	enabled  bool
	lastErr  string
	router   chi.Router
	subs     []mqttc.Subscription
	// dynamic holds filters the plugin added at runtime via Host.Subscribe.
	dynamic map[string]mqttc.Subscription

	queue   chan mqttc.Message
	cancel  context.CancelFunc
	done    chan struct{}
	dropped atomic.Uint64
}

// NewRuntime prepares the plugin host. emit is how plugin events reach
// connected browsers; it may be nil during tests.
func NewRuntime(db *store.Store, mgr *mqttc.Manager, log *slog.Logger, emit func(string, any)) *Runtime {
	if log == nil {
		log = slog.Default()
	}
	if emit == nil {
		emit = func(string, any) {}
	}
	return &Runtime{
		log:       log.With("component", "plugins"),
		db:        db,
		mqtt:      mgr,
		emit:      emit,
		instances: map[string]*instance{},
	}
}

// Start loads persisted plugin settings, enables the plugins that should be
// running, and begins routing MQTT messages to them.
func (r *Runtime) Start(ctx context.Context, defaults map[string]Defaults) error {
	r.rctx, r.stop = context.WithCancel(context.Background())

	persisted := map[string]store.PluginSettings{}
	all, err := r.db.ListPluginSettings()
	if err != nil {
		return fmt.Errorf("plugin: load settings: %w", err)
	}
	for _, ps := range all {
		persisted[ps.PluginID] = ps
	}

	for _, id := range Registered() {
		factory, err := lookup(id)
		if err != nil {
			continue
		}

		inst := &instance{settings: map[string]any{}}
		// Meta must be readable before Init so the UI can list a plugin that
		// is installed but switched off.
		inst.meta = factory().Meta()

		if ps, ok := persisted[id]; ok {
			inst.enabled = ps.Enabled
			inst.settings = ps.Settings
		} else if d, ok := defaults[id]; ok {
			inst.enabled = d.Enabled
			if d.Settings != nil {
				inst.settings = d.Settings
			}
		}
		applySchemaDefaults(&inst.meta, inst.settings)

		r.mu.Lock()
		r.instances[id] = inst
		r.mu.Unlock()

		if inst.enabled {
			if err := r.start(ctx, id); err != nil {
				r.log.Error("enabling plugin failed", "plugin", id, "error", err)
			}
		}
	}

	r.unobs = r.mqtt.AddObserver(mqttc.Observer{
		OnMessage: r.dispatch,
		OnStatus:  r.onStatus,
	})
	return nil
}

// Stop shuts every plugin down.
func (r *Runtime) Stop() {
	if r.unobs != nil {
		r.unobs()
	}
	for _, id := range r.ids() {
		if err := r.stopInstance(context.Background(), id); err != nil {
			r.log.Warn("stopping plugin failed", "plugin", id, "error", err)
		}
	}
	if r.stop != nil {
		r.stop()
	}
}

// List returns every compiled-in plugin with its current state.
func (r *Runtime) List() []Info {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Info, 0, len(r.instances))
	for _, inst := range r.instances {
		out = append(out, Info{
			Meta:     inst.meta,
			Enabled:  inst.enabled,
			Error:    inst.lastErr,
			Settings: inst.settings,
			Dropped:  inst.dropped.Load(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Meta.Name < out[j].Meta.Name })
	return out
}

// Get returns one plugin's state.
func (r *Runtime) Get(id string) (Info, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	inst, ok := r.instances[id]
	if !ok {
		return Info{}, false
	}
	return Info{
		Meta:     inst.meta,
		Enabled:  inst.enabled,
		Error:    inst.lastErr,
		Settings: inst.settings,
		Dropped:  inst.dropped.Load(),
	}, true
}

// SetEnabled turns a plugin on or off and persists the choice.
func (r *Runtime) SetEnabled(ctx context.Context, id string, enabled bool) error {
	r.mu.RLock()
	inst, ok := r.instances[id]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownPlugin, id)
	}

	if enabled {
		if err := r.start(ctx, id); err != nil {
			return err
		}
	} else if err := r.stopInstance(ctx, id); err != nil {
		return err
	}

	r.mu.Lock()
	inst.enabled = enabled
	settings := inst.settings
	r.mu.Unlock()

	return r.db.SavePluginSettings(store.PluginSettings{
		PluginID: id, Enabled: enabled, Settings: settings,
	})
}

// SaveSettings replaces a plugin's configuration, restarting it if it is
// running so the new values take effect.
func (r *Runtime) SaveSettings(ctx context.Context, id string, settings map[string]any) error {
	r.mu.Lock()
	inst, ok := r.instances[id]
	if !ok {
		r.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrUnknownPlugin, id)
	}
	if settings == nil {
		settings = map[string]any{}
	}
	applySchemaDefaults(&inst.meta, settings)
	inst.settings = settings
	wasEnabled := inst.enabled
	r.mu.Unlock()

	if err := r.db.SavePluginSettings(store.PluginSettings{
		PluginID: id, Enabled: wasEnabled, Settings: settings,
	}); err != nil {
		return err
	}

	if wasEnabled {
		if err := r.stopInstance(ctx, id); err != nil {
			return err
		}
		return r.start(ctx, id)
	}
	return nil
}

// RoutePrefix is where plugin-owned HTTP routes live. It is deliberately
// separate from /api/plugins, which is mqttview's own plugin administration.
const RoutePrefix = "/api/p"

// Handler serves plugin-owned routes at /api/p/{pluginID}/*. Routes are
// resolved per request rather than mounted, because plugins come and go while
// the server runs.
func (r *Runtime) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		id := chi.URLParam(req, "pluginID")

		r.mu.RLock()
		inst, ok := r.instances[id]
		var router chi.Router
		var enabled bool
		if ok {
			router, enabled = inst.router, inst.enabled
		}
		r.mu.RUnlock()

		switch {
		case !ok:
			httpx.WriteErrorf(w, http.StatusNotFound, "no plugin named %q", id)
		case !enabled:
			httpx.WriteErrorf(w, http.StatusConflict, "plugin %q is disabled", id)
		case router == nil:
			httpx.WriteErrorf(w, http.StatusNotFound, "plugin %q has no HTTP API", id)
		default:
			// Strip /api/p/<id> so the plugin sees paths relative to its own
			// root. The plugin's router also needs a fresh chi context: the
			// outer router's context still carries the full route path, which
			// would otherwise be matched instead of the stripped one.
			sub := strings.TrimPrefix(req.URL.Path, RoutePrefix+"/"+id)
			if sub == "" {
				sub = "/"
			}
			clone := req.Clone(context.WithValue(req.Context(), chi.RouteCtxKey, chi.NewRouteContext()))
			clone.URL.Path = sub
			router.ServeHTTP(w, clone)
		}
	})
}

func (r *Runtime) ids() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.instances))
	for id := range r.instances {
		out = append(out, id)
	}
	return out
}

// start builds a fresh plugin instance, initialises it and wires up its
// subscriptions and worker goroutine.
func (r *Runtime) start(ctx context.Context, id string) error {
	factory, err := lookup(id)
	if err != nil {
		return err
	}

	r.mu.RLock()
	inst, ok := r.instances[id]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownPlugin, id)
	}
	if inst.plugin != nil {
		return nil // already running
	}

	p := factory()
	host := &pluginHost{
		runtime:  r,
		pluginID: id,
		log:      r.log.With("plugin", id),
		kv:       &kvStore{db: r.db, pluginID: id},
	}

	if err := p.Init(ctx, host); err != nil {
		r.mu.Lock()
		inst.lastErr = err.Error()
		r.mu.Unlock()
		return fmt.Errorf("plugin %s: init: %w", id, err)
	}

	router := chi.NewRouter()
	p.Routes(router)

	runCtx, cancel := context.WithCancel(r.rctx)
	inst2 := inst
	queue := make(chan mqttc.Message, queueSize)
	done := make(chan struct{})

	// Ask for the plugin's subscriptions before taking the lock: a plugin is
	// entitled to read its settings from Subscriptions, and Host.Settings
	// needs the same (non-reentrant) mutex.
	subs := p.Subscriptions()

	r.mu.Lock()
	inst2.plugin = p
	inst2.router = router
	inst2.subs = subs
	inst2.dynamic = map[string]mqttc.Subscription{}
	inst2.queue = queue
	inst2.cancel = cancel
	inst2.done = done
	inst2.lastErr = ""
	r.mu.Unlock()

	go func() {
		defer close(done)
		for {
			select {
			case <-runCtx.Done():
				return
			case msg := <-queue:
				p.HandleMessage(runCtx, msg)
			}
		}
	}()

	r.applySubscriptions(ctx, id)
	r.log.Info("plugin enabled", "plugin", id)
	return nil
}

func (r *Runtime) stopInstance(ctx context.Context, id string) error {
	r.mu.Lock()
	inst, ok := r.instances[id]
	if !ok || inst.plugin == nil {
		r.mu.Unlock()
		return nil
	}
	p := inst.plugin
	cancel := inst.cancel
	done := inst.done
	filters := make([]string, 0, len(inst.subs))
	for _, s := range inst.subs {
		filters = append(filters, s.Filter)
	}
	inst.plugin = nil
	inst.router = nil
	inst.queue = nil
	inst.cancel = nil
	inst.done = nil
	for f := range inst.dynamic {
		filters = append(filters, f)
	}
	inst.subs = nil
	inst.dynamic = nil
	r.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}

	for _, conn := range r.mqtt.List() {
		if err := conn.UnsubscribeEphemeral(ctx, filters); err != nil {
			r.log.Warn("removing plugin subscriptions failed",
				"plugin", id, "connection", conn.Spec().ID, "error", err)
		}
	}

	err := p.Close()
	r.log.Info("plugin disabled", "plugin", id)
	return err
}

// applySubscriptions makes sure a plugin's filters are subscribed on every
// connection. It is idempotent and safe to call after every reconnect.
func (r *Runtime) applySubscriptions(ctx context.Context, id string) {
	r.mu.RLock()
	inst, ok := r.instances[id]
	var subs []mqttc.Subscription
	if ok {
		subs = append(subs, inst.subs...)
	}
	r.mu.RUnlock()
	if len(subs) == 0 {
		return
	}

	for _, conn := range r.mqtt.List() {
		if err := conn.SubscribeEphemeral(ctx, subs); err != nil {
			r.log.Warn("applying plugin subscriptions failed",
				"plugin", id, "connection", conn.Spec().ID, "error", err)
		}
	}
}

// ApplyAll re-applies every enabled plugin's subscriptions, e.g. after a new
// connection is created.
func (r *Runtime) ApplyAll(ctx context.Context) {
	for _, id := range r.ids() {
		r.applySubscriptions(ctx, id)
	}
}

func (r *Runtime) onStatus(st mqttc.Status) {
	if st.State != mqttc.StateConnected {
		return
	}
	// A connection that has just come up may be new, or may have been
	// created while a plugin was already running.
	go r.ApplyAll(r.rctx)
}

// dispatch fans a message out to every plugin that subscribed to it. It runs
// on the MQTT client's goroutine, so it only ever does a non-blocking send.
func (r *Runtime) dispatch(msg mqttc.Message) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, inst := range r.instances {
		if inst.plugin == nil || inst.queue == nil {
			continue
		}
		if !matchesAny(inst.subs, msg.Topic) && !matchesDynamic(inst.dynamic, msg.Topic) {
			continue
		}
		select {
		case inst.queue <- msg:
		default:
			inst.dropped.Add(1)
		}
	}
}

func matchesDynamic(subs map[string]mqttc.Subscription, topic string) bool {
	for filter := range subs {
		if mqttc.MatchFilter(filter, topic) {
			return true
		}
	}
	return false
}

func matchesAny(subs []mqttc.Subscription, topic string) bool {
	for _, s := range subs {
		if mqttc.MatchFilter(s.Filter, topic) {
			return true
		}
	}
	return false
}

// applySchemaDefaults fills in any setting the operator did not specify, so a
// plugin never has to guard every lookup.
func applySchemaDefaults(meta *Meta, settings map[string]any) {
	for _, f := range meta.SettingsSchema {
		if f.Default == nil {
			continue
		}
		if _, ok := settings[f.Key]; !ok {
			settings[f.Key] = f.Default
		}
	}
}

// pluginHost is the Host implementation handed to each plugin.
type pluginHost struct {
	runtime  *Runtime
	pluginID string
	log      *slog.Logger
	kv       KV
}

func (h *pluginHost) Logger() *slog.Logger { return h.log }
func (h *pluginHost) Store() KV            { return h.kv }

func (h *pluginHost) Settings() map[string]any {
	h.runtime.mu.RLock()
	defer h.runtime.mu.RUnlock()
	inst, ok := h.runtime.instances[h.pluginID]
	if !ok {
		return map[string]any{}
	}
	// Copy so a plugin cannot mutate the runtime's view.
	out := make(map[string]any, len(inst.settings))
	for k, v := range inst.settings {
		out[k] = v
	}
	return out
}

func (h *pluginHost) Connections() []*mqttc.Conn { return h.runtime.mqtt.List() }

func (h *pluginHost) Connection(id string) (*mqttc.Conn, bool) { return h.runtime.mqtt.Get(id) }

func (h *pluginHost) Publish(ctx context.Context, connectionID string, req mqttc.PublishRequest) error {
	if connectionID == "" {
		return errors.New("plugin: publish needs a connection id")
	}
	return h.runtime.mqtt.Publish(ctx, connectionID, req)
}

func (h *pluginHost) Subscribe(ctx context.Context, connectionID string, subs []mqttc.Subscription) error {
	if len(subs) == 0 {
		return nil
	}

	h.runtime.mu.Lock()
	if inst, ok := h.runtime.instances[h.pluginID]; ok && inst.dynamic != nil {
		for _, s := range subs {
			inst.dynamic[s.Filter] = s
		}
	}
	h.runtime.mu.Unlock()

	return h.forEachConnection(connectionID, func(c *mqttc.Conn) error {
		return c.SubscribeEphemeral(ctx, subs)
	})
}

func (h *pluginHost) Unsubscribe(ctx context.Context, connectionID string, filters []string) error {
	if len(filters) == 0 {
		return nil
	}

	h.runtime.mu.Lock()
	if inst, ok := h.runtime.instances[h.pluginID]; ok && inst.dynamic != nil {
		for _, f := range filters {
			delete(inst.dynamic, f)
		}
	}
	h.runtime.mu.Unlock()

	return h.forEachConnection(connectionID, func(c *mqttc.Conn) error {
		return c.UnsubscribeEphemeral(ctx, filters)
	})
}

// forEachConnection applies fn to one connection, or to all of them when the
// ID is empty. The first error is returned but the remaining connections are
// still attempted, so one dead broker does not skip the others.
func (h *pluginHost) forEachConnection(connectionID string, fn func(*mqttc.Conn) error) error {
	if connectionID != "" {
		c, ok := h.runtime.mqtt.Get(connectionID)
		if !ok {
			return fmt.Errorf("plugin: no connection with id %q", connectionID)
		}
		return fn(c)
	}

	var firstErr error
	for _, c := range h.runtime.mqtt.List() {
		if err := fn(c); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (h *pluginHost) Emit(event string, payload any) {
	h.runtime.emit("plugin:"+h.pluginID+":"+event, payload)
}

// kvStore adapts the store's plugin_state table to the KV interface.
type kvStore struct {
	db       *store.Store
	pluginID string
}

func (k *kvStore) Get(key string) (string, bool, error) {
	v, err := k.db.PluginGet(k.pluginID, key)
	if errors.Is(err, store.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

func (k *kvStore) Set(key, value string) error     { return k.db.PluginSet(k.pluginID, key, value) }
func (k *kvStore) Delete(key string) error         { return k.db.PluginDelete(k.pluginID, key) }
func (k *kvStore) All() (map[string]string, error) { return k.db.PluginList(k.pluginID) }
