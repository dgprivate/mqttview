# Working on mqttview

Rules for anyone — human or Claude — changing this repository. They exist
because each one was learned the expensive way; the reasoning is given so you
can tell when a rule genuinely does not apply rather than guessing.

## What this is

A single Go binary with a React frontend embedded in it. `cmd/mqttview` is the
server, `internal/` is everything it does, `web/` is the UI, and
`cmd/mqttview-mcp` is a separate binary that exposes the PLC plugin over MCP.

It ships three ways: the binary, the hardened container image, and a Home
Assistant app that wraps the same image. There are two of those, `addon/` and
`addon-hassconfig/`, and they are the same app with different permissions: the
second maps Home Assistant's configuration directory read-only so it can pick
the broker up from the MQTT integration. Everything except four fields of
`config.yaml` is byte-identical between them and a test enforces that, so a
change goes in `addon/` and gets copied. `custom_components/` is a HACS
integration that only adds a sidebar link — it is not the app and does not
remove the login.

Data lives in one SQLite file plus an encryption key beside it. There is no
other state: no cache to warm, no queue, no second service.

## Before you say it is done

Run all of this. Not one of them, all of them:

```bash
gofmt -l ./cmd ./internal        # must print nothing
go vet ./...
go test -race ./...
golangci-lint run                # config in .golangci.yml
cd web && npx tsc --noEmit && npm run build
```

`make build` produces the binary with the frontend baked in.

**Never report work as finished without running the tests.** If they fail, say
so and show the output. A summary that says "should work" is worth less than
nothing, because it costs somebody else the time to discover otherwise.

## Tests

**Every change ships with tests.** Not "where it makes sense" — every change. A
bug fix ships with the test that fails without it. CI enforces a coverage floor
of 88%; raise it as the number rises, and never lower it to make a build pass.

That 88 is deliberately below the 90.6 a developer machine measures. The gap is
the runner: 127 of 550 functions cover slightly less on a slower, smaller box,
because a ticker fires fewer times or `runtime.NumCPU` takes another branch.
Some of that is irreducible. Some of it is tests that pass without exercising
what they claim — a long poll whose goroutine fires after the assertion — and
those are bugs in the tests. Fix them and put the floor back to 90.

Reaching a number is not the point, and a test written only to touch a line is
worse than no test: it locks in whatever the code does today. Several of the
assertions here started out wrong and the *code* was right — check which it is
before changing either.

Write tests that state a property, not tests that restate the implementation:

- Name what must be true: `TestRecoveryCodeIsSpentExactlyOnce`, not `TestUse`.
- Assert the consequence, not the call. "A used code cannot sign in again" is a
  test. "UseRecoveryCode was called" is not.
- When a fuzzer or a linter finds something, commit the input as a regression
  test. `internal/plugins/hass/testdata/fuzz/` exists for exactly that.

### The component suites

`test/` holds the two end-to-end suites, and they cover the parts no unit test
can reach:

- **`test/homeassistant`** builds the real binary, runs an app's real `run.sh`
  against a fake Supervisor, and asserts every mode: standalone with a login,
  ingress without one, both apps' broker discovery paths, and the shape the
  Supervisor requires of the repository. The two apps are compared field by
  field so they cannot drift apart.
- **`test/mosquitto`** talks to a real Mosquitto — every transport (TCP, TLS,
  mutual TLS, WebSockets, secure WebSockets, Unix socket) against every
  protocol version, plus retained values, wills, sessions and reconnection. A
  TCP relay in front of the broker (`link_test.go`) is what makes wills and
  reconnects testable at all: the client API has no way to vanish, and killing
  the broker breaks both ends at once.

  It runs the whole matrix against **several broker versions**: whatever is
  installed locally, plus the images in `defaultImages` (2.0.22 and 2.1.2 at
  the time of writing), overridable with `MQTTVIEW_MOSQUITTO_IMAGES`. One
  version is not a compatibility test — 2.1 rejected something 2.0 accepted
  within a day of the matrix existing.

They found four real defects the day they were written, including an app option
that had never worked. If you change `run.sh`, a manifest, the client or the
ingress path, these are the tests that will tell you.

Both skip themselves when what they need is missing — `mosquitto`, `jq`, a
built frontend. CI installs those and then **counts the skips**, because a
suite that quietly skips itself is worse than no suite.

Anything that parses input from a broker, a device or a person gets a fuzz
target. `go test -fuzz` targets live beside the code they exercise; the CI job
runs each one on a corpus that grows week to week.

## Things that must not change quietly

Each of these is load-bearing. Changing one is a decision to be discussed, not
a refactor to be slipped in.

**The image is hardened, and CI checks it.** Unprivileged user 10001, no
package manager, no setuid binaries, read-only root, every capability dropped,
an SBOM inside the image. `security/README.md` documents each claim and the
command that proves it; the `docker` job in CI asserts them. If you change the
Dockerfile, run that job's checks locally.

Do **not** delete `/lib/apk/db` when trimming the image. It is the installed
package database, and without it Trivy sees zero OS packages and reports a
clean image because it cannot find anything to judge. Verify a scan names the
OS and a package count before believing it.

**Base images are pinned by digest and actions by commit SHA.** A tag is
mutable, and an action runs with the workflow's token. Renovate moves the pins.

**The PLC command catalogue is an allow-list.** `internal/plugins/plc/command.go`
enumerates every command the plugin will send. The PLC accepts far more — arming
the alarm, driving the water valve, wiping persistent state. Adding to that list
means deciding a browser button may do it in somebody's house. Do not.

**Credentials are encrypted at rest and never returned by the API.** Broker
passwords, TLS private keys and TOTP secrets go through `internal/secrets`.
`connectionView` and `User` deliberately omit them; keep it that way.

**Forwarded headers are not trusted by default.** `X-Forwarded-For` is read only
when `auth.trust_proxy_headers` is on, because the value keys the sign-in rate
limit and anyone can send that header. Do not reintroduce `middleware.RealIP`.

**Home Assistant mode believes a header, and one check is why that is safe.**
`internal/auth/ingress.go` reads `X-Remote-User-*` to decide who somebody is —
but only after `checkIngressSource` has proved the request came from the
Supervisor. Reorder those, widen `trusted_proxies` to a default that includes
anything else, or read the headers anywhere outside that path, and mqttview
becomes an MQTT console where the attacker picks their own username. The same
goes for `X-Ingress-Path`: it is read in ingress mode only, and it is
constrained to something that can only be a path before it reaches the page.

The add-on publishes no port, deliberately. Adding one gives the same server a
second door that the source check has to hold shut forever.

**A user is loaded through two different queries.** `GetUser` and `SessionUser`
each have their own column list, and `SessionUser` is what every authenticated
request uses. Add a column to `users` and you must add it to both, or the field
will be silently empty for every signed-in user. There is a regression test for
this in `internal/store/twofactor_test.go`; keep it passing.

## Migrations

Append to the `migrations` slice in `internal/store/store.go`. Never edit a
migration that has shipped: existing installs have already run it and will not
run it again. Every migration is applied in a transaction and recorded by name.

## Conventions

**Code and comments in English.** Slovenian belongs in conversation, not in the
repository.

**Comments explain why, not what.** `// increment i` is noise. `// The limiter
stays armed until the second factor is in: resetting it here would let an
attacker who has the password grind the code` is the reason somebody will need
in a year. Match the density of the surrounding code.

**Errors say what to do about it.** `internal/mqttc/diagnose.go` is the model:
keep the underlying error, append a hint, never substitute. "context deadline
exceeded" names the timeout that expired, not the problem.

**Do not invent a second opinion about something the source already decides.**
The PLC computes current imbalance and raises its own alarm from it; a
threshold of our own alongside it was wrong and was removed. The same goes for
units, limits and severities that some other system owns.

**SAML signature verification is not ours to reimplement.** `crewjam/saml`
wraps `goxmldsig`, and a mistake in signature handling over canonicalised XML
is a silent authentication bypass, not a visible bug. Keep both dependencies
current: govulncheck caught a signature-bypass advisory in goxmldsig within
minutes of it being added here, and that is the whole reason the job exists.

**MQTT 5 over WebSockets is dialled by us, not by autopaho.** `internal/mqttc/websocket_v5.go`
exists because the library writes a packet in several pieces and one of them,
the empty property section of a SUBSCRIBE, becomes a zero-length WebSocket
frame. Mosquitto 2.1 treats that as the end of the packet and disconnects the
client as malformed; 2.0 does not. The frame is legal and pointless, so
mqttview does not send it. Do not replace the dialer with the library's
without checking `TestWebSockets` against 2.1.

**Never guess at a protocol.** The PLC command schema came from reading the
command processor's source on the Beckhoff PLC that publishes it, not from a
plausible-looking example. If the authority for a format is not to hand, say so
instead of inferring one — the Home Assistant ingress headers are the current
example of the second half of that rule.

## Frontend

Mobile-first. The base rules target phones and `@media (min-width: 641px)` adds
what a wider screen affords. Tables need `className="responsive"` **and**
`data-label` on each cell, or they stay a horizontally scrolling grid on a
phone — `data-label` alone does nothing. Tap targets are at least 44 px; inputs
are at least 16 px or iOS zooms on focus.

Check a real render before claiming a layout works. Screenshot both widths.
Sixty-four tiles that looked fine in the code were an unreadable wall on the
actual installation.

## Plugins

`docs/PLUGINS.md` is the contract. A plugin observes messages, keeps its own
state, exposes HTTP routes under `/api/p/<id>/` and pushes events to the
browser. Batch UI notifications; a broker at 45 messages a second will
otherwise re-render the page 45 times.

Anything that acts on the world is off by default, needs the operator role, and
is logged with the user who did it.

## What to ask about rather than decide

- Turning on control for anything physical, or widening what control can reach.
- Lowering the coverage floor, or disabling a linter rather than justifying an
  exclusion.
- Anything that changes what an existing install must do to keep working:
  the data directory layout, the UID, a migration that drops a column.
- Publishing anywhere new, or adding a dependency to the sign-in path.
