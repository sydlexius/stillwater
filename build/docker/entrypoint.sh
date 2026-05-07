#!/bin/sh
set -e

PUID="${PUID:-99}"
PGID="${PGID:-100}"

# Derive SW_HEALTH_URL when direct TLS is configured so the Dockerfile
# HEALTHCHECK probes HTTPS on the right port. SW_TLS_PORT wins when set to
# a real port; the literal value 0 is the documented "reuse SW_PORT" sentinel
# (the Go config treats it the same as unset), so the shell must too.
if [ -n "${SW_TLS_CERT_FILE:-}" ] && [ -z "${SW_HEALTH_URL:-}" ]; then
    if [ -n "${SW_TLS_PORT:-}" ] && [ "${SW_TLS_PORT}" != "0" ]; then
        SW_HEALTH_PORT="${SW_TLS_PORT}"
    else
        SW_HEALTH_PORT="${SW_PORT:-1973}"
    fi
    SW_HEALTH_URL="https://localhost:${SW_HEALTH_PORT}${SW_BASE_PATH:-}/api/v1/health"
    export SW_HEALTH_URL
fi

# Remap the stillwater user/group to the requested UID/GID, then drop
# privileges via su-exec so the application never runs as root.
if [ "$(id -u)" = "0" ]; then
    CURRENT_GID="$(id -g stillwater 2>/dev/null || echo '')"
    CURRENT_UID="$(id -u stillwater 2>/dev/null || echo '')"
    # Resolve group owning PGID upfront so restarts that skip the remap
    # branch still know which group to use for adduser and chown.
    PGID_GROUP="$(awk -F: -v gid="${PGID}" '$3 == gid { print $1; exit }' /etc/group)"

    # Remap group if needed
    if [ "${CURRENT_GID}" != "${PGID}" ]; then
        # Delete user first (holds group as primary reference)
        deluser stillwater 2>/dev/null || true
        if [ -z "${PGID_GROUP}" ]; then
            # GID is free -- create the stillwater group with it
            delgroup stillwater 2>/dev/null || true
            addgroup -g "${PGID}" stillwater
            PGID_GROUP="stillwater"
        elif [ "${PGID_GROUP}" != "stillwater" ]; then
            # GID is taken by another group -- reuse it, drop our old group
            delgroup stillwater 2>/dev/null || true
        fi
        # Third case: PGID_GROUP == "stillwater" -- already correct, no action
    fi

    # Remap user if UID or GID changed
    if [ "${CURRENT_UID}" != "${PUID}" ] || [ "${CURRENT_GID}" != "${PGID}" ]; then
        deluser stillwater 2>/dev/null || true
        adduser -u "${PUID}" -G "${PGID_GROUP:-stillwater}" -s /bin/sh -D stillwater
    fi

    # Migrate /data -> /config for existing installs
    if [ -d /data ] && [ -f /data/stillwater.db ] && [ ! -f /config/stillwater.db ]; then
        for f in stillwater.db config.yaml encryption.key; do
            [ -e "/data/$f" ] && ln -sf "/data/$f" "/config/$f"
        done
    fi

    chown -R stillwater:"${PGID_GROUP:-stillwater}" /config /music 2>/dev/null || true

    case "${1:-}" in
        reset-credentials)
            exec su-exec stillwater stillwater "$@"
            ;;
        *)
            exec su-exec stillwater "$@"
            ;;
    esac
else
    # Already non-root (e.g. Kubernetes runAsUser override).
    case "${1:-}" in
        reset-credentials)
            exec stillwater "$@"
            ;;
        *)
            exec "$@"
            ;;
    esac
fi
