#!/bin/sh
set -e

PUID="${PUID:-99}"
PGID="${PGID:-100}"

# Remap the stillwater user/group to the requested UID/GID, then drop
# privileges via su-exec so the application never runs as root.
if [ "$(id -u)" = "0" ]; then
    CURRENT_GID="$(id -g stillwater 2>/dev/null || echo '')"
    CURRENT_UID="$(id -u stillwater 2>/dev/null || echo '')"

    # Must delete user before group (user holds group as primary reference)
    if [ "${CURRENT_UID}" != "${PUID}" ] || [ "${CURRENT_GID}" != "${PGID}" ]; then
        deluser stillwater 2>/dev/null || true
    fi

    if [ "${CURRENT_GID}" != "${PGID}" ]; then
        delgroup stillwater 2>/dev/null || true
        addgroup -g "${PGID}" stillwater
    fi

    # Recreate user if UID changed or if deleted for GID change
    if [ "${CURRENT_UID}" != "${PUID}" ] || [ "${CURRENT_GID}" != "${PGID}" ]; then
        adduser -u "${PUID}" -G stillwater -s /bin/sh -D stillwater
    fi

    chown -R stillwater:stillwater /data /music 2>/dev/null || true

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
