# Build the frontend first so a change to Go code does not invalidate the npm
# layer, and vice versa.
FROM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
ARG VERSION=docker
# CGO stays off: the SQLite driver is pure Go, so the result runs on scratch.
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /mqttview ./cmd/mqttview

FROM alpine:3.21
# ca-certificates is needed to verify TLS brokers and OIDC issuers; tzdata so
# timestamps in the UI match the operator's expectations.
RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -u 10001 mqttview && \
    mkdir -p /data && chown mqttview:mqttview /data

COPY --from=build /mqttview /usr/local/bin/mqttview

USER mqttview
WORKDIR /data
VOLUME /data
EXPOSE 8114

ENV MQTTVIEW_ADDR=0.0.0.0:8114 \
    MQTTVIEW_DATA_DIR=/data

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s \
    CMD wget -qO- http://127.0.0.1:8114/api/health || exit 1

ENTRYPOINT ["mqttview"]
