# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub's
[security advisory](https://github.com/mqttview/mqttview/security/advisories/new)
form rather than opening a public issue.

Include what you can: the affected version, how to reproduce it, and what an
attacker gets out of it. A working proof of concept is welcome but not required.

You should get an acknowledgement within a few days. Once a fix is out we are
happy to credit you in the advisory unless you would rather stay anonymous.

## What mqttview holds

Think of an mqttview instance as being as sensitive as the brokers it can reach:

- **Broker credentials.** Passwords and TLS private keys are encrypted at rest
  with AES-256-GCM, but the server necessarily decrypts them to connect. The key
  lives in `data_dir/secret.key` (mode 0600) or `MQTTVIEW_SECRET_KEY`.
- **Broker access.** Any user with the `operator` role can publish to any
  configured broker. On a home automation broker that means physical control of
  the house.
- **Message contents.** The topic tree and message history are held in memory
  and are visible to every signed-in user, including `viewer`. There is no
  per-topic access control yet — if a broker carries data some users must not
  see, give it a separate mqttview instance.

## What the code already does

- argon2id password hashing (64 MiB, 3 passes), constant-time comparison, and a
  matching dummy verification on unknown accounts so response timing does not
  reveal whether an address exists
- Login rate limiting per email and client address
- Session tokens stored as SHA-256 hashes, so a database leak does not yield
  usable cookies; `HttpOnly`, `SameSite=Lax`, and `Secure` when `base_url` is
  HTTPS
- Double-submit CSRF tokens on every state-changing request
- WebSocket origin checks derived from `base_url`
- OIDC with PKCE and nonce verification; unverified email claims are rejected
- A strict Content-Security-Policy, `X-Frame-Options: DENY` and `nosniff`
- Bounded request bodies, and `DisallowUnknownFields` on JSON decoding
- Broker passwords and private keys are never returned by the API

## Deployment advice

- Terminate TLS, and set `base_url` to the HTTPS origin so cookies are marked
  `Secure`.
- Do not expose mqttview to the internet without either TLS plus SSO, or an
  authenticating proxy in front.
- Back up `data_dir/secret.key` separately from the database, and treat it as a
  credential.
- Give people the lowest role that works. `viewer` cannot publish.
- "Skip certificate verification" defeats the point of TLS. It exists for
  self-signed home brokers; do not use it against anything crossing a network
  you do not control.

## Supported versions

Until 1.0, only the latest release gets fixes.
