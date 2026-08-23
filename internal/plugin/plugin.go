// Package plugin defines mqttview's extension contract and the runtime that
// hosts plugins.
//
// A plugin observes MQTT traffic on the connections it is enabled for, keeps
// whatever derived state it likes, exposes its own HTTP endpoints under
// /api/plugins/<id>/ and pushes live updates to the browser. The bundled
// Home Assistant plugin is written against exactly this interface and is the
// reference for what a plugin can do.
//
// Plugins are compiled in and registered from an init function. Out-of-process
// plugins are a deliberate non-goal for now: an in-process interface keeps the
// message path allocation-light, and a compiled plugin cannot be smuggled onto
// a server without a rebuild.
package plugin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/go-chi/chi/v5"

	"github.com/dgprivate/mqttview/internal/mqttc"
)

// Meta describes a plugin to the UI and to operators.
type Meta struct {
	// ID is the stable identifier used in URLs, config and the database. Use
	// lowercase kebab-case, e.g. "home-assistant".
	ID string `json:"id"`
	// Name is the human-readable name.
	Name string `json:"name"`
	// Version is the plugin's own version, independent of mqttview's.
	Version string `json:"version"`
	// Description is one or two sentences shown on the plugins page.
	Description string `json:"description"`
	// Author and URL are optional attribution.
	Author string `json:"author,omitempty"`
	URL    string `json:"url,omitempty"`
	// Panel names the frontend component that renders this plugin's UI. An
	// empty value means the plugin has no page of its own.
	Panel string `json:"panel,omitempty"`
	// SettingsSchema describes the plugin's configuration fields so the UI can
	// render a settings form without knowing the plugin.
	SettingsSchema []SettingField `json:"settingsSchema,omitempty"`
}

// SettingField is one configurable value.
type SettingField struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Type        string `json:"type"` // "string", "bool", "number", "select"
	Default     any    `json:"default,omitempty"`
	Description string `json:"description,omitempty"`
	// Options is used when Type is "select".
	Options []SettingOption `json:"options,omitempty"`
}

// SettingOption is one choice in a select field.
type SettingOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// KV is a plugin's private, namespaced key-value store. Values survive
// restarts, which is how a plugin avoids re-deriving everything from retained
// messages on every boot.
type KV interface {
	Get(key string) (string, bool, error)
	Set(key, value string) error
	Delete(key string) error
	All() (map[string]string, error)
}

// Host is what a plugin is handed at Init: everything it may do to the rest of
// mqttview, and nothing more.
type Host interface {
	// Logger is scoped to the plugin.
	Logger() *slog.Logger
	// Store is the plugin's private KV namespace.
	Store() KV
	// Settings returns the operator-supplied configuration.
	Settings() map[string]any
	// Connections lists the broker connections this plugin is active on.
	Connections() []*mqttc.Conn
	// Connection looks up one connection by ID.
	Connection(id string) (*mqttc.Conn, bool)
	// Publish sends a message on a connection. This is how a plugin acts on
	// the world, e.g. turning a Home Assistant switch on.
	Publish(ctx context.Context, connectionID string, req mqttc.PublishRequest) error
	// Subscribe adds topic filters at runtime, beyond the static set returned
	// by Subscriptions. A plugin that discovers which topics matter only
	// after reading a config message needs this; it also makes the plugin's
	// messages start flowing without subscribing to the whole broker.
	// Passing an empty connectionID applies the filters to every connection.
	Subscribe(ctx context.Context, connectionID string, subs []mqttc.Subscription) error
	// Unsubscribe removes filters added with Subscribe.
	Unsubscribe(ctx context.Context, connectionID string, filters []string) error
	// Emit pushes an event to every browser watching this plugin. The event
	// name is namespaced automatically.
	Emit(event string, payload any)
}

// Plugin is the interface every plugin implements.
type Plugin interface {
	// Meta is called before Init and must not depend on any state.
	Meta() Meta
	// Init prepares the plugin. Returning an error leaves the plugin disabled
	// with the error shown in the UI.
	Init(ctx context.Context, host Host) error
	// Subscriptions are the topic filters the host subscribes on the
	// plugin's behalf, on every connection it is active for.
	Subscriptions() []mqttc.Subscription
	// HandleMessage receives every message matching Subscriptions. It runs on
	// the plugin's own goroutine, so it may block without stalling MQTT.
	HandleMessage(ctx context.Context, msg mqttc.Message)
	// Routes registers the plugin's HTTP handlers. They are mounted under
	// /api/plugins/<id>/ behind the same authentication as the core API; the
	// authenticated user is available via auth.UserFrom.
	Routes(r chi.Router)
	// Close releases resources. It is called on disable and on shutdown.
	Close() error
}

// Factory constructs a fresh plugin instance. A new instance is built every
// time a plugin is enabled, so plugins need not be re-entrant across cycles.
type Factory func() Plugin

var (
	registryMu sync.RWMutex
	registry   = map[string]Factory{}
)

// Register adds a plugin factory to the global registry. Call it from an init
// function. It panics on a duplicate ID, since that is a build-time mistake.
func Register(id string, f Factory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[id]; dup {
		panic(fmt.Sprintf("plugin: duplicate registration for %q", id))
	}
	registry[id] = f
}

// Registered returns the IDs of every compiled-in plugin, sorted.
func Registered() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	ids := make([]string, 0, len(registry))
	for id := range registry {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// ErrUnknownPlugin is returned for an ID that was never registered.
var ErrUnknownPlugin = errors.New("plugin: not registered")

func lookup(id string) (Factory, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	f, ok := registry[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownPlugin, id)
	}
	return f, nil
}
