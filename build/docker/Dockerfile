# syntax=docker/dockerfile:1

# Build stage
FROM golang:1.26-alpine@sha256:91eda9776261207ea25fd06b5b7fed8d397dd2c0a283e77f2ab6e91bfa71079d AS builder

RUN apk add --no-cache curl libstdc++ libgcc

WORKDIR /src

# Download Tailwind CSS standalone CLI (architecture-aware)
ARG TAILWIND_VERSION=v4.2.0
RUN case "$(uname -m)" in \
      x86_64)  TAILWIND_ARCH=x64 ;; \
      aarch64) TAILWIND_ARCH=arm64 ;; \
      *)       echo "unsupported arch: $(uname -m)" && exit 1 ;; \
    esac \
    && curl -fsSLO "https://github.com/tailwindlabs/tailwindcss/releases/download/${TAILWIND_VERSION}/tailwindcss-linux-${TAILWIND_ARCH}-musl" \
    && chmod +x "tailwindcss-linux-${TAILWIND_ARCH}-musl" \
    && mv "tailwindcss-linux-${TAILWIND_ARCH}-musl" /usr/local/bin/tailwindcss

# Cache Go modules
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build Tailwind CSS and Go binary in a single layer (hadolint DL3059).
RUN tailwindcss -i web/static/css/input.css -o web/static/css/styles.css --minify \
    && CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bin/stillwater ./cmd/stillwater

# Runtime stage
FROM alpine:3.23@sha256:5b10f432ef3da1b8d4c7eb6c487f2f5a8f096bc91145e68878dd4a5019afde11

LABEL org.opencontainers.image.source="https://github.com/sydlexius/stillwater" \
      org.opencontainers.image.description="Artist metadata and image manager for Emby, Jellyfin, and Kodi" \
      org.opencontainers.image.licenses="GPL-3.0"

RUN apk add --no-cache ca-certificates tzdata su-exec curl && \
    apk upgrade --no-cache

# Create app user (guarded: GID/UID may already be claimed after apk upgrade)
RUN addgroup stillwater 2>/dev/null || true && \
    adduser -u 99 -G stillwater -s /bin/sh -D stillwater 2>/dev/null || true && \
    id stillwater

# Create config and music directories
RUN mkdir -p /config /music && chown stillwater:stillwater /config /music

COPY --from=builder /bin/stillwater /usr/local/bin/stillwater
COPY build/docker/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

WORKDIR /app

# Advertise both the plain HTTP port and the optional TLS port. EXPOSE is
# metadata only -- the actual binds come from SW_PORT / SW_TLS_PORT at run
# time -- but listing both makes split-port mode discoverable to docker run -P
# and to orchestrators that scrape image metadata.
ARG SW_TLS_PORT=1974
EXPOSE 1973
EXPOSE ${SW_TLS_PORT}

# Healthcheck. The shell-expanded URL lets the entrypoint override
# SW_HEALTH_URL when SW_TLS_CERT_FILE is set so the probe targets HTTPS on
# the right port. -k is intentional: a localhost healthcheck against a
# self-signed cert is acceptable.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD sh -c 'curl -fsk "${SW_HEALTH_URL:-http://localhost:${SW_PORT:-1973}${SW_BASE_PATH:-}/api/v1/health}" || exit 1'

VOLUME ["/config", "/music"]

ENTRYPOINT ["/entrypoint.sh"]
CMD ["stillwater"]
