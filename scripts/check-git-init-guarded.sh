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
# FAIL CLOSED. Every rule below is an ALLOW-list: an invocation is guarded only
# if it positively matches a known-good form. Anything unrecognized is a
# violation. The first version of this check inverted that in four places and
# every one was a silent bypass (see the mutation cases in
# scripts/test-check-git-init-guarded.sh):
#
#   - GUARDS ARE CHECKED IN EXECUTION ORDER. A file-level guard counts only for
#     `git init` lines BELOW it. Cleanup AFTER the invocation is not a guard;
#     the damage is already done.
#   - AN `env` PREFIX MUST REMOVE GIT_DIR. `env FOO=bar git init` and
#     `env -u FOO git init` are not guards, they are unrelated env usage.
#   - OPTION FORMS COUNT. `git -C "$dir" init` is the same hazard as
#     `git init "$dir"`; GIT_DIR outranks `-C`.
#
# PORTABILITY IS PART OF THE CONTRACT, not a nicety. This runs in the local
# pre-push path on macOS, where /bin/bash is 3.2.57 and grep is BSD. So: no
# `mapfile` (a bash 4.0 builtin -- it would leave the file list EMPTY and the
# check would exit 0 having examined nothing, which is precisely the
# silent-pass class this script exists to close), and no GNU-only `\b`
# word boundary (BSD grep may treat it as a literal backspace, which would
# report a correctly-guarded script as unguarded).
#
# This check proves a guard is PRESENT, which is the part a new script forgets.
# That it WORKS is proved behaviorally by scripts/test-git-clean-env.sh.
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

# A `git init` invocation, including option forms (`git -C <dir> init`). The
# intervening tokens must look like options, so `git commit -m "init"` does not
# match while `git --git-dir=x init` does.
INIT_RE='(^|[^[:alnum:]_-])git([[:space:]]+-[^[:space:]]+([[:space:]]+[^[:space:]-][^[:space:]]*)?)*[[:space:]]+init([[:space:]]|$)'

# GIT_DIR as a whole word. Two portable pieces rather than one expression:
# `unset[[:space:]]` already consumes the delimiter, so folding the boundary
# into a single regex demands a SECOND separator and silently never matches
# `unset GIT_DIR ...`. Verified against BSD grep and GNU grep alike. Not `\b`,
# which is a GNU extension BSD grep may read as a literal backspace.
UNSET_RE='^[[:space:]]*unset([[:space:]]|$)'
GIT_DIR_WORD_RE='(^|[[:space:]])GIT_DIR([[:space:]]|$)'

VIOLATIONS=0
EXAMINED=0

report() {
    echo "FAIL: $1" >&2
    VIOLATIONS=$((VIOLATIONS + 1))
}

# first_line_matching <regex> <file> -- line number of the first match, or 0.
first_line_matching() {
    grep -nE "$1" "$2" 2>/dev/null | grep -vE '^[0-9]+:[[:space:]]*#' |
        head -1 | cut -d: -f1 || true
}

# first_unset_of_git_dir <file> -- line number of the first `unset ... GIT_DIR`,
# or 0. Both predicates must hold on the SAME line.
first_unset_of_git_dir() {
    local n
    n=$(grep -nE "$UNSET_RE" "$1" 2>/dev/null |
        grep -vE '^[0-9]+:[[:space:]]*#' |
        grep -E "$GIT_DIR_WORD_RE" |
        head -1 | cut -d: -f1 || true)
    [ -n "$n" ] || n=0
    printf '%s' "$n"
}

# Tracked-only, so an untracked scratch script cannot fail someone's push.
# A `while read` loop, NOT mapfile: mapfile is bash 4.0 and this runs on macOS
# system bash 3.2, where it would silently produce an empty list.
FILES=""
while IFS= read -r f; do
    FILES="$FILES$f
"
done < <(git ls-files scripts .githooks | sort)

while IFS= read -r f; do
    [ -n "$f" ] || continue
    [ -f "$f" ] || continue
    case "$f" in
        *.md | *.json | *.yml | *.yaml | *.txt) continue ;;
    esac

    # Lines invoking `git init`, excluding comments.
    init_lines=$(grep -nE "$INIT_RE" "$f" 2>/dev/null |
        grep -vE '^[0-9]+:[[:space:]]*#' || true)
    [ -n "$init_lines" ] || continue

    EXAMINED=$((EXAMINED + 1))

    if [ "$LIST_ONLY" -eq 1 ]; then
        printf '%s\n' "$f"
        continue
    fi

    # Earliest line at which the environment becomes clean for the rest of the
    # file. The library guard counts only if the file also SOURCES the library
    # (a bare call would be an undefined function).
    guard_line=0
    if grep -q 'lib/git-clean-env\.sh' "$f"; then
        guard_line=$(first_line_matching 'git_clean_env_(unset|array)[[:space:]]*$' "$f")
        [ -n "$guard_line" ] || guard_line=0
    fi
    unset_line=$(first_unset_of_git_dir "$f")
    if [ "$unset_line" -gt 0 ]; then
        if [ "$guard_line" -eq 0 ] || [ "$unset_line" -lt "$guard_line" ]; then
            guard_line=$unset_line
        fi
    fi

    while IFS= read -r line; do
        [ -n "$line" ] || continue
        num=${line%%:*}
        body=${line#*:}

        # A file-level guard covers only what comes BELOW it.
        if [ "$guard_line" -gt 0 ] && [ "$guard_line" -lt "$num" ]; then
            continue
        fi

        # Line-level: a cleaned env array built by the library.
        if printf '%s' "$body" |
            grep -qE '\$\{(GIT_)?CLEAN_ENV\[@\]\}"?[[:space:]]+git'; then
            continue
        fi
        # Line-level: an explicit `env -u GIT_DIR ... git init`. Requiring
        # GIT_DIR by name is the point -- `env FOO=bar git init` and
        # `env -u FOO git init` clear nothing that matters.
        if printf '%s' "$body" |
            grep -qE '(^|[[:space:]])env[[:space:]]+.*-u[[:space:]]+GIT_DIR([[:space:]]|$)'; then
            continue
        fi

        report "$f:$num runs \`git init\` with an unguarded git environment."
        echo "        $(printf '%s' "$body" | sed 's/^[[:space:]]*//')" >&2
        if [ "$guard_line" -gt 0 ]; then
            echo "        (the file's guard is at line $guard_line, BELOW this" >&2
            echo "         invocation -- cleanup after the fact is not a guard)" >&2
        fi
    done <<EOF
$init_lines
EOF
done <<EOF
$FILES
EOF

# A check that examined nothing must not report success. On bash 3.2 the
# original `mapfile` left the file list empty and this script exited 0 having
# looked at no files at all -- a green light wired to nothing.
if [ "$EXAMINED" -eq 0 ]; then
    echo "FAIL: examined 0 files containing \`git init\`." >&2
    echo "      The repository has such files, so this means the scan itself is" >&2
    echo "      broken (an empty file list, a shell builtin missing on this" >&2
    echo "      bash, or a regex that matched nothing) -- not that all is well." >&2
    exit 1
fi

if [ "$LIST_ONLY" -eq 1 ]; then
    exit 0
fi

if [ "$VIOLATIONS" -gt 0 ]; then
    cat >&2 <<'EOF'

An inherited GIT_DIR redirects the initialization above: it re-initializes the
directory GIT_DIR names and ignores the path argument. Run from a worktree by
the pre-push hook, that writes core.bare=true into the MAIN repository's shared
config and silently disables its mass-deletion guard (#3051).

Fix: source the shared guard near the top of the script -- ABOVE every `git`
invocation, since cleanup afterwards does not help:

    # shellcheck source=scripts/lib/git-clean-env.sh
    . "$REPO_ROOT/scripts/lib/git-clean-env.sh"
    git_clean_env_unset

A script that must keep its OWN git environment (a hook-invoked check operating
on the caller's repository) cleans each invocation instead:

    git_clean_env_array
    "${GIT_CLEAN_ENV[@]}" git init -q "$dir"

See scripts/lib/git-clean-env.sh for what is stripped and what survives.
EOF
    exit 1
fi

echo "OK: $EXAMINED files run \`git init\`; every invocation clears the inherited git environment."
