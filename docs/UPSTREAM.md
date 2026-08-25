# Defects found in things mqttview depends on

Two, both found by the test suites here, both reproducible, neither fixed in
the dependency's latest release at the time of writing. They are written down
so that the workarounds in this repository have a reason attached and can be
removed when the reason goes away.

## eclipse/paho.golang v0.23.0 — data race between a reconnect and a publish

**Where:** `paho/session/state/state.go`

`ConAckReceived` replaces the send quota when a connection comes up:

```go
// state.go:206
s.inflight = newSendQuota(recvMax)
```

`AddToSession` reads it while queuing an outbound packet:

```go
// state.go:379
if err := s.inflight.Acquire(ctx); err != nil {
```

The two run on different goroutines — autopaho's reconnect loop and whoever
called `Publish` — and the field is not guarded on both paths. The race
detector reports it as a write/read pair on the same address.

**How it shows:** publish continuously at QoS 1 while the broker drops the
session, which is what `TestAnMQTT5SessionRecoversAfterTheBrokerRestarts` in
`internal/mqttc` does. It reports roughly once in a hundred runs of the full
suite under `-race`, and more often on a loaded machine.

**Consequence:** the send quota is the accounting for how many QoS 1 and 2
messages may be in flight. Losing an update to it means a client that either
stalls waiting for capacity it already has, or exceeds the receive maximum the
broker asked for.

**Status:** fixed upstream —
[eclipse-paho/paho.golang#341](https://github.com/eclipse-paho/paho.golang/pull/341),
merged 2026-08-24. `AddToSession` takes its reference to the quota under the
mutex it already holds, rather than reading the field after releasing it; it
cannot hold the mutex across the acquire, because that call blocks by design.

mqttview is on it. There is no release carrying it yet — the newest tag,
v0.23.0, predates it — so `go.mod` names the merge commit
(`v0.23.1-0.20260824221508-ae76bd52efac`). Move to the tag when one appears;
nothing here depends on the pseudo-version other than wanting the fix. The
copy of the change is kept in `docs/upstream/` for reference.

Since the update, `TestAnMQTT5SessionRecoversAfterTheBrokerRestarts` — the test
that found this, and that failed roughly once in a hundred runs — has run 100
times under `-race` on a deliberately loaded machine without a failure.

## eclipse-mosquitto 2.1 — an empty WebSocket frame ends the packet

**Where:** the WebSocket handling rewritten for 2.1; 2.0.22 and earlier are
unaffected.

A zero-length binary WebSocket frame in the middle of an MQTT packet makes 2.1
treat the packet as finished, and it disconnects the client with "malformed
packet". A probe against both versions, sending the same SUBSCRIBE five ways:

| framing                        | 2.0.22 | 2.1.2 |
| ------------------------------ | ------ | ----- |
| one frame                      | ok     | ok    |
| split across two frames        | ok     | ok    |
| one byte per frame             | ok     | ok    |
| CONNECT split, SUBSCRIBE whole | ok     | ok    |
| an empty frame first           | ok     | **hangs** |
| a text frame                   | ok     | refused (correctly) |

The empty frame is legal. RFC 6455 allows a zero-length payload, and MQTT is
explicit that a receiver "MUST NOT assume that MQTT Control Packets are aligned
on WebSocket frame boundaries" — the frames carry a byte stream, and one
carrying no bytes contributes nothing to it.

**How it showed:** mqttview's MQTT 5 client connects to 2.1 over WebSockets,
then fails to subscribe and receives nothing, while reporting a healthy
connection. autopaho turns each write into a frame and paho writes a packet in
pieces, one of which — the empty property section of a SUBSCRIBE — is zero
bytes.

**Worked around:** yes, in `internal/mqttc/websocket_v5.go`. mqttview dials the
WebSocket itself and never emits an empty frame. The bytes that make up the
packet are identical either way, so nothing is hidden by this; it is cheaper
than asking everybody running 2.1 to change transport. `test/mosquitto` runs
its matrix against 2.1 so a regression is visible.

**Fixed upstream too:**
[eclipse-mosquitto/mosquitto#3724](https://github.com/eclipse-mosquitto/mosquitto/pull/3724),
closing their issue #3704, which describes the same defect. `net__read_ws()`
returned a positive byte count for a frame it wrote nothing for — the value
left in `len` by the last header field read — so the MQTT parser consumed that
many stale buffer bytes. It reports `EAGAIN` instead, which is what the same
function already does for PING and CLOSE.

The workaround here stays regardless: it is what keeps mqttview working against
the 2.1 releases already installed.
