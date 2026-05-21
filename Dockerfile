# Multi-stage Dockerfile for zabbix-nsx-t-exporter.
# Builds a static-ish Go binary then runs it on a minimal alpine image.

FROM golang:1.26-alpine AS builder

RUN apk add --no-cache --no-progress ca-certificates git

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG BUILD_VERSION=dev
ARG BUILD_COMMIT=unknown
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags "-s -w -X main.version=${BUILD_VERSION} -X main.commit=${BUILD_COMMIT}" \
      -o /out/nsx-t-exporter .

################################################################################

FROM alpine:3.20

RUN apk upgrade --no-cache --no-progress \
  && apk add --no-cache --no-progress ca-certificates tzdata \
  && addgroup -g 4200 app \
  && adduser -h /home/app -s /sbin/nologin -G app -D -u 4200 app

COPY --from=builder /out/nsx-t-exporter /usr/local/bin/nsx-t-exporter

ARG BUILD_VERSION=dev
ARG BUILD_COMMIT=unknown
LABEL org.opencontainers.image.title="zabbix-nsx-t-exporter" \
      org.opencontainers.image.description="VMware NSX-T 4.2 Prometheus exporter for Zabbix 7.0 monitoring" \
      org.opencontainers.image.source="https://github.com/adaptera/zabbix-nsx-t-exporter" \
      org.opencontainers.image.url="https://github.com/adaptera/zabbix-nsx-t-exporter" \
      org.opencontainers.image.version="${BUILD_VERSION}" \
      org.opencontainers.image.revision="${BUILD_COMMIT}" \
      org.opencontainers.image.licenses="Apache-2.0"

USER 4200:4200
WORKDIR /home/app
EXPOSE 9999
ENTRYPOINT ["/usr/local/bin/nsx-t-exporter"]
