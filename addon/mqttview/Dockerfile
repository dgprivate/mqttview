# The add-on image.
#
# It builds mqttview from the repository's own hardened Dockerfile rather than
# repeating the build here, so the add-on and the standalone image are the same
# binary with the same provenance. Only the entrypoint differs: the add-on has
# to read /data/options.json, which is how Home Assistant passes configuration.
ARG BUILD_FROM
ARG MQTTVIEW_IMAGE=ghcr.io/mqttview/mqttview:latest

FROM ${MQTTVIEW_IMAGE} AS mqttview

FROM ${BUILD_FROM}

# jq reads options.json. bashio is already in the Home Assistant base image and
# is what every add-on uses to talk to the Supervisor.
# hadolint ignore=DL3018
RUN apk add --no-cache jq

COPY --from=mqttview /usr/local/bin/mqttview /usr/local/bin/mqttview

COPY run.sh /run.sh
RUN chmod +x /run.sh

# Root here, unlike the standalone image. The Home Assistant base image's init
# is what starts the service, /data is owned by root inside an add-on
# container, and an add-on has no way to change either. The container is
# already confined by the Supervisor: no host network, no devices, and only
# the ingress port reachable.
CMD ["/run.sh"]
