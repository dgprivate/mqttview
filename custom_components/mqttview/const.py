"""Constants for the mqttview integration."""

from typing import Final

DOMAIN: Final = "mqttview"

# The URL of a standalone mqttview, as the browser reaches it. Not as Home
# Assistant reaches it: the panel is an iframe, so it is the browser that
# fetches this, and "localhost" would mean the phone.
CONF_URL: Final = "url"

# What the sidebar entry says and shows.
CONF_TITLE: Final = "title"
CONF_ICON: Final = "icon"

DEFAULT_TITLE: Final = "mqttview"
DEFAULT_ICON: Final = "mdi:transit-connection-variant"

# The path the panel is registered at, below Home Assistant's own root.
PANEL_URL_PATH: Final = "mqttview"
