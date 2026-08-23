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
} > "${CONFIG}"

echo "mqttview: starting in Home Assistant mode (log level ${log_level})"

exec /usr/local/bin/mqttview \
    -config "${CONFIG}" \
    -log-level "${log_level}"
