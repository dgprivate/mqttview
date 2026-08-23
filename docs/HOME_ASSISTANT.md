# mqttview and Home Assistant

mqttview runs in two modes. This document is about the second one.

| Mode           | Who decides you may use it | How it is installed                    |
| -------------- | -------------------------- | -------------------------------------- |
| **standalone** | mqttview                   | Go binary, Docker image, compose        |
| **ingress**    | Home Assistant             | Home Assistant add-on, sidebar panel    |

## Which one do I want?

Answer one question: **does your Home Assistant have an Add-on Store?**

- **Home Assistant OS or Supervised** — yes. Install the **add-on**. mqttview
  appears in the sidebar, there is no login, and no port is published.
- **Home Assistant Container or Core** — no. There are no add-ons at all, and
  ingress does not exist. Run mqttview standalone and use the **HACS
  integration** to put a link to it in the sidebar. mqttview keeps its own
  sign-in, because nothing has authenticated you on its behalf.

### A note on HACS

HACS installs integrations, dashboard cards and themes. It does **not** install
add-ons — those come from an add-on repository added in the Add-on Store. Both
live in this repository, which is why the instructions differ.

So "install through HACS and get a tab with no login" is two different things
that cannot both be true at once: the no-login part comes from ingress, and
ingress comes from the add-on. If you have Supervisor, take the add-on; it is
the better experience by some distance.

## The add-on

Settings → Add-ons → Add-on Store → ⋮ → Repositories → add
`https://github.com/mqttview/mqttview`, then install **mqttview**.

Options are documented in `addon/mqttview/DOCS.md`.

### How the no-login part works, and what it rests on

Under ingress, Home Assistant is the only thing that can reach the add-on. It
authenticates the person, checks they may see the panel, and forwards the
request with headers saying who they are:

```
X-Remote-User-Id            a stable id, survives a rename
X-Remote-User-Name          the login name
X-Remote-User-Display-Name  what Home Assistant shows
X-Ingress-Path              /api/hassio_ingress/<token>
```

mqttview reads those headers **only after** checking the request came from the
Supervisor at `172.30.32.2`. That check is the whole of the security here.
Without it, anybody who could reach the port would be whoever they typed into
a header, so:

- the add-on publishes **no port**;
- `trusted_proxies` is not an add-on option, because there is no other address
  an add-on has any reason to trust;
- an empty `trusted_proxies` is a configuration error, not "trust everyone".

`X-Ingress-Path` is read in ingress mode only, and constrained to something
that can only be a path — no other origin, no quotes, no control characters.
Outside ingress mode it is ignored entirely, because outside ingress mode
nothing has checked who set it.

### Roles

Home Assistant does not tell an add-on whether you are one of its
administrators. mqttview cannot copy a fact it is not told, so instead:

- everybody who opens the panel gets `default_role` (`operator` by default);
- `admin_users` lists Home Assistant usernames or IDs that get `admin`;
- both are re-applied on every request, so a change takes effect on the next
  page load — including taking somebody *out* of `admin_users`.

Accounts are created the first time somebody opens the panel and keyed on the
stable user id, so renaming somebody in Home Assistant does not strand their
mqttview account and its settings.

### What is switched off in ingress mode

Every local sign-in route: login, logout, password change, two-factor
enrolment, OIDC and SAML. They are not merely hidden in the UI — they are not
mounted, so there is no endpoint to reach. A password endpoint for a password
nobody ever set is only somewhere to knock.

The first-administrator bootstrap is skipped too, for the same reason: it would
print a generated password for a door that does not exist.

### Older Supervisor versions

Some do not send the identity headers. mqttview refuses rather than guessing,
because "no identity" must never quietly become "some identity". If that is
your situation, set `fallback_user`: everybody who opens the panel then shares
one named account. It is a real downgrade, and it is opt-in for that reason.

## The HACS integration

For Container and Core installs.

1. HACS → Integrations → ⋮ → Custom repositories → add this repository as an
   **Integration**.
2. Install **mqttview**, restart Home Assistant.
3. Settings → Devices & Services → Add Integration → mqttview, and give it the
   URL your **browser** uses to reach mqttview.

That URL is fetched by the browser, not by Home Assistant: the panel is an
iframe. `http://localhost:8114` would mean the phone you are holding.

### It will be blank until you allow framing

mqttview refuses to be framed by default — a page that frames it can overlay
it and collect clicks meant for a broker command. Allow your Home Assistant
origin explicitly:

```yaml
# mqttview.yaml
frame_ancestors:
  - https://homeassistant.example.com
```

And mixed content applies as usual: an `https://` Home Assistant cannot frame
an `http://` mqttview. Put both behind TLS.

### It still has a login

Nothing has authenticated you on mqttview's behalf here, so it asks. That is
the honest behaviour rather than a limitation to work around: an iframe from
another origin is not evidence of anything. Use SSO if you want one sign-in —
mqttview speaks OIDC and SAML, and Home Assistant is not either.

## Running both

There is nothing stopping you running the add-on for the panel and a
standalone instance for access from outside. They are separate installs with
separate databases; a broker connection added in one does not appear in the
other.
