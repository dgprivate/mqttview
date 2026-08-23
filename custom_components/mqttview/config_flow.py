"""Config flow for the mqttview panel."""

from __future__ import annotations

from typing import Any
from urllib.parse import urlparse

import voluptuous as vol

from homeassistant.config_entries import (
    ConfigEntry,
    ConfigFlow,
    ConfigFlowResult,
    OptionsFlow,
)
from homeassistant.core import callback

from .const import (
    CONF_ICON,
    CONF_TITLE,
    CONF_URL,
    DEFAULT_ICON,
    DEFAULT_TITLE,
    DOMAIN,
)


def _clean_url(raw: str) -> str | None:
    """Return the URL if it is one a browser could load in an iframe.

    Checked here rather than left to fail silently later: a bad URL produces an
    empty grey rectangle with the reason only in the browser console, which is
    the least discoverable failure this integration can have.
    """
    parsed = urlparse(raw.strip())
    if parsed.scheme not in ("http", "https") or not parsed.netloc:
        return None
    return raw.strip().rstrip("/")


class MqttviewConfigFlow(ConfigFlow, domain=DOMAIN):
    """Ask where mqttview is."""

    VERSION = 1

    async def async_step_user(
        self, user_input: dict[str, Any] | None = None
    ) -> ConfigFlowResult:
        """Handle the form."""
        errors: dict[str, str] = {}

        if user_input is not None:
            url = _clean_url(user_input[CONF_URL])
            if url is None:
                errors[CONF_URL] = "invalid_url"
            else:
                # One panel, one sidebar entry: a second entry would fight the
                # first for the same URL path.
                await self.async_set_unique_id(DOMAIN)
                self._abort_if_unique_id_configured()

                return self.async_create_entry(
                    title=user_input.get(CONF_TITLE, DEFAULT_TITLE),
                    data={
                        CONF_URL: url,
                        CONF_TITLE: user_input.get(CONF_TITLE, DEFAULT_TITLE),
                        CONF_ICON: user_input.get(CONF_ICON, DEFAULT_ICON),
                    },
                )

        return self.async_show_form(
            step_id="user",
            data_schema=vol.Schema(
                {
                    vol.Required(
                        CONF_URL, default=(user_input or {}).get(CONF_URL, "")
                    ): str,
                    vol.Optional(CONF_TITLE, default=DEFAULT_TITLE): str,
                    vol.Optional(CONF_ICON, default=DEFAULT_ICON): str,
                }
            ),
            errors=errors,
        )

    @staticmethod
    @callback
    def async_get_options_flow(entry: ConfigEntry) -> OptionsFlow:
        """Return the options flow."""
        return MqttviewOptionsFlow()


class MqttviewOptionsFlow(OptionsFlow):
    """Change the sidebar title and icon without removing the entry."""

    async def async_step_init(
        self, user_input: dict[str, Any] | None = None
    ) -> ConfigFlowResult:
        """Handle the options form."""
        if user_input is not None:
            return self.async_create_entry(data=user_input)

        current = {**self.config_entry.data, **self.config_entry.options}
        return self.async_show_form(
            step_id="init",
            data_schema=vol.Schema(
                {
                    vol.Optional(
                        CONF_TITLE, default=current.get(CONF_TITLE, DEFAULT_TITLE)
                    ): str,
                    vol.Optional(
                        CONF_ICON, default=current.get(CONF_ICON, DEFAULT_ICON)
                    ): str,
                }
            ),
        )
