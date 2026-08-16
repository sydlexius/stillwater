#!/usr/bin/env bash
#
# test-check-git-init-guarded.sh -- mutation tests for
# scripts/check-git-init-guarded.sh (#3051, fix round for PR #3054).
#
# A guard that quietly returns OK manufactures confidence, so the guard needs
# its own tests. Every case below corresponds to a real defect in the first
# implementation, found in review rather than by reading it:
#
#   - `mapfile` is a bash 4.0 builtin. On macOS system bash 3.2 the file list
#     came back EMPTY, the loop body never ran, and the check exited 0 having
#     examined nothing. Case A runs the check under /bin/bash when that is 3.2,
#     and Case B asserts a non-zero examined count regardless of shell -- so the
#     next 4.0-ism cannot reintroduce a silent pass on a machine where the test
#     itself runs under bash 5.
#   - Guards were matched file-wide, so cleanup written BELOW a `git init`
#     counted as protecting it (Case D).
#   - Any `env` prefix satisfied the line-level rule, so `env FOO=bar git init`
#     and `env -u FOO git init` passed while clearing nothing (Cases E, F).
#   - Option forms were not detected at all: `git -C "$dir" init` matched no
#     rule and sailed through (Case G).
#
# Hermetic: each case writes a fixture script into a throwaway clone of the
# repo's scripts/ layout and runs the checker against it. Never mutates the real
# working tree -- an earlier version of this suite dropped fixtures into
# scripts/ and relied on cleanup, which loses the race against any concurrent
# gate run.
#
# Run: bash scripts/test-check-git-init-guarded.sh

set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
CHECK="$REPO_ROOT/scripts/check-git-init-guarded.sh"

# This suite builds git fixtures, so it needs the same guard the rest of the
# gate uses -- otherwise it reproduces the bug the checker exists to catch.
# shellcheck source=scripts/lib/git-clean-env.sh
. "$REPO_ROOT/scripts/lib/git-clean-env.sh"
git_clean_env_unset

export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_SYSTEM=/dev/null

WORK=$(mktemp -d)
WORK=$(cd "$WORK" && pwd -P)
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

# new_repo -- a throwaway git repo carrying the checker and its library, so the
# checker's `git ls-files` sees only fixtures we control.
new_repo() {
    local d="$WORK/repo$1"
    mkdir -p "$d/scripts/lib" "$d/.githooks"
    cp "$CHECK" "$d/scripts/check-git-init-guarded.sh"
    cp "$REPO_ROOT/scripts/lib/git-clean-env.sh" "$d/scripts/lib/git-clean-env.sh"
    git init -q "$d"
    git -C "$d" config user.email test@example.com
    git -C "$d" config user.name Test
    printf '%s\n' "$d"
}

# run_check <repo> [shell] -- echo "<exit>|<output>".
run_check() {
    local d="$1" sh="${2:-bash}" out rc
    git -C "$d" add -A >/dev/null 2>&1
    set +e
    out=$("$sh" "$d/scripts/check-git-init-guarded.sh" 2>&1)
    rc=$?
    set -e
    printf '%s|%s' "$rc" "$out"
}

# expect <label> <repo> <want-exit> -- run and compare.
expect() {
    local label="$1" d="$2" want="$3" res rc out
    res=$(run_check "$d")
    rc=${res%%|*}
    out=${res#*|}
    if [ "$rc" -eq "$want" ]; then
        ok "$label"
    else
        bad "$label (wanted exit $want, got $rc)" "$out"
    fi
}

echo "=== check-git-init-guarded self-tests (#3051) ==="
echo ""

# ---------------------------------------------------------------------------
echo "Case A: runs correctly under macOS system bash 3.2"
# The original used mapfile (bash 4.0) and silently examined zero files here.
if [ -x /bin/bash ] && /bin/bash --version | head -1 | grep -q 'version 3\.'; then
    D=$(new_repo A)
    cat >"$D/scripts/fixture.sh" <<'EOF'
#!/usr/bin/env bash
. "$R/scripts/lib/git-clean-env.sh"
git_clean_env_unset
git init -q "$dir"
EOF
    git -C "$D" add -A >/dev/null 2>&1
    set +e
    OUT=$(/bin/bash "$D/scripts/check-git-init-guarded.sh" 2>&1)
    RC=$?
    set -e
    if [ "$RC" -ne 0 ]; then
        bad "the checker failed under bash 3.2" "$OUT"
    elif printf '%s' "$OUT" | grep -qE '^OK: [1-9][0-9]* files'; then
        ok "bash 3.2: exits 0 AND reports a non-zero examined count"
    else
        bad "bash 3.2: exit 0 but the examined count is missing or zero" \
            "this is the silent-pass shape the check exists to prevent" "$OUT"
    fi
else
    bad "PRECONDITION: /bin/bash is not 3.x -- cannot verify the 3.2 path here" \
        "found: $(/bin/bash --version 2>&1 | head -1)"
fi
echo ""

# ---------------------------------------------------------------------------
echo "Case B: a scan that examines nothing FAILS instead of reporting OK"
# Shell-independent backstop for Case A: whatever makes the file list empty, the
# check must never call that success.
D=$(new_repo B)
rm -f "$D/scripts/lib/git-clean-env.sh"
# A repo whose only tracked script is the checker itself still has a `git init`
# site, so force the empty case by pointing it at a repo with none.
D2="$WORK/empty"
mkdir -p "$D2/scripts"
printf '#!/usr/bin/env bash\necho hi\n' >"$D2/scripts/nothing.sh"
cp "$CHECK" "$D2/check-copy.sh"
git init -q "$D2"
git -C "$D2" config user.email t@e
git -C "$D2" config user.name T
git -C "$D2" add -A >/dev/null 2>&1
set +e
OUT=$(cd "$D2" && bash "$D2/check-copy.sh" 2>&1)
RC=$?
set -e
if [ "$RC" -ne 0 ] && printf '%s' "$OUT" | grep -q 'examined 0 files'; then
    ok "a zero-file scan exits non-zero and says so"
else
    bad "a zero-file scan reported success (exit $RC)" "$OUT"
fi
echo ""

# ---------------------------------------------------------------------------
echo "Case C: a correctly guarded script PASSES (no false positive)"
D=$(new_repo C)
cat >"$D/scripts/fixture.sh" <<'EOF'
#!/usr/bin/env bash
. "$R/scripts/lib/git-clean-env.sh"
git_clean_env_unset
git init -q "$dir"
EOF
expect "library guard above the invocation is accepted" "$D" 0

D=$(new_repo C2)
cat >"$D/scripts/fixture.sh" <<'EOF'
#!/usr/bin/env bash
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE
git init -q "$dir"
EOF
expect "direct \`unset GIT_DIR\` above the invocation is accepted" "$D" 0

D=$(new_repo C3)
cat >"$D/scripts/fixture.sh" <<'EOF'
#!/usr/bin/env bash
git_clean_env_array
"${GIT_CLEAN_ENV[@]}" git init -q "$dir"
EOF
expect "the cleaned-array prefix is accepted" "$D" 0

D=$(new_repo C4)
cat >"$D/scripts/fixture.sh" <<'EOF'
#!/usr/bin/env bash
env -u GIT_DIR git init -q "$dir"
EOF
expect "an explicit \`env -u GIT_DIR\` prefix is accepted" "$D" 0
echo ""

# ---------------------------------------------------------------------------
echo "Case D: a guard BELOW the invocation is refused (execution order)"
D=$(new_repo D)
cat >"$D/scripts/fixture.sh" <<'EOF'
#!/usr/bin/env bash
. "$R/scripts/lib/git-clean-env.sh"
git init -q "$dir"
git_clean_env_unset
EOF
expect "cleanup after the fact is not a guard" "$D" 1
echo ""

# ---------------------------------------------------------------------------
echo "Case E: \`env FOO=bar git init\` is refused (env prefix clearing nothing)"
D=$(new_repo E)
cat >"$D/scripts/fixture.sh" <<'EOF'
#!/usr/bin/env bash
env FOO=bar git init -q "$dir"
EOF
expect "an unrelated env assignment is not a guard" "$D" 1
echo ""

# ---------------------------------------------------------------------------
echo "Case F: \`env -u FOO git init\` is refused (unsets the wrong variable)"
D=$(new_repo F)
cat >"$D/scripts/fixture.sh" <<'EOF'
#!/usr/bin/env bash
env -u FOO git init -q "$dir"
EOF
expect "unsetting an unrelated variable is not a guard" "$D" 1
echo ""

# ---------------------------------------------------------------------------
echo "Case G: option forms are detected (\`git -C <dir> init\`)"
D=$(new_repo G)
cat >"$D/scripts/fixture.sh" <<'EOF'
#!/usr/bin/env bash
git -C "$dir" init -q repo
EOF
expect "\`git -C <dir> init\` is detected and refused" "$D" 1

D=$(new_repo G2)
cat >"$D/scripts/fixture.sh" <<'EOF'
#!/usr/bin/env bash
git commit -m "init the thing"
EOF
expect "\`git commit -m \"init ...\"\` is NOT mistaken for a git init site" "$D" 0
echo ""

# ---------------------------------------------------------------------------
echo "Case H: a bare unguarded \`git init\` is refused"
D=$(new_repo H)
cat >"$D/scripts/fixture.sh" <<'EOF'
#!/usr/bin/env bash
git init -q "$dir"
EOF
expect "the original defect shape still fails" "$D" 1
echo ""

# ---------------------------------------------------------------------------
echo "Case J: guard detection survives BSD grep (no GNU-only regex)"
# The checker runs on macOS, where /usr/bin/grep is BSD. A GNU-only construct
# (`\b`) can read as a literal backspace there and report a correctly-guarded
# script as UNGUARDED -- a false positive that fails the gate. PATH is pinned so
# a homebrew GNU grep earlier on PATH cannot mask the difference.
#
# NOTE: this machine's /usr/bin/grep is "BSD grep, GNU compatible" and does
# honor \b, so this case cannot reproduce the failure here -- it pins the
# CURRENT portable form as working under BSD grep rather than proving the GNU
# form broken. On a stricter BSD grep it would catch the regression.
D=$(new_repo J)
cat >"$D/scripts/fixture.sh" <<'EOF'
#!/usr/bin/env bash
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE
git init -q "$dir"
EOF
git -C "$D" add -A >/dev/null 2>&1
set +e
OUT=$(PATH=/usr/bin:/bin /bin/bash "$D/scripts/check-git-init-guarded.sh" 2>&1)
RC=$?
set -e
if [ "$RC" -eq 0 ]; then
    ok "BSD grep + bash 3.2: \`unset GIT_DIR\` is still detected as a guard"
else
    bad "BSD grep run reported the guarded fixture as unguarded (exit $RC)" \
        "a GNU-only regex construct has crept back in" "$OUT"
fi
echo ""

# ---------------------------------------------------------------------------
echo "Case I: --list is non-empty and excludes comment-only mentions"
set +e
LIST=$(bash "$CHECK" --list 2>&1)
LRC=$?
set -e
if [ "$LRC" -ne 0 ]; then
    bad "--list exited $LRC" "$LIST"
elif [ -z "$LIST" ]; then
    bad "--list returned nothing" \
        "test-git-clean-env.sh derives its coverage from this, so an empty" \
        "list would silently make that suite's coverage case vacuous"
else
    ok "--list returns $(printf '%s\n' "$LIST" | grep -c .) call sites"
fi
echo ""

echo "----------------------------------------"
echo "passed: $PASSED  failed: $FAILED"
[ "$FAILED" -eq 0 ] || exit 1
