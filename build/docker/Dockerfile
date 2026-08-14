# syntax=docker/dockerfile:1

# Build stage
FROM golang:1.26.6-alpine@sha256:af8d6740070b8906d12eae1c3e3ea0957fb63f492051ea05e354c38ef9fe88df AS builder

RUN apk add --no-cache curl libstdc++ libgcc

WORKDIR /src

# Download Tailwind CSS standalone CLI (architecture-aware) and verify checksum.
# Bump TAILWIND_VERSION + the matching SHA-256 for both arches together.
# Compute fresh hashes via:
#   curl -fsSLO https://github.com/tailwindlabs/tailwindcss/releases/download/<vX>/tailwindcss-linux-{x64,arm64}-musl
#   shasum -a 256 tailwindcss-linux-*-musl
# This Dockerfile pins the musl binaries (x64 + arm64). The CI workflows pin the
# glibc linux-x64 binary in .github/actions/setup-tailwind, which owns
# TAILWIND_VERSION + its x64 SHA. Bump TAILWIND_VERSION in both places together,
# but the musl SHAs here differ from the action's x64 SHA and stay separate.
ARG TAILWIND_VERSION=v4.2.0
ARG TAILWIND_SHA256_X64=44b85e87dd1812b5b7e46b6522c80cfa045474c736afa6c9ad2279b7d4944354
ARG TAILWIND_SHA256_ARM64=2db78c3d59d0b7f75fee28a7ea0d0dc6284761bd039bf633a9f5daac80060829
RUN case "$(uname -m)" in \
      x86_64)  TAILWIND_ARCH=x64;   TAILWIND_SHA256="$TAILWIND_SHA256_X64" ;; \
      aarch64) TAILWIND_ARCH=arm64; TAILWIND_SHA256="$TAILWIND_SHA256_ARM64" ;; \
      *)       echo "unsupported arch: $(uname -m)" && exit 1 ;; \
    esac \
    && curl -fsSLO "https://github.com/tailwindlabs/tailwindcss/releases/download/${TAILWIND_VERSION}/tailwindcss-linux-${TAILWIND_ARCH}-musl" \
    && echo "${TAILWIND_SHA256}  tailwindcss-linux-${TAILWIND_ARCH}-musl" > /tmp/tw.sha256 \
    && sha256sum -c /tmp/tw.sha256 \
    && rm /tmp/tw.sha256 \
    && chmod +x "tailwindcss-linux-${TAILWIND_ARCH}-musl" \
    && mv "tailwindcss-linux-${TAILWIND_ARCH}-musl" /usr/local/bin/tailwindcss

# Cache Go modules
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build Tailwind CSS and Go binary in a single layer (hadolint DL3059).
RUN tailwindcss -i web/static/css/input.css -o web/static/css/styles.css --minify \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /bin/stillwater ./cmd/stillwater

# --- SYNC with Dockerfile.goreleaser runtime stanza ---
# The runtime stage below must stay in sync with Dockerfile.goreleaser.
# Allowed intentional differences (do not "fix" these):
#   - LABEL block: present here (for `docker build` / CI); absent in
#     Dockerfile.goreleaser (goreleaser injects OCI labels via .goreleaser.yml).
#   - Binary COPY source: `--from=builder /bin/stillwater` here; in
#     Dockerfile.goreleaser it is `ARG TARGETPLATFORM` + `COPY $TARGETPLATFORM/stillwater`
#     (goreleaser's multi-platform buildx context, no builder stage).
# Any other runtime change (base image digest, apk packages, users, ports,
# healthcheck, volumes, entrypoint) MUST be mirrored in Dockerfile.goreleaser.
# --- END SYNC ---

# Runtime stage
FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b

LABEL org.opencontainers.image.source="https://github.com/sydlexius/stillwater" \
      org.opencontainers.image.description="Artist metadata and image manager for Emby, Jellyfin, and Kodi" \
      org.opencontainers.image.licenses="GPL-3.0"

RUN apk add --no-cache ca-certificates tzdata su-exec && \
    apk upgrade --no-cache

# Create app user (guarded: GID/UID may already be claimed after apk upgrade)
RUN addgroup stillwater 2>/dev/null || true && \
    adduser -u 99 -G stillwater -s /bin/sh -D stillwater 2>/dev/null || true && \
    id stillwater

# Create config and music directories
RUN mkdir -p /config /music && chown stillwater:stillwater /config /music

COPY --from=builder /bin/stillwater /usr/local/bin/stillwater
COPY build/docker/entrypoint.sh /entrypoint.sh
COPY build/docker/healthcheck.sh /healthcheck.sh
RUN chmod +x /entrypoint.sh /healthcheck.sh

WORKDIR /app

# Advertise both the plain HTTP port and the optional TLS port. EXPOSE is
# metadata only -- the actual binds come from SW_PORT / SW_TLS_PORT at run
# time -- but listing both makes split-port mode discoverable to docker run -P
# and to orchestrators that scrape image metadata.
ARG SW_TLS_PORT=1974
EXPOSE 1973
EXPOSE ${SW_TLS_PORT}
# HTTP/3 (QUIC) listens on UDP. Mirrors the TCP HTTPS port; advertised so
# `docker run -P` and orchestrators that scrape image metadata bind UDP too
# when SW_HTTP3_ENABLED=true. The actual UDP bind happens at run time.
EXPOSE 1973/udp
EXPOSE ${SW_TLS_PORT}/udp

# Healthcheck. Docker runs HEALTHCHECK as a fresh process sourced from the
# image's Env config, not as a child of PID 1, so it does NOT see
# entrypoint.sh's runtime `export SW_HEALTH_URL` -- healthcheck.sh
# re-derives the same HTTPS-on-the-right-port URL itself from variables
# that ARE visible to it (SW_TLS_CERT_FILE, SW_TLS_PORT, SW_PORT,
# SW_BASE_PATH are operator-supplied via `docker run -e` / compose, which
# Docker does propagate). See build/docker/healthcheck.sh for the exact
# derivation. JSON/exec form here still allows shell parameter expansion
# because the exec vector is ["sh", "-c", ...].
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["sh", "-c", "/healthcheck.sh"]

VOLUME ["/config", "/music"]

ENTRYPOINT ["/entrypoint.sh"]
CMD ["stillwater"]
