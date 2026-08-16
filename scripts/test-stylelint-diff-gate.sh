#!/usr/bin/env bash
#
# test-stylelint-diff-gate.sh -- tests for scripts/stylelint-diff-gate.sh, the
# diff-scoped stylelint ratchet (#2402), specifically the "nothing to check"
# vs "cannot check" distinction added to fix a fresh-worktree false failure.
#
# A fresh worktree created by `make worktree` has no node_modules. Before this
# fix, running the pre-push gate there failed the CSS-lint step even on a
# branch whose diff touched zero CSS files, because the stylelint/jq
# precondition ran before anything looked at the diff. That is "nothing to
# check" (there is no CSS to lint) being treated the same as "cannot check"
# (there is CSS to lint but the tool is missing) -- a bug either way it goes:
# too strict when there is no CSS, and it would be a silent-failure guard if
# "fixed" by skipping unconditionally whenever the tool happens to be absent.
#
# Hermetic. Every case runs in a throwaway git repository under a temp dir and
# never touches the real project tree. Case 3 additionally needs a real
# stylelint install to prove the ratchet still catches a violation when the
# tool IS present; it uses a scratch install under /tmp, never the repo's own
# node_modules (whose absence is the very thing under test).
#
# Run: bash scripts/test-stylelint-diff-gate.sh

set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
GATE_SCRIPT_SRC="$REPO_ROOT/scripts/stylelint-diff-gate.sh"
# Strip the inherited git environment before any fixture is built: `git init`
# honours a hook-supplied GIT_DIR and would write into the MAIN repository's
# shared config instead (#3051; see the library header for the mechanism).
# shellcheck source=scripts/lib/git-clean-env.sh
. "$REPO_ROOT/scripts/lib/git-clean-env.sh"
git_clean_env_unset

WORK=$(mktemp -d)
# Resolve to the physical path (macOS /tmp is a symlink to /private/tmp).
# stylelint reports each violation's "source" as the physical path it
# actually opened; if WORK still pointed through the symlink, REPO_ROOT
# (computed the same way inside stylelint-diff-gate.sh) would not match it
# and the ltrimstr() in the gate script would silently no-op, leaving every
# violation's location as an unstripped absolute path that can never match
# the relative "file:line" entries in $ADDED_LINES. That is a test-fixture
# artifact of this machine, not the defect under test, so resolve it away
# rather than let case T3 fail for the wrong reason.
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

require() {
    if ! eval "$1"; then
        bad "PRECONDITION FAILED: $1" "${@:2}"
        return 1
    fi
    return 0
}

# new_repo <name> -- a throwaway repo laid out the way the gate script expects
# (it locates itself via `dirname "$0"` then goes up one level for REPO_ROOT,
# so the script must live at <repo>/scripts/stylelint-diff-gate.sh).
new_repo() {
    local name="$1"
    local dir="$WORK/$name"
    mkdir -p "$dir/scripts" "$dir/web/static/css"
    cp "$GATE_SCRIPT_SRC" "$dir/scripts/stylelint-diff-gate.sh"
    chmod +x "$dir/scripts/stylelint-diff-gate.sh"
    git init -q "$dir"
    git -C "$dir" config user.name "Test"
    git -C "$dir" config user.email "test@localhost"
    printf '%s\n' "$dir"
}

# run_gate <dir> <base> -- runs the gate exactly as pre-push-gate.sh does,
# with no node_modules of its own (that absence is the fixture, matching a
# fresh `make worktree`). Prints combined output; returns the exit code.
run_gate() {
    local dir="$1"
    local base="$2"
    (cd "$dir" && env -u PATH PATH="$PATH" bash scripts/stylelint-diff-gate.sh "$base" 2>&1)
}

echo "Control: a fresh fixture repo has no node_modules of its own"
CTRL=$(new_repo control)
if require "[ ! -d '$CTRL/node_modules' ]" "fixture must start with no node_modules"; then
    ok "fixture has no node_modules (matches a fresh worktree)"
fi

# --------------------------------------------------------------------------
# Case T1: diff touches NO CSS, stylelint ABSENT -> exit 0, skip reported.
# --------------------------------------------------------------------------
# This is the regression being fixed: a pure-Go branch in a fresh worktree
# must not fail a check that has nothing to inspect, regardless of whether
# the tool happens to be installed.
echo
echo "T1: diff touches no CSS, stylelint absent -> skip, exit 0"
R1=$(new_repo t1-no-css)
echo "initial" > "$R1/README.md"
git -C "$R1" add README.md
git -C "$R1" commit -q -m "baseline"
BASE1=$(git -C "$R1" rev-parse HEAD)
echo "package main" > "$R1/main.go"
git -C "$R1" add main.go
git -C "$R1" commit -q -m "add go file, no css"
CHANGED1=$(git -C "$R1" diff --name-only "$BASE1" HEAD -- 'web/static/css/*.css')
if require "[ -z '$CHANGED1' ]" "fixture diff must genuinely touch zero CSS files"; then
    if OUT1=$(run_gate "$R1" "$BASE1"); then
        if grep -qi 'skip' <<< "$OUT1"; then
            ok "exit 0 and output reports the skip"
        else
            bad "exit 0 but output does not mention a skip" "$OUT1"
        fi
    else
        bad "REGRESSION: failed on a diff with zero CSS files, in a worktree with no stylelint" "$OUT1"
    fi
fi

# --------------------------------------------------------------------------
# Case T2: diff touches CSS, stylelint ABSENT -> non-zero exit, loud failure.
# --------------------------------------------------------------------------
# The anti-silent-failure guard: there IS something to check here, and the
# tool to check it is missing, so this must fail loudly, not skip.
echo
echo "T2: diff touches CSS, stylelint absent -> fail loudly, non-zero exit"
R2=$(new_repo t2-css-no-tool)
echo "initial" > "$R2/README.md"
git -C "$R2" add README.md
git -C "$R2" commit -q -m "baseline"
BASE2=$(git -C "$R2" rev-parse HEAD)
cat > "$R2/web/static/css/new.css" << 'EOF'
.foo {
  color: red;
}
EOF
git -C "$R2" add web/static/css/new.css
git -C "$R2" commit -q -m "add css, no stylelint installed"
CHANGED2=$(git -C "$R2" diff --name-only "$BASE2" HEAD -- 'web/static/css/*.css')
if require "[ -n '$CHANGED2' ]" "fixture diff must genuinely touch a CSS file"; then
    if OUT2=$(run_gate "$R2" "$BASE2"); then
        bad "PASSED although the diff touches CSS and stylelint is not installed" \
            "This is the silent-failure-guard shape this fix must not introduce." "$OUT2"
    else
        if grep -qi 'stylelint' <<< "$OUT2" && grep -qi 'npm ci' <<< "$OUT2"; then
            ok "blocked, naming stylelint and the npm ci remedy"
        else
            bad "blocked, but the message is not the actionable stylelint-missing message" "$OUT2"
        fi
    fi
fi

# Mutation proof: an unconditional skip (deleting the CSS-touched check) must
# make T2 fail. If it doesn't, T2 is not actually pinning the distinction
# between "nothing to check" and "cannot check".
echo
echo "T2-mutation: an unconditional skip must break T2"
MUT_SCRIPT="$WORK/mutant-stylelint-diff-gate.sh"
cp "$GATE_SCRIPT_SRC" "$MUT_SCRIPT"
# Force HAS_ADDED_LINES to always read as 0, regardless of the actual diff --
# this is the unconditional-skip mutant: "nothing to check" is asserted no
# matter what the diff contains.
sed -i.bak 's/^HAS_ADDED_LINES=1$/HAS_ADDED_LINES=1; HAS_ADDED_LINES=0 #MUTANT/' "$MUT_SCRIPT"
if require "grep -q '#MUTANT' '$MUT_SCRIPT'" "mutation must have applied"; then
    R2M=$(new_repo t2-mutant)
    cp "$MUT_SCRIPT" "$R2M/scripts/stylelint-diff-gate.sh"
    cat > "$R2M/web/static/css/new.css" << 'EOF'
.foo {
  color: red;
}
EOF
    git -C "$R2M" add web/static/css/new.css
    git -C "$R2M" commit -q -m "baseline with css already present"
    BASE2M=$(git -C "$R2M" rev-parse HEAD)
    printf '\n.bar {\n  color: blue;\n}\n' >> "$R2M/web/static/css/new.css"
    git -C "$R2M" add web/static/css/new.css
    git -C "$R2M" commit -q -m "add more css, mutant build"
    if OUT2M=$(run_gate "$R2M" "$BASE2M"); then
        bad "mutant (unconditional skip) still passed T2's scenario -- T2 does not pin the guard" "$OUT2M"
    else
        ok "mutant (unconditional skip) correctly fails T2's scenario -- guard is pinned"
    fi
fi

# --------------------------------------------------------------------------
# Case T3: diff touches CSS, stylelint PRESENT -> ratchet still runs and
# still catches a violation on an added line.
# --------------------------------------------------------------------------
echo
echo "T3: diff touches CSS, stylelint present -> ratchet still catches a violation"
STYLELINT_SCRATCH="/tmp/w6-blockers/stylelint-probe"
if require "[ -d '$STYLELINT_SCRATCH/node_modules/stylelint' ]" \
    "a scratch stylelint install must exist at $STYLELINT_SCRATCH (installed once, outside any worktree, purely to prove the present-tool path; never the project's own node_modules)"; then
    R3=$(new_repo t3-css-with-tool)
    ln -s "$STYLELINT_SCRATCH/node_modules" "$R3/node_modules"
    cat > "$R3/.stylelintrc.json" << 'EOF'
{
  "rules": {
    "block-no-empty": true
  }
}
EOF
    cat > "$R3/web/static/css/clean.css" << 'EOF'
.clean {
  color: blue;
}
EOF
    git -C "$R3" add .stylelintrc.json web/static/css/clean.css
    git -C "$R3" commit -q -m "baseline, clean css, config present"
    BASE3=$(git -C "$R3" rev-parse HEAD)
    cat >> "$R3/web/static/css/clean.css" << 'EOF'

.violation {}
EOF
    git -C "$R3" add web/static/css/clean.css
    git -C "$R3" commit -q -m "add an empty-block violation on a new line"
    if OUT3=$(run_gate "$R3" "$BASE3"); then
        bad "PASSED despite a genuine block-no-empty violation on an added line" "$OUT3"
    else
        if grep -qi 'block-no-empty\|new stylelint violation' <<< "$OUT3"; then
            ok "ratchet still runs with stylelint present and catches the new violation"
        else
            bad "blocked, but not clearly by the ratchet catching the violation" "$OUT3"
        fi
    fi
fi

# --------------------------------------------------------------------------
echo
echo "----------------------------------------"
echo "passed: $PASSED  failed: $FAILED"
if [ "$FAILED" -ne 0 ]; then
    exit 1
fi
echo "All stylelint-diff-gate checks behaved as specified."
