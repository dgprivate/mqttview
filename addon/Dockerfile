# The add-on image.
#
# It builds mqttview from the repository's own hardened Dockerfile rather than
# repeating the build here, so the add-on and the standalone image are the same
# binary with the same provenance. Only the entrypoint differs: the add-on has
# to read /data/options.json, which is how Home Assistant passes configuration.
ARG BUILD_FROM
ARG MQTTVIEW_IMAGE=hausbit/mqttview@sha256:2f6ec31254c627a8a4a493c0aa3b45d0d6f1f62bee3f052120ce223291dca6cb

FROM ${MQTTVIEW_IMAGE} AS mqttview

FROM ${BUILD_FROM}

# jq reads options.json and the Supervisor's answers; curl asks it. Neither is
# assumed to be in the base image — the MQTT import failed silently once
# because the request never ran, and a missing tool is indistinguishable from a
# Supervisor that has nothing to say.
# hadolint ignore=DL3018
RUN apk add --no-cache jq curl

COPY --from=mqttview /usr/local/bin/mqttview /usr/local/bin/mqttview

COPY run.sh /run.sh
RUN chmod +x /run.sh

# Root here, unlike the standalone image. The Home Assistant base image's init
# is what starts the service, /data is owned by root inside an add-on
# container, and an add-on has no way to change either. The container is
# already confined by the Supervisor: no host network, no devices, and only
# the ingress port reachable.
CMD ["/run.sh"]
