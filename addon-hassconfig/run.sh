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
    #
    # Not `.[$key] // empty`: jq's alternative operator fires on false as well
    # as on null, so a boolean option set to false read back as unset. That is
    # how `import_mqtt: false` went on importing — the one value somebody sets
    # explicitly to mean "don't" was the one value that could not be read.
    jq -r --arg key "$1" \
        'if has($key) and .[$key] != null then .[$key] else empty end' "${OPTIONS}"
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
    # An explicitly configured broker wins: somebody who typed it in means it,
    # and the Supervisor cannot tell an app about a broker that no app
    # provides — one entered into the MQTT integration, or running elsewhere on
    # the network, is invisible from here no matter what is asked.
    local url user pass
    url="$(option mqtt_url)"
    if [ -n "${url}" ]; then
        user="$(option mqtt_username)"
        pass="$(option mqtt_password)"
        say "adding the broker from this app's configuration: ${url}"
        # Named after the host rather than "Home Assistant": this one is not
        # Home Assistant's, and a list of brokers wants to say which is which.
        emit_connection "${url}" "${user}" "${pass}" "$(broker_name "${url}")"
        emit_tls
        return 0
    fi

    # bashio first: it is the Supervisor's own client, it knows where the token
    # lives and what the service API looks like, and it will keep knowing after
    # the next rename. Home Assistant renamed HASSIO_TOKEN to SUPERVISOR_TOKEN
    # once already, and add-ons to apps since.
    if [ -r /usr/lib/bashio/bashio.sh ]; then
        # shellcheck source=/dev/null
        . /usr/lib/bashio/bashio.sh

        if ! bashio::services.available mqtt; then
            say "the Supervisor has no MQTT service to share: it only knows about"
            say "brokers an app provides, so one entered into the MQTT"
            say "integration is invisible to it."
            mqtt_from_storage
            return 0
        fi

        local host port user pass ssl scheme
        host="$(bashio::services mqtt 'host')"
        port="$(bashio::services mqtt 'port')"
        user="$(bashio::services mqtt 'username')"
        pass="$(bashio::services mqtt 'password')"
        ssl="$(bashio::services mqtt 'ssl')"

        if [ -z "${host}" ] || [ "${host}" = "null" ]; then
            say "the MQTT service named no host; skipping the import"
            return 0
        fi

        scheme="mqtt"
        [ "${ssl}" = "true" ] && scheme="mqtts"

        say "importing the broker Home Assistant uses: ${scheme}://${host}:${port}"
        emit_connection "${scheme}://${host}:${port}" "${user}" "${pass}"
        return 0
    fi

    # No bashio is not a reason to skip the last resort; it is a reason to have
    # one.
    say "bashio is missing, so the Supervisor cannot be asked about MQTT"
    mqtt_from_storage
}

# mqtt_from_storage reads the MQTT integration's own settings out of Home
# Assistant's config entry storage.
#
# This is not an API. `.storage` is internal, carries no compatibility promise,
# and the directory holding it also holds secrets.yaml and every other
# integration's credentials. Only the variant of this app that asks for that
# access at install time can reach it, and the check below is what makes one
# script correct in both: the capability is inert without the permission rather
# than switched on by an option, because an option cannot un-grant a mapping.
mqtt_from_storage() {
    local entries=/homeassistant/.storage/core.config_entries
    [ -r "${entries}" ] || entries=/config/.storage/core.config_entries
    if [ ! -r "${entries}" ]; then
        say "this app has no access to Home Assistant's configuration, so there"
        say "is nothing further to try. Set mqtt_url here, or add the broker in"
        say "mqttview once — either way it is stored and stays."
        return 0
    fi

    local entry
    entry="$(jq -c '.data.entries[]? | select(.domain == "mqtt") | .data' \
        "${entries}" 2>/dev/null | head -1)"
    if [ -z "${entry}" ] || [ "${entry}" = "null" ]; then
        say "Home Assistant has no MQTT integration configured"
        return 0
    fi

    local host port user pass ca cert key insecure scheme
    host="$(echo "${entry}" | jq -r '.broker // empty')"
    port="$(echo "${entry}" | jq -r '.port // 1883')"
    user="$(echo "${entry}" | jq -r '.username // empty')"
    pass="$(echo "${entry}" | jq -r '.password // empty')"
    ca="$(echo "${entry}" | jq -r '.certificate // empty')"
    cert="$(echo "${entry}" | jq -r '.client_cert // empty')"
    key="$(echo "${entry}" | jq -r '.client_key // empty')"
    insecure="$(echo "${entry}" | jq -r '.tls_insecure // false')"

    if [ -z "${host}" ]; then
        say "the MQTT integration names no broker; skipping"
        return 0
    fi

    # The integration records TLS by what it was given rather than by a flag,
    # so the scheme is inferred from a certificate being present or the port
    # being the conventional TLS one.
    scheme="mqtt"
    if [ -n "${ca}" ] || [ -n "${cert}" ] || [ "${port}" = "8883" ]; then
        scheme="mqtts"
    fi

    say "reading the broker from the MQTT integration: ${scheme}://${host}:${port}"
    emit_connection "${scheme}://${host}:${port}" "${user}" "${pass}" "$(broker_name "${host}")"

    # "auto" means Home Assistant's own bundle rather than a file on disk.
    [ -n "${ca}" ] && [ "${ca}" != "auto" ] && echo "    ca_file: ${ca}"
    [ -n "${cert}" ] && echo "    client_cert_file: ${cert}"
    [ -n "${key}" ] && echo "    client_key_file: ${key}"
    [ "${insecure}" = "true" ] && echo "    insecure_skip_verify: true"
    return 0
}

# emit_tls adds the TLS block, for a broker that wants a client certificate.
#
# The files are named rather than copied: mqttview reads them at startup and
# stores the contents encrypted, so the paths only have to be right once.
emit_tls() {
    local ca cert key server insecure
    ca="$(option mqtt_ca_file)"
    cert="$(option mqtt_client_cert_file)"
    key="$(option mqtt_client_key_file)"
    server="$(option mqtt_server_name)"
    insecure="$(option mqtt_insecure)"

    [ -n "${ca}${cert}${key}${server}" ] || [ "${insecure}" = "true" ] || return 0

    for f in "${ca}" "${cert}" "${key}"; do
        [ -z "${f}" ] && continue
        [ -r "${f}" ] || say "warning: ${f} cannot be read; mqttview will say so and stop"
    done

    [ -n "${ca}" ] && echo "    ca_file: ${ca}"
    [ -n "${cert}" ] && echo "    client_cert_file: ${cert}"
    [ -n "${key}" ] && echo "    client_key_file: ${key}"
    [ -n "${server}" ] && echo "    server_name: ${server}"
    [ "${insecure}" = "true" ] && {
        say "warning: mqtt_insecure accepts any certificate for ${url:-the broker}"
        echo "    insecure_skip_verify: true"
    }
    return 0
}

# broker_name is the host out of a URL, which is what a person calls a broker.
broker_name() {
    echo "$1" | sed -E 's|^[a-z]+://||; s|/.*$||; s|:[0-9]+$||'
}

# emit_connection writes the connections block mqttview seeds from. The name is
# what seeding matches on, so it has to be stable: change it and a restart adds
# a second connection beside the first.
emit_connection() {
    local url="$1" user="$2" pass="$3" name="${4:-Home Assistant}"
    echo ""
    echo "connections:"
    echo "  - name: ${name}"
    echo "    url: ${url}"
    [ -n "${user}" ] && [ "${user}" != "null" ] && echo "    username: ${user}"
    [ -n "${pass}" ] && [ "${pass}" != "null" ] && echo "    password: ${pass}"
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

say "starting in Home Assistant mode (log level ${log_level})"
# The version the binary reports, next to the add-on's own, because they are
# different things and a mismatch is otherwise invisible.
say "binary: $(/usr/local/bin/mqttview -version 2>/dev/null || echo unknown)"

exec /usr/local/bin/mqttview \
    -config "${CONFIG}" \
    -log-level "${log_level}"
