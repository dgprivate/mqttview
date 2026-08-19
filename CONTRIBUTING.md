# Contributing to mqttview

Thanks for taking the time. This document covers what you need to know before
opening a pull request.

## Getting set up

```bash
git clone https://github.com/mqttview/mqttview.git
cd mqttview

# Terminal 1 — the API
make dev

# Terminal 2 — the frontend, proxying /api to the Go server
cd web && npm install && npm run dev
```

Open <http://127.0.0.1:5173>. The admin password is printed once by the Go
server on first run.

You do not need an MQTT broker installed: the tests run one in-process, and
`docker run -p 1883:1883 eclipse-mosquitto` is enough for manual work.

## Before you open a PR

```bash
make lint    # go vet + tsc
make test    # Go tests, race detector in CI
make fmt     # gofmt
```

CI runs the tests with `-race`. Anything touching the client layer, the plugin
runtime or the hub should be run that way locally too.

## What we look for

**Comments explain why, not what.** `// increment i` is noise; `// paho 3 only
restores subscriptions for persistent sessions, so we always replay our own
set` is the reason the next person will not delete the line.

**Errors say what to do.** `"topic filter must not be empty"` beats
`"validation error"`. The message ends up in front of a user in a browser.

**Do not guess on the user's behalf.** The Home Assistant plugin does not
implement Jinja, so when it meets a template it cannot evaluate it shows the raw
payload and marks it. That is the pattern: be honest about the limit rather than
displaying a plausible-looking wrong number.

**Nothing blocks the message path.** MQTT callbacks run on the client's
goroutine. If your code might be slow, buffer and drop rather than block, and
make the drop visible.

**Mobile matters.** Every view has to work on a phone. Keep the CSS
mobile-first, tap targets at least 44×44px, inputs at 16px so iOS does not zoom,
and turn wide tables into cards below 640px. Check your change at 375px wide.

## Testing

Tests run against a real broker (`internal/testutil`), not mocks, because the
bugs worth catching in this codebase are protocol bugs. Prefer an integration
test that connects, publishes and asserts on what came back.

Pure logic — topic matching, discovery parsing, template evaluation, command
building — should have table-driven unit tests.

## Adding a plugin

See [docs/PLUGINS.md](docs/PLUGINS.md). Before proposing a plugin for the main
repository, consider whether it needs to live here: the interface is stable
enough to maintain one out of tree.

## Adding protocol or transport support

If you are adding a client library or transport, add it to
`TestRoundTripEveryVersion` in `internal/mqttc/manager_test.go`. A transport
without a test that connects and publishes is not supported, it is aspirational.

## Reporting bugs

Include the broker and version, the MQTT protocol version, mqttview's version
(`./mqttview -version`), and the relevant part of the log at `-log-level debug`.
For discovery bugs, the raw config payload is worth more than a description of
it — please redact anything sensitive first.

## Security

Do not open a public issue for a vulnerability. See [SECURITY.md](SECURITY.md).

## Licence

Contributions are accepted under the MIT licence that covers the project.
