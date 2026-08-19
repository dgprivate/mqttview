# Configuration

mqttview reads `mqttview.yaml` from the working directory (override with
`-config`), then applies environment variables on top. Environment always wins,
so a container can keep secrets out of the file.

Everything has a default. Running `./mqttview` with no config at all works.

## Command-line flags

| Flag | Environment | Default | Meaning |
| --- | --- | --- | --- |
| `-config` | `MQTTVIEW_CONFIG` | `mqttview.yaml` | Config file path. A missing file is not an error. |
| `-addr` | `MQTTVIEW_ADDR` | `127.0.0.1:8114` | Listen address. |
| `-data` | `MQTTVIEW_DATA_DIR` | `./data` | Database and encryption key location. |
| `-log-level` | `MQTTVIEW_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |
| `-bootstrap-email` | `MQTTVIEW_BOOTSTRAP_EMAIL` | `admin@localhost` | First admin, created only when no users exist. |
| `-bootstrap-password` | `MQTTVIEW_BOOTSTRAP_PASSWORD` | generated | Its password. Generated and printed once if unset. |
| `-version` | — | — | Print the version and exit. |

## Server

```yaml
addr: 127.0.0.1:8114
base_url: https://mqtt.example.com
data_dir: /var/lib/mqttview
secret_key: ""     # 32 bytes, hex or base64; generated into data_dir if empty
```

`base_url` matters more than it looks. It is used to build OAuth redirect URIs,
to decide whether session cookies get the `Secure` flag, and to check WebSocket
origins. Behind a reverse proxy, set it to the public URL or SSO and the live
view will both misbehave.

`secret_key` encrypts broker passwords and TLS private keys at rest. If it is
empty, mqttview generates `data_dir/secret.key` (mode 0600) on first run.
**Losing it means every stored broker password must be re-entered**; changing it
has the same effect. In Kubernetes, prefer `MQTTVIEW_SECRET_KEY` from a Secret.

## TLS for the UI

```yaml
tls:
  enabled: true
  cert_file: /etc/mqttview/tls.crt
  key_file: /etc/mqttview/tls.key
```

Independent of the TLS used to reach brokers. `MQTTVIEW_TLS_CERT` implies
`enabled: true`.

## Authentication

```yaml
auth:
  session_ttl_hours: 168
  allow_local: true
  allow_signup: false
  providers: {}
```

- `allow_local` — username and password sign-in. Turn it off to force SSO.
- `allow_signup` — whether an unrecognised but verified SSO identity may create
  an account. With `false`, an admin pre-creates users (leaving the password
  blank) and SSO links to them by email.

Startup refuses to proceed if `allow_local` is false and no provider is enabled,
rather than booting a server nobody can sign in to.

### SSO providers

Any OIDC-compliant issuer works. Google is not special-cased; it is just an
issuer URL.

```yaml
auth:
  providers:
    google:
      enabled: true
      display_name: Google
      issuer: https://accounts.google.com
      client_id: xxx.apps.googleusercontent.com
      client_secret: ...
      scopes: [openid, email, profile]
      allowed_domains: [example.com]
      admin_emails: [you@example.com]
```

The redirect URI to register with the provider is:

```
<base_url>/api/auth/sso/<provider-id>/callback
```

Per-provider environment overrides, where `<ID>` is the key uppercased with
hyphens turned into underscores:

- `MQTTVIEW_OIDC_<ID>_CLIENT_ID`
- `MQTTVIEW_OIDC_<ID>_CLIENT_SECRET`
- `MQTTVIEW_OIDC_<ID>_ISSUER`
- `MQTTVIEW_OIDC_<ID>_ENABLED`

Notes:

- Logins use PKCE and a nonce; the state is carried in an encrypted, short-lived
  cookie, so several replicas can share the flow with no shared session store.
- An email arrives unusable unless the provider marks it verified. A missing
  `email_verified` claim counts as unverified — accepting it would let a
  provider that permits arbitrary email claims impersonate any account.
- `allowed_domains` filters by email domain; empty means any domain the provider
  vouches for.
- `admin_emails` grants `admin` on first login only. Later role changes are made
  in the UI.

### Roles

| Role | Can |
| --- | --- |
| `viewer` | See connections, the topic tree, live messages and devices. |
| `operator` | Everything above, plus publish, change subscriptions, connect/disconnect, and control devices. |
| `admin` | Everything above, plus manage brokers, users and plugins. |

The last enabled admin cannot be demoted, disabled or deleted.

## Plugins

```yaml
plugins:
  home-assistant:
    enabled: true
    settings:
      discoveryPrefix: homeassistant
      subscribeQos: "0"
      allowControl: true
```

This section seeds the **first run only**. Once a plugin's settings exist in the
database, the UI is the source of truth and this block is ignored — otherwise a
config file would silently revert what someone changed in the UI.

## Environment variable summary

| Variable | Effect |
| --- | --- |
| `MQTTVIEW_CONFIG` | Config file path |
| `MQTTVIEW_ADDR` | Listen address |
| `MQTTVIEW_BASE_URL` | Public origin |
| `MQTTVIEW_DATA_DIR` | Data directory |
| `MQTTVIEW_SECRET_KEY` | Encryption key for stored broker credentials |
| `MQTTVIEW_TLS_CERT` / `MQTTVIEW_TLS_KEY` | UI TLS |
| `MQTTVIEW_ALLOW_LOCAL` | Enable/disable password login |
| `MQTTVIEW_ALLOW_SIGNUP` | Enable/disable SSO self-signup |
| `MQTTVIEW_LOG_LEVEL` | Log level |
| `MQTTVIEW_BOOTSTRAP_EMAIL` / `_PASSWORD` | First admin |
| `MQTTVIEW_OIDC_<ID>_*` | Per-provider credentials |
