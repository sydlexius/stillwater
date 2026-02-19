#!/bin/sh
set -e

PUID=${PUID:-1000}
PGID=${PGID:-1000}

# Update stillwater group and user IDs if they differ
if [ "$(id -g stillwater)" != "$PGID" ]; then
    delgroup stillwater 2>/dev/null || true
    addgroup -g "$PGID" stillwater
fi

if [ "$(id -u stillwater)" != "$PUID" ]; then
    deluser stillwater 2>/dev/null || true
    adduser -u "$PUID" -G stillwater -s /bin/sh -D stillwater
fi

# Ensure data directory ownership
chown -R stillwater:stillwater /data

# Run as the configured user
exec su-exec stillwater:stillwater "$@"
