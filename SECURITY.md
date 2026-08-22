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
- SAML 2.0 assertions verified for signature, audience, conditions and
  timestamps against exactly one expected request ID, so an assertion minted for
  another login cannot be replayed into this one
- Two-factor authentication is TOTP (RFC 6238) with a one-step drift window and
  constant-time comparison. Secrets are encrypted at rest and recovery codes are
  single use and hashed. An enrolment is not enforced until a code proves it, so
  a half-finished setup cannot lock an account out, and the sign-in rate limiter
  stays armed between the password and the code
- `X-Forwarded-For` is trusted only when `auth.trust_proxy_headers` is on,
  because that value keys the sign-in rate limit
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

## What the project does about supply chain and the container

[`security/README.md`](security/README.md) is the detailed version: which CIS
Docker Benchmark controls the container satisfies and the command that verifies
each one, how a finding that cannot be fixed and does not apply is recorded as
an OpenVEX statement, and — just as important — what is deliberately **not**
claimed.

In short:

* the image runs unprivileged with no capabilities, a read-only root
  filesystem, no package manager and no setuid binaries
* it carries a CycloneDX SBOM of itself; published images also carry registry
  SBOM and provenance attestations and a keyless Sigstore signature
* every base image and every GitHub Action is pinned by digest or commit SHA,
  Renovate moves those pins, and the image is rebuilt weekly so that base
  patches actually ship
* CI fails on a fixable HIGH or CRITICAL finding, and runs CodeQL, govulncheck,
  `npm audit` and OpenSSF Scorecard
* each tagged release carries its SBOM as an asset with a Sigstore bundle
  beside it:

```bash
cosign verify-blob --bundle mqttview-v0.1.0.sbom.cdx.json.sigstore.json \
  --certificate-identity-regexp '^https://github.com/.+/.github/workflows/publish-image.yml@.+' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  mqttview-v0.1.0.sbom.cdx.json
```

Verify a published image the same way:

```bash
cosign verify ghcr.io/dgprivate/mqttview@sha256:... \
  --certificate-identity-regexp '^https://github.com/.+/.github/workflows/publish-image.yml@.+' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Supported versions

Until 1.0, only the latest release gets fixes. `latest` and the current tag get
the weekly rebuild; older tags stay published for reproducibility but are not
patched.
