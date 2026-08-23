# Writing an mqttview plugin

A plugin observes MQTT traffic, keeps whatever state it likes, serves its own
HTTP endpoints and can publish back to the broker. The bundled Home Assistant
plugin (`internal/plugins/hass`) is written against exactly this interface and
is the reference implementation — if something here is unclear, read that.

Plugins are compiled into the binary. That is deliberate: an in-process
interface keeps the message path cheap, and a plugin cannot be dropped onto a
running server without a rebuild and a deploy.

## The interface

```go
type Plugin interface {
    Meta() Meta
    Init(ctx context.Context, host Host) error
    Subscriptions() []mqttc.Subscription
    HandleMessage(ctx context.Context, msg mqttc.Message)
    Routes(r chi.Router)
    Close() error
}
```

`Meta` is read before `Init`, so it must not depend on any state — it is how the
UI lists a plugin that is installed but switched off.

`Subscriptions` is called after `Init`, so it may read settings. The host
subscribes these filters on every broker connection, as *ephemeral*
subscriptions: they are applied to the live session but never written into the
user's saved connection, so disabling your plugin leaves their config untouched.

`HandleMessage` runs on your own goroutine, fed by a buffered channel. You may
block; the MQTT read loop will not. If you fall far enough behind, messages are
dropped and the count is shown on the plugins page — dropping is preferred over
stalling the live view for everyone.

## The host

```go
type Host interface {
    Logger() *slog.Logger
    Store() KV
    Settings() map[string]any
    Connections() []*mqttc.Conn
    Connection(id string) (*mqttc.Conn, bool)
    Publish(ctx context.Context, connectionID string, req mqttc.PublishRequest) error
    Subscribe(ctx context.Context, connectionID string, subs []mqttc.Subscription) error
    Unsubscribe(ctx context.Context, connectionID string, filters []string) error
    Emit(event string, payload any)
}
```

`Subscribe` is for topics you only learn about at runtime. The Home Assistant
plugin statically subscribes to `homeassistant/#`, then calls `Subscribe` for
each device's state topic as it reads the config. That is what lets it work
without subscribing to `#`.

Pass an empty `connectionID` to apply something to every broker.

`Store()` is a private key-value namespace backed by SQLite. It survives
restarts; use it for user preferences rather than for state you can rebuild from
retained messages.

`Emit(event, payload)` pushes to every connected browser as
`plugin:<your-id>:<event>`. Batch these — a Zigbee network coming online
produces hundreds of updates a second and the browser only needs the end state.

> **Do not call `Host.Settings()` from inside a callback the runtime invoked
> while holding its lock.** `Subscriptions()` is safe (the runtime asks for it
> before locking), but this is the one sharp edge in the API.

## HTTP routes

`Routes` registers handlers mounted at `/api/p/<your-id>/`. They sit behind
mqttview's authentication, so a request always has a signed-in user:

```go
func (p *Plugin) Routes(r chi.Router) {
    r.Get("/things", p.handleThings)
    r.Post("/things/{id}/do", p.handleDo)
}

func (p *Plugin) handleDo(w http.ResponseWriter, r *http.Request) {
    user, ok := auth.UserFrom(r.Context())
    if !ok || !user.Role.AtLeast(store.RoleOperator) {
        httpx.WriteError(w, http.StatusForbidden, "this needs the operator role")
        return
    }
    // ...
}
```

Authentication is handled for you; **authorisation beyond `viewer` is yours**.
Anything that publishes to a broker should require at least `operator`.

## Settings

Declare a `SettingsSchema` in `Meta` and the UI renders a form for it — no
frontend code needed:

```go
SettingsSchema: []plugin.SettingField{
    {Key: "prefix", Label: "Prefix", Type: "string", Default: "acme"},
    {Key: "enabled", Label: "Do the thing", Type: "bool", Default: true},
    {Key: "mode", Label: "Mode", Type: "select", Default: "fast",
        Options: []plugin.SettingOption{{Value: "fast", Label: "Fast"}}},
}
```

Defaults are filled in before `Init`, so you never have to guard a lookup.
Saving settings restarts the plugin so the new values take effect.

## Registering

```go
func init() {
    plugin.Register("acme", func() plugin.Plugin { return &Plugin{} })
}
```

Then add a blank import in `cmd/mqttview/main.go`:

```go
_ "github.com/dgprivate/mqttview/internal/plugins/acme"
```

A fresh instance is built every time the plugin is enabled, so you do not need
to make one instance survive repeated enable/disable cycles.

## A frontend panel

Set `Panel` in `Meta` to a component name and add the matching page in
`web/src/pages/`, wired into `web/src/App.tsx`. Your page talks to
`/api/p/<your-id>/…` and listens for your events:

```ts
useFrames((frame) => {
  if (frame.type === 'event' && frame.event === 'plugin:acme:changed') {
    void reload()
  }
})
```

## Checklist

- [ ] `Meta()` works on a zero-value instance
- [ ] `Close()` stops every goroutine you started, and is safe to call twice
- [ ] Publishing routes check for `operator`
- [ ] You subscribe to the narrowest filter that works, not `#`
- [ ] Events are batched
- [ ] Anything you cannot interpret faithfully is reported as unknown rather
      than guessed at
