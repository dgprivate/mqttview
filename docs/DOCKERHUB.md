# mqttview

**A realtime web UI for MQTT brokers.** Every protocol version, every transport,
proper authentication, and a plugin system that turns raw topics into something
you recognise.

[Source, issues and documentation on GitHub](https://github.com/dgprivate/mqttview) ·
[Configuration](https://github.com/dgprivate/mqttview/blob/main/docs/CONFIGURATION.md) ·
[Home Assistant](https://github.com/dgprivate/mqttview/blob/main/docs/HOME_ASSISTANT.md) ·
[Security](https://github.com/dgprivate/mqttview/blob/main/SECURITY.md)

---

## Run it

```bash
docker run -d -p 127.0.0.1:8114:8114 \
  -e MQTTVIEW_BASE_URL=http://127.0.0.1:8114 \
  -e MQTTVIEW_SECRET_KEY="$(openssl rand -hex 32)" \
  -v mqttview-data:/data \
  hausbit/mqttview
```

The first start prints a generated administrator password to the container
log, once. `MQTTVIEW_SECRET_KEY` encrypts stored broker passwords and TLS
private keys — keep it, or you will be re-entering every credential.

With compose, see
[compose.yaml](https://github.com/dgprivate/mqttview/blob/main/compose.yaml),
which ships the hardened runtime settings the image expects.

## Tags

| Tag | What it is |
| --- | --- |
| `latest` | The newest build of `main` |
| `main` | The same thing, named after the branch |
| `sha-<short>` | One specific commit, for pinning |
| `x.y.z` | A release tag |

`linux/amd64` and `linux/arm64`.

## What is in the image

A single static Go binary with the frontend embedded, on Alpine. No package
manager, no shell utilities beyond what Alpine ships, no setuid binaries. It
runs as uid 10001 on a read-only root filesystem; only `/data` is writable, and
that is the volume holding the database and the encryption key.

Each image carries a CycloneDX SBOM of itself at
`/usr/share/mqttview/sbom.cdx.json`:

```bash
docker run --rm --entrypoint cat hausbit/mqttview:latest \
  /usr/share/mqttview/sbom.cdx.json | head
```

Images are signed with cosign, keyless, bound to the workflow that built them:

```bash
cosign verify docker.io/hausbit/mqttview:latest \
  --certificate-identity-regexp '^https://github.com/dgprivate/mqttview/.github/workflows/publish-image.yml@.+' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

[security/README.md](https://github.com/dgprivate/mqttview/blob/main/security/README.md)
documents every hardening claim with the command that proves it, rather than a
badge.

## Home Assistant

There is an add-on, and it needs no login: under ingress, Home Assistant has
already decided who you are.

Settings → Add-ons → Add-on Store → ⋮ → Repositories →
`https://github.com/dgprivate/mqttview`

Running Home Assistant Container or Core, which have no add-ons? Run this image
and add the sidebar link with the HACS integration in the same repository.
[docs/HOME_ASSISTANT.md](https://github.com/dgprivate/mqttview/blob/main/docs/HOME_ASSISTANT.md)
explains which path fits your install and what the no-login mode rests on.

## Configuration

Everything has an environment variable; the full list is in
[docs/CONFIGURATION.md](https://github.com/dgprivate/mqttview/blob/main/docs/CONFIGURATION.md).
`mqttview -check-config` prints what a configuration actually resolves to,
including anything the environment overrode, without starting a thing.

## Licence

MIT. Source at
[github.com/dgprivate/mqttview](https://github.com/dgprivate/mqttview).
