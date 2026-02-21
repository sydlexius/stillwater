#!/bin/sh
set -e

PUID=${PUID:-1000}
PGID=${PGID:-1000}

# Resolve group: reuse existing group if GID is taken, otherwise create stillwater group
if [ "$(id -g stillwater 2>/dev/null)" != "$PGID" ]; then
    delgroup stillwater 2>/dev/null || true
    SW_GROUP=$(getent group "$PGID" | cut -d: -f1)
    if [ -z "$SW_GROUP" ]; then
        addgroup -g "$PGID" stillwater
        SW_GROUP="stillwater"
    fi
else
    SW_GROUP="stillwater"
fi

# Resolve user: recreate with desired UID and group membership
if [ "$(id -u stillwater 2>/dev/null)" != "$PUID" ]; then
    deluser stillwater 2>/dev/null || true
    adduser -u "$PUID" -G "$SW_GROUP" -s /bin/sh -D stillwater
fi

# Ensure data directory ownership using numeric IDs
chown -R "$PUID:$PGID" /data

# If first argument is a subcommand, prepend the binary path
case "${1:-}" in
    reset-credentials)
        exec su-exec "$PUID:$PGID" /app/stillwater "$@"
        ;;
    *)
        exec su-exec "$PUID:$PGID" "$@"
        ;;
esac
