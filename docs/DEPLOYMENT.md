# Deployment

mqttview ships as one static binary with the frontend embedded, so there is no
asset directory to serve and nothing to install alongside it. The container
image is that same binary on Alpine.

## Docker Compose

```bash
cp .env.example .env
sed -i "s/^MQTTVIEW_SECRET_KEY=.*/MQTTVIEW_SECRET_KEY=$(openssl rand -hex 32)/" .env
docker compose up -d
```

`compose.yaml` defines:

| Service | Profile | Purpose |
| --- | --- | --- |
| `mqttview` | default | The application. Published on `${MQTTVIEW_BIND}:${MQTTVIEW_PORT}`. |
| `mosquitto-a` | `demo` | A throwaway broker on host port 1883. |
| `mosquitto-b` | `demo` | A second one on 1884, so multi-broker support is visible. |

The demo brokers are behind a profile, so a plain `docker compose up -d` starts
only mqttview. Bring them in with `docker compose --profile demo up -d`, then
add `mqtt://mosquitto-a:1883` and `mqtt://mosquitto-b:1883` in the UI — Compose
DNS resolves the service names from inside the mqttview container.

Do not use `deploy/mosquitto/mosquitto.conf` for anything real: it allows
anonymous access, which is fine for two loopback containers holding nothing.

### The container's shape

- Runs as uid 10001, not root
- `read_only: true` with a tmpfs on `/tmp`; only the `/data` volume is writable
- `no-new-privileges`
- A health check on `/api/health`, so `docker compose ps` reports honestly
- No CGO, so the SQLite driver needs no system libraries

### Environment

`.env` feeds the Compose file. Everything is optional except, in practice,
`MQTTVIEW_SECRET_KEY` and `MQTTVIEW_BASE_URL`.

| Variable | Why it matters |
| --- | --- |
| `MQTTVIEW_SECRET_KEY` | Decrypts stored broker passwords and TLS keys. Generate once with `openssl rand -hex 32`. If left empty, one is generated into the volume — fine for a single host, wrong for anything you might rebuild. **Losing it means re-entering every broker credential.** |
| `MQTTVIEW_BASE_URL` | The URL a browser actually uses. OAuth redirect URIs, the `Secure` cookie flag and the WebSocket origin check all derive from it. Get it wrong and SSO breaks or the live view refuses to connect. |
| `MQTTVIEW_BIND` / `MQTTVIEW_PORT` | Where the port is published. Loopback by default. |
| `MQTTVIEW_BOOTSTRAP_EMAIL` / `_PASSWORD` | Used only on the first start, when no users exist. Leave the password empty and it is generated and printed to the container log once. |

See [CONFIGURATION.md](CONFIGURATION.md) for the rest, including SSO.

### Persistence

Everything lives in the `mqttview-data` volume: `mqttview.db` and, unless you
supplied one, `secret.key`. Back up both — and note that the key is a
credential, so it should not sit in the same place as the database.

```bash
docker run --rm -v mqttview_mqttview-data:/data -v "$PWD:/backup" \
  alpine tar czf /backup/mqttview-backup.tgz -C /data .
```

### Upgrading

```bash
git pull
docker compose build
docker compose up -d
```

Schema migrations run automatically at startup and are recorded in
`schema_migrations`. Connections reconnect on their own afterwards.

## Behind a reverse proxy

Terminate TLS at the proxy, set `MQTTVIEW_BASE_URL` to the public origin, and
forward WebSocket upgrades on `/api/ws`:

```nginx
location / {
    proxy_pass http://127.0.0.1:8114;
    proxy_set_header Host              $host;
    proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;

    # Required: without these the live view silently never connects.
    proxy_set_header Upgrade    $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_read_timeout 3600s;
}
```

Caddy needs no WebSocket configuration:

```
mqtt.example.com {
    reverse_proxy 127.0.0.1:8114
}
```

mqttview can also terminate TLS itself — see the `tls` section of the config.

## systemd

```ini
[Unit]
Description=mqttview
After=network-online.target
Wants=network-online.target

[Service]
User=mqttview
ExecStart=/usr/local/bin/mqttview -config /etc/mqttview/mqttview.yaml
Restart=always
RestartSec=5
StateDirectory=mqttview
Environment=MQTTVIEW_DATA_DIR=/var/lib/mqttview

# The process needs nothing but its state directory.
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
```

## Sizing

Memory is dominated by the topic tree and message ring, both per connection. The
tree stops at 200,000 topics (and says so in the UI) and keeps up to 256 KiB per
topic; history defaults to 2,000 messages of up to 64 KiB each. A handful of
ordinary home or building brokers sits comfortably in a couple of hundred MiB.

If you are pointing it at a broker carrying millions of topics, subscribe to the
subtrees you care about rather than `#`.
