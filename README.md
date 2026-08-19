# mqttview

A realtime web UI for MQTT brokers — every protocol version, every transport,
proper authentication, and a plugin system that can turn raw topics into
something you actually recognise.

mqttview is a single Go binary with the frontend embedded. Point it at a
broker and you get a live topic tree, a message stream, a publisher, and — with
the bundled Home Assistant plugin — a device view built from MQTT discovery
messages.

## Why

Most MQTT tools make you choose. Desktop clients are single-user and show you
raw topics. Broker dashboards show you throughput but not payloads. Home
Assistant understands devices but is not a debugging tool. mqttview is meant to
be the thing you leave running: multi-user, browser-based, and able to present
the same broker as either a topic tree or a set of devices.

## Features

**Protocol coverage**

- MQTT 3.1, 3.1.1 and 5.0, selectable per connection
- TCP, TLS, WebSocket, secure WebSocket and Unix sockets
- Username/password, client certificates (mutual TLS), custom CA bundles, SNI
  override, ALPN, TLS 1.2/1.3 floor
- Last Will and Testament, clean/persistent sessions, MQTT 5 session expiry
- MQTT 5 subscription options (no-local, retain-as-published, retain handling)
  and publish properties

**Live view**

- Topic tree built from live traffic, loaded one level at a time so a broker
  with hundreds of thousands of topics stays usable
- Message stream over a single WebSocket per tab, with per-client rate limiting
  and an honest "messages dropped" counter rather than a silently thinned feed
- Last known value, retained flag, QoS, size and update count per topic
- JSON payloads pretty-printed; binary payloads shown as hex instead of mojibake
- Publish with QoS, retain and MQTT 5 properties

**Access control**

- Local accounts with argon2id password hashing
- Optional single sign-on through any OIDC provider — Google, Authentik, Keycloak,
  Okta, Entra ID — with PKCE, nonce checking and domain allow-lists
- Three roles: `viewer` reads, `operator` publishes and controls, `admin`
  manages brokers, users and plugins
- Session cookies are `HttpOnly` and stored hashed; writes require a
  double-submit CSRF token; WebSocket origins are checked against `base_url`
- Broker passwords and private keys are encrypted at rest with AES-256-GCM and
  are never sent back to the browser

**Plugins**

- A small, documented Go interface: observe messages, keep state, expose HTTP
  routes, push events to the browser, publish back to the broker
- Bundled: **Home Assistant MQTT discovery** — see below
- See [docs/PLUGINS.md](docs/PLUGINS.md) to write your own

## The Home Assistant plugin

Home Assistant's MQTT discovery convention is the closest thing MQTT has to a
device model, and Zigbee2MQTT, Tasmota, ESPHome, Shelly and Z-Wave JS all speak
it. The plugin reads those announcements and turns a wall of topics into a list
of devices.

It handles the parts that make real payloads hard:

- The full key abbreviation table (`stat_t`, `pl_on`, `dev`, `uniq_id`, …)
- `~` base-topic expansion, including inside availability blocks
- Device and origin metadata, with entities grouped by device identifier or MAC
- Both discovery formats: per-entity configs and the 2024.10+ device-scoped
  payload with a `cmps` block
- Availability topics with `availability_mode`, and `json_attributes_topic`
- Control: on/off, brightness, position, temperature, preset, select, number,
  lock, cover, button and more — validated against the ranges and option lists
  the device itself advertised

It subscribes to the discovery prefix only, then subscribes to each state topic
as it learns about it, so enabling it does not turn mqttview into a firehose.

**On `value_template`:** these are Jinja2, and mqttview does not embed a Jinja
engine. It evaluates the common shapes (`{{ value }}`, `{{ value_json.a.b }}`,
`{{ value_json.x | round(1) }}`) and, for anything else, shows the raw payload
and marks the value with an asterisk. It will not guess.

## Quick start

```bash
git clone https://github.com/mqttview/mqttview.git
cd mqttview

# Build the frontend into web/dist, then the binary that embeds it.
make build

./mqttview -addr 127.0.0.1:8114
```

On first run mqttview creates an administrator account and prints the generated
password once. Open <http://127.0.0.1:8114>, sign in, and add a broker.

### Docker

```bash
docker build -t mqttview .
docker run -p 8114:8114 -v mqttview-data:/data mqttview
```

## Configuration

Everything has a working default; a config file is optional. See
[mqttview.example.yaml](mqttview.example.yaml) for the annotated version.

```yaml
addr: 127.0.0.1:8114
base_url: https://mqtt.example.com   # used for OAuth redirects and WS origin checks
data_dir: ./data

auth:
  allow_local: true
  allow_signup: false
  providers:
    google:
      enabled: true
      display_name: Google
      issuer: https://accounts.google.com
      client_id: ...
      client_secret: ...
      allowed_domains: [example.com]
      admin_emails: [you@example.com]

plugins:
  home-assistant:
    enabled: true
    settings:
      discoveryPrefix: homeassistant
```

Secrets can stay out of the file entirely — every provider credential has an
environment override (`MQTTVIEW_OIDC_GOOGLE_CLIENT_SECRET`, and so on). See
[docs/CONFIGURATION.md](docs/CONFIGURATION.md) for the full list.

### Running behind a reverse proxy

Set `base_url` to the public origin. The proxy must forward WebSocket upgrades
on `/api/ws`, and should set `X-Forwarded-For`. mqttview can also terminate TLS
itself via the `tls` section.

## Development

```bash
# Terminal 1 — API on :8114
go run ./cmd/mqttview -addr 127.0.0.1:8114

# Terminal 2 — Vite dev server on :5173, proxying /api to the Go server
cd web && npm install && npm run dev
```

Tests run against a real in-process MQTT broker, so they exercise actual
protocol packets rather than mocks:

```bash
go test ./...
```

## Security notes

- mqttview holds credentials for every broker it can reach. Run it behind TLS
  and keep `data/secret.key` (or `MQTTVIEW_SECRET_KEY`) safe: it is what
  decrypts stored broker passwords.
- "Skip certificate verification" exists because self-signed home brokers are
  everywhere, but a connection using it is marked insecure in the UI.
- Found a vulnerability? See [SECURITY.md](SECURITY.md).

## Licence

MIT — see [LICENSE](LICENSE).
