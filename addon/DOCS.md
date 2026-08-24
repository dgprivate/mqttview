# mqttview

A realtime web UI for MQTT brokers, as a Home Assistant panel.

Connect to as many brokers as you like, watch the topic tree fill up live,
publish and subscribe by hand, and — if you have them — get a Home Assistant
device view and a Beckhoff PLC panel from the bundled plugins.

## Installation

1. Settings → **Apps** → ⋮ → Repositories (older versions call these
   "Add-ons")
2. Add `https://github.com/dgprivate/mqttview`
3. Install **mqttview**, then Start.

The repository also offers **mqttview (Home Assistant config access)**: the
same app, allowed to read your configuration directory so it can pick the
broker up from your MQTT integration instead of asking you for it. Take this
one unless you want that — it is a wider grant than it sounds, and DOCS.md
there explains why it cannot be an option here.

mqttview appears in the sidebar. There is no login: Home Assistant has already
decided you may open the panel, and asking again would be a second password to
lose rather than a second lock.

## Updating

Home Assistant does not notice a new version until the store is refreshed. Left
out, `ha apps update` answers *No update available* for a version that is
sitting right there:

```bash
ha store reload
ha apps update <slug>
ha apps restart <slug>
```

In the interface it is ⋮ → **Check for updates** on the store page, then the
app's Update button.

The log names both versions on every start — the app's, and the mqttview binary
inside it. They are different things, and the binary is pinned by digest, so an
update to the app is an update to a specific binary rather than to whatever was
cached.

## Configuration

### `default_role`

What a Home Assistant user may do the first time they open the panel.

| Role       | Can                                                           |
| ---------- | ------------------------------------------------------------- |
| `viewer`   | See everything. Change nothing.                                |
| `operator` | Publish, subscribe, and use plugin controls. The default.      |
| `admin`    | Also add and remove broker connections, and manage users.      |

Home Assistant does **not** tell an add-on whether you are one of its
administrators, so mqttview cannot copy that. Everybody who can open the panel
gets `default_role`, and `admin_users` names the exceptions.

### `admin_users`

Home Assistant usernames (or user IDs) that get the `admin` role. Changes take
effect on the next page load — including removals.

```yaml
admin_users:
  - dean
```

### `fallback_user`

Leave this empty unless you have to.

Home Assistant tells the add-on who you are in a header. Older Supervisor
versions do not send it, and mqttview then refuses rather than guessing. Set
`fallback_user` to a name and everybody who opens the panel shares that one
account instead. It is the difference between "no add-on" and "no separate
users", and only you can decide which is worse.

### `frame_ancestors`

Extra origins allowed to embed the mqttview UI in an iframe. Ingress does not
need this; it is here for people also reaching the same instance from
somewhere else.

## Where this comes from

Source, issues and the rest of the documentation:
<https://github.com/dgprivate/mqttview>. The add-on builds on the same image
that is published as
[`hausbit/mqttview`](https://hub.docker.com/r/hausbit/mqttview), so the binary
in here is the one anybody else runs.

## Data

Everything lives in the add-on's `/data`: the SQLite database, the encryption
key that broker passwords and TLS private keys are stored under, and plugin
state. It survives an add-on update and is included in a Home Assistant backup.

Broker credentials are encrypted with a key generated on first start and kept
beside the database. A backup contains both, which is what makes a restore
work — treat the backup accordingly.

### Adding a broker

In mqttview, not here. Open the panel, **Connections → Add**, and fill in the
form: URL, credentials, TLS with a CA and a client certificate if the broker
wants one, and a connection test before you save. Credentials and private keys
are encrypted with a key kept beside the database.

There used to be `mqtt_url` and a handful of `mqtt_*` options in this file.
They are gone. They were a second, smaller version of a form that already
exists, validated by nothing, and the one thing they reliably did was make the
app harder to start.

A broker Home Assistant already provides — the Mosquitto app, usually — is
added on first start without being asked. That only ever adds a connection that
is not there; one you have edited is left alone. What happened, or did not, is
in the app log.

### Can it not just read the MQTT integration's settings?

Not through anything supported. The Supervisor only knows about brokers an app
provides, and Home Assistant's API strips credentials out of a config entry
before returning it.

They are readable on disk, in `.storage`, by an app that maps the Home
Assistant configuration folder. That is what the companion app **mqttview
(Home Assistant config access)** does — but the mapping is not "read the MQTT
settings", it is read access to `secrets.yaml` and every other integration's
credentials, in exchange for saving one form. Take the plain app unless you
want that trade.

## The Mosquitto app

If you run it, its broker is added for you on first start. Its credentials are
whatever Home Assistant user you created for it; the address is usually
`mqtt://core-mosquitto:1883`.

## Ports

There are none, on purpose. Ingress does not use a published port, and mqttview
in Home Assistant mode refuses any request that did not come through the
Supervisor — so a published port would only ever serve a refusal.

If you want to reach mqttview from outside Home Assistant as well, run the
standalone image alongside; it has its own sign-in, two-factor authentication
and SSO. See the project README.

## Troubleshooting

**The panel is blank.** Look at the add-on log. If it says a request did not
come from a trusted proxy, something other than the Supervisor is reaching the
add-on.

**"Open this from Home Assistant".** You opened the port directly rather than
the sidebar entry.

**Everybody shows up as the same person.** `fallback_user` is set, or your
Supervisor is too old to send identity headers. Check the log for the
`X-Remote-User-Id` warning.
