# syntax=docker/dockerfile:1

# Hardened, least-privilege image for mqttview.
#
#  * multi-stage: the Go toolchain, npm and the package manager stay in builders
#  * every base image pinned by digest, so a build is reproducible
#  * runs as an unprivileged user, on a filesystem it cannot write to
#  * carries an SBOM of itself, so what shipped can be audited later
#
# Refresh a digest with:
#   docker pull alpine:3.21 && docker image inspect alpine:3.21 \
#     --format '{{index .RepoDigests 0}}'
# Renovate keeps these moving; see .github/renovate.json.
ARG NODE_IMAGE=node@sha256:c610fcdfb1d5b4740dd70c284ed3cb16bb857e0f7166196e36a5501df7a3aa32
ARG GO_IMAGE=golang@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc
ARG RUNTIME_IMAGE=alpine@sha256:48b0309ca019d89d40f670aa1bc06e426dc0931948452e8491e3d65087abc07d
# syft, used to record what ends up in the image (see the "sbom" stage)
ARG SYFT_IMAGE=anchore/syft@sha256:678bfa565b60f747aac0f8e964fe5588a24445b8d0a480e91f6efd70020dfbb0

# 10001 rather than the 65532 "nonroot" convention: this image predates the
# hardening and the /data volume on existing installs is owned by 10001.
# Changing it would silently break every upgrade.
ARG APP_UID=10001
ARG APP_GID=10001

# Traceability: pass these in to record where the image came from, e.g.
#   VCS_REF=$(git rev-parse --short HEAD) docker compose build
ARG VERSION=docker
ARG VCS_REF=unknown
ARG BUILD_DATE=unknown


# --------------------------------------------------------------------------
# Frontend: built first, so a change to Go code does not invalidate npm
# --------------------------------------------------------------------------
# --platform=$BUILDPLATFORM: the frontend is the same bytes whatever the target
# architecture, so it is built once, natively, rather than once per platform
# under QEMU. Emulated npm is minutes each time and produces identical output.
FROM --platform=$BUILDPLATFORM ${NODE_IMAGE} AS web

WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build


# --------------------------------------------------------------------------
# Backend: a static binary with the frontend embedded
# --------------------------------------------------------------------------
# Also native: Go cross-compiles, and with CGO off there is nothing that needs
# the target's libc. Building emulated would be slower by an order of magnitude
# for exactly the same binary.
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build

ARG VERSION
# Supplied by buildx for each platform being built.
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist

# CGO stays off: the SQLite driver is pure Go, so the result needs no libc at
# runtime. -trimpath keeps build paths out of the binary, and the buildid is
# cleared so the same source produces the same bytes.
# GOARM comes from the variant: linux/arm/v7 is GOARM=7 and v6 is GOARM=6,
# which is what an original Pi and a Pi Zero need. Go will not infer it from
# GOARCH=arm alone, and a v7 binary on a v6 board fails with an illegal
# instruction rather than anything that reads like a mistake.
RUN CGO_ENABLED=0 GOFLAGS=-buildvcs=false \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} GOARM=${TARGETVARIANT#v} \
    go build -trimpath \
    -ldflags "-s -w -buildid= -X main.version=${VERSION}" \
    -o /mqttview ./cmd/mqttview


# --------------------------------------------------------------------------
# Runtime base: no toolchain, no package manager, no root
# --------------------------------------------------------------------------
FROM ${RUNTIME_IMAGE} AS runtime-base

ARG APP_UID
ARG APP_GID

# ca-certificates verifies TLS brokers and OIDC issuers; tzdata makes the
# timestamps in the UI match what the operator expects. apk upgrade runs in the
# same layer as the install, so the two cannot drift apart (CIS 4.7).
RUN apk add --no-cache ca-certificates tzdata && \
    apk upgrade --no-cache && \
    addgroup -g "${APP_GID}" mqttview && \
    adduser -u "${APP_UID}" -G mqttview -h /home/mqttview -s /sbin/nologin -D mqttview && \
    mkdir -p /data && chown "${APP_UID}:${APP_GID}" /data && \
    # Nothing in here needs to escalate privilege, so drop every setuid and
    # setgid bit rather than trusting that no path ever reaches one.
    find / -xdev -perm /6000 -type f -exec chmod a-s {} + || true

# The binary stays root-owned and read-only to the app user, so the process
# cannot rewrite its own code even if the filesystem were made writable.
COPY --from=build --chown=root:root --chmod=0755 /mqttview /usr/local/bin/mqttview

# Removed last, so the steps above still had a package manager. Only the tool
# and its signing keys go: /lib/apk/db is the installed-package database, and
# deleting it would leave scanners unable to see a single OS package — they
# would report a clean image because they could not find anything to judge.
RUN rm -rf /sbin/apk /usr/bin/apk /etc/apk/keys


# --------------------------------------------------------------------------
# SBOM: an inventory of everything the runtime image contains
# --------------------------------------------------------------------------
# A file in the image rather than a BuildKit attestation, because attestations
# need the containerd image store and this works with any builder. Run on the
# build platform: syft reads the copied filesystem, it never executes anything
# from it, so emulating the target architecture would buy nothing.
FROM --platform=$BUILDPLATFORM ${SYFT_IMAGE} AS sbom

COPY --from=runtime-base / /scan

# "installed" keeps the catalogers that report what is actually present and
# leaves out the "declared" ones, which would read go.mod and list build-time
# dependencies that never reached the image.
ENV SYFT_FILE_METADATA_SELECTION=none
RUN ["/syft", "scan", "dir:/scan", "--source-name", "mqttview", \
     "--select-catalogers", "installed", \
     "-o", "cyclonedx-json@1.6=/sbom.cdx.json"]


# --------------------------------------------------------------------------
# Runtime: the base image plus its own bill of materials
# --------------------------------------------------------------------------
FROM runtime-base AS runtime

ARG APP_UID
ARG APP_GID
ARG VERSION
ARG VCS_REF
ARG BUILD_DATE

COPY --from=sbom --chown=root:root /sbom.cdx.json /usr/share/mqttview/sbom.cdx.json
# Findings that cannot be fixed and do not apply here, in the form scanners
# read: trivy --vex / grype --vex. See security/README.md.
COPY --chown=root:root security/mqttview.openvex.json /usr/share/mqttview/vex.openvex.json

LABEL org.opencontainers.image.title="mqttview" \
      org.opencontainers.image.description="Realtime web UI for MQTT: topic tree, live messages, plugins" \
      org.opencontainers.image.source="https://github.com/dgprivate/mqttview" \
      org.opencontainers.image.documentation="https://github.com/dgprivate/mqttview#readme" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${VCS_REF}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      si.mqttview.sbom.path="/usr/share/mqttview/sbom.cdx.json" \
      si.mqttview.sbom.format="CycloneDX JSON"

USER ${APP_UID}:${APP_GID}
WORKDIR /data
VOLUME /data
EXPOSE 8114

ENV MQTTVIEW_ADDR=0.0.0.0:8114 \
    MQTTVIEW_DATA_DIR=/data

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/usr/local/bin/mqttview", "-health-check"]

ENTRYPOINT ["mqttview"]
