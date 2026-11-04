# syntax=docker/dockerfile:1

# Build from the parent directory so the sibling msg module is inside the build
# context (go.mod replaces github.com/charlesrobsampson/msg with ../msg):
#
#   docker compose build        # from the aether checkout
#   docker build -f aether/Dockerfile ..   # equivalent manual build
FROM golang:1.27-alpine AS builder

RUN apk add --no-cache ca-certificates git

WORKDIR /src
COPY aether/go.mod aether/go.sum /src/aether/
COPY msg/go.mod msg/go.sum /src/msg/
WORKDIR /src/aether
RUN go mod download
WORKDIR /src/msg
RUN go mod download

COPY aether/ /src/aether/
COPY msg/ /src/msg/

RUN mkdir -p /out
WORKDIR /src/aether
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/aether ./cmd/aether
WORKDIR /src/msg
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/msg ./cmd/msg

# Alpine (not scratch) keeps CA certificates and zoneinfo available for
# outbound HTTPS and user-local timezone conversion.
FROM alpine:3.22 AS runtime

RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S -g 65532 aether \
    && adduser -S -D -H -u 65532 -G aether aether \
    && mkdir -p /config /data /home/aether \
    && chown -R aether:aether /config /data /home/aether

COPY --from=builder /out/aether /usr/local/bin/aether
COPY --from=builder /out/msg /usr/local/bin/msg

USER aether
WORKDIR /home/aether
EXPOSE 8080

# The image carries both binaries so docker-compose can run the msg receiver as
# a sidecar without a second build. AETHER_CONFIG_DIR is mounted at /config.
CMD ["aether", "serve", "--config-dir", "/config"]
