# mqttview and Home Assistant

mqttview runs in two modes. This document is about the second one.

| Mode           | Who decides you may use it | How it is installed                 |
| -------------- | -------------------------- | ----------------------------------- |
| **standalone** | mqttview                   | Go binary, Docker image, compose     |
| **ingress**    | Home Assistant             | Home Assistant app, sidebar panel    |

Home Assistant renamed **add-ons** to **apps**. Both names appear below,
because which one you see depends on your version and the CLI still answers to
`ha addons` as an alias for `ha apps`.

## Which one do I want?

Answer one question: **does your Home Assistant have an app store?** Settings →
About shows the *Installation Type*, and that is what decides it.

- **Home Assistant OS or Supervised** — yes. Install the **app**. mqttview
  appears in the sidebar, there is no login, and no port is published.
- **Home Assistant Container or Core** — no. There are no apps at all, and
  ingress does not exist. Run mqttview standalone and use the **HACS
  integration** to put a link to it in the sidebar. mqttview keeps its own
  sign-in, because nothing has authenticated you on its behalf.

### A note on HACS

HACS installs integrations, dashboard cards and themes. It does **not** install
apps — those come from a repository added in the app store. They are separate
repositories as well as separate mechanisms, which is why the instructions
differ.

So "install through HACS and get a tab with no login" is two different things
that cannot both be true at once: the no-login part comes from ingress, and
ingress comes from the app. If you have Supervisor, take the app; it is the
better experience by some distance.

## The app

Settings → **Apps** → ⋮ → Repositories → add
`https://github.com/dgprivate/mqttview`, then install **mqttview**.

### Two apps, and which to install

The repository offers the same app twice. They are the same binary, the same
options and the same panel; they differ in one line of the manifest.

| App                                    | Slug                  | May read                                     |
| -------------------------------------- | --------------------- | -------------------------------------------- |
| **mqttview**                           | `mqttview`            | Who you are, and nothing else                |
| **mqttview (Home Assistant config access)** | `mqttview_hassconfig` | Also `/homeassistant` and `/ssl`, read-only |

**Install the plain one** unless you want the second thing it does: read the
broker out of your MQTT integration — address, username, password and
certificate paths — and add it for you, with no retyping.

That access cannot be an option on the plain app. A mapping is granted when an
app is installed, and an option can only decide whether an app uses access it
already has; a switch marked "off" next to a directory it can read anyway would
be theatre. So it is a separate install, and declining it means not installing
it.

What you are granting is wider than what it is used for. `/homeassistant` is
the whole configuration directory: `secrets.yaml`, and the stored credentials
of every integration you have. mqttview reads one entry out of
`.storage/core.config_entries` and writes nothing — `homeassistant_config:ro`
is read-only and the tests assert both — but the grant is the directory, not
the entry.

The two are separate apps with separate data. Switching from one to the other
means adding your brokers again.

From a terminal on the host, which is faster than finding the menu:

```bash
ha store add https://github.com/dgprivate/mqttview
ha store reload
ha store apps | grep -i mqttview      # prints the slug, with a repository prefix
ha apps install <slug>
ha apps start <slug>
ha apps logs <slug>
```

### Where it lives

`repository.yaml` in the repository root and `addon/` beside it. That shape is
required: the Supervisor reads the manifest from the root and each app from a
directory next to it, and it does not look any deeper. With the two nested one
level down, adding the repository fails with `is not a valid app repository`
and no further detail.

The app builds itself on first install, which takes a minute: it pulls
`hausbit/mqttview` and adds the entrypoint that turns Home Assistant's options
into mqttview's configuration. There is no separate app image to keep in step
with the binary, which is one fewer thing to get wrong.

### How the no-login part works, and what it rests on

Under ingress, Home Assistant is the only thing that can reach the app. It
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

- the app publishes **no port**;
- `trusted_proxies` is not an option, because there is no other address an app
  has any reason to trust;
- an empty `trusted_proxies` is a configuration error, not "trust everyone".

`X-Ingress-Path` is read in ingress mode only, and constrained to something
that can only be a path — no other origin, no quotes, no control characters.
Outside ingress mode it is ignored entirely, because outside ingress mode
nothing has checked who set it.

### HTTP and HTTPS both

A Home Assistant is commonly reached two ways: a `.local` name inside the house
over plain HTTP, and a proxied name outside over HTTPS. The panel works at
both, from one install, without configuration.

That is worth stating because it took two attempts. The CSRF cookie is not
marked `Secure`: a Secure cookie set over HTTPS cannot be replaced from an HTTP
page, so a scheme-dependent flag makes the panel refuse writes at whichever
address you visited second. The cookie is not a credential — reaching the
endpoint at all needs Home Assistant's own session — and `internal/auth/ingress.go`
carries the full reasoning.

### The broker you already have

If your broker is the **Mosquitto app**, the app asks the Supervisor for it and
creates the connection on first start — nobody copies a host, a port and a
password into a second form.

If it is not, add it in mqttview — Connections → Add, which is a form built for
exactly this, with TLS, client certificates and a connection test. The app's
configuration deliberately offers no way to type a broker into it: that was a
second, smaller version of the same form, validated by nothing, and it is what
made the app fail to start when the Supervisor answered an import request with
an error.

The Supervisor's service registry is written by apps that *provide* a broker,
and the MQTT integration reads from it rather than writing to it. A broker
entered into the integration, or running elsewhere on the network, is therefore
invisible to an app by any route through the Supervisor.

Which is where the second app comes in. `mqttview_hassconfig` may read the
configuration directory, so when the Supervisor has nothing to share it reads
the MQTT integration's own entry instead: broker, port, username, password,
CA, client certificate and key. A broker with mutual TLS is set up without
anybody moving three files and retyping two paths. The plain app runs the same
code, finds the directory is not there, says so in its log and carries on —
the capability is inert without the permission rather than switched off by an
option.

It only ever adds. A connection you have since edited is left exactly as it is,
because a configuration that reasserts itself on every restart is a setting
that will not stay set — which is also why it needs no switch: an import that
never overwrites anything is not something to turn off.

The same mechanism is available outside Home Assistant: `connections:` in
`mqttview.yaml` declares brokers to create if they are not there yet.

### Roles

Home Assistant does not tell an app whether you are one of its
administrators. mqttview cannot copy a fact it is not told, so instead:

- **the first person to open the panel becomes an administrator**, once, on an
  installation that has none — otherwise the first run is a dead end, because
  the default role cannot add a broker and the panel tells whoever just
  installed it to ask an administrator, who is them;
- everybody after that gets `default_role` (`operator` by default);
- `admin_users` lists Home Assistant usernames or IDs that get `admin`;
- the last administrator is never demoted by configuration, for the same reason
  the API refuses to delete them;
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

There is nothing stopping you running the app for the panel and a
standalone instance for access from outside. They are separate installs with
separate databases; a broker connection added in one does not appear in the
other.
