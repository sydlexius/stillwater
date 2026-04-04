#!/usr/bin/env bash
# dev-restart.sh -- build, kill the running Stillwater server, and relaunch.
# Only kills processes named "stillwater" to avoid collateral damage (e.g.
# browsers listening on the same port).
set -euo pipefail

cd "$(dirname "$0")/.."

echo "==> Building..."
make build

echo "==> Stopping previous Stillwater instance..."
pkill -f './stillwater$' 2>/dev/null || true
pkill -x stillwater 2>/dev/null || true
sleep 1

echo "==> Loading .env..."
if [ -f .env ]; then
  set -a
  # shellcheck disable=SC1091
  . .env
  set +a
fi

echo "==> Launching Stillwater..."
./stillwater &
SWPID=$!
sleep 2

if kill -0 "$SWPID" 2>/dev/null; then
  echo "==> Stillwater running (PID $SWPID) on http://localhost:${SW_PORT:-1973}"
else
  echo "==> ERROR: Stillwater failed to start"
  exit 1
fi
