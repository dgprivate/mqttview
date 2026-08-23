# mqttview

[![CI](https://github.com/dgprivate/mqttview/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/dgprivate/mqttview/actions/workflows/ci.yml)
[![publish image](https://github.com/dgprivate/mqttview/actions/workflows/publish-image.yml/badge.svg?branch=main)](https://github.com/dgprivate/mqttview/actions/workflows/publish-image.yml)
[![codecov](https://codecov.io/github/dgprivate/mqttview/branch/main/graph/badge.svg)](https://app.codecov.io/github/dgprivate/mqttview)
[![CodeQL](https://github.com/dgprivate/mqttview/actions/workflows/codeql.yml/badge.svg?branch=main)](https://github.com/dgprivate/mqttview/actions/workflows/codeql.yml)
[![Trivy](https://github.com/dgprivate/mqttview/actions/workflows/trivy.yml/badge.svg?branch=main)](https://github.com/dgprivate/mqttview/actions/workflows/trivy.yml)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/dgprivate/mqttview/badge)](https://scorecard.dev/viewer/?uri=github.com/dgprivate/mqttview)
[![Docker Hub](https://img.shields.io/docker/v/hausbit/mqttview?sort=semver&label=Docker%20Hub)](https://hub.docker.com/r/hausbit/mqttview)
[![image size](https://img.shields.io/docker/image-size/hausbit/mqttview/latest)](https://hub.docker.com/r/hausbit/mqttview/tags)
[![pulls](https://img.shields.io/docker/pulls/hausbit/mqttview)](https://hub.docker.com/r/hausbit/mqttview)
[![Go](https://img.shields.io/badge/go-1.27-blue)](https://github.com/dgprivate/mqttview/blob/main/go.mod)
[![License MIT](https://img.shields.io/badge/license-MIT-blue)](https://github.com/dgprivate/mqttview/blob/main/LICENSE)

A realtime web UI for MQTT brokers — every protocol version, every transport,
proper authentication, and a plugin system that can turn raw topics into
something you actually recognise.

Published as [`hausbit/mqttview`](https://hub.docker.com/r/hausbit/mqttview) for
`linux/amd64` and `linux/arm64`, signed, with an SBOM inside the image. There is
a [Home Assistant add-on](docs/HOME_ASSISTANT.md) too, and it needs no login.

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
- Two-factor authentication: TOTP (RFC 6238) with single-use recovery codes,
  optional per account or required for every local account
- Single sign-on two ways: **OIDC** through any compliant issuer — Google,
  Authentik, Keycloak, Okta, Entra ID — with PKCE and nonce checking, and
  **SAML 2.0** with signed assertions, published SP metadata and attribute
  mapping. Both honour domain allow-lists and can grant admin on first login
- Three roles: `viewer` reads, `operator` publishes and controls, `admin`
  manages brokers, users and plugins
- Session cookies are `HttpOnly` and stored hashed; writes require a
  double-submit CSRF token; WebSocket origins are checked against `base_url`
- Broker passwords and private keys are encrypted at rest with AES-256-GCM and
  are never sent back to the browser

**Plugins**

- A small, documented Go interface: observe messages, keep state, expose HTTP
  routes, push events to the browser, publish back to the broker
- Bundled: **Home Assistant MQTT discovery** and **Beckhoff PLC** — see below
- An MCP server (`cmd/mqttview-mcp`) puts the PLC's live signals in front of an
  AI agent, so PLC logic can be written against the button you just pressed
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

## The Beckhoff PLC plugin

Not every installation announces itself. A PLC typically publishes one topic per
numeric address — `plc/digital/input/245` — and keeps the meaning of that address
somewhere else entirely. This plugin folds the two back together: it reads the
metadata stream keyed by the point's PLC name (`DI-31-5`) and joins it onto the
values, so the topic tree becomes a list of named points in named rooms.

It understands digital inputs and outputs, temperature channels, DALI ballasts
with their levels and error codes, shades, door locks and valves, three-phase
electricity with imbalance and alarms, M-Bus meters, and the controller's own
watchdog and stream ages.

**Discovery view.** The panel keeps a journal of digital transitions and shows
the most recent one in large type. Press a wall switch, read back which address
moved and what it is called. Only real transitions are recorded — the retained
values that arrive when a broker connects are skipped, so the log means
something the moment it appears.

**Naming.** The PLC describes only the points somebody configured; the rest are
addresses. Name one yourself from the discovery log or the I/O table and it
takes precedence, with a free-text note for what the signal ought to do. Names
are stored in mqttview, not pushed to the PLC, and survive a restart. Press a
button, name it, and the wiring list writes itself.

**Control** is off until you switch it on, and what it will send is an
allow-list rather than everything the PLC accepts:

| Tier | Commands | Setting |
| --- | --- | --- |
| Lights | DALI on, off, level, query, refresh; PLC state refresh | Allow DALI control |
| Outputs | digital set, addresses 1–80 | Allow driving digital outputs |

The second tier is deliberately separate and deliberately awkward: among the
eighty outputs are a door lock, a water valve and a cooktop relay. Everything
else the PLC understands — arming the alarm, tripping a panic, driving the
valve, flipping the direct-to-Home-Assistant mode, wiping persistent state — is
absent from the catalogue and refused. An allow-list means a command added to
the PLC tomorrow cannot quietly become reachable from a browser. Sending also
needs the operator role, and every command is logged with the user who sent it.

### Programming a PLC with an agent

`cmd/mqttview-mcp` serves that same journal over the Model Context Protocol, so
an AI agent can watch the house while you walk around it:

```bash
go build -o mqttview-mcp ./cmd/mqttview-mcp

MQTTVIEW_URL=http://127.0.0.1:8114 \
MQTTVIEW_EMAIL=you@example.com \
MQTTVIEW_PASSWORD=... \
./mqttview-mcp
```

Register it as a stdio MCP server. The tools are `plc_wait_for_signal`, which
blocks until something moves and names it, `plc_name_point`, which records what
you call it, plus `plc_recent_signals`, `plc_find_points`, `plc_lights` and
`plc_overview`.

The workflow it is built for: ask the agent to wait, press the button, and it is
told the address, PLC name, human label and location of the point you just
touched — then tell it what that one is called and what it should do. What comes
out is a specification that names a real point — "when DI-31-5 rises, do this" —
rather than one that guesses at an address.

The MCP server cannot actuate anything. Naming writes to mqttview's own store;
everything else is a read. Commands stay behind the panel, where the setting and
the role are.

## Many brokers, one instance

mqttview is built around holding several brokers open at once, not switching
between them. Each connection has its own protocol version, credentials, TLS
material, subscriptions, topic tree and message history — a production broker on
MQTT 5 over TLS and a workshop Raspberry Pi on 3.1.1 plaintext coexist happily,
and neither one's traffic appears in the other's view.

Connections reconnect on their own, auto-connect at startup if you ask them to,
and survive restarts with their credentials decrypted from the data volume.
Plugins see every connection: the Home Assistant device list can span all of
them or filter to one.

## Home Assistant

mqttview installs as a **Home Assistant add-on** with a sidebar panel and no
login of its own: under ingress, Home Assistant has already decided you may
open the panel, and asking for a second password would be one more credential
to lose rather than one more lock.

Settings → **Apps** → ⋮ → Repositories → add
`https://github.com/dgprivate/mqttview`, then install **mqttview**.

(Home Assistant renamed add-ons to *apps*; older versions call the same thing
Add-ons. From a terminal on the host it is
`ha store add https://github.com/dgprivate/mqttview`.)

No port is published, and mqttview refuses any request that did not come
through the Supervisor — that check is what makes the identity headers worth
anything.

Running Home Assistant **Container** or **Core**? Those have no add-ons at all.
Run mqttview standalone and add the sidebar link with the HACS integration in
this repository; mqttview keeps its own sign-in there, because nothing has
authenticated you on its behalf. Note that HACS installs integrations and
cards, not add-ons — the two paths are separate on purpose.

**[docs/HOME_ASSISTANT.md](docs/HOME_ASSISTANT.md)** explains which path fits
your install, how the no-login mode works, and what it rests on.

## Quick start with Docker Compose

```bash
git clone https://github.com/dgprivate/mqttview.git
cd mqttview
cp .env.example .env

# Generate the key that encrypts stored broker credentials, then keep it.
sed -i "s/^MQTTVIEW_SECRET_KEY=.*/MQTTVIEW_SECRET_KEY=$(openssl rand -hex 32)/" .env

docker compose up -d
docker compose logs mqttview        # the generated admin password is printed once
```

Open <http://127.0.0.1:8114> and sign in.

Want two brokers to try it on? The `demo` profile starts a pair of Mosquitto
containers alongside:

```bash
docker compose --profile demo up -d
```

Then add both in the UI as `mqtt://mosquitto-a:1883` and
`mqtt://mosquitto-b:1883` — they are on the same Compose network, so the service
names resolve.

The image is a multi-stage build: Node builds the frontend, Go embeds it into a
static CGO-free binary, and the result runs as a non-root user on Alpine with a
read-only root filesystem and a health check. Only `/data` is writable, and that
is the volume holding the database and the encryption key.

### Plain Docker

```bash
docker run -d -p 127.0.0.1:8114:8114 \
  -e MQTTVIEW_BASE_URL=http://127.0.0.1:8114 \
  -e MQTTVIEW_SECRET_KEY="$(openssl rand -hex 32)" \
  -v mqttview-data:/data \
  hausbit/mqttview
```

The published image is multi-architecture and signed. `docker build -t mqttview .`
builds the same thing from source if you would rather not trust a registry —
which is the point of the SBOM and the signature below.

### From source

```bash
make build            # frontend into web/dist, then the binary that embeds it
./mqttview -addr 127.0.0.1:8114
```

`./mqttview -check-config` reports what a config file resolves to, including
everything the environment overrode, without starting anything.

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

Deployment specifics — the container's shape, backups, reverse proxies and
systemd — are in [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md).

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

## Hardening

The image runs unprivileged with no Linux capabilities, a read-only root
filesystem, no package manager and no setuid binaries, and it carries a
CycloneDX SBOM of itself. Every base image is pinned by digest and every GitHub
Action by commit SHA, with Renovate moving both. CI runs CodeQL over the Go and
the TypeScript, govulncheck, `npm audit`, golangci-lint, a Trivy scan that fails
on a fixable HIGH or CRITICAL, and coverage-guided fuzzing of the parsers that
read input mqttview does not control. Published images are signed with keyless
cosign and carry SBOM and provenance attestations.

[`security/README.md`](security/README.md) has the CIS Docker Benchmark table,
the command that verifies each claim, and what is deliberately not claimed.

```bash
# Check the claims rather than the badges
docker compose run --rm --entrypoint /bin/sh mqttview -c 'id; grep CapEff /proc/self/status'
docker run --rm --entrypoint cat mqttview:latest /usr/share/mqttview/sbom.cdx.json | head
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
