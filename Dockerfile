# syntax=docker/dockerfile:1

# ---- build ----
FROM golang:1.26-alpine AS build
WORKDIR /src

# Dependencies first, so editing source does not re-download them.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

# The release version, stamped into the binary: docker build --build-arg VERSION=0.1.0
ARG VERSION=dev
COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/tailwise ./cmd/tailwise

# ---- runtime ----
FROM alpine:3.22
RUN apk add --no-cache ca-certificates \
    && adduser -D -H -u 10001 tailwise

COPY --from=build /out/tailwise /usr/local/bin/tailwise

# No config is baked in: the engine runs on its built-in defaults (Jev as
# the scorer, embedded NATS) and the environment. Pass JEV_API_KEY, and
# TAILWISE_EXPORT_ENDPOINT (or a mounted config with -config) for where
# kept traces go.

USER tailwise
# 8080: JSON API. 4317: OTLP gRPC. 4318: OTLP HTTP.
EXPOSE 8080 4317 4318
HEALTHCHECK --interval=10s --timeout=3s --start-period=30s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["tailwise"]
