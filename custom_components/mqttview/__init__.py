"""Put a standalone mqttview in the Home Assistant sidebar.

This is the option for Home Assistant Container and Core, which have no
Supervisor and therefore no add-ons. It registers an iframe panel pointing at
an mqttview you run yourself.

What it does not do is remove the login. That belongs to ingress, and ingress
belongs to the Supervisor: the add-on in this same repository is the version
where Home Assistant authenticates you and mqttview believes it. Here, Home
Assistant is only a link — mqttview still asks who you are, because nothing has
told it.

For the iframe to render at all, the mqttview instance has to allow Home
Assistant to frame it. mqttview refuses framing by default, which is the right
default for a page with buttons that publish to a broker. Set `frame_ancestors`
in mqttview's configuration to your Home Assistant origin.
"""

from __future__ import annotations

import logging

from homeassistant.components import frontend, panel_custom
from homeassistant.config_entries import ConfigEntry
from homeassistant.core import HomeAssistant

from .const import (
    CONF_ICON,
    CONF_TITLE,
    CONF_URL,
    DEFAULT_ICON,
    DEFAULT_TITLE,
    DOMAIN,
    PANEL_URL_PATH,
)

_LOGGER = logging.getLogger(__name__)


async def async_setup_entry(hass: HomeAssistant, entry: ConfigEntry) -> bool:
    """Register the sidebar panel for a configured mqttview."""
    url: str = entry.data[CONF_URL]
    title: str = entry.options.get(
        CONF_TITLE, entry.data.get(CONF_TITLE, DEFAULT_TITLE)
    )
    icon: str = entry.options.get(CONF_ICON, entry.data.get(CONF_ICON, DEFAULT_ICON))

    await panel_custom.async_register_panel(
        hass,
        frontend_url_path=PANEL_URL_PATH,
        webcomponent_name="ha-panel-iframe",
        sidebar_title=title,
        sidebar_icon=icon,
        # An iframe panel: Home Assistant frames the URL rather than serving
        # anything itself.
        config={"url": url},
        require_admin=False,
        # The panel is registered from a config entry, so it has to be
        # removable when the entry goes.
        update=True,
    )

    entry.async_on_unload(entry.add_update_listener(_async_reload))
    return True


async def async_unload_entry(hass: HomeAssistant, entry: ConfigEntry) -> bool:
    """Remove the sidebar panel."""
    frontend.async_remove_panel(hass, PANEL_URL_PATH)
    return True


async def _async_reload(hass: HomeAssistant, entry: ConfigEntry) -> None:
    """Re-register the panel after the title or icon changed."""
    await hass.config_entries.async_reload(entry.entry_id)
