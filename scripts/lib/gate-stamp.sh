#!/usr/bin/env bash
# scripts/lib/gate-stamp.sh -- the gate's proof-of-pass stamp (#3436), sourced
# by scripts/pre-push-gate.sh (writer), by every child helper that can skip a
# check, and by scripts/gate-receipt-valid.sh (reader), so all share ONE
# definition of "the tree is clean" and ONE way to report a skipped check.
#
# The stamp (<git dir>/pre-push-gate-stamp.json) says: THIS tree, compared
# against THIS base, THIS tip of local main and THIS patch-coverage base,
# passed with THESE RUN_* modes. Every comparison ref a gate step reads is one
# of those fields (the patch-coverage step is handed patch_base explicitly, so
# an inherited BASE cannot change what it compares against). It is written ONLY
# when the run can positively show nothing was skipped for a reason the stamp
# does not record, and the validator refuses a stamp older than
# GATE_STAMP_MAX_AGE_HOURS (gate-receipt-valid.sh), which bounds what cannot be
# listed (tool versions, the vulnerability database, helpers outside the repo):
#   - Skip arms go through gate_skip <class> <line> (the exceptions are listed
#     in scripts/test-gate-receipt-valid.sh). A `deterministic` skip is one the same tree,
#     base and RUN_* modes always produce (no Go changes, file absent from the
#     tree, a RUN_* mode the stamp records). A `blocking` skip depends on the
#     host (a tool missing from PATH, an override variable): it appends a
#     reason line to $GATE_STAMP_BLOCK_FILE, and any line there means no stamp.
#   - The blocker file is a FILE, not a shell variable, because the gate calls
#     helpers as child processes and runs arms in subshells and pipelines, none
#     of which can set a parent's variable. gate_stamp_begin exports the path.
#   - scripts/test-gate-receipt-valid.sh greps the gate, the hook and every
#     helper it calls for skip-style echo/printf/print lines outside gate_skip,
#     so a new arm that PRINTS such a line fails until classified. It is a text
#     heuristic: a skip that prints no skip/warn word, or is built from a
#     variable, is not caught and needs review.
#   - gate_stamp_begin deletes any old stamp and records the tree, clean state,
#     tip of main and patch base at the START; gate_stamp_write needs clean at
#     both ends and the same tree, main and patch base (a run that edited,
#     committed, saw main move or fetched mid-way proves nothing).

# One definition of "dirty" for writer and reader. --untracked-files and
# --ignore-submodules are explicit so a user's status.showUntrackedFiles=no or
# diff.ignoreSubmodules cannot hide a change.
gate_tree_status() {
  git status --porcelain --untracked-files=normal --ignore-submodules=none 2>/dev/null || echo "git-status-failed"
}

# gate_patch_base <commit> -- merge-base of <commit> with the first ref that
# resolves in the ladder origin/main, main, origin/master, master. This is the
# ONE place the ladder lives: the gate passes the result to the patch-coverage
# helper as BASE, and the stamp records it, so they cannot drift apart.
gate_patch_base() {
  local ref
  for ref in origin/main main origin/master master; do
    if git rev-parse --verify -q "$ref" >/dev/null 2>&1; then
      git merge-base "$ref" "$1" 2>/dev/null
      return
    fi
  done
  return 1
}

# Environment variables that can change a check's verdict without changing the
# tree. A KNOWN LIST, not a proof: any of these set non-empty at gate start
# withholds the stamp (the gate itself still runs normally). Format
# name-or-glob|reason. Not listed on purpose: COVER_OUT, PATCH_COVERAGE_THRESHOLD
# and PATCH_COVERAGE_EXCLUDE (the gate sets them inline for the helper),
# PATH/HOME/proxy and CACHE-LOCATION variables. GOLANGCI_LINT_CACHE in particular is
# exported by scripts/lib/run-paths.sh (which developers and the gate source), so
# listing it made every such shell stampless (#3436 incident); golangci-lint has no
# documented verdict-changing environment variable, so no lint entry is listed. An
# entry the repo's own tooling sets must never be added here.
# Built from: grep for ${VAR reads in scripts/pre-push-gate.sh and the helpers it
# runs, the helper's own reads (patch-coverage.sh), tests/a11y process.env reads,
# and the Go/Node variables that select or alter what compiles, lints or runs.
GATE_ENV_DENY='
BASE|an inherited BASE once reached the patch-coverage helper (now passed explicitly)
SKIP_*|any SKIP_ override (SKIP_OPENAPI_BREAKING skips a check)
GOFLAGS|can add -run, -tags or -count to every go command
GOTOOLCHAIN|selects the Go toolchain that builds and tests
GOEXPERIMENT|changes what the compiler builds
GOOS|builds and tests another platform
GOARCH|builds and tests another architecture
CGO_ENABLED|changes which files compile
GOWORK|swaps the module set
NODE_OPTIONS|changes how node-based checks run
PATCH_COVERAGE_ALLOW_DIRTY|patch-coverage.sh tolerates a dirty tree with it
SW_TEST_URL|points the a11y specs at another server
SW_BINARY|points the provider smoke at another binary
SW_BASE|points the provider smoke at another server
SW_PORT|the provider smoke and the a11y target health-check this port; another server may hold it
'
# gate_env_hits -- print "NAME|reason" once per listed variable that is set and
# non-empty. Shared by the stamp writer and the receipt validator.
gate_env_hits() {
  local name pat reason
  for name in $(compgen -e); do
    while IFS='|' read -r pat reason; do
      [ -n "$pat" ] || continue
      # shellcheck disable=SC2254  # the list holds glob patterns on purpose
      case "$name" in $pat) ;; *) continue ;; esac
      [ -n "${!name:-}" ] || continue
      printf '%s|%s\n' "$name" "$reason"
    done <<<"$GATE_ENV_DENY"
  done
}
# gate_env_blockers -- block the stamp once per listed variable that is set.
gate_env_blockers() {
  local name reason
  while IFS='|' read -r name reason; do
    [ -n "$name" ] || continue
    gate_skip blocking "NOTE: no gate stamp will be written: $name is set ($reason)"
  done < <(gate_env_hits)
}

gate_stamp_begin() {
  local gd
  # GATE_STAMP_DIR is for self-tests that start the real gate (they must not
  # delete the worktree's real stamp); the validator never reads it.
  gd="${GATE_STAMP_DIR:-$(git rev-parse --absolute-git-dir 2>/dev/null || true)}"
  GATE_STAMP_FILE="$gd/pre-push-gate-stamp.json"
  rm -f "$GATE_STAMP_FILE" 2>/dev/null || true
  # A blocker file that cannot be created is itself a refusal at write time.
  GATE_STAMP_BLOCK_FILE="$(mktemp "$gd/pre-push-gate-blockers.XXXXXX" 2>/dev/null || true)"
  export GATE_STAMP_BLOCK_FILE
  GATE_STAMP_START_TREE="$(git rev-parse 'HEAD^{tree}' 2>/dev/null || true)"
  GATE_STAMP_START_STATUS="$(gate_tree_status)"
  GATE_STAMP_START_MAIN="$(git rev-parse --verify -q 'main^{commit}' 2>/dev/null || echo none)"
  GATE_STAMP_START_PBASE="$(gate_patch_base HEAD || echo none)"
  gate_env_blockers
}

# gate_stamp_block <reason> -- a check was skipped or weakened; no stamp.
# Safe from a child process, a subshell or a pipeline: it appends to a file.
gate_stamp_block() {
  [ -n "${GATE_STAMP_BLOCK_FILE:-}" ] || return 0
  # A failed append must not lose the blocker: remove the file so the write
  # refuses ("unavailable") instead of reading an empty one as "nothing skipped".
  printf '%s\n' "$1" >>"$GATE_STAMP_BLOCK_FILE" 2>/dev/null || rm -f "$GATE_STAMP_BLOCK_FILE" 2>/dev/null || true
}

# gate_skip <deterministic|blocking> <line> -- print the skip line and classify
# it. An unknown class counts as blocking (fail closed).
gate_skip() {
  echo "$2"
  case "$1" in
    deterministic) ;;
    blocking) gate_stamp_block "$2" ;;
    *) gate_stamp_block "unclassified skip: $2" ;;
  esac
}

# gate_stamp_write <base> <race> <vuln> <provider_smoke> <a11y>
gate_stamp_write() {
  local base="$1" why="" end_tree base_oid main_tip pbase blockers=""
  end_tree="$(git rev-parse 'HEAD^{tree}' 2>/dev/null || true)"
  base_oid="$(git rev-parse --verify -q "$base^{commit}" 2>/dev/null || true)"
  main_tip="$(git rev-parse --verify -q 'main^{commit}' 2>/dev/null || echo none)"
  pbase="$(gate_patch_base HEAD || echo none)"
  if [ -z "${GATE_STAMP_BLOCK_FILE:-}" ] || [ ! -r "$GATE_STAMP_BLOCK_FILE" ]; then
    why="the blocker file is unavailable, so skipped checks cannot be ruled out"
  else
    blockers="$(tr '\n' ';' <"$GATE_STAMP_BLOCK_FILE")"
    rm -f "$GATE_STAMP_BLOCK_FILE" 2>/dev/null || true
  fi
  if [ -n "$why" ]; then :
  elif [ -n "$blockers" ]; then why="a check was skipped or weakened: $blockers"
  elif [ -n "$GATE_STAMP_START_STATUS" ]; then why="the tree was dirty when the gate started"
  elif [ -n "$(gate_tree_status)" ]; then why="the tree was dirty when the gate finished"
  elif [ -z "$GATE_STAMP_START_TREE" ] || [ "$end_tree" != "$GATE_STAMP_START_TREE" ]; then why="HEAD's tree changed during the run"
  elif [ "$main_tip" != "$GATE_STAMP_START_MAIN" ]; then why="main moved during the run"
  elif [ "$pbase" = none ] || [ "$pbase" != "$GATE_STAMP_START_PBASE" ]; then why="the patch-coverage base moved or did not resolve during the run"
  elif [ -z "$base_oid" ]; then why="the base did not resolve"
  fi
  if [ -n "$why" ]; then
    echo "NOTE: no gate stamp written ($why); the pre-push hook will run the gate itself"
    return 0
  fi
  printf '{"schema":"pre-push-gate-stamp/v1","tree":"%s","base":"%s","main":"%s","patch_base":"%s","created":%s,"modes":{"race":"%s","vuln":"%s","provider_smoke":"%s","a11y":"%s"}}\n' \
    "$end_tree" "$base_oid" "$main_tip" "$pbase" "$(date +%s)" "$2" "$3" "$4" "$5" \
    > "$GATE_STAMP_FILE.tmp.$$" 2>/dev/null && mv "$GATE_STAMP_FILE.tmp.$$" "$GATE_STAMP_FILE" 2>/dev/null \
    || echo "NOTE: could not write the gate stamp; the pre-push hook will run the gate itself"
}
