#!/usr/bin/env bash
#
# test-remove-worktree.sh -- tests for make remove-worktree target (#3251).
#
# Hermetic. Sets HOME to a temp dir with a stub cleanup-worktree.sh script;
# uses a temp worktrees.md file and a temp worktree directory. Nothing touches
# the developer's real ~/.claude or real worktree structure.
#
# Run: bash scripts/test-remove-worktree.sh
#
# Four cases:
# - Case A: cleanup script fails and leaves the worktree dir intact -> make exits non-zero AND row is preserved
# - Case B: cleanup script succeeds and deletes the worktree dir -> make exits 0 AND row is removed
# - Case C: cleanup script fails BUT deletes the worktree dir -> make exits 0 AND row is removed
# - Case D: override path is stale/nonexistent but real path still exists -> make exits non-zero AND row is preserved

set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

WORK=$(mktemp -d)
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

echo "remove-worktree"
echo

# --------------------------------------------------------------------------
# CASE A: cleanup-worktree.sh exits with failure and leaves the worktree dir.
# make remove-worktree must exit non-zero and PRESERVE the tracker row.
# --------------------------------------------------------------------------

CASE_A_HOME="$WORK/case-a"
mkdir -p "$CASE_A_HOME"/.claude/scripts "$CASE_A_HOME"/repo "$CASE_A_HOME"/stillwater-x

# Stub cleanup script that FAILS and does NOT delete the worktree
cat > "$CASE_A_HOME"/.claude/scripts/cleanup-worktree.sh << 'STUB'
#!/bin/bash
exit 1
STUB
chmod +x "$CASE_A_HOME"/.claude/scripts/cleanup-worktree.sh

# Temp worktrees.md with a row for stillwater-x
cat > "$CASE_A_HOME"/worktrees.md << 'MD'
| Name | Branch | Issue |
| --- | --- | --- |
| stillwater-x | fix/test | #9999 |
MD

CASE_A_WT="$CASE_A_HOME/stillwater-x"
CASE_A_MD="$CASE_A_HOME/worktrees.md"

# Run the make target with custom HOME, WORKTREES_MD, and REMOVE_WORKTREE_TEST_DIR
rc=0
case_a_output=""
case_a_output=$(
  HOME="$CASE_A_HOME" \
  WORKTREES_MD="$CASE_A_MD" \
  REMOVE_WORKTREE_TEST_DIR="$CASE_A_WT" \
  make -C "$REPO_ROOT" remove-worktree NAME=x 2>&1
) || rc=$?

# Exit code must be non-zero
if [ "$rc" -ne 0 ]; then
    ok "case A: make exits non-zero when cleanup fails"
else
    bad "case A: make exits non-zero when cleanup fails" "exit was $rc" "output: $case_a_output"
fi

# Output must contain the specific error message
if printf '%s' "$case_a_output" | grep -q "still present after cleanup-worktree.sh"; then
    ok "case A: error message indicates worktree still present"
else
    bad "case A: error message indicates worktree still present" "output: $case_a_output"
fi

# Worktree dir must still exist
if [ -d "$CASE_A_WT" ]; then
    ok "case A: worktree dir is preserved when cleanup fails"
else
    bad "case A: worktree dir is preserved when cleanup fails" "dir was deleted"
fi

# Tracker row must still exist
if grep -q '^| stillwater-x ' "$CASE_A_MD"; then
    ok "case A: tracker row is preserved when cleanup fails"
else
    bad "case A: tracker row is preserved when cleanup fails" "row was removed"
fi

# --------------------------------------------------------------------------
# CASE B: cleanup-worktree.sh succeeds and deletes the worktree dir.
# make remove-worktree must exit 0 and REMOVE the tracker row.
# --------------------------------------------------------------------------

CASE_B_HOME="$WORK/case-b"
mkdir -p "$CASE_B_HOME"/.claude/scripts "$CASE_B_HOME"/repo "$CASE_B_HOME"/stillwater-y

# Stub cleanup script that SUCCEEDS and DELETES the worktree
# The make target passes REMOVE_WORKTREE_TEST_DIR as an env var; we use it to delete the directory.
cat > "$CASE_B_HOME"/.claude/scripts/cleanup-worktree.sh << 'STUB'
#!/bin/bash
# Test stub: delete the directory pointed to by REMOVE_WORKTREE_TEST_DIR if it exists
if [ -n "${REMOVE_WORKTREE_TEST_DIR:-}" ] && [ -d "$REMOVE_WORKTREE_TEST_DIR" ]; then
  rm -rf "$REMOVE_WORKTREE_TEST_DIR"
fi
exit 0
STUB
chmod +x "$CASE_B_HOME"/.claude/scripts/cleanup-worktree.sh

# Temp worktrees.md with a row for stillwater-y
cat > "$CASE_B_HOME"/worktrees.md << 'MD'
| Name | Branch | Issue |
| --- | --- | --- |
| stillwater-y | fix/test | #8888 |
MD

CASE_B_WT="$CASE_B_HOME/stillwater-y"
CASE_B_MD="$CASE_B_HOME/worktrees.md"

# Run the make target with custom HOME, WORKTREES_MD, and REMOVE_WORKTREE_TEST_DIR
rc=0
HOME="$CASE_B_HOME" \
WORKTREES_MD="$CASE_B_MD" \
REMOVE_WORKTREE_TEST_DIR="$CASE_B_WT" \
make -C "$REPO_ROOT" remove-worktree NAME=y >/dev/null 2>&1 || rc=$?

# Exit code must be zero
if [ "$rc" -eq 0 ]; then
    ok "case B: make exits 0 when cleanup succeeds"
else
    bad "case B: make exits 0 when cleanup succeeds" "exit was $rc"
fi

# Worktree dir must be gone
if [ ! -d "$CASE_B_WT" ]; then
    ok "case B: worktree dir is removed when cleanup succeeds"
else
    bad "case B: worktree dir is removed when cleanup succeeds" "dir still exists"
fi

# Tracker row must be gone
if ! grep -q '^| stillwater-y ' "$CASE_B_MD"; then
    ok "case B: tracker row is removed when cleanup succeeds"
else
    bad "case B: tracker row is removed when cleanup succeeds" "row still exists"
fi

# --------------------------------------------------------------------------
# CASE C: cleanup-worktree.sh exits non-zero BUT deletes the worktree dir.
# make remove-worktree must exit 0 and REMOVE the tracker row.
# (The old warning message exists because cleanup can fail even when the
# worktree is already gone; we decide on the DIRECTORY, not the exit code.)
# --------------------------------------------------------------------------

CASE_C_HOME="$WORK/case-c"
mkdir -p "$CASE_C_HOME"/.claude/scripts "$CASE_C_HOME"/repo "$CASE_C_HOME"/stillwater-z

# Stub cleanup script that FAILS but DELETES the worktree
cat > "$CASE_C_HOME"/.claude/scripts/cleanup-worktree.sh << 'STUB'
#!/bin/bash
# Test stub: delete the directory pointed to by REMOVE_WORKTREE_TEST_DIR and then fail
if [ -n "${REMOVE_WORKTREE_TEST_DIR:-}" ] && [ -d "$REMOVE_WORKTREE_TEST_DIR" ]; then
  rm -rf "$REMOVE_WORKTREE_TEST_DIR"
fi
exit 1
STUB
chmod +x "$CASE_C_HOME"/.claude/scripts/cleanup-worktree.sh

# Temp worktrees.md with a row for stillwater-z
cat > "$CASE_C_HOME"/worktrees.md << 'MD'
| Name | Branch | Issue |
| --- | --- | --- |
| stillwater-z | fix/test | #7777 |
MD

CASE_C_WT="$CASE_C_HOME/stillwater-z"
CASE_C_MD="$CASE_C_HOME/worktrees.md"

# Run the make target with custom HOME, WORKTREES_MD, and REMOVE_WORKTREE_TEST_DIR
rc=0
HOME="$CASE_C_HOME" \
WORKTREES_MD="$CASE_C_MD" \
REMOVE_WORKTREE_TEST_DIR="$CASE_C_WT" \
make -C "$REPO_ROOT" remove-worktree NAME=z >/dev/null 2>&1 || rc=$?

# Exit code must be zero (dir is gone, so we strip the row)
if [ "$rc" -eq 0 ]; then
    ok "case C: make exits 0 when cleanup fails but dir is gone"
else
    bad "case C: make exits 0 when cleanup fails but dir is gone" "exit was $rc"
fi

# Worktree dir must be gone
if [ ! -d "$CASE_C_WT" ]; then
    ok "case C: worktree dir is removed by cleanup"
else
    bad "case C: worktree dir is removed by cleanup" "dir still exists"
fi

# Tracker row must be gone (because the dir is gone, even though cleanup failed)
if ! grep -q '^| stillwater-z ' "$CASE_C_MD"; then
    ok "case C: tracker row is removed when cleanup fails but dir is gone"
else
    bad "case C: tracker row is removed when cleanup fails but dir is gone" "row still exists"
fi

# --------------------------------------------------------------------------
# CASE D: override path is stale/nonexistent, but real path still exists.
# make remove-worktree must exit non-zero and PRESERVE the tracker row.
# (Detects exported stale REMOVE_WORKTREE_TEST_DIR; the target must check
# the real ../stillwater-$(NAME) path in addition to the override.)
# --------------------------------------------------------------------------

# Create a unique real worktree directory as a sibling to REPO_ROOT
UNIQUE_ID="sw-test-$$RANDOM-$$"
REAL_WT_PATH="$REPO_ROOT/../stillwater-$UNIQUE_ID"
# Refuse to run if it already exists (safety check)
if [ -d "$REAL_WT_PATH" ]; then
    echo "ERROR: Real worktree path already exists: $REAL_WT_PATH" >&2
    exit 1
fi
mkdir -p "$REAL_WT_PATH"

CASE_D_HOME="$WORK/case-d"
mkdir -p "$CASE_D_HOME"/.claude/scripts "$CASE_D_HOME"/repo

# Stub cleanup script (returns 0, doesn't delete anything for this case)
cat > "$CASE_D_HOME"/.claude/scripts/cleanup-worktree.sh << 'STUB'
#!/bin/bash
exit 0
STUB
chmod +x "$CASE_D_HOME"/.claude/scripts/cleanup-worktree.sh

# Temp worktrees.md with a row
cat > "$CASE_D_HOME"/worktrees.md << 'MD'
| Name | Branch | Issue |
| --- | --- | --- |
| stillwater-sw-test-dummy | fix/test | #6666 |
MD

CASE_D_MD="$CASE_D_HOME"/worktrees.md

# Override points to a nonexistent temp path, but the REAL path exists
NONEXISTENT_OVERRIDE="$WORK/does-not-exist"

# Run the make target: override is stale, but real path exists
rc=0
HOME="$CASE_D_HOME" \
WORKTREES_MD="$CASE_D_MD" \
REMOVE_WORKTREE_TEST_DIR="$NONEXISTENT_OVERRIDE" \
make -C "$REPO_ROOT" remove-worktree NAME="$UNIQUE_ID" >/dev/null 2>&1 || rc=$?

# Clean up the real worktree directory we created
rm -rf "$REAL_WT_PATH"

# Exit code must be non-zero (real path still exists)
if [ "$rc" -ne 0 ]; then
    ok "case D: make exits non-zero when real path exists despite override override being stale"
else
    bad "case D: make exits non-zero when real path exists despite override being stale" "exit was $rc"
fi

# Tracker row must still exist (real dir exists, so row is kept)
if grep -q '^| stillwater-sw-test-dummy ' "$CASE_D_MD"; then
    ok "case D: tracker row is preserved when real path exists despite override being stale"
else
    bad "case D: tracker row is preserved when real path exists despite override being stale" "row was removed"
fi

echo
echo "passed: $PASSED   failed: $FAILED"
[ "$FAILED" -eq 0 ]
