#!/usr/bin/env bash
# Start mqttview with the options Home Assistant was given.
#
# The add-on's whole job is translating /data/options.json into mqttview's own
# configuration, then getting out of the way. Nothing is invented here: every
# value either comes from the options or is a consequence of running behind
# ingress.
set -euo pipefail

readonly OPTIONS=/data/options.json
readonly CONFIG=/data/mqttview.yaml

option() {
    # A missing key reads as empty rather than "null", which is what would end
    # up in the config file otherwise.
    jq -r --arg key "$1" '.[$key] // empty' "${OPTIONS}"
}

option_list() {
    # A YAML list at the given indentation. An empty list produces nothing, so
    # the caller omits the key entirely rather than writing a null.
    jq -r --arg key "$1" --arg pad "$2" \
        '.[$key] // [] | .[] | $pad + "- " + (. | tostring)' "${OPTIONS}"
}

# Home Assistant already knows about a broker if the MQTT integration is set
# up, and asking the Supervisor for it beats making somebody copy the host,
# port and password into a second form. `services: mqtt:want` in config.yaml is
# what grants this; "want" rather than "need" so the app still starts without
# one.
# say prints to stderr, which is where the add-on log comes from. The config
# file is stdout here, so anything informational has to go the other way.
say() { echo "mqttview: $*" >&2; }

mqtt_connection() {
    if [ -z "${SUPERVISOR_TOKEN:-}" ]; then
        say "no Supervisor token, so not asking about MQTT"
        return 0
    fi

    local service status
    # -w writes the status after the body so both are available; -f is not used
    # because a 400 body says more than a non-zero exit does.
    service="$(curl -sS -w '\n%{http_code}' \
        -H "Authorization: Bearer ${SUPERVISOR_TOKEN}" \
        http://supervisor/services/mqtt 2>/dev/null)" || {
        say "could not reach the Supervisor to ask about MQTT"
        return 0
    }
    status="$(echo "${service}" | tail -1)"
    service="$(echo "${service}" | sed '$d')"

    if [ "${status}" != "200" ]; then
        say "the Supervisor has no MQTT service to share (HTTP ${status})."
        say "that is normal when the broker is configured directly in the MQTT"
        say "integration rather than provided by an add-on — add it in mqttview."
        return 0
    fi

    local host port user pass ssl scheme
    host="$(echo "${service}" | jq -r '.data.host // empty')"
    if [ -z "${host}" ]; then
        say "the Supervisor answered about MQTT but named no host; skipping"
        return 0
    fi
    port="$(echo "${service}" | jq -r '.data.port // 1883')"
    user="$(echo "${service}" | jq -r '.data.username // empty')"
    pass="$(echo "${service}" | jq -r '.data.password // empty')"
    ssl="$(echo "${service}" | jq -r '.data.ssl // false')"

    scheme="mqtt"
    [ "${ssl}" = "true" ] && scheme="mqtts"

    say "importing the broker Home Assistant uses: ${scheme}://${host}:${port}"

    echo ""
    echo "connections:"
    echo "  - name: Home Assistant"
    echo "    url: ${scheme}://${host}:${port}"
    [ -n "${user}" ] && echo "    username: ${user}"
    [ -n "${pass}" ] && echo "    password: ${pass}"
    echo "    subscribe:"
    echo "      - \"#\""
}

log_level="$(option log_level)"
default_role="$(option default_role)"
fallback_user="$(option fallback_user)"

: "${log_level:=info}"
: "${default_role:=operator}"

{
    echo "# Written by the add-on on every start. Edits here are overwritten;"
    echo "# change the add-on's configuration in Home Assistant instead."
    echo ""
    # 0.0.0.0 inside the add-on's own container, which is not the host: the
    # Supervisor reaches it on the internal network, and nothing else can.
    echo "addr: 0.0.0.0:8114"
    echo "data_dir: /data"
    echo ""
    echo "auth:"
    echo "  mode: ingress"
    echo "  ingress:"
    # 172.30.32.2 is the Supervisor. This is the check that makes the identity
    # headers mean anything, so it is not an option: an add-on has no reason to
    # trust any other address, and making it configurable would only make it
    # possible to get wrong.
    echo "    trusted_proxies:"
    echo "      - 172.30.32.2"
    echo "    default_role: ${default_role}"

    admins="$(option_list admin_users "      ")"
    if [ -n "${admins}" ]; then
        echo "    admin_users:"
        echo "${admins}"
    fi

    if [ -n "${fallback_user}" ]; then
        echo "    fallback_user: ${fallback_user}"
    fi

    ancestors="$(option_list frame_ancestors "  ")"
    if [ -n "${ancestors}" ]; then
        echo ""
        echo "frame_ancestors:"
        echo "${ancestors}"
    fi

    if [ "$(option import_mqtt)" = "false" ]; then
        say "import_mqtt is off, so not asking the Supervisor about MQTT"
    else
        mqtt_connection
    fi
} > "${CONFIG}"

echo "mqttview: starting in Home Assistant mode (log level ${log_level})"

exec /usr/local/bin/mqttview \
    -config "${CONFIG}" \
    -log-level "${log_level}"
