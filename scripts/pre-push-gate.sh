#!/bin/bash
# pre-push-gate.sh -- deterministic pre-push checks; run before code review
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
if [ -n "$MODIFIED_GO_FILES" ]; then
  MODIFIED_GO_PKGS=$(printf '%s\n' "$MODIFIED_GO_FILES" \
    | xargs -n1 dirname \
    | sort -u \
    | sed 's|^|./|; s|$|/...|')
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
#     `-race`, no `-coverpkg=./...`). This is deliberately narrower than the
#     full suite: it's a quick "did I obviously break a test" signal, and it
#     produces the coverage profile the patch-coverage step below consumes.
#     An ORDINARY TEST-ASSERTION FAILURE in this default path is ADVISORY
#     (warn, don't block) -- CI's required `Test` job runs the full race
#     suite across 9 shards and is the authoritative gate. A BUILD/COMPILE
#     failure in the changed packages is different and always BLOCKS: `go
#     test` never emits a coverage profile when the package doesn't compile,
#     so treating it as advisory-only would let the empty-profile fallback
#     silently swallow it as "nothing to measure" below. If no Go files
#     changed since BASE, the run is skipped (nothing to test).
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
  # genuine build breakage misread as an ordinary (advisory) test failure --
  # masking it instead of blocking the push. This is belt-and-suspenders on
  # top of the cleanup() EXIT trap, which only covers the common case where
  # the script exits normally.
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
      if [ -s "$COVER_OUT" ]; then
        # Ordinary test-assertion failure: go test still ran to completion and
        # emitted a real profile. Advisory only -- CI's full -race suite is
        # authoritative; patch coverage below still runs against this profile.
        echo ""
        echo "WARN: changed-packages test run failed (see output above) -- not blocking this push."
        echo "WARN: CI's Test job runs the full -race suite and is authoritative; set RUN_RACE=1 to make this blocking locally."
      else
        # Build/compile failure: go test never produced a profile at all,
        # meaning the changed packages don't even compile. This is strictly
        # worse than an ordinary test failure and must never be masked as
        # advisory or fall through to the empty-profile skip below, which
        # would let patch-coverage.sh read it as "nothing to enforce" and
        # exit 0 -- a silent gate weakening on a genuinely broken build.
        echo ""
        echo "FAIL: changed-packages test run produced no coverage profile -- this indicates a build/compile error in the changed packages (not just a failing test assertion). Fix the build before pushing." >&2
        exit 1
      fi
    fi
    if [ ! -s "$COVER_OUT" ]; then
      # Reachable only when run_changed_pkgs_test exited 0 (or was skipped
      # outright because no Go files changed) yet left no profile -- there is
      # legitimately nothing to measure. Skip patch coverage explicitly with
      # a WARN instead of writing a minimal "mode: atomic" placeholder profile:
      # patch-coverage.sh treats an empty profile as "no executable lines,
      # nothing to enforce" and exits 0, which would read identically to a
      # real, passing coverage check for genuine Go changes.
      echo "WARN: no coverage profile produced by the changed-packages test run -- patch coverage skipped (nothing to measure for this push)"
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
#   - RUN_VULN unset (auto, the DEFAULT): run iff Go-relevant files changed
#     since BASE (any non-generated *.go file, or go.mod/go.sum), otherwise
#     SKIP -- nothing reachable-vulnerability-wise could have changed. A
#     failure in this auto path is ADVISORY (warn, don't block): CI's
#     required job is the strict, authoritative gate.
vuln_flag="$(printf '%s' "${RUN_VULN:-}" | tr '[:upper:]' '[:lower:]' | tr -d '[:space:]')"

# vuln_relevant: did this branch touch any file that could change
# govulncheck's result -- a Go source file (MODIFIED_GO_FILES, already
# derived above and reused here) or the dependency manifests?
vuln_relevant() {
  [ -n "$MODIFIED_GO_FILES" ] && return 0
  git diff --name-only "$BASE" HEAD 2>/dev/null | grep -qE '^go\.(mod|sum)$'
}

run_vuln=0
vuln_blocking=0
case "$vuln_flag" in
  0 | false | no | off)
    echo "vuln: skipped (RUN_VULN=${RUN_VULN} forces opt-out; CI still runs govulncheck)"
    ;;
  1 | true | yes | on)
    run_vuln=1
    vuln_blocking=1
    ;;
  *)
    if ! vuln_relevant; then
      echo "vuln: skipped (no Go source or go.mod/go.sum changes since BASE; set RUN_VULN=1 to force)"
    else
      run_vuln=1
    fi
    ;;
esac

if [ "$run_vuln" -eq 1 ]; then
  # Pinned to the same version as `make vulncheck` / CI (fresh-clone-friendly
  # `go run`, no local install required). Default source-based reachability
  # mode (no -scan=module) so only actually-reachable vulnerabilities gate,
  # and whole-module ./... scope to match CI's authoritative behavior.
  if ! go run golang.org/x/vuln/cmd/govulncheck@v1.1.4 ./...; then
    echo ""
    if [ "$vuln_blocking" -eq 1 ]; then
      echo "FAIL: govulncheck exited non-zero -- a reachable vulnerability, or a tool/download/run error; see the govulncheck output above" >&2
      exit 1
    fi
    echo "WARN: vuln: advisory failure in the auto path -- not blocking this push."
    echo "WARN: vuln: CI's Go Vulnerability Check job is authoritative; set RUN_VULN=1 to make this blocking locally."
  else
    echo "OK"
  fi
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
echo "=== Generated files ==="
bash "$SCRIPT_DIR/check-generated.sh"

echo ""
echo "=== Doc facts ==="
# Assert hand-written docs still cite code-derived facts (rule count, envelope
# version, Go minimum, reverse-proxy body-size) correctly. Catches the silent
# drift documented in #1711; the surrounding prose stays hand-written.
bash "$SCRIPT_DIR/check-doc-facts.sh"

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
error_leaks=$(git diff "$BASE"..HEAD -- 'internal/api/handlers.go' 'internal/api/handlers_*.go' \
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
if command -v oasdiff &>/dev/null; then
  tmp_openapi="$SW_RUN_DIR/openapi-base.yaml"
  if git show main:internal/api/openapi.yaml > "$tmp_openapi" 2>/dev/null; then
    breaking=$(oasdiff breaking "$tmp_openapi" internal/api/openapi.yaml 2>&1 || true)
    if [ -n "$breaking" ]; then
      echo "WARNING: Breaking OpenAPI changes detected (may be intentional):"
      echo "$breaking"
    else
      echo "No breaking changes."
    fi
  else
    echo "Skipped (openapi.yaml not yet on main)."
  fi
else
  echo "Skipped (oasdiff not installed -- install: go install github.com/oasdiff/oasdiff@latest)"
fi

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
  # Prefer the repo-vendored helper so a fresh clone works without any
  # user-local install. Fall back to ~/.claude/scripts/patch-coverage.sh only
  # if the repo copy is missing (e.g. mid-rebase against a commit that
  # pre-dates the vendoring).
  PATCH_COVERAGE_HELPER="$SCRIPT_DIR/patch-coverage.sh"
  if [ ! -x "$PATCH_COVERAGE_HELPER" ]; then
    PATCH_COVERAGE_HELPER="$HOME/.claude/scripts/patch-coverage.sh"
  fi
  if [ ! -x "$PATCH_COVERAGE_HELPER" ]; then
    echo "pre-push-gate: patch-coverage.sh not found in scripts/ or ~/.claude/scripts/" >&2
    exit 1
  fi
  if COVER_OUT="$COVER_OUT" PATCH_COVERAGE_THRESHOLD=78 \
      PATCH_COVERAGE_EXCLUDE="*_templ.go cmd/stillwater/main.go scripts/" \
      bash "$PATCH_COVERAGE_HELPER"; then
    :
  else
    echo "WARN: if this looks spurious, run \`RUN_RACE=1 bash scripts/pre-push-gate.sh\` for the full-profile (CI-equivalent) coverage." >&2
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
# RUN_PROVIDER_SMOKE three-state gate, mirroring the RUN_RACE / RUN_VULN
# pattern above: CI's "Provider Failure Smoke" job (gate.yml) is a required
# check, gated there by a dorny/paths-filter on '**/*.go',
# scripts/smoke-provider-failure.sh, scripts/pre-push-gate.sh,
# .github/workflows/gate.yml, go.mod, and go.sum -- so it is CI-authoritative
# and load-sensitive locally (builds a binary, boots a temporary server).
# Mirrors that same filter here so the local auto-run tracks CI's own
# relevance decision instead of drifting.
#   - RUN_PROVIDER_SMOKE truthy (1/true/yes/on): force a RUN, BLOCKING on
#     failure regardless of changed files. Prior behavior; the escape hatch
#     for a full, CI-equivalent local run.
#   - RUN_PROVIDER_SMOKE falsy  (0/false/no/off): force a SKIP (escape hatch
#     when the local server can't boot, e.g. a port conflict; CI still gates
#     this).
#   - RUN_PROVIDER_SMOKE unset (auto, the DEFAULT): run iff Go source or the
#     smoke/gate scripts or go.mod/go.sum changed since BASE, otherwise SKIP.
#     A failure in this auto path is ADVISORY (warn, don't block): CI's
#     required job is the strict, authoritative gate.
provider_smoke_flag="$(printf '%s' "${RUN_PROVIDER_SMOKE:-}" | tr '[:upper:]' '[:lower:]' | tr -d '[:space:]')"

# provider_smoke_relevant: mirrors gate.yml's provider-failure-smoke
# paths-filter (Go source, the smoke/gate scripts themselves, or the
# dependency manifests).
provider_smoke_relevant() {
  [ -n "$MODIFIED_GO_FILES" ] && return 0
  git diff --name-only "$BASE" HEAD 2>/dev/null | grep -qE '^(go\.(mod|sum)|scripts/smoke-provider-failure\.sh|scripts/pre-push-gate\.sh|\.github/workflows/gate\.yml)$'
}

SMOKE_FAILURE_SCRIPT="$SCRIPT_DIR/smoke-provider-failure.sh"

run_provider_smoke=0
provider_smoke_blocking=0
case "$provider_smoke_flag" in
  0 | false | no | off)
    echo "provider-smoke: skipped (RUN_PROVIDER_SMOKE=${RUN_PROVIDER_SMOKE} forces opt-out; CI still runs the provider failure smoke)"
    ;;
  1 | true | yes | on)
    run_provider_smoke=1
    provider_smoke_blocking=1
    ;;
  *)
    if ! provider_smoke_relevant; then
      echo "provider-smoke: skipped (no Go/smoke-script/go.mod/go.sum changes since BASE; set RUN_PROVIDER_SMOKE=1 to force)"
    else
      run_provider_smoke=1
    fi
    ;;
esac

if [ "$run_provider_smoke" -eq 1 ]; then
  if [ ! -x "$SMOKE_FAILURE_SCRIPT" ]; then
    echo "pre-push-gate: smoke-provider-failure.sh not found or not executable in scripts/" >&2
    exit 1
  fi
  if ! bash "$SMOKE_FAILURE_SCRIPT" 2>&1; then
    echo ""
    if [ "$provider_smoke_blocking" -eq 1 ]; then
      echo "FAIL: provider failure smoke test reported failures (see output above)."
      exit 1
    fi
    echo "WARN: provider-smoke: advisory failure in the auto path -- not blocking this push."
    echo "WARN: provider-smoke: CI's Provider Failure Smoke job is authoritative; set RUN_PROVIDER_SMOKE=1 to make this blocking locally."
  else
    echo "OK"
  fi
fi

echo ""
echo "=== Accessibility (axe-core) ==="
# Runs the axe-core rendered-contrast smoke when a11y-relevant files changed
# since BASE, mirroring CI's changes filter (.github/workflows/ci.yml a11y
# paths), so a WCAG regression is caught locally instead of only by the CI a11y
# job (the #2139 gap, where a borderline /next/settings contrast failure passed
# the local gate and only CI caught it). `make test-a11y` builds the binary,
# boots an ephemeral server, and runs Playwright + @axe-core/playwright.
#
# Degrades gracefully (#2140), and stays self-contained (no shared state with
# other steps) to minimize merge conflicts with sibling branches:
#   - RUN_A11Y truthy (1/true/yes/on): force a RUN, BLOCKING on failure
#     regardless of changed files.
#   - RUN_A11Y falsy  (0/false/no/off): force a SKIP (escape hatch when the
#     Playwright toolchain is unavailable and you must push anyway; CI still
#     gates a11y).
#   - RUN_A11Y unset (auto, the default): run iff a11y-relevant files changed
#     AND the Playwright toolchain is installed; otherwise SKIP -- never FAIL --
#     so a fresh clone without `npx playwright install` can still push, matching
#     the oasdiff/python3 optional-tool SKIP pattern above. A failure in this
#     auto path is ADVISORY (warn, don't block): #2223 root-caused a local-only
#     harness flake (a CPU-starved theme-toggle timeout, not a real contrast
#     violation) that hard-blocked pushes on unrelated changes. CI runs the
#     full suite and remains the strict, authoritative a11y gate.
a11y_flag="$(printf '%s' "${RUN_A11Y:-}" | tr '[:upper:]' '[:lower:]' | tr -d '[:space:]')"

# a11y_changed: did this branch touch any file CI's a11y filter watches?
a11y_changed() {
  git diff --name-only "$BASE" HEAD 2>/dev/null \
    | grep -qE '^(web/templates/|web/static/css/|web/static/js/|tests/a11y/|playwright\.config\.js|package(-lock)?\.json)|\.templ$'
}
# a11y_toolchain_ready: are the Playwright browser binaries available? make
# test-a11y runs `npm ci` itself (so node deps are not the gating concern) but
# never `npx playwright install`, so the BROWSER cache is the real dependency.
# Its location is platform-specific -- ~/.cache (Linux/CI), ~/Library/Caches
# (macOS), %LOCALAPPDATA% (Windows) -- and PLAYWRIGHT_BROWSERS_PATH overrides it,
# so probe all of them rather than a single Linux path.
a11y_toolchain_ready() {
  command -v npx >/dev/null 2>&1 || return 1
  for d in "${PLAYWRIGHT_BROWSERS_PATH:-}" "$HOME/.cache/ms-playwright" "$HOME/Library/Caches/ms-playwright" "${LOCALAPPDATA:-}/ms-playwright"; do
    [ -n "$d" ] && [ -d "$d" ] && return 0
  done
  return 1
}

run_a11y=0
a11y_blocking=0
case "$a11y_flag" in
  0 | false | no | off)
    echo "a11y: skipped (RUN_A11Y=${RUN_A11Y} forces opt-out; CI still gates a11y)"
    ;;
  1 | true | yes | on)
    run_a11y=1
    a11y_blocking=1
    ;;
  *)
    if ! a11y_changed; then
      echo "a11y: skipped (no a11y-relevant changes since BASE; set RUN_A11Y=1 to force)"
    elif ! a11y_toolchain_ready; then
      echo "a11y: skipped (Playwright browsers not installed -- run 'npx playwright install chromium' to enable locally; CI still gates a11y)"
    else
      run_a11y=1
    fi
    ;;
esac

if [ "$run_a11y" -eq 1 ]; then
  if ! make test-a11y; then
    echo ""
    if [ "$a11y_blocking" -eq 1 ]; then
      echo "FAIL: accessibility (axe-core) smoke tests reported failures (see output above)."
      exit 1
    fi
    echo "WARN: a11y: advisory failure in the auto path (see #2223) -- not blocking this push."
    echo "WARN: a11y: CI runs the full suite and enforces it strictly; set RUN_A11Y=1 to make this blocking locally."
  else
    echo "OK"
  fi
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
  echo "pre-push-gate: prefs-coverage.py not found in scripts/ or ~/.claude/scripts/ -- skipping (advisory tool missing)"
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

echo "=== Bruno route parity check ==="
# Verify every /api/v1 route registered in internal/api/router.go is either
# exercised by a Bruno request (api/bruno/**/*.bru) or explicitly recorded in
# api/bruno/parity-ignore.json. Catches a new API endpoint shipped without an
# accompanying Bruno smoke/contract request. Self-contained; mirrors the fuzz
# matrix drift guard above. Hard-fail on non-zero exit.
BRUNO_PARITY_SCRIPT="$SCRIPT_DIR/check-bruno-parity.sh"
if [ ! -x "$BRUNO_PARITY_SCRIPT" ]; then
  echo "pre-push-gate: check-bruno-parity.sh not found or not executable in scripts/" >&2
  exit 1
fi
if ! bash "$BRUNO_PARITY_SCRIPT"; then
  echo ""
  echo "FAIL: Bruno route parity check reported drift (see output above)."
  exit 1
fi

echo ""
echo "All hard checks passed. Proceed with /pr-review-toolkit:review-pr."
