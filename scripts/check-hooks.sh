#!/usr/bin/env bash
# Verify the pre-push gate is actually wired up.
# Exits 0 when configuration is correct, non-zero otherwise.

set -euo pipefail

hooks_path=$(git config --get core.hooksPath 2>/dev/null || true)

if [ "$hooks_path" != ".githooks" ]; then
    echo "FAIL: core.hooksPath is '$hooks_path', expected '.githooks'" >&2
    echo "Fix: run 'make hooks' from this worktree" >&2
    exit 1
fi

for h in pre-commit post-commit pre-push; do
    if [ ! -x ".githooks/$h" ]; then
        echo "FAIL: .githooks/$h missing or not executable" >&2
        echo "Fix: run 'make hooks' from this worktree" >&2
        exit 1
    fi
done

echo "OK: hooks configured -- core.hooksPath=.githooks, pre-commit + post-commit + pre-push executable"
