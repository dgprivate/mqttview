# The mqttview Home Assistant app

Home Assistant renamed **add-ons** to **apps**; both names mean this.

Settings → **Apps** → ⋮ → Repositories → add
`https://github.com/dgprivate/mqttview`, then install **mqttview**.

From a terminal on the host, which is quicker than finding the menu:

```bash
ha store add https://github.com/dgprivate/mqttview
ha store reload
ha store apps | grep -i mqttview      # the slug, with a repository prefix
ha apps install <slug>
```

The manifest is `config.yaml` in this directory, and `repository.yaml` sits in
the repository root — the Supervisor requires exactly that shape and does not
look inside subdirectories.

`DOCS.md` is what Home Assistant shows on the app's Documentation tab.
[docs/HOME_ASSISTANT.md](../docs/HOME_ASSISTANT.md) explains how the no-login
mode works and what it rests on.

**Apps are not installed through HACS.** HACS installs integrations, dashboard
cards and themes. The `custom_components/mqttview` integration in this
repository *is* installable through HACS, and it is the option for Home
Assistant Container and Core, which have no apps at all.
