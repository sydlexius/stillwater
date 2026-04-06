#!/bin/sh
set -e

if [ "${PUID:-99}" != "99" ] || [ "${PGID:-100}" != "100" ]; then
    echo "WARNING: PUID/PGID are ignored by this image. The container always runs as the built-in stillwater user/group (uid=99 gid=100). Configure host ownership/permissions for mounted volumes externally." >&2
fi

case "${1:-}" in
    reset-credentials)
        exec stillwater "$@"
        ;;
    *)
        exec "$@"
        ;;
esac
