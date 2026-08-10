#!/bin/bash
# pre-push-gate.sh -- deterministic pre-push checks; run before code review
#
# WHAT THIS GATE IS FOR (#2983)
# -----------------------------
# The default ("auto") path -- no RUN_* flags set -- contains ONLY checks that
# are fast AND blocking. Two rules define it, and neither is negotiable:
#
#   1. NO ADVISORY STEP IN THE AUTO PATH. Every step either BLOCKS the push or
#      is not in the default path at all. A step that runs, fails, and then
#      lets the gate print "All hard checks passed" is worse than an absent
#      step: it costs wall-clock time and then teaches the reader to distrust
#      the banner. Before #2983 the a11y tier, govulncheck, the provider
#      failure smoke, and an ordinary test-assertion failure were all advisory
#      here, and the repo instructions carried a warning label explaining how
#      to read the gate's own success message. That warning label was the
#      defect. `scripts/check-gate-invariant.sh` (run near the top of this
#      gate) mechanizes the rule so it cannot silently regress.
#
#   2. A CHECK IS ONLY ELIGIBLE TO LEAVE IF A *REQUIRED* CI CHECK COVERS IT.
#      "There is a CI job that does something similar" is not enough -- a job
#      that is not in the `Protect main` ruleset can go red and the PR merges
#      anyway (#2503). The checks that remain below are here either because
#      they are cheap and blocking, or because CI does NOT require an
#      equivalent (patch coverage, the codecov/floor mirror, fuzz-matrix
#      drift, prefs-coverage, the OpenAPI breaking-change diff, the raw-error
#      leak sweep) and dropping them would move a defect all the way to a
#      human reviewer.
#
# The expensive integration-shaped tiers keep their code here but default to
# SKIP, each with a blocking opt-in:
#
#   RUN_A11Y=1            accessibility (axe-core)   CI: "A11y Smoke Tests (Playwright + axe-core)" (ci.yml)
#   RUN_PROVIDER_SMOKE=1  provider failure smoke     CI: "Provider Failure Smoke" (gate.yml)
#   RUN_VULN=1            govulncheck                CI: "Go Vulnerability Check" (security.yml)
#   RUN_RACE=1            full -race suite           CI: "Test" (ci.yml)
#
# Bruno route parity left this gate entirely; CI's required "Bruno Route
# Parity" job (gate.yml) owns it. scripts/check-bruno-parity.sh is retained
# because that job invokes it.
#
# Exit status:
#   0 = all hard checks passed
#   1 = a hard check failed (test, lint, openapi, etc.)
#   2 = invalid input / setup state (e.g. BASE rev cannot be resolved by
#       `git rev-parse --verify -q "$BASE^{commit}"` -- see the BASE guard
#       directly below)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
BASE=$(git merge-base main HEAD 2>/dev/null || echo "HEAD~1")

# Validate BASE resolves to a real commit so downstream steps that pass it to
# git diff / golangci-lint --new-from-rev fail loudly instead of silently
# degrading to "no diff -> nothing to check -> pass" (the silent-degradation
# class documented in reference_pre_push_gate_hardening.md).
if ! git rev-parse --verify -q "$BASE^{commit}" >/dev/null; then
  echo "FAIL: cannot resolve BASE ('$BASE') to a commit; aborting gate" >&2
  exit 2
fi

# Source the per-worktree run-path helper. Provides $SW_RUN_DIR keyed by the
# worktree basename so concurrent gate runs in different worktrees write to
# disjoint paths and can never clobber each other's coverage profiles. See
# scripts/lib/run-paths.sh for the full rationale.
. "$SCRIPT_DIR/lib/run-paths.sh"

# Acquire an exclusive lock on this worktree's run-dir. Two gate invocations
# in the same worktree both write to $SW_RUN_DIR/cover.out; without the lock,
# whichever finishes last leaves a truncated profile and the patch-coverage
# step then fails with "profile not found or empty". `mkdir` is atomic, so
# the first caller wins; the loser exits with a clear pointer at the live
# pid. Stale lock recovery: if the recorded pid no longer exists (gate was
# killed, terminal was closed, machine rebooted), the lock is cleared and
# re-acquired once. Lives in $SW_RUN_DIR so it cleans up when callers want
# a fresh slate via `rm -rf $SW_RUN_DIR`.
LOCK_DIR="$SW_RUN_DIR/.gate-lock"
# Grace window before an empty/malformed pid file counts as stale. The only
# legitimate way to observe an empty $LOCK_DIR/pid is by racing into the
# window between `mkdir "$LOCK_DIR"` and `echo $$ > $LOCK_DIR/pid`, which
# lasts microseconds. A few seconds of grace covers any plausible scheduler
# delay; a previous run that crashed between those two lines recovers after
# the window elapses. Kept small so a legitimately-killed gate is recovered
# quickly on the next attempt.
LOCK_INIT_GRACE_SECONDS=5
acquire_lock() {
  if mkdir "$LOCK_DIR" 2>/dev/null; then
    echo "$$" > "$LOCK_DIR/pid"
    return 0
  fi
  local holder stale=0
  holder=$(cat "$LOCK_DIR/pid" 2>/dev/null || true)
  # `stat -c` is GNU (Linux CI); `stat -f` is BSD (macOS dev). Fall back to
  # epoch=0 (=> "old enough to be stale") only if both fail, so a missing
  # stat does not leave the lock un-recoverable.
  local now lock_mtime lock_age
  now=$(date +%s)
  lock_mtime=$(stat -c %Y "$LOCK_DIR" 2>/dev/null \
            || stat -f %m "$LOCK_DIR" 2>/dev/null \
            || echo 0)
  lock_age=$(( now - lock_mtime ))
  # A lock is stale when the recorded pid is missing/malformed (race between
  # `mkdir` and `echo $$ > pid`, or the previous run crashed before writing
  # pid), or when the recorded pid is no longer alive. The age gate avoids
  # the TOCTOU window where a racer reads empty pid right after another
  # caller's mkdir and clobbers a live lock; only treat missing/malformed
  # pid as stale after the grace window. Treating empty/garbage pid as
  # "not stale" indefinitely would block every future run with a permanent
  # exit 2 if a gate was killed mid-init, so the age gate is the recovery
  # path for that case too.
  if [[ ! "$holder" =~ ^[0-9]+$ ]]; then
    if [ "$lock_age" -ge "$LOCK_INIT_GRACE_SECONDS" ]; then
      stale=1
    else
      echo "FAIL: pre-push-gate lock is initializing in this worktree; retry in a moment." >&2
      exit 2
    fi
  elif ! kill -0 "$holder" 2>/dev/null; then
    stale=1
  fi
  if [ "$stale" -eq 1 ]; then
    echo "pre-push-gate: clearing stale lock (pid='${holder:-empty}')" >&2
    rm -rf "$LOCK_DIR"
    if mkdir "$LOCK_DIR" 2>/dev/null; then
      echo "$$" > "$LOCK_DIR/pid"
      return 0
    fi
  fi
  echo "FAIL: another pre-push-gate is running in this worktree (pid ${holder:-unknown})." >&2
  echo "      Wait for it to finish or kill it before retrying." >&2
  exit 2
}
acquire_lock

COVER_OUT="$SW_RUN_DIR/cover.out"
tmp_openapi=""
cleanup() {
  rm -f "${COVER_OUT:-}" "${tmp_openapi:-}"
  rm -rf "${LOCK_DIR:-}"
}
trap cleanup EXIT

echo "=== Conflict markers (tracked files) ==="
# Catch unresolved merge markers across every tracked file regardless of
# extension. Mkdocs.yml conflict in PR #1357 round 1 slipped through because
# the local sweep filter only included *.go/*.json/*.templ/*.md. This check
# runs in milliseconds and fail-fasts before the test suite eats 2-3 minutes.
# Markers checked: <<<<<<< (start), ======= (separator), >>>>>>> (end), each
# requiring a trailing space or EOL to avoid matching legitimate content like
# a markdown ASCII rule of exactly seven equals.
markers=$(git ls-files -z \
    | xargs -0 grep -nE '^(<{7}|={7}|>{7})( |$)' 2>/dev/null \
    | head -50 || true)
if [ -n "$markers" ]; then
    echo "FAIL: unresolved merge conflict markers in tracked files:"
    echo "$markers" | sed 's/^/  /'
    echo ""
    echo "Resolve the conflicts (search for '<<<<<<<') and re-run the gate."
    exit 1
fi
echo "OK"

echo ""
echo "=== Gate invariant (no advisory step in the default path) ==="
# Assert this script still satisfies the rule in the header block: a check in
# the default path either BLOCKS or is not in the default path. Three greps
# over one file, so it runs unconditionally and fail-fasts.
#
# The gate checking ITSELF is the point. The advisory steps #2983 removed were
# each added by someone with a good local reason, so the counter-pressure has
# to be something that fails rather than something that reminds. Mirrored by
# CI's "Gate Invariant" job (gate.yml) for the --no-verify path.
bash "$SCRIPT_DIR/check-gate-invariant.sh"

echo ""
echo "=== Tool version drift ==="
# Assert the lint/spell tool versions pinned independently in the bash hook,
# the pre-commit framework config, and the CI workflows all agree. A drift
# lets a local hook pass while CI fails on the same tree (and vice versa);
# golangci-lint minor versions also resolve //nolint differently (#1560).
# Fast grep-only check, so it fail-fasts before the multi-minute test suite.
TOOL_VERSIONS_HELPER="$SCRIPT_DIR/check-tool-versions.sh"
if [ ! -x "$TOOL_VERSIONS_HELPER" ]; then
  echo "pre-push-gate: check-tool-versions.sh not found or not executable in scripts/" >&2
  exit 1
fi
bash "$TOOL_VERSIONS_HELPER"

echo ""
echo "=== Action pin drift ==="
# Assert every sub-action of one action repo (github/codeql-action/{init,analyze,
# upload-sarif}, actions/cache{,/restore}) is pinned to the SAME commit SHA. They
# ship from one repo and are version-locked to each other, but Dependabot names
# each subpath separately and will bump one without the others -- which is exactly
# how #2490 broke every CodeQL job on every PR. dependabot.yml now groups these
# families, but a group is a policy, not an assertion: it cannot catch a skew
# introduced by hand, by a bad merge, or in an ungrouped family.
# Fast grep-only check, so it fail-fasts before the multi-minute test suite.
ACTION_PINS_HELPER="$SCRIPT_DIR/check-action-pins.sh"
if [ ! -x "$ACTION_PINS_HELPER" ]; then
  echo "pre-push-gate: check-action-pins.sh not found or not executable in scripts/" >&2
  exit 1
fi
bash "$ACTION_PINS_HELPER"

echo ""
echo "=== Changed Go files/packages ==="
# Derived once, up front, so both the Tests step below and the measurement-
# linter re-pass further down (in the Lint section) reuse the same set instead
# of computing it twice. Motivation: M52 PR #1644 bumped
# SSEHub.SubscribeToEventBus from cog=28 to cog=34 (cap 30); local gate PASS,
# CI FAIL. Issue #1645.
MODIFIED_GO_FILES=$(git diff --name-only --diff-filter=ACMR "$BASE" -- '*.go' \
  | grep -v '_templ\.go$' || true)
# Guard against BSD xargs (macOS) running `dirname` with zero args when the
# input is empty; GNU xargs has --no-run-if-empty but BSD does not. Empty
# file list -> empty package list -> callers below skip cleanly.
#
# EXACT PACKAGES, NOT `/...` SUBTREES (#2983). This used to emit `./<dir>/...`,
# which pulls in every SUBPACKAGE of a changed directory even when no file in
# those subpackages changed: one edit to internal/provider/*.go dragged in all
# twelve provider adapters, and one edit to internal/api/*.go dragged in
# filterparams + middleware. Those subpackages are exactly what CI's sharded,
# required `Test` job exists to run. Patch coverage -- the reason this profile
# is produced at all -- only ever measures lines in the CHANGED files, so the
# subtree expansion bought coverage of code no local check reads, at the price
# of the gate's longest step. The narrower set is a strict subset of what CI
# runs, so nothing stops being tested; it stops being tested TWICE.
if [ -n "$MODIFIED_GO_FILES" ]; then
  MODIFIED_GO_PKGS=$(printf '%s\n' "$MODIFIED_GO_FILES" \
    | xargs -n1 dirname \
    | sort -u \
    | sed 's|^|./|')
else
  MODIFIED_GO_PKGS=""
fi

echo ""
echo "=== Tests ==="
# RUN_RACE three-state gate, mirroring the RUN_A11Y pattern further down in
# this file (the "Accessibility (axe-core)" section):
#   - RUN_RACE truthy (1/true/yes/on): full `go test -race -coverpkg=./...
#     ./...`, BLOCKING on failure. Today's behavior; the escape hatch for a
#     complete, CI-equivalent local run.
#   - RUN_RACE falsy  (0/false/no/off): skip the test run entirely.
#   - RUN_RACE unset (the DEFAULT): run ONE fast, changed-packages-only,
#     NON-race test (`go test -coverprofile=... $MODIFIED_GO_PKGS`, no
#     `-race`, no `-coverpkg=./...`, EXACT packages rather than `/...`
#     subtrees -- see the "Changed Go files/packages" step above). This is
#     deliberately narrower than the full suite: it's a quick "did I obviously
#     break a test" signal, and it produces the coverage profile the
#     patch-coverage step below consumes. If no Go files changed since BASE,
#     the run is skipped (nothing to test).
#
#     THIS STEP IS BLOCKING, in every one of its failure modes (#2983). It
#     used to treat an ordinary test-assertion failure as advisory on the
#     grounds that CI's required `Test` job is authoritative -- which is true
#     of the VERDICT and irrelevant to the COST. The whole reason to keep a
#     test run local is that a break caught here costs a re-run, while the
#     same break caught in CI costs a red check plus a re-push, and caught by
#     a reviewer costs an entire review round. An advisory local failure buys
#     the cost of the run and then discards the only thing it produced.
#
#     The build/compile-failure branch is still distinguished from an ordinary
#     failing assertion, because the two need different MESSAGES and because
#     `go test` emits no coverage profile when a package does not compile --
#     but both now exit 1.
#
#     Escape hatch for a genuinely expensive changed package (internal/api's
#     own suite is ~160s locally): `RUN_RACE=0` skips the test run and patch
#     coverage together. That is an explicit, recorded opt-out, not a silent
#     downgrade of a failure the gate already observed.
race_flag="$(printf '%s' "${RUN_RACE:-}" | tr '[:upper:]' '[:lower:]' | tr -d '[:space:]')"

run_full_race_suite() {
  # -coverpkg=./... matches CI's methodology (.github/workflows/ci.yml): each
  # test binary is instrumented against every package, so a package exercised
  # mainly by other packages' integration tests (e.g. internal/connection via
  # api/publish/imagebridge) is credited that cross-package coverage. Without
  # it the profile reads lower than CI (#2062). Costs extra instrumentation
  # time vs per-package coverage -- the tradeoff for floor/patch numbers that
  # match CI.
  go test -race -count=1 -covermode=atomic -coverpkg=./... -coverprofile="$COVER_OUT" ./...

  # Union the profile before patch-coverage consumes it. A single
  # `go test ./... -coverpkg=./...` invocation emits each block once PER test
  # binary (every binary instruments every package), so the raw profile is
  # heavily duplicated. `go tool cover` dedups on read, but patch-coverage.sh
  # sums nstmts across duplicate blocks and would read every package at ~1/N
  # of its real coverage, failing spuriously. gocovmerge applies the same
  # per-block UNION that CI runs across its 9 shards
  # (.github/workflows/ci.yml:575) so the local numbers match CI.
  go tool gocovmerge "$COVER_OUT" > "$COVER_OUT.merged"
  mv "$COVER_OUT.merged" "$COVER_OUT"
}

run_changed_pkgs_test() {
  if [ -z "$MODIFIED_GO_PKGS" ]; then
    echo "tests: skipped (no Go files changed since BASE)"
    return 0
  fi
  # Clear any stale profile before running. A profile left behind by a prior
  # INTERRUPTED run of this gate (killed mid-test, crashed before cleanup's
  # EXIT trap fired) would otherwise still be sitting at $COVER_OUT when this
  # invocation's `go test` fails to compile: the build-failure branch below
  # tells a compile failure apart from an ordinary test failure by checking
  # whether $COVER_OUT is non-empty, and a stale non-empty file would make a
  # genuine build breakage misread as an ordinary test failure. Both block
  # now, so the consequence is a misleading MESSAGE rather than a masked
  # failure -- still worth preventing. This is belt-and-suspenders on top of
  # the cleanup() EXIT trap, which only covers the common case where the
  # script exits normally.
  rm -f "$COVER_OUT"
  # shellcheck disable=SC2086  # word-splitting on newlines is intentional
  go test -count=1 -covermode=atomic -coverprofile="$COVER_OUT" $MODIFIED_GO_PKGS
}

# SKIP_PATCH_COVERAGE gates the "Patch coverage" step below: it has nothing
# to check when RUN_RACE=0 skipped the test run entirely (no profile was
# produced at all), or when the default/changed-packages path legitimately
# has nothing to measure. SKIP_PATCH_COVERAGE_REASON carries the specific
# reason through to that step's SKIP message so it never misreports why
# coverage wasn't enforced (see the "Patch coverage" section below).
SKIP_PATCH_COVERAGE=0
SKIP_PATCH_COVERAGE_REASON=""
case "$race_flag" in
  0 | false | no | off)
    echo "tests: skipped (RUN_RACE=${RUN_RACE} forces opt-out; CI still runs the full race suite)"
    echo "tests: no coverage profile generated -- patch coverage also skipped for this push"
    SKIP_PATCH_COVERAGE_REASON="RUN_RACE=${RUN_RACE} skipped the test run, so no profile is available; CI's Coverage Floor / codecov still gate this"
    SKIP_PATCH_COVERAGE=1
    ;;
  1 | true | yes | on)
    run_full_race_suite
    ;;
  *)
    if ! run_changed_pkgs_test; then
      # BLOCKING in every failure mode since #2983 -- see the rationale in the
      # RUN_RACE comment block above. The message distinguishes the two cases
      # only as a hint, never as a verdict:
      #
      # DO NOT restore a behavioral split keyed on the profile being empty.
      # That discriminator is UNRELIABLE. It rested on "go test emits no
      # coverage profile when a package doesn't compile", which no longer
      # holds: on Go 1.26 a deliberate syntax error in a changed package
      # produced `[build failed]` AND a non-empty $COVER_OUT (verified
      # 2026-08-10). While the assertion branch was advisory, that made a
      # genuine build break silently non-blocking -- the exact masking the
      # comment claimed to prevent. Both branches exit 1 now, so an
      # unreliable discriminator costs at most a slightly-off hint.
      if [ -s "$COVER_OUT" ]; then
        test_fail_hint="Usually a failing assertion; check the output for '[build failed]' too."
      else
        test_fail_hint="No coverage profile was produced, which usually means the changed packages do not compile."
      fi
      echo ""
      echo "FAIL: changed-packages test run failed (see output above)." >&2
      echo "      $test_fail_hint" >&2
      echo "      Fix it, or use RUN_RACE=0 to skip the local test run and patch coverage deliberately." >&2
      exit 1
    fi
    if [ ! -s "$COVER_OUT" ]; then
      # Reachable only when run_changed_pkgs_test exited 0 (or was skipped
      # outright because no Go files changed) yet left no profile -- there is
      # legitimately nothing to measure. Skip patch coverage explicitly with
      # a SKIP instead of writing a minimal "mode: atomic" placeholder profile:
      # patch-coverage.sh treats an empty profile as "no executable lines,
      # nothing to enforce" and exits 0, which would read identically to a
      # real, passing coverage check for genuine Go changes.
      echo "SKIP: no coverage profile produced by the changed-packages test run -- patch coverage skipped (nothing to measure for this push)"
      SKIP_PATCH_COVERAGE_REASON="changed-packages test run produced no coverage profile (no Go packages changed since BASE, or the changed packages have nothing testable)"
      SKIP_PATCH_COVERAGE=1
    fi
    ;;
esac

echo ""
echo "=== Vulnerability scan (govulncheck) ==="
# RUN_VULN three-state gate, mirroring the RUN_RACE pattern above (and
# RUN_A11Y further down): CI's "Go Vulnerability Check" job (security.yml)
# runs unconditionally on every push/PR to main with no paths-filter and is
# the authoritative, required gate -- it is network-dependent (downloads the
# vuln DB) and takes ~30-60s, so running it again on every local push is a
# slow, occasionally-flaky duplicate of a check CI already enforces.
#   - RUN_VULN truthy (1/true/yes/on): force a RUN, BLOCKING on failure
#     regardless of changed files. Today's prior behavior; the escape hatch
#     for a full, CI-equivalent local run.
#   - RUN_VULN falsy  (0/false/no/off): force a SKIP (escape hatch when
#     offline or the vuln DB fetch is misbehaving; CI still gates this).
#   - RUN_VULN unset (auto, the DEFAULT since #2983): SKIP. The auto path used
#     to run govulncheck whenever Go-relevant files changed and then treat a
#     failure as ADVISORY, which is exactly the shape #2983 forbids: 30-60s of
#     network-dependent work whose verdict the gate then declines to act on.
#     The authoritative check is CI's required "Go Vulnerability Check" job
#     (security.yml), which runs unconditionally on every push and PR to main
#     with no paths-filter on the trigger.
vuln_flag="$(printf '%s' "${RUN_VULN:-}" | tr '[:upper:]' '[:lower:]' | tr -d '[:space:]')"

run_vuln=0
case "$vuln_flag" in
  0 | false | no | off)
    echo "vuln: skipped (RUN_VULN=${RUN_VULN} forces opt-out; CI still runs govulncheck)"
    ;;
  1 | true | yes | on)
    run_vuln=1
    ;;
  *)
    echo "vuln: skipped by default -- CI's required 'Go Vulnerability Check' job (security.yml) is authoritative; set RUN_VULN=1 for a blocking local run"
    ;;
esac

if [ "$run_vuln" -eq 1 ]; then
  # Pinned to the same version as `make vulncheck` / CI (fresh-clone-friendly
  # `go run`, no local install required). Default source-based reachability
  # mode (no -scan=module) so only actually-reachable vulnerabilities gate,
  # and whole-module ./... scope to match CI's authoritative behavior.
  if ! go run golang.org/x/vuln/cmd/govulncheck@v1.1.4 ./...; then
    echo ""
    echo "FAIL: govulncheck exited non-zero -- a reachable vulnerability, or a tool/download/run error; see the govulncheck output above" >&2
    exit 1
  fi
  echo "OK"
fi

echo ""
echo "=== Lint (diff-only) ==="
# Lint only the lines changed since BASE. With a warm cache this runs in
# ~5s; cold it can take ~30s. Closes the `git commit --no-verify` bypass:
# the pre-commit hook lints staged files, but a `--no-verify` commit + plain
# `git push` historically reached this gate without any lint pass, letting
# regressions slip to CI. BASE is validated at intake, so an unreadable rev
# is caught above this point rather than silently producing an empty diff.
#
# Hard-fail (not SKIP) when golangci-lint is missing: the lint step is the
# entire purpose of closing the no-verify bypass. SKIP would re-open the
# bypass on machines without the tool. Distinct from the oasdiff / python3
# SKIPs above which gate optional warnings, not the project's lint policy.
if ! command -v golangci-lint >/dev/null 2>&1; then
  echo "FAIL: golangci-lint not in PATH (install: brew install golangci-lint)" >&2
  exit 1
fi

# A REMOVED WORKTREE POISONS THE SHARED LINT CACHE. golangci-lint's cache is
# USER-GLOBAL (~/Library/Caches/golangci-lint on macOS), not per-worktree and not
# per-repo, so deleting one worktree leaves entries keyed to paths that no longer
# exist. The next gate in a SIBLING worktree then reports findings against files
# it cannot open.
#
# The cost is misdirection, not just time: the findings are not real, but they
# fail the gate, and the natural response is to hunt a bug in code that is fine.
# Measured in the sibling canticle repo on 2026-08-05: three blocked gate runs in
# one session -- one of them a release tag push -- with 107 findings on the worst
# occurrence, every one naming a path inside a removed directory and zero in the
# working tree. Ported from sydlexius/canticle#738.
#
# Detected HERE rather than at the removal site on purpose: worktrees are removed
# by tooling maintained outside this repo, so no local target or wrapper can hook
# it reliably. The roster lives under the COMMON git dir, shared by every worktree
# of this clone, so a removal recorded by one gate run is visible to the next run
# in any sibling.
#
# Cleans only on a DISAPPEARANCE. Adding a worktree is harmless, and cleaning
# unconditionally would discard a warm cache on every run -- lint is one of the
# slowest steps in this gate, so that trade is deliberate.
WT_ROSTER="$(git rev-parse --git-common-dir)/golangci-worktree-roster"
# STRIP THE PREFIX, do not field-split. `awk '{print $2}'` truncates a path at the
# first space, and a truncated path never matches the live list -- so it reads as
# "removed" on EVERY run and would clean the cache every time, silently
# destroying the warm-cache trade above. LC_ALL=C throughout because comm requires
# both inputs in the SAME collation and the roster outlives the run that wrote it.
WT_NOW="$(git worktree list --porcelain | sed -n 's/^worktree //p' | LC_ALL=C sort)"
if [ -f "$WT_ROSTER" ]; then
  # comm -23 prints lines unique to the first (recorded) side: paths that were
  # present at the last gate run and are gone now.
  if WT_GONE="$(LC_ALL=C comm -23 "$WT_ROSTER" <(printf '%s\n' "$WT_NOW"))" && [ -n "$WT_GONE" ]; then
    echo "==> worktree removed since the last gate run; cleaning the shared lint cache:"
    printf '%s\n' "$WT_GONE" | sed 's/^/      - /'
    golangci-lint cache clean || echo "    WARNING: cache clean failed; phantom findings may follow" >&2
  fi
fi
# WRITE THE ROSTER ATOMICALLY AND BEST-EFFORT. Two distinct failures, both fatal
# in the obvious `printf ... > "$WT_ROSTER"` form:
#   - `set -e` is on, so an unwritable roster (read-only home, full disk) aborts
#     the WHOLE gate before lint even runs. Roster bookkeeping is an optimization
#     for the NEXT run; it must never block this push. Warn and continue, exactly
#     as the `cache clean` failure above does.
#   - `>` truncates in place, so a gate running concurrently in a sibling worktree
#     can read an empty or partial roster and see every live worktree as removed,
#     wiping the warm cache the block above exists to protect. rename(2) within one
#     directory is atomic, so a concurrent reader gets either the old roster or the
#     new one, never a half-written one. The temp file is created in the same
#     directory for that reason -- a cross-filesystem mv would copy, not rename.
# The chain sits in an `if` CONDITION because commands there are exempt from
# errexit; as bare statements each failure would abort the gate again.
WT_ROSTER_TMP="$(mktemp "${WT_ROSTER}.XXXXXX" 2>/dev/null || true)"
if [ -n "$WT_ROSTER_TMP" ] &&
  printf '%s\n' "$WT_NOW" >"$WT_ROSTER_TMP" &&
  mv -f "$WT_ROSTER_TMP" "$WT_ROSTER"; then
  :
else
  [ -n "$WT_ROSTER_TMP" ] && rm -f "$WT_ROSTER_TMP"
  echo "    WARNING: could not update the worktree roster ($WT_ROSTER); a worktree removed before the next gate run may go undetected" >&2
fi

golangci-lint run --new-from-rev="$BASE" ./...

# Second pass: re-lint the touched Go files with measurement linters that
# `--new-from-rev` can silently dedup. `--new-from-rev` keys on
# file:line:linter:message, so a pre-existing function whose body changes
# enough to push gocognit/cyclop/funlen past their threshold reports the
# same message string at the same file:line and gets dedup'd away. CI runs
# the full lint without `--new-from-rev` and surfaces the bump as a failure;
# this pass closes that local-vs-CI gap.
#
# Scoped to changed packages (not ./...) so the cost is bounded; excludes
# _templ.go because generated code is excluded from the configured ruleset
# elsewhere and we don't want to drag templ noise into a focused gate. Only
# gocognit is enabled in .golangci.yml today among the measurement linters;
# if cyclop/funlen are added later, extend --enable-only to match.
#
# Package directories (not individual file paths) are passed to
# golangci-lint so the typechecker can resolve cross-file symbols defined
# in sibling files of the same package. Feeding bare *.go file paths
# breaks typecheck and silently suppresses gocognit findings (issue #1650).
# Trade-off: lints the whole touched package(s), not just touched
# functions; still avoids the full-repo cost of dropping --new-from-rev
# entirely. For most PRs the package set is 1-3 directories.
#
# Motivation: M52 PR #1644 bumped SSEHub.SubscribeToEventBus from
# cog=28 to cog=34 (cap 30); local gate PASS, CI FAIL. Issue #1645.
#
# MODIFIED_GO_FILES/MODIFIED_GO_PKGS were already derived once in the
# "Changed Go files/packages" step above (also consumed by the Tests step);
# reused here rather than recomputed.
if [ -n "$MODIFIED_GO_PKGS" ]; then
  echo "--- measurement-linter re-pass on $(echo "$MODIFIED_GO_PKGS" | wc -l | tr -d ' ') changed package(s) ---"
  # --default=none + --enable=gocognit narrows the active linter set to just
  # gocognit while still reading .golangci.yml for settings (so the
  # min-complexity: 30 threshold is honored). _test.go files inherit the
  # `_test\.go -> gocognit` exclusion in .golangci.yml.
  # shellcheck disable=SC2086  # word-splitting on newlines is intentional
  golangci-lint run --default=none --enable=gocognit $MODIFIED_GO_PKGS
fi
echo "OK"

echo ""
echo "=== OpenAPI consistency ==="
go test -count=1 -run TestOpenAPIConsistency -v ./internal/api/

echo ""
echo "=== CSS comments ==="
# Assert no hand-written CSS comment terminates itself. A `*/` inside comment
# PROSE closes the comment, so everything after it is parsed as CSS -- which in
# #2525 silently swallowed the entire `@theme` block and meant its `swd-*`
# utilities were never generated. No visual damage, no error: just a design-system
# guardrail that was quietly off. Runs BEFORE the generated-files check below, so
# the source defect is named directly rather than surfacing as a confusing diff in
# the Tailwind build product.
bash "$SCRIPT_DIR/check-css-comments.sh"

echo ""
echo "=== Worktree settings link (#2879) ==="
# Hermetic, no network, sub-second: runs unconditionally rather than diff-scoped.
# What it guards is invisible at runtime -- a worktree whose Claude Code grants
# silently fall back to the user-global set produces a permission PROMPT, and a
# backgrounded agent cannot answer one, so it stalls with no error anywhere. A
# regression here would be noticed as "the agent hung", days later, by someone
# not looking at this script.
bash "$SCRIPT_DIR/test-link-worktree-settings.sh"

echo ""
echo "=== zizmor suppression scope ==="
# A `# zizmor: ignore[dangerous-triggers]` suppresses the audit for the WHOLE
# `on:` mapping, not the one trigger it was written for. So a dangerous trigger
# added to an already-suppressed block raises no finding and no code-scanning
# alert -- re-running zizmor cannot catch it, because the suppression is exactly
# what makes it invisible. This pins each suppressed block's allowed trigger set
# instead. Cheap (no network, parse-only) so it runs unconditionally.
#
# The guard's own mutation tests run first: its first implementation passed
# every smoke test and still had two bypasses, so the self-test is what keeps
# the guard honest.
bash "$SCRIPT_DIR/test-check-zizmor-suppressions.sh"
bash "$SCRIPT_DIR/check-zizmor-suppressions.sh"

echo ""
echo "=== CSS lint (diff-scoped ratchet, #2402) ==="
# Design-token layer stylelint gate. The token migration is not complete
# (input.css still carries ~135 pre-existing literal-value violations), so
# this is a ratchet: only violations on lines this diff ADDED can fail the
# build, matching coverage-floor.sh's one-way-ratchet shape.
#
# The jq/stylelint precondition lives inside stylelint-diff-gate.sh itself,
# not here, because it can only be evaluated correctly after looking at the
# diff: a diff that touches no CSS has nothing to check and must SKIP even
# with the tools absent (a fresh `make worktree` has no node_modules), while
# a diff that DOES touch CSS with the tools missing must hard-fail (not
# SKIP), same rationale as the golangci-lint check above -- this closes a
# `--no-verify` bypass, so a missing tool must not silently reopen it. Do not
# duplicate that check here; a copy that runs before the diff is examined is
# exactly the bug this ordering fixes.
"$SCRIPT_DIR/stylelint-diff-gate.sh" "$BASE"

echo ""
echo "=== Generated files ==="
bash "$SCRIPT_DIR/check-generated.sh"

echo ""
echo "=== Doc facts ==="
# Assert hand-written docs still cite code-derived facts (rule count, envelope
# version, Go minimum, reverse-proxy body-size) correctly. Catches the silent
# drift documented in #1711; the surrounding prose stays hand-written.
bash "$SCRIPT_DIR/check-doc-facts.sh"

echo ""
echo "=== Codecov/floor mirror ==="
# Assert codecov.yml's per-package project targets mirror
# testdata/coverage-floor.json exactly. Catches the silent drift documented in
# #2756 (internal/server sat at 91% in codecov.yml while the floor had been
# lowered to 87% in #2066, with nothing enforcing the two stay in sync).
bash "$SCRIPT_DIR/check-codecov-floor-mirror.sh"

echo ""
echo "=== ProperDocs config YAML ==="
# Catch syntax errors (incl. residual conflict markers, indentation slips,
# duplicate keys) in the properdocs config before CI's "Build site" job does.
# Stdlib PyYAML only -- no need for properdocs itself locally. If python3 is
# missing or PyYAML is unavailable, skip with a one-line warning rather than
# fail the gate (a dev without a Python toolchain shouldn't be blocked).
if [ -f docs/site/properdocs.yml ]; then
    if command -v python3 >/dev/null 2>&1; then
        if python3 -c 'import yaml' 2>/dev/null; then
            # ProperDocs configs use PyYAML "!!python/name:" / "!!python/object"
            # tags (e.g. the pymdownx.superfences custom fence that enables
            # Mermaid rendering). safe_load rejects those legitimate tags, so
            # validate with a SafeLoader extended to treat the python-specific
            # tag families as opaque. This still catches real syntax errors
            # (residual conflict markers, indentation slips) without requiring
            # properdocs itself to be importable here.
            if ! python3 - docs/site/properdocs.yml 2>&1 <<'PY'
import sys
import yaml


class ProperDocsLoader(yaml.SafeLoader):
    """SafeLoader that tolerates ProperDocs/pymdownx python tags."""


def _ignore_python_tag(loader, suffix, node):
    return None


for _prefix in (
    "tag:yaml.org,2002:python/name:",
    "tag:yaml.org,2002:python/object:",
    "tag:yaml.org,2002:python/object/apply:",
):
    ProperDocsLoader.add_multi_constructor(_prefix, _ignore_python_tag)

with open(sys.argv[1], encoding="utf-8") as fh:
    yaml.load(fh, Loader=ProperDocsLoader)
PY
            then
                echo "FAIL: docs/site/properdocs.yml is not valid YAML (see error above)."
                exit 1
            fi
            echo "OK"
        else
            echo "SKIP: PyYAML not installed (pip install pyyaml -- runs only on demand)"
        fi
    else
        echo "SKIP: python3 not in PATH"
    fi
else
    echo "SKIP: docs/site/properdocs.yml not present"
fi

echo ""
echo "=== Raw error leak check ==="
# Scope to production handler code only: test files legitimately assert on
# err.Error()/err.String() and never reach a client response.
error_leaks=$(git diff "$BASE"..HEAD -- 'internal/api/handlers.go' 'internal/api/handlers_*.go' ':(exclude)internal/api/*_test.go' \
  | grep '^+' \
  | grep -E 'err\.(Error|String)\(\)' \
  | grep -vE '\bslog\.|\blogger\.|\blog\.' || true)
if [ -n "$error_leaks" ]; then
  echo "CRITICAL: Raw error text may be leaking to clients:"
  echo "$error_leaks"
  echo ""
  echo "Client-visible messages must be generic. Log full errors server-side with slog."
  exit 1
fi
echo "OK"

echo ""
echo "=== OpenAPI breaking changes ==="
# Pinned to v1.25.1: the previously-installed "main" dev build cannot parse
# this spec's OpenAPI 3.1 dialect at all -- it models exclusiveMinimum as a
# bool (3.0 semantics) and hard-fails on the numeric form 3.1 requires (see
# internal/api/openapi.yaml's exclusiveMinimum comment, and #2759). v1.25.1
# was confirmed to parse this spec cleanly and to correctly flag a real
# breaking change (verified by deliberately removing a path and confirming
# the tool reports it, then reverting).
#
# The previous version of this step folded stderr into stdout (`2>&1`) and
# discarded the exit code (`|| true`), so a hard parser failure -- output on
# stderr, but nothing "breaking" was ever actually computed -- was announced
# to the operator as "WARNING: Breaking OpenAPI changes detected", i.e. a
# tool crash reported as a finding (#2759). Fixed by capturing stdout and
# stderr separately and branching on the real exit code, giving three
# distinct, unambiguous outcomes: tool failed to run at all (FAIL, loud),
# ran clean, or ran and found a real breaking change.
# SKIP_OPENAPI_BREAKING three-state gate, mirroring the RUN_RACE pattern
# above and the RUN_A11Y pattern further down:
#   - SKIP_OPENAPI_BREAKING truthy (1/true/yes/on): skip this entire step
#     (install + diff), unconditionally. This is an explicit operator
#     override for a DELIBERATE breaking change -- it suppresses not just a
#     real breaking-change FAIL but also a tool-crash or go-not-installed
#     FAIL, since the whole point is "let me push anyway, I've verified this
#     intentionally". CI's spectral lint (openapi.yml) still validates the
#     spec independently; this only bypasses the LOCAL breaking-change diff.
#   - SKIP_OPENAPI_BREAKING unset/falsy (the DEFAULT): run the step exactly
#     as before -- rc=0 clean, rc=1 hard FAIL exit 1, rc!=0/1 (tool crash) or
#     go missing hard FAIL exit 1.
openapi_skip_flag="$(printf '%s' "${SKIP_OPENAPI_BREAKING:-}" | tr '[:upper:]' '[:lower:]' | tr -d '[:space:]')"
case "$openapi_skip_flag" in
  1 | true | yes | on)
    echo "Skipped (SKIP_OPENAPI_BREAKING is set; CI does not gate breaking OpenAPI changes, so verify intentional breaks manually)"
    ;;
  *)
if command -v go &>/dev/null; then
  tmp_openapi="$SW_RUN_DIR/openapi-base.yaml"
  oasdiff_err="$SW_RUN_DIR/oasdiff-stderr.txt"
  # Install the pinned binary into the run-dir rather than `go run`: `go run`
  # wraps the child's real exit code in its own (uniformly 1 on any child
  # non-zero exit, per `go help run`), which would collapse the "tool
  # crashed" (real exit 102) and "found breaking changes" (real exit 1)
  # cases into the same code and defeat the three-way branch below.
  oasdiff_bin="$SW_RUN_DIR/oasdiff-v1.25.1"
  if [ ! -x "$oasdiff_bin" ]; then
    if ! GOBIN="$SW_RUN_DIR" go install github.com/oasdiff/oasdiff@v1.25.1 2>"$oasdiff_err"; then
      echo "FAIL: could not install oasdiff@v1.25.1 -- breaking-change detection is unavailable:" >&2
      cat "$oasdiff_err" >&2
      exit 1
    fi
    mv "$SW_RUN_DIR/oasdiff" "$oasdiff_bin"
  fi
  if git show main:internal/api/openapi.yaml > "$tmp_openapi" 2>/dev/null; then
    breaking_out=$("$oasdiff_bin" breaking --fail-on ERR \
      "$tmp_openapi" internal/api/openapi.yaml 2>"$oasdiff_err") && oasdiff_rc=0 || oasdiff_rc=$?
    case "$oasdiff_rc" in
      0)
        echo "No breaking changes."
        ;;
      1)
        echo "FAIL: Breaking OpenAPI changes detected:"
        echo "$breaking_out"
        exit 1
        ;;
      *)
        echo "FAIL: oasdiff could not run against this spec (exit $oasdiff_rc) -- breaking-change detection is unavailable:" >&2
        cat "$oasdiff_err" >&2
        exit 1
        ;;
    esac
  else
    echo "Skipped (openapi.yaml not yet on main)."
  fi
else
  echo "FAIL: go not installed -- oasdiff (go install github.com/oasdiff/oasdiff@v1.25.1) requires the Go toolchain; breaking-change detection is unavailable" >&2
  exit 1
fi
    ;;
esac

echo ""
echo "=== Patch coverage ==="
if [ "$SKIP_PATCH_COVERAGE" -eq 1 ]; then
  # SKIP_PATCH_COVERAGE is now set for more than one reason (RUN_RACE=0
  # opt-out, or the changed-packages path legitimately producing no coverage
  # profile) -- report the actual reason set alongside the flag rather than
  # hard-coding the RUN_RACE explanation, which would misreport why coverage
  # wasn't enforced for the other paths.
  echo "SKIP: patch coverage (${SKIP_PATCH_COVERAGE_REASON:-no coverage profile available for this push})"
else
  # Matches codecov.yml's 78% patch threshold (codecov.yml:14).
  #
  # The Tests step above no longer always runs a `-coverpkg=./...` profile:
  # by default it runs a changed-packages-only, non-`-coverpkg` profile, so a
  # package covered mainly by OTHER packages' integration tests (e.g.
  # internal/connection via api/publish/imagebridge) can read LOWER here than
  # in CI -- a FALSE-POSITIVE local patch-cov block, never a false-negative
  # (see the cross-package methodology note in the Tests step above, and
  # #2062). If this check looks spurious, run `RUN_RACE=1` for the full,
  # CI-equivalent `-coverpkg=./...` profile before trusting the number.
  #
  # patch-coverage.sh uses exit codes 0|1|2 (2 = config error). This wrapper
  # is documented as 0|1, so collapse any non-zero child status to 1. Using
  # an `if` here (rather than calling the script bare under `set -e`) lets
  # us capture the exit code without the shell bailing out first.
  #
  # BASE is intentionally not forwarded: patch-coverage.sh has its own
  # resolution that errors out if `main` is missing, which is stricter than
  # this script's silent HEAD~1 fallback. Letting the child resolve BASE
  # avoids narrowing patch coverage to only the tip commit on a branch
  # whose base ref isn't reachable.
  #
  # This helper lives only in the orchestrate plugin (~/.claude/scripts/),
  # not vendored in-repo: a prior repo-vendored copy carried a silent
  # wrong-number bug (summed nstmts across duplicate coverage blocks under
  # -coverpkg=./..., collapsing readings toward 0) and shadowed the fixed
  # plugin copy (#2437).
  PATCH_COVERAGE_HELPER="$HOME/.claude/scripts/patch-coverage.sh"
  if [ ! -x "$PATCH_COVERAGE_HELPER" ]; then
    echo "pre-push-gate: patch-coverage.sh not found at ~/.claude/scripts/patch-coverage.sh (ships with the orchestrate plugin)" >&2
    exit 1
  fi
  if COVER_OUT="$COVER_OUT" PATCH_COVERAGE_THRESHOLD=78 \
      PATCH_COVERAGE_EXCLUDE="*_templ.go cmd/stillwater/main.go scripts/" \
      bash "$PATCH_COVERAGE_HELPER"; then
    :
  else
    echo "HINT: if this looks spurious, run \`RUN_RACE=1 bash scripts/pre-push-gate.sh\` for the full-profile (CI-equivalent) coverage." >&2
    exit 1
  fi
fi

echo ""
echo "=== Fuzz matrix drift check ==="
# Verify that the static fuzz matrix in .github/workflows/fuzz.yml lists
# every `func Fuzz*` defined in internal/. A set comparison (not a count)
# catches rename/swap drift that preserves cardinality but breaks parity.
live_fuzz_file="$SW_RUN_DIR/live-fuzz-targets.txt"
matrix_fuzz_file="$SW_RUN_DIR/matrix-fuzz-targets.txt"

grep -RhoE --include='*.go' '^func Fuzz[A-Za-z0-9_]+' internal/ 2>/dev/null \
  | awk '{print $2}' | sort -u > "$live_fuzz_file"
grep -Eo 'fuzz_func:[[:space:]]*"?Fuzz[A-Za-z0-9_]+' .github/workflows/fuzz.yml \
  | sed -E 's/.*(Fuzz[A-Za-z0-9_]+).*/\1/' | sort -u > "$matrix_fuzz_file"

missing=$(comm -23 "$live_fuzz_file" "$matrix_fuzz_file")
extra=$(comm -13 "$live_fuzz_file" "$matrix_fuzz_file")
if [ -n "$missing$extra" ]; then
  echo "FAIL: fuzz matrix is out of sync with internal/ Fuzz* functions."
  [ -n "$missing" ] && echo "  Missing in fuzz.yml matrix:" && echo "$missing" | sed 's/^/    /'
  [ -n "$extra" ] && echo "  Extra in fuzz.yml matrix (no live target):" && echo "$extra" | sed 's/^/    /'
  exit 1
fi
echo "OK: $(wc -l < "$live_fuzz_file" | tr -d ' ') fuzz targets, matrix set matches."

echo ""
echo "=== Provider failure smoke test ==="
# RUN_PROVIDER_SMOKE three-state gate, mirroring the RUN_VULN pattern above.
# CI's "Provider Failure Smoke" job (gate.yml) is a REQUIRED check and is the
# authoritative gate. Locally the step builds a binary and boots a temporary
# server (~60s, load-sensitive), so it is exactly the integration-shaped cost
# #2983 moved out of the default path.
#   - RUN_PROVIDER_SMOKE truthy (1/true/yes/on): RUN, BLOCKING on failure.
#   - RUN_PROVIDER_SMOKE falsy  (0/false/no/off): explicit SKIP.
#   - RUN_PROVIDER_SMOKE unset (the DEFAULT since #2983): SKIP. The previous
#     auto path ran it on any Go change and then treated a failure as
#     ADVISORY -- the pattern this gate no longer contains.
provider_smoke_flag="$(printf '%s' "${RUN_PROVIDER_SMOKE:-}" | tr '[:upper:]' '[:lower:]' | tr -d '[:space:]')"

SMOKE_FAILURE_SCRIPT="$SCRIPT_DIR/smoke-provider-failure.sh"

run_provider_smoke=0
case "$provider_smoke_flag" in
  0 | false | no | off)
    echo "provider-smoke: skipped (RUN_PROVIDER_SMOKE=${RUN_PROVIDER_SMOKE} forces opt-out; CI still runs the provider failure smoke)"
    ;;
  1 | true | yes | on)
    run_provider_smoke=1
    ;;
  *)
    echo "provider-smoke: skipped by default -- CI's required 'Provider Failure Smoke' job (gate.yml) is authoritative; set RUN_PROVIDER_SMOKE=1 for a blocking local run"
    ;;
esac

if [ "$run_provider_smoke" -eq 1 ]; then
  # The script is invoked via `bash` below, so its exec bit is irrelevant --
  # only presence matters (a non-executable-but-present script still runs).
  # A missing script is a local-environment fault (stale checkout, partial
  # sync); it blocks, because the only way to reach this branch now is an
  # explicit RUN_PROVIDER_SMOKE=1, and silently doing nothing in response to
  # "run this" is the failure mode this whole change exists to remove.
  if [ ! -f "$SMOKE_FAILURE_SCRIPT" ]; then
    echo "FAIL: smoke-provider-failure.sh not found in scripts/ (RUN_PROVIDER_SMOKE=1 asked for a blocking run)" >&2
    exit 1
  fi
  if ! bash "$SMOKE_FAILURE_SCRIPT" 2>&1; then
    echo ""
    echo "FAIL: provider failure smoke test reported failures (see output above)."
    exit 1
  fi
  echo "OK"
fi

echo ""
echo "=== Accessibility (axe-core) ==="
# RUN_A11Y three-state gate. `make test-a11y` builds the binary, boots an
# ephemeral server, and drives Playwright + @axe-core/playwright across two
# browser projects -- ~2.4 minutes, the most expensive step this gate ever ran.
#   - RUN_A11Y truthy (1/true/yes/on): RUN, BLOCKING on failure. UNCHANGED --
#     this is the path to use for a round that touches templates, CSS, or
#     tests/a11y/, and it is what a UI change should be verified with before
#     the push.
#   - RUN_A11Y falsy  (0/false/no/off): explicit SKIP.
#   - RUN_A11Y unset (the DEFAULT since #2983): SKIP.
#
# Why the default changed: the auto path used to run the tier whenever
# a11y-relevant files changed and the Playwright engines were installed, and
# then treat a failure as ADVISORY, because #2223 root-caused a local-only
# harness flake (a CPU-starved theme-toggle timeout, not a real contrast
# violation) that hard-blocked unrelated pushes. That fix traded a false block
# for a false PASS: the gate paid the full 2.4 minutes and then printed "All
# hard checks passed" over a red tier. The repo instructions had to carry a
# warning label explaining how to read the gate's own success banner, which is
# the tell that the step did not belong in the default path. CI's required
# "A11y Smoke Tests (Playwright + axe-core)" check (ci.yml) is the
# authoritative gate and is not subject to the local flake, since it does not
# run on a developer machine competing for CPU.
a11y_flag="$(printf '%s' "${RUN_A11Y:-}" | tr '[:upper:]' '[:lower:]' | tr -d '[:space:]')"

run_a11y=0
case "$a11y_flag" in
  0 | false | no | off)
    echo "a11y: skipped (RUN_A11Y=${RUN_A11Y} forces opt-out; CI still gates a11y)"
    ;;
  1 | true | yes | on)
    run_a11y=1
    ;;
  *)
    echo "a11y: skipped by default -- CI's required 'A11y Smoke Tests (Playwright + axe-core)' check (ci.yml) is authoritative; set RUN_A11Y=1 for a blocking local run (needs 'npx playwright install chromium firefox')"
    ;;
esac

if [ "$run_a11y" -eq 1 ]; then
  if ! make test-a11y; then
    echo ""
    echo "FAIL: accessibility (axe-core) smoke tests reported failures (see output above)."
    exit 1
  fi
  echo "OK"
fi

echo "=== UI-preference coverage (prefs-coverage) ==="
# Enforces .prefs.toml (#2195): for each changed surface file matching a
# [[pref]] surface glob, asserts the pref's verify token/class is still
# referenced. Layer 1 only -- it catches a surface edit that silently drops
# the token/class wiring a preference to its rendered effect (the #2194
# Background-Opacity regression class). It does NOT catch a CSS
# cascade-override (a more specific rule beating a pref-driven var); that
# needs a rendered getComputedStyle assertion (Layer 2, tracked separately),
# not this static grep.
#
# Requires python3 with tomllib (3.11+). Degrades to a SKIP + warning (not a
# hard failure) when python3 is missing or too old, matching the
# oasdiff/a11y-toolchain optional-tool tolerance elsewhere in this gate.
#
# BASE is forwarded here (unlike the patch-coverage.sh call site) for CI /
# shallow-clone robustness: this gate has already resolved and rev-parse-
# validated $BASE (the merge-base SHA, above). In a shallow CI checkout that
# never fetched `origin/main`, prefs-coverage.py's own resolve_base() ladder
# would miss that ref and diverge from the base the rest of the gate uses;
# passing the validated SHA keeps them in lockstep. prefs-coverage.py honors
# $BASE first and fails CLOSED (exit 2) if a forwarded BASE won't resolve.
#
# Prefer the repo-vendored copy so a fresh clone / CI works without any
# user-local install. Fall back to ~/.claude/scripts/prefs-coverage.py only
# if the repo copy is missing (e.g. mid-rebase against a commit that
# pre-dates the vendoring), same fallback pattern as patch-coverage.sh above.
PREFS_COVERAGE_HELPER="$SCRIPT_DIR/prefs-coverage.py"
if [ ! -f "$PREFS_COVERAGE_HELPER" ]; then
  PREFS_COVERAGE_HELPER="$HOME/.claude/scripts/prefs-coverage.py"
fi
if [ ! -f "$PREFS_COVERAGE_HELPER" ]; then
  # The helper is VENDORED in scripts/, so its absence is a broken checkout,
  # not a missing optional toolchain. Blocking, for the same reason the
  # golangci-lint absence above is: a step that silently declines to run is
  # indistinguishable from one that ran and passed (#2983).
  echo "FAIL: prefs-coverage.py not found in scripts/ or ~/.claude/scripts/ -- the repo-vendored copy is missing (broken checkout?)" >&2
  exit 1
elif ! command -v python3 >/dev/null 2>&1; then
  echo "pre-push-gate: python3 not found -- skipping prefs-coverage (install python3.11+ to enable locally; CI still gates this)"
elif ! python3 -c 'import tomllib' >/dev/null 2>&1; then
  echo "pre-push-gate: python3 lacks tomllib (need 3.11+) -- skipping prefs-coverage (CI still gates this)"
else
  prefs_status=0
  BASE="$BASE" python3 "$PREFS_COVERAGE_HELPER" || prefs_status=$?
  case "$prefs_status" in
    0)
      :
      ;;
    1)
      echo ""
      echo "FAIL: prefs-coverage reported an un-exempted MISSING (see output above)."
      exit 1
      ;;
    *)
      echo ""
      echo "FAIL: prefs-coverage config/parse error (exit $prefs_status; see output above)."
      exit 1
      ;;
  esac
fi

# Bruno route parity ran here until #2983. It is a CI-only check now: the
# required "Bruno Route Parity" job (.github/workflows/gate.yml) runs the same
# scripts/check-bruno-parity.sh, and the local copy cost ~7s of the gate to
# reproduce a verdict a required check already produces. The script itself is
# retained because that job invokes it.

echo ""
# EVERY STEP ABOVE EITHER BLOCKS OR IS EXPLICITLY SKIPPED -- no step in this
# gate can fail while this line prints (#2983). A "SKIP:" line means a check
# did not run; a check that RAN and FAILED exits non-zero before reaching
# here. scripts/check-gate-invariant.sh enforces that mechanically.
echo "All hard checks passed. Proceed with /pr-review-toolkit:review-pr."
