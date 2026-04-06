#!/bin/sh
set -e

if [ "${PUID:-99}" != "99" ] || [ "${PGID:-100}" != "100" ]; then
    echo "WARNING: PUID/PGID remapping is not supported in this image. Running as stillwater (uid=99)." >&2
fi

exec "$@"
