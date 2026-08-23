# mqttview

A realtime web UI for MQTT brokers, as a Home Assistant panel.

Connect to as many brokers as you like, watch the topic tree fill up live,
publish and subscribe by hand, and — if you have them — get a Home Assistant
device view and a Beckhoff PLC panel from the bundled plugins.

## Installation

1. Settings → Add-ons → Add-on Store → ⋮ → Repositories
2. Add `https://github.com/dgprivate/mqttview`
3. Install **mqttview**, then Start.

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

### `mqtt_url`, `mqtt_username`, `mqtt_password`

A broker to add on first start.

```yaml
mqtt_url: mqtt://mqtt.example.com:1883
mqtt_username: someone
mqtt_password: a-password
```

`mqtts://` for TLS. The connection is named after the host and subscribes to
`#`, and you can change all of it in mqttview afterwards — this only ever adds
a connection that is not there, and never touches one you have edited.

For a broker that wants a **client certificate**, put the files in this app's
configuration folder and name them:

```yaml
mqtt_url: mqtts://mqtt.example.com:8883
mqtt_username: someone
mqtt_password: a-password
mqtt_ca_file: /config/ca.crt
mqtt_client_cert_file: /config/client.crt
mqtt_client_key_file: /config/client.key
mqtt_server_name: mqtt.example.com     # if the certificate names something else
```

mqttview reads them once at start and stores the contents encrypted, so the
paths only have to be right the first time. A file it cannot read stops the
start with the name of the setting, rather than creating a connection that
would fail at the handshake — or, worse, one that connects unverified.

### Can it not just read the MQTT integration's settings?

Not through anything supported. The Supervisor only knows about brokers an app
provides, and Home Assistant's API strips credentials out of a config entry
before returning it.

They are readable on disk, in `.storage`, by an app that maps the Home
Assistant configuration folder — but that mapping is not "read the MQTT
settings", it is read access to `secrets.yaml` and every other integration's
credentials, in exchange for saving one form. `.storage` is also an internal
format with no compatibility promise. mqttview does not ask for it.

### `import_mqtt`

On by default. Asks Home Assistant for the broker it already uses, when
`mqtt_url` is not set.

This works when the broker is **provided by an app** — the Mosquitto app,
usually — because that is the only kind the Supervisor knows about. The
Supervisor's service registry is filled in by apps that offer a broker; the
MQTT integration reads from it rather than writing to it. So a broker you
entered into the MQTT integration yourself, or one running elsewhere on your
network, cannot be discovered from here by any means. Use `mqtt_url` for those.

Whatever happens is in the app log, including the reason it did nothing.

## The MQTT broker

The add-on asks Home Assistant for the MQTT service, so if you run the
Mosquitto add-on the broker's address is already known to it. You still add the
connection in mqttview yourself: mqttview is a tool for looking at brokers
generally, and pre-filling one would be a guess about which one you meant.

For the Mosquitto add-on, that is usually:

- URL: `mqtt://core-mosquitto:1883`
- Username and password: a Home Assistant user you created for it

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
