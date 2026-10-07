#!/usr/bin/env bash
# gate-receipt-valid.sh -- decide whether the pre-push hook may SKIP the gate
# because a receipt already proves the same gate passed for what is pushed
# (#3436). Called by .githooks/pre-push with the hook's stdin lines
# (`<local ref> <local sha> <remote ref> <remote sha>`) on this script's stdin.
#
# Exit 0  = honored (skip the gate; one line on stdout says why it is safe)
# Exit 1  = not honored (run the gate; one line on stdout says why, when a
#           receipt exists)
# Exit 64 = invalid input (a bad PRE_PUSH_FORCE_GATE or RUN_* value), refused
#           the same way the gate refuses it. 64 is a code bash never emits
#           itself (a syntax error is 2, a signal 128+n), so the hook can tell
#           "refused" from "this script crashed". ANY other status, a crash
#           included, makes the hook run the gate: never a skip, never a block.
#
# FAIL OPEN TO THE GATE, NEVER TO A SKIP: every doubt (missing, unreadable,
# malformed or mismatched receipt or stamp, no jq, a git error) exits 1.
#
# Two files in `$(git rev-parse --absolute-git-dir)` carry the proof:
#   prep-pr-receipt.json       written by the orchestrate gate runner:
#                              schema gate-receipt/v1, result, tree_sha.
#   pre-push-gate-stamp.json   written by scripts/pre-push-gate.sh when it
#                              passes without skipping or weakening any check
#                              (see scripts/lib/gate-stamp.sh): the tree it
#                              gated, the BASE, the tip of local main and the
#                              patch-coverage base it compared against, its
#                              RUN_* modes, and a `created` epoch.
#
# A skip needs ALL of:
#   1. receipt parses, schema gate-receipt/v1, producer gate-runner, result pass;
#   2. receipt tree_sha == the tree of EVERY commit being pushed. Each ref line
#      must be a branch (or HEAD) push of a real commit, or a full 40/64-hex
#      object id equal to the line's sha pushed to a refs/heads/* destination
#      (safe-push's form); a delete (all-zero
#      sha), a tag push, or no stdin at all means run the gate. Multiple refs
#      are allowed only when every one has that same tree (simplest correct
#      option: nothing is skipped on a partial match);
#   3. the working tree is clean (untracked files count as dirty, whatever the
#      user's status.showUntrackedFiles says; the definition is shared with the
#      stamp writer, scripts/lib/gate-stamp.sh);
#   4. the stamp parses, is schema pre-push-gate-stamp/v1, and its tree equals
#      the receipt tree; its base equals `git merge-base main <each pushed
#      commit>` now (rebasing changes what lint compared against); its
#      patch_base equals gate_patch_base of each pushed commit (the base the
#      patch-coverage helper resolves for itself: it prefers origin/main, so a
#      fetch plus rebase moves it even when local main did not); its main
#      equals the current tip of local main (one gate step diffs against it, so
#      any move of main re-runs the gate); and its RUN_* modes
#      are at least as strict as this run's own resolved modes (a gate run
#      with RUN_RACE=0 skipped the tests and never satisfies the default hook
#      run).
# Because the match is on the TREE, "the gate script or this script changed
# since the receipt" needs no separate rule: those files are part of the tree,
# so editing one changes the tree and voids the receipt. A squash or reword of
# an already-gated tree keeps its tree and is therefore honored, by design.
#
# PRE_PUSH_FORCE_GATE=1 forces the gate; an unrecognized value is refused.
#   5. the stamp is at most GATE_STAMP_MAX_AGE_HOURS old (its `created` epoch
#      must be numeric and not in the future by more than a few minutes). This
#      bounds what no list can cover: tool versions, the out-of-repo
#      patch-coverage helper, the vulnerability database.
set -euo pipefail

GATE_STAMP_MAX_AGE_HOURS=4
GATE_STAMP_FUTURE_SLACK_SECONDS=300

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=scripts/lib/run-flags.sh
. "$SCRIPT_DIR/lib/run-flags.sh"
# shellcheck source=scripts/lib/gate-stamp.sh
. "$SCRIPT_DIR/lib/gate-stamp.sh"

# resolve_flag <NAME> <raw>: resolve_run_flag exits 2 on a bad value, which is
# also bash's own syntax-error status, so probe it in a subshell. ONLY its
# refusal status (2) becomes the dedicated code 64 (message on stderr); any
# other failure (function undefined: 127, a crash) exits 1 and runs the gate.
resolve_flag() {
  local probe=0
  (resolve_run_flag "$1" "$2") >/dev/null || probe=$?
  case "$probe" in
    0) ;;
    2) exit 64 ;;
    *) exit 1 ;;
  esac
  resolve_run_flag "$1" "$2"
}

resolve_flag PRE_PUSH_FORCE_GATE "${PRE_PUSH_FORCE_GATE:-}"; FORCE="$RESOLVED_RUN_FLAG"
resolve_flag RUN_RACE "${RUN_RACE:-}"; RACE_MODE="$RESOLVED_RUN_FLAG"
resolve_flag RUN_VULN "${RUN_VULN:-}"; VULN_MODE="$RESOLVED_RUN_FLAG"
resolve_flag RUN_PROVIDER_SMOKE "${RUN_PROVIDER_SMOKE:-}"; PROVIDER_MODE="$RESOLVED_RUN_FLAG"
resolve_flag RUN_A11Y "${RUN_A11Y:-}"; A11Y_MODE="$RESOLVED_RUN_FLAG"

GIT_DIR_ABS=$(git rev-parse --absolute-git-dir 2>/dev/null) || exit 1
RECEIPT="$GIT_DIR_ABS/prep-pr-receipt.json"
STAMP="$GIT_DIR_ABS/pre-push-gate-stamp.json"

# refuse <reason> -- the one line explaining why, then "not honored".
refuse() {
  echo "pre-push: gate receipt not honored ($1); running the gate"
  exit 1
}

if [ "$FORCE" = "on" ]; then
  echo "pre-push: PRE_PUSH_FORCE_GATE set; running the gate"
  exit 1
fi

# No receipt at all is the ordinary case (nothing to explain): stay quiet.
[ -f "$RECEIPT" ] || exit 1
command -v jq >/dev/null 2>&1 || refuse "jq not installed"

r_schema=$(jq -er '.schema' "$RECEIPT" 2>/dev/null) || refuse "receipt unreadable or malformed"
[ "$r_schema" = "gate-receipt/v1" ] || refuse "receipt schema is '$r_schema'"
r_producer=$(jq -er '.producer' "$RECEIPT" 2>/dev/null) || refuse "receipt has no producer"
[ "$r_producer" = "gate-runner" ] || refuse "receipt producer is '$r_producer', not gate-runner"
r_result=$(jq -er '.result' "$RECEIPT" 2>/dev/null) || refuse "receipt has no result"
[ "$r_result" = "pass" ] || refuse "receipt result is '$r_result', not pass"
r_tree=$(jq -er '.tree_sha' "$RECEIPT" 2>/dev/null) || refuse "receipt has no tree_sha"
r_commit=$(jq -er '.commit_sha' "$RECEIPT" 2>/dev/null) || refuse "receipt has no commit_sha"

# Untracked counts as dirty; the receipt and stamp live in the git dir, so
# they are not in this listing.
[ -z "$(gate_tree_status)" ] || refuse "working tree is dirty"

[ -f "$STAMP" ] || refuse "no gate stamp from a passing gate run"
s_schema=$(jq -er '.schema' "$STAMP" 2>/dev/null) || refuse "gate stamp unreadable or malformed"
[ "$s_schema" = "pre-push-gate-stamp/v1" ] || refuse "gate stamp schema is '$s_schema'"
s_tree=$(jq -er '.tree' "$STAMP" 2>/dev/null) || refuse "gate stamp has no tree"
[ "$s_tree" = "$r_tree" ] || refuse "gate stamp tree ${s_tree:0:12} differs from the receipt tree"
s_base=$(jq -er '.base' "$STAMP" 2>/dev/null) || refuse "gate stamp has no base"
s_main=$(jq -er '.main' "$STAMP" 2>/dev/null) || refuse "gate stamp has no main tip"
s_created=$(jq -er '.created' "$STAMP" 2>/dev/null) || refuse "gate stamp has no created time"
case "$s_created" in "" | *[!0-9]*) refuse "gate stamp created time is not a number" ;; esac
now_epoch=$(date +%s)
[ "$s_created" -le $((now_epoch + GATE_STAMP_FUTURE_SLACK_SECONDS)) ] || refuse "gate stamp is dated in the future"
[ $((now_epoch - s_created)) -le $((GATE_STAMP_MAX_AGE_HOURS * 3600)) ] || refuse "gate stamp is older than ${GATE_STAMP_MAX_AGE_HOURS} hours"
s_pbase=$(jq -er '.patch_base' "$STAMP" 2>/dev/null) || refuse "gate stamp has no patch_base"
now_main=$(git rev-parse --verify -q 'main^{commit}' 2>/dev/null || echo none)
[ "$s_main" = "$now_main" ] || refuse "main moved since the gate ran"

# Every pushed ref must be a branch/HEAD push of a commit whose tree matches
# the receipt and whose merge-base with main is the base the gate compared.
seen=0
p_commit=""
while read -r lref lsha rref _rsha; do
  [ -n "${lref:-}" ] || continue
  seen=$((seen + 1))
  case "$lref" in
    refs/heads/* | HEAD) ;;
    # safe-push pushes `<full object id>:refs/heads/<b>`, so git hands the hook the
    # id as the local ref. Honored only when it is a full id equal to the sha
    # field and the destination is a branch.
    *[!0-9a-f]* | "") refuse "pushed ref '$lref' is not a branch or a full object id" ;;
    *)
      case "${#lref}:$rref" in
        40:refs/heads/* | 64:refs/heads/*) [ "$lref" = "${lsha:-}" ] || refuse "pushed object id '$lref' differs from the sha field" ;;
        *) refuse "pushed ref '$lref' is not a branch, or an object id pushed to a non-branch ref" ;;
      esac
      ;;
  esac
  case "${lsha:-}" in
    "" | *[!0]*) ;;
    *) refuse "push deletes a ref" ;;
  esac
  p_tree=$(git rev-parse --verify -q "$lsha^{commit}^{tree}" 2>/dev/null) \
    || refuse "cannot resolve pushed commit '$lsha'"
  [ "$p_tree" = "$r_tree" ] \
    || refuse "receipt tree ${r_tree:0:12} is not the pushed tree ${p_tree:0:12}"
  # The gate computes its base from HEAD; here it comes from the pushed commit.
  now_base=$(git merge-base main "$lsha" 2>/dev/null || echo "$lsha~1")
  now_base=$(git rev-parse --verify -q "$now_base^{commit}" 2>/dev/null) || refuse "cannot resolve the base of '$lsha'"
  [ "$s_base" = "$now_base" ] || refuse "base moved since the gate ran (${s_base:0:12} -> ${now_base:0:12})"
  now_pbase=$(gate_patch_base "$lsha" || echo none)
  [ "$s_pbase" = "$now_pbase" ] || refuse "patch-coverage base moved since the gate ran (${s_pbase:0:12} -> ${now_pbase:0:12})"
  p_commit=$lsha
done
[ "$seen" -gt 0 ] || refuse "no pushed refs on stdin"

# rank <flag> <mode>: higher is stricter. RUN_RACE's default still runs the
# changed-package tests, so default outranks off; for the other tiers default
# and off are both a skip.
rank() {
  case "$2" in
    on) echo 2 ;;
    default) if [ "$1" = race ]; then echo 1; else echo 0; fi ;;
    *) echo 0 ;;
  esac
}
for pair in "race:$RACE_MODE" "vuln:$VULN_MODE" "provider_smoke:$PROVIDER_MODE" "a11y:$A11Y_MODE"; do
  flag=${pair%%:*}
  want=${pair#*:}
  have=$(jq -er --arg f "$flag" '.modes[$f]' "$STAMP" 2>/dev/null) || refuse "gate stamp has no mode for $flag"
  [ "$(rank "$flag" "$have")" -ge "$(rank "$flag" "$want")" ] \
    || refuse "gate ran with $flag=$have, this push needs $flag=$want"
done

mtime=$(stat -c %Y "$RECEIPT" 2>/dev/null || stat -f %m "$RECEIPT" 2>/dev/null || echo 0)
age=$(($(date +%s) - mtime))
echo "pre-push: skipping the gate; a passing receipt covers pushed commit ${p_commit:0:12}, tree ${r_tree:0:12} (receipt commit ${r_commit:0:12}, ${age}s old). PRE_PUSH_FORCE_GATE=1 forces the gate."
exit 0
