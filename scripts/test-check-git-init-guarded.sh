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
SKIPPED=0

ok() {
    echo "  PASS  $1"
    PASSED=$((PASSED + 1))
}

# skip <what-was-not-verified> <why> -- for a precondition that CANNOT hold on
# this platform, as opposed to one that failed. A skip is LOUD by design: its
# own line, its own tally column, and the concrete reason. It is never folded
# into the pass count, because "we did not check" and "we checked and it was
# fine" must not read the same to someone scanning output on an unfamiliar
# machine.
#
# Reserved for environment facts the harness cannot supply (no bash 3.x on this
# host). A behavioral precondition -- a fixture that failed to set itself up --
# is still a FAILURE: there the environment could have supplied it and did not.
skip() {
    echo "  SKIP  $1"
    printf '        %s\n' "${@:2}"
    SKIPPED=$((SKIPPED + 1))
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
echo "Case A: runs correctly under a bash 3.x (the macOS system shell)"
# The original used mapfile (bash 4.0) and silently examined zero files here.
#
# CASE A AND CASE B ARE DELIBERATELY LAYERED, NOT REDUNDANT. A is the direct
# verification and only a host with a bash 3.x can perform it -- macOS, where
# /bin/bash is 3.2.57 and where the bug actually bit. B asserts the same
# property (a scan that examines nothing must not report OK) in a
# shell-independent way, so CI's Linux runners still cover the regression even
# though they have no 3.x to run A against. Neither replaces the other: without
# A the 3.2 path is never really executed, and without B the property goes
# unchecked everywhere A cannot run.
#
# So A SKIPS rather than FAILS when no bash 3.x exists. An unmeetable
# precondition is not a defect in the code under test, and failing on it made
# this suite red on every Linux runner while every real assertion passed. The
# skip is loud, and names the version found, so nobody mistakes a machine that
# could not check for a machine that checked and was happy.
BASH3=""
for cand in /bin/bash /usr/bin/bash bash3 bash-3.2; do
    p=$(command -v "$cand" 2>/dev/null) || continue
    [ -x "$p" ] || continue
    if "$p" --version 2>/dev/null | head -1 | grep -q 'version 3\.'; then
        BASH3="$p"
        break
    fi
done

if [ -n "$BASH3" ]; then
    D=$(new_repo A)
    cat >"$D/scripts/fixture.sh" <<'EOF'
#!/usr/bin/env bash
. "$R/scripts/lib/git-clean-env.sh"
git_clean_env_unset
git init -q "$dir"
EOF
    git -C "$D" add -A >/dev/null 2>&1
    set +e
    OUT=$("$BASH3" "$D/scripts/check-git-init-guarded.sh" 2>&1)
    RC=$?
    set -e
    B3V=$("$BASH3" --version 2>/dev/null | head -1)
    if [ "$RC" -ne 0 ]; then
        bad "the checker failed under $BASH3" "$B3V" "$OUT"
    elif printf '%s' "$OUT" | grep -qE '^OK: [1-9][0-9]* files'; then
        ok "$BASH3: exits 0 AND reports a non-zero examined count"
    else
        bad "$BASH3: exit 0 but the examined count is missing or zero" \
            "this is the silent-pass shape the check exists to prevent" "$OUT"
    fi
else
    skip "the bash 3.x path was NOT exercised on this host" \
        "no bash 3.x found (tried: /bin/bash /usr/bin/bash bash3 bash-3.2)" \
        "this shell is: $(bash --version 2>&1 | head -1)" \
        "Case B covers the same property shell-independently, so the regression" \
        "is still guarded here; run this suite on macOS, where /bin/bash is" \
        "3.2.57, for the direct verification."
fi
echo ""

# ---------------------------------------------------------------------------
echo "Case B: a scan that examines nothing FAILS instead of reporting OK"
# Shell-independent backstop for Case A, and on CI the ONLY protection for the
# silent-pass class: Case A skips on Linux (no bash 3.x), so if this case is
# wrong the class is unguarded there.
#
# THE FIXTURE MUST PUT THE CHECKER AT <repo>/scripts/, not at <repo>/. The
# checker resolves REPO_ROOT as `dirname $0`/.., so a copy at the repo root
# makes REPO_ROOT the repo's PARENT -- outside any git repository. Then
# `git ls-files` FAILS, the file list is empty for that reason, and this case
# passed on a git error while claiming to exercise the empty-scan path. It even
# printed `fatal: not a git repository` and still reported PASS.
#
# Worth naming because mutation testing cannot catch this shape: mutate the
# production code and the test still fails, because it was already failing for
# an unrelated reason that produces the same observable. The assertion was
# right and the FIXTURE was wrong.
#
# So this case now asserts the mechanism, not just the verdict: the run must NOT
# report a git error, and must reach the real "examined 0 files" path.
D2=$(new_repo B)
# The checker must live at <repo>/scripts/ so REPO_ROOT resolves to the FIXTURE
# repo. It is left UNTRACKED, and its library removed, so `git ls-files` -- which
# is tracked-only -- legitimately returns no file containing `git init`. That
# gives a genuine zero-site scan INSIDE a valid repository, and incidentally
# exercises the checker's tracked-only rule.
rm -f "$D2/scripts/lib/git-clean-env.sh"
printf '#!/usr/bin/env bash\necho hi\n' >"$D2/scripts/nothing.sh"
git -C "$D2" add scripts/nothing.sh >/dev/null 2>&1
set +e
OUT=$(cd "$D2" && bash "$D2/scripts/check-git-init-guarded.sh" 2>&1)
RC=$?
set -e
if printf '%s' "$OUT" | grep -qi 'not a git repository'; then
    bad "the fixture ran OUTSIDE a git repository -- this case proves nothing" \
        "REPO_ROOT resolved outside the fixture repo; the empty file list is a" \
        "git failure, not the empty scan this case claims to exercise" "$OUT"
elif [ "$RC" -eq 0 ]; then
    bad "a zero-file scan reported success (exit $RC)" "$OUT"
elif printf '%s' "$OUT" | grep -q 'examined 0 files'; then
    ok "a genuine zero-file scan (inside a repo) exits non-zero and says so"
else
    bad "the scan failed, but not via the examined-0 backstop (exit $RC)" "$OUT"
fi
echo ""

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
# This case RUNS everywhere (Linux CI included), but what it proves depends on
# the host, so it reports the grep it actually exercised rather than claiming
# BSD unconditionally. On Linux /usr/bin/grep is GNU, and there this degrades to
# a plain "still detected with PATH pinned" check -- worth running, but not the
# portability evidence its name suggests.
#
# Even on macOS the evidence is partial: /usr/bin/grep here is "BSD grep, GNU
# compatible" and DOES honor \b, so this pins the CURRENT portable form as
# working rather than proving the GNU form broken. A stricter BSD grep would
# catch that regression; this host cannot.
D=$(new_repo J)
cat >"$D/scripts/fixture.sh" <<'EOF'
#!/usr/bin/env bash
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE
git init -q "$dir"
EOF
git -C "$D" add -A >/dev/null 2>&1
set +e
GREPV=$(PATH=/usr/bin:/bin grep --version 2>/dev/null | head -1)
case "$GREPV" in
    *BSD*) GREPKIND="BSD grep" ;;
    *GNU*) GREPKIND="GNU grep (not BSD -- weaker evidence on this host)" ;;
    *) GREPKIND="grep of unknown flavor" ;;
esac
OUT=$(PATH=/usr/bin:/bin /bin/bash "$D/scripts/check-git-init-guarded.sh" 2>&1)
RC=$?
set -e
if [ "$RC" -eq 0 ]; then
    ok "$GREPKIND: \`unset GIT_DIR\` is still detected as a guard"
else
    bad "PATH-pinned run reported the guarded fixture as unguarded (exit $RC)" \
        "a GNU-only regex construct has crept back in" \
        "grep was: $GREPV" "$OUT"
fi
echo ""

# ---------------------------------------------------------------------------
echo "Case I: --list is non-empty and excludes commented-out mentions"
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
    ok "--list returns $(printf '%s\n' "$LIST" | grep -c .) files containing an apparent \`git init\`"
fi
echo ""

echo "----------------------------------------"
echo "passed: $PASSED  failed: $FAILED  skipped: $SKIPPED"
if [ "$SKIPPED" -gt 0 ]; then
    echo "NOTE: $SKIPPED case(s) did NOT run here -- see the SKIP lines above for"
    echo "      what went unverified on this host. Green with skips is not the"
    echo "      same as green."
fi
[ "$FAILED" -eq 0 ] || exit 1
