# mqttview add-on repository

Add this repository to Home Assistant to install mqttview as an add-on with a
sidebar panel and no separate login:

Settings → Add-ons → Add-on Store → ⋮ → Repositories →
`https://github.com/mqttview/mqttview`

The add-on manifest lives in `mqttview/`. See `mqttview/DOCS.md` for what the
options do, and `docs/HOME_ASSISTANT.md` in the project root for how the mode
works and what it assumes.

**Add-ons are not installed through HACS.** HACS installs integrations,
dashboard cards and themes; add-ons come from an add-on repository like this
one. The `custom_components/mqttview` integration in this repository *is*
installable through HACS, and is the option for Home Assistant Container
installs, which have no add-ons at all — see the same document for which one
fits.
