# syntax=docker/dockerfile:1

# Build stage
FROM golang:1.26-alpine AS builder

RUN apk add --no-cache curl libstdc++ libgcc

WORKDIR /src

# Download Tailwind CSS standalone CLI
ARG TAILWIND_VERSION=v4.2.0
RUN curl -sLO https://github.com/tailwindlabs/tailwindcss/releases/download/${TAILWIND_VERSION}/tailwindcss-linux-x64-musl \
    && chmod +x tailwindcss-linux-x64-musl \
    && mv tailwindcss-linux-x64-musl /usr/local/bin/tailwindcss

# Cache Go modules
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build Tailwind CSS
RUN tailwindcss -i web/static/css/input.css -o web/static/css/styles.css --minify

# Build Go binary
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bin/stillwater ./cmd/stillwater

# Runtime stage
FROM alpine:3.23

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

EXPOSE 1973

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD sh -c 'curl -f "http://localhost:1973${SW_BASE_PATH:-}/api/v1/health" || exit 1'

VOLUME ["/config", "/music"]

ENTRYPOINT ["/entrypoint.sh"]
CMD ["stillwater"]
