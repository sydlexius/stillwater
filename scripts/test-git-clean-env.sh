#!/usr/bin/env bash
#
# test-git-clean-env.sh -- behavioral regression tests for #3051: a gate helper
# run with an inherited GIT_DIR must not write into the invoking repo's config.
#
# `git init <path>` honors an inherited GIT_DIR: it re-initializes GIT_DIR and
# ignores <path>. Git hooks export GIT_DIR, and a worktree SHARES the main
# repository's `.git/config`, so a fixture line in a gate helper wrote
# core.bare=true into the MAIN repo, silently disabling its mass-deletion guard.
# Mechanism: scripts/lib/git-clean-env.sh.
#
# Each helper is invoked FOR REAL with a throwaway worktree's GIT_DIR exported
# the way a hook exports it, and the throwaway MAIN repo's WHOLE local config is
# compared afterwards -- a stray re-init could rewrite other [core] values, so
# one key is not enough.
#
# PRECONDITIONS ARE ASSERTED, NOT ASSUMED: an early hand-reproduction ran the
# helpers from a sandbox with no scripts/ directory, so every invocation died
# instantly and all of them looked clean.
#
# Run: bash scripts/test-git-clean-env.sh

set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

# This suite builds git fixtures, so it needs the guard it is testing --
# otherwise it reproduces the bug it exists to catch.
# shellcheck source=scripts/lib/git-clean-env.sh
. "$REPO_ROOT/scripts/lib/git-clean-env.sh"
git_clean_env_unset

# Fixtures must not inherit the developer's global config either.
export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_SYSTEM=/dev/null

WORK=$(mktemp -d)
WORK=$(cd "$WORK" && pwd -P) # macOS /tmp is a symlink to /private/tmp
trap 'rm -rf "$WORK"' EXIT

PASSED=0
FAILED=0

ok() {
    echo "  PASS  $1"
    PASSED=$((PASSED + 1))
}

bad() {
    echo "  FAIL  $1" >&2
    [ $# -gt 1 ] && printf '        %s\n' "${@:2}" >&2
    FAILED=$((FAILED + 1))
}

# require <description> <test-expression...> -- a failed precondition is a
# FAILED test, never a skip.
require() {
    local desc="$1"
    shift
    if "$@"; then
        return 0
    fi
    bad "PRECONDITION FAILED: $desc"
    return 1
}

# new_fixture <name> -- echo "<main-dir>|<worktree-dir>|<worktree-git-dir>".
new_fixture() {
    local name="$1"
    local main="$WORK/$name-main"
    local wt="$WORK/$name-wt"

    git init -q "$main"
    git -C "$main" config user.email test@example.com
    git -C "$main" config user.name "Test"
    git -C "$main" commit -q --allow-empty -m "base"
    git -C "$main" worktree add -q "$wt" -b "$name-branch"

    printf '%s|%s|%s\n' "$main" "$wt" "$(git -C "$wt" rev-parse --absolute-git-dir)"
}

# assert_config_unchanged <label> <main-repo> <before-config>
assert_config_unchanged() {
    local label="$1" main="$2" before="$3"
    local after
    after=$(git -C "$main" config --local --list | sort)
    if [ "$after" = "$before" ]; then
        ok "$label: the main repository's config is untouched"
        return 0
    fi
    bad "$label: the main repository's config was REWRITTEN" \
        "core.bare is now: $(git -C "$main" config --get core.bare || echo '(unset)')" \
        "diff (- before / + after):" \
        "$(diff <(printf '%s\n' "$before") <(printf '%s\n' "$after") | sed 's/^/  /' || true)"
    return 1
}

echo "=== git-clean-env guard (#3051) ==="
echo ""

# ---------------------------------------------------------------------------
echo "Case 1: the raw mechanism -- an inherited GIT_DIR redirects \`git init\`"
IFS='|' read -r M1 W1 G1 <<<"$(new_fixture case1)"

if require "the fixture is a real linked worktree" \
    test -f "$W1/.git" &&
    require "GIT_DIR points inside the main repo's git dir" \
        test -d "$G1" &&
    require "core.bare starts false" \
        test "$(git -C "$M1" config --get core.bare)" = "false"; then

    # Deliberately UNGUARDED: this case documents the hazard itself. Everything
    # else in this suite is guarded.
    probe="$W1/probe"
    mkdir -p "$probe"
    GIT_DIR="$G1" git init -q "$probe" 2>/dev/null || true

    if [ "$(git -C "$M1" config --get core.bare)" = "true" ]; then
        ok "unguarded \`git init\` flips the MAIN repo to bare (the bug reproduces)"
    else
        bad "the bug did NOT reproduce -- this suite cannot prove the fix works" \
            "git may have changed behaviour, or the fixture is not a worktree"
    fi

    # The same call, guarded, must leave it alone -- in BOTH forms the library
    # offers. Applied the way a real script applies it: GIT_DIR is already in the
    # environment at start and the script strips it. (Building the array first
    # and setting GIT_DIR after would strip nothing; the array only knows what
    # was set when it was built.) The subshell keeps GIT_DIR out of later cases.
    git -C "$M1" config core.bare false
    B1=$(git -C "$M1" config --local --list | sort)
    i=1
    for form in unset array; do
        i=$((i + 1))
        git -C "$M1" config core.bare false
        (
            # shellcheck disable=SC2030,SC2031  # subshell-local by design
            export GIT_DIR="$G1"
            . "$REPO_ROOT/scripts/lib/git-clean-env.sh"
            if [ "$form" = unset ]; then
                git_clean_env_unset
                git init -q "$W1/probe$i"
            else
                git_clean_env_array
                "${GIT_CLEAN_ENV[@]}" git init -q "$W1/probe$i"
            fi
        )
        assert_config_unchanged "git_clean_env_$form" "$M1" "$B1" || true
        require "git_clean_env_$form initialized the directory it was ASKED for" \
            test -d "$W1/probe$i/.git" || true
    done
fi
echo ""

# ---------------------------------------------------------------------------
# Every gate helper that builds a fixture git repository.
HELPERS=(
    test-check-codecov-floor-mirror.sh
    test-check-goreleaser-extra-files.sh
    test-stylelint-diff-gate.sh
    test-check-commit-signing.sh
    test-link-worktree-settings.sh
)

# check-commit-signing.sh runs `git init` too but is deliberately NOT above: a
# hook-invoked check whose exit code depends on the caller's signing config, so
# with no signer it exits BEFORE its probe repo and a "config unchanged" result
# would be vacuous. Covered where it can be exercised honestly -- Case 9 of
# test-check-commit-signing.sh drives the real hook through a real worktree with
# an ephemeral signer, then asserts the MAIN repo's core.bare.
#
# The two guard scripts themselves are excluded: check-git-init-guarded.sh's
# `git init` occurrences are inside its remediation heredoc, and this suite's
# own are its fixtures, guarded by the library it sources at the top.
EXCLUDED=(check-commit-signing.sh check-git-init-guarded.sh test-git-clean-env.sh)

n=1
for helper in "${HELPERS[@]}"; do
    n=$((n + 1))
    echo "Case $n: scripts/$helper leaves the invoking repository alone"

    if ! require "scripts/$helper exists" test -f "$REPO_ROOT/scripts/$helper"; then
        echo ""
        continue
    fi

    slug=${helper%.sh}
    IFS='|' read -r MAIN _WT GDIR <<<"$(new_fixture "$slug")"

    require "GIT_DIR is the worktree's git dir under the main repo" \
        test -d "$GDIR" || continue
    require "core.bare starts false" \
        test "$(git -C "$MAIN" config --get core.bare)" = "false" || continue

    BEFORE=$(git -C "$MAIN" config --local --list | sort)

    # Invoke exactly as the pre-push hook would: GIT_DIR exported, from the real
    # scripts/ directory (a helper run from a tree with no scripts/ dies
    # instantly and looks innocent).
    set +e
    OUT=$(cd "$REPO_ROOT" && GIT_DIR="$GDIR" bash "scripts/$helper" 2>&1)
    RC=$?
    set -e

    # The helper must actually have RUN. Its own verdict is not this suite's
    # business (some cases need tooling that may be absent locally), but silence
    # means it died before any `git init` and the assertion below is vacuous.
    if ! require "scripts/$helper produced output (it actually ran)" \
        test -n "$OUT"; then
        echo ""
        continue
    fi
    if [ "$RC" -ne 0 ] && ! printf '%s' "$OUT" | grep -qE 'PASS|passed|OK'; then
        bad "scripts/$helper produced no successful case at all (exit $RC)" \
            "the run may have died before any fixture was built, making the" \
            "config assertion below vacuous. First lines:" \
            "$(printf '%s' "$OUT" | head -3)"
        echo ""
        continue
    fi

    assert_config_unchanged "scripts/$helper" "$MAIN" "$BEFORE" || true
    echo ""
done

# ---------------------------------------------------------------------------
n=$((n + 1))
echo "Case $n: the full pre-push gate's helper block is covered"
# Every script running `git init` must appear in HELPERS or EXCLUDED, or this
# suite silently stops covering a live call site -- the original defect's exact
# shape. The list comes from check-git-init-guarded.sh --list, not a second grep
# here: a separate grep drifts, and the first one matched a script's own prose.
missing=""
while IFS= read -r f; do
    base=$(basename "$f")
    case " ${HELPERS[*]} ${EXCLUDED[*]} " in
        *" $base "*) continue ;;
    esac
    missing="$missing $base"
done < <(cd "$REPO_ROOT" && bash scripts/check-git-init-guarded.sh --list)

if [ -z "$missing" ]; then
    ok "every script containing \`git init\` is exercised by a case above"
else
    bad "scripts run \`git init\` but are not covered by this suite:$missing" \
        "add them to HELPERS, or exclude them with a stated reason"
fi
echo ""

echo "----------------------------------------"
echo "passed: $PASSED  failed: $FAILED"
[ "$FAILED" -eq 0 ] || exit 1
