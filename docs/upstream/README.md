# Fixes prepared for upstream

The findings themselves are in [../UPSTREAM.md](../UPSTREAM.md). This directory
holds the changes written to fix them, so they are not lost between being
written and being submitted.

## paho-golang-inflight-race.patch

Fixes the data race between a reconnect and a publish in
`eclipse/paho.golang`, described in `docs/UPSTREAM.md`. Written against
`3115495` on `master` (v0.23.0 plus 19 commits).

Two files: three lines of `paho/session/state/state.go`, and a test that
reproduces the race.

### What was checked

- The test reports the race on the unpatched code (four reports in one run)
  and passes on the patched one.
- `go test -race ./autopaho/... ./paho/...` gives the same result before and
  after: everything passes except `autopaho/queue/file`, which fails on the
  untouched upstream tree here as well and has nothing to do with this.
- mqttview's own `TestAnMQTT5SessionRecoversAfterTheBrokerRestarts` — the test
  that found this, and which failed about once in a hundred runs — ran 40
  times under `-race` against the patched library with no failures.

### Opening the pull request

Eclipse projects require the Eclipse Contributor Agreement, signed with the
same email as the commit author. The commit is authored as
`Dean Gostiša <dean@black.si>` and carries `Signed-off-by:` with that address,
which is what their CI checks; if you sign the ECA with a different address,
amend the commit to match.

```bash
git clone https://github.com/eclipse/paho.golang /tmp/paho.golang
cd /tmp/paho.golang
git checkout -b fix/inflight-race
git am /path/to/paho-golang-inflight-race.patch
git remote add fork git@github.com:<your-user>/paho.golang.git   # fork it first
git push fork fix/inflight-race
```

Then open the pull request against `eclipse/paho.golang:master`. The commit
message is written to serve as the description: it has both stacks, why the
obvious fix (holding the mutex across the acquire) is wrong, and why the
behaviour is unchanged.
