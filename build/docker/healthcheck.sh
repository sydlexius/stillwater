#!/bin/sh
# HEALTHCHECK probe for the stillwater container.
#
# Docker runs HEALTHCHECK as a fresh process sourced from the image's Env
# config, so it derives its own probe URL here rather than relying on
# anything set at runtime by another process. SW_TLS_CERT_FILE, SW_TLS_PORT,
# SW_PORT, and SW_BASE_PATH are all operator-supplied via `docker run -e` /
# compose, which Docker does propagate into the healthcheck process.
#
# Derivation:
#   - an operator-set SW_HEALTH_URL always wins
#   - TLS mode is SW_TLS_CERT_FILE non-empty -> scheme https
#   - SW_TLS_PORT wins when set and not the literal "0" (the documented
#     "reuse SW_PORT" sentinel); otherwise SW_PORT, default 1973
#   - non-TLS keeps the plain http://localhost:${SW_PORT:-1973}... probe
set -e

if [ -n "${SW_HEALTH_URL:-}" ]; then
    URL="${SW_HEALTH_URL}"
elif [ -n "${SW_TLS_CERT_FILE:-}" ]; then
    if [ -n "${SW_TLS_PORT:-}" ] && [ "${SW_TLS_PORT}" != "0" ]; then
        HEALTH_PORT="${SW_TLS_PORT}"
    else
        HEALTH_PORT="${SW_PORT:-1973}"
    fi
    URL="https://localhost:${HEALTH_PORT}${SW_BASE_PATH:-}/api/v1/health"
else
    URL="http://localhost:${SW_PORT:-1973}${SW_BASE_PATH:-}/api/v1/health"
fi

# --no-check-certificate is intentional: a localhost healthcheck against a
# self-signed cert is acceptable.
wget -q -O /dev/null --no-check-certificate "${URL}" || exit 1
