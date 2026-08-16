#!/usr/bin/env bash
#
# check-git-init-guarded.sh -- refuse a `git init` under scripts/ or .githooks/
# that does not first clear the inherited git environment (#3051).
#
# With GIT_DIR set, `git init <path>` re-initializes GIT_DIR and ignores <path>.
# Git hooks export GIT_DIR, so a fixture line in a gate helper run from a
# worktree -- whose config IS the main repo's -- writes core.bare=true into the
# MAIN repository and silently disables its mass-deletion guard. See
# scripts/lib/git-clean-env.sh for the full mechanism.
#
# Four scripts hit this independently, and two of them had each written a
# private guard that never reached the others. That is a class that regenerates
# every time someone adds a fixture repo, so it gets a mechanical check -- same
# spirit as check-gate-invariant.sh enforcing a rule about the gate on the gate.
#
# GUARDED means: the file sources lib/git-clean-env.sh AND calls one of its
# functions; or the `git init` line is prefixed with a cleaned env array; or the
# file unsets GIT_DIR directly. Deliberately coarse -- this proves a guard is
# PRESENT, which is the part a new script forgets. That it WORKS is proved
# behaviorally by scripts/test-git-clean-env.sh.
#
# Run: bash scripts/check-git-init-guarded.sh [--list]

set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$REPO_ROOT"

# --list prints one path per file that RUNS `git init` and exits.
# test-git-clean-env.sh uses it to assert its own coverage, so both agree on
# what "a git init site" means from ONE definition. A second grep over there
# drifts: its first version matched this file's own explanatory prose.
LIST_ONLY=0
[ "${1:-}" = "--list" ] && LIST_ONLY=1

VIOLATIONS=0

report() {
    echo "FAIL: $1" >&2
    VIOLATIONS=$((VIOLATIONS + 1))
}

# Tracked-only, so an untracked scratch script cannot fail someone's push.
mapfile -t FILES < <(git ls-files scripts .githooks | sort)

for f in "${FILES[@]}"; do
    [ -f "$f" ] || continue
    # Only text files git would treat as scripts.
    case "$f" in
        *.md | *.json | *.yml | *.yaml | *.txt) continue ;;
    esac

    # Lines invoking `git init`, excluding comments and this file's own prose.
    init_lines=$(grep -nE '(^|[^[:alnum:]_-])git[[:space:]]+init([[:space:]]|$)' "$f" |
        grep -vE '^[0-9]+:[[:space:]]*#' || true)
    [ -n "$init_lines" ] || continue

    if [ "$LIST_ONLY" -eq 1 ]; then
        printf '%s\n' "$f"
        continue
    fi

    # File-level guard: sources the shared library and actually calls it.
    if grep -q 'lib/git-clean-env\.sh' "$f" &&
        grep -qE 'git_clean_env_(unset|array)' "$f"; then
        continue
    fi
    # File-level guard: unsets the location variable directly.
    if grep -qE '^[[:space:]]*unset[[:space:]].*\bGIT_DIR\b' "$f"; then
        continue
    fi

    while IFS= read -r line; do
        num=${line%%:*}
        body=${line#*:}
        # Line-level guard: a cleaned env array prefixes the invocation.
        if printf '%s' "$body" | grep -qE '\$\{(GIT_)?CLEAN_ENV\[@\]\}[[:space:]]+git[[:space:]]+init'; then
            continue
        fi
        if printf '%s' "$body" | grep -qE '(^|[[:space:]])env[[:space:]]+(-u[[:space:]]+[A-Za-z_]+[[:space:]]+)*.*git[[:space:]]+init'; then
            continue
        fi
        report "$f:$num runs \`git init\` with an unguarded git environment."
        echo "        $(printf '%s' "$body" | sed 's/^[[:space:]]*//')" >&2
    done <<<"$init_lines"
done

[ "$LIST_ONLY" -eq 1 ] && exit 0

if [ "$VIOLATIONS" -gt 0 ]; then
    cat >&2 <<'EOF'

An inherited GIT_DIR makes `git init <path>` re-initialize GIT_DIR and ignore
<path>. Run from a worktree by the pre-push hook, that writes core.bare=true
into the MAIN repository's shared config and silently disables its
mass-deletion guard (#3051).

Fix: source the shared guard near the top of the script, before any fixture is
built:

    # shellcheck source=scripts/lib/git-clean-env.sh
    . "$REPO_ROOT/scripts/lib/git-clean-env.sh"
    git_clean_env_unset

A script that must keep its OWN git environment (a hook-invoked check operating
on the caller's repository) uses the array form instead:

    git_clean_env_array
    "${GIT_CLEAN_ENV[@]}" git init -q "$dir"

See scripts/lib/git-clean-env.sh for what is stripped and what survives.
EOF
    exit 1
fi

echo "OK: every \`git init\` under scripts/ and .githooks/ clears the inherited git environment."
