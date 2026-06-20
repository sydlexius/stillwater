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
echo "=== Tests ==="
go test -race -count=1 -covermode=atomic -coverprofile="$COVER_OUT" ./...

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
MODIFIED_GO_FILES=$(git diff --name-only --diff-filter=ACMR "$BASE" -- '*.go' \
  | grep -v '_templ\.go$' || true)
# Guard against BSD xargs (macOS) running `dirname` with zero args when the
# input is empty; GNU xargs has --no-run-if-empty but BSD does not. Empty
# file list -> empty package list -> the `if` block below skips cleanly.
if [ -n "$MODIFIED_GO_FILES" ]; then
  MODIFIED_GO_PKGS=$(printf '%s\n' "$MODIFIED_GO_FILES" \
    | xargs -n1 dirname \
    | sort -u \
    | sed 's|^|./|; s|$|/...|')
else
  MODIFIED_GO_PKGS=""
fi
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
echo "=== Mkdocs config YAML ==="
# Catch syntax errors (incl. residual conflict markers, indentation slips,
# duplicate keys) in the mkdocs config before CI's "Build site" job does.
# Stdlib PyYAML only -- no need for mkdocs itself locally. If python3 is
# missing or PyYAML is unavailable, skip with a one-line warning rather than
# fail the gate (a dev without a Python toolchain shouldn't be blocked).
if [ -f docs/site/mkdocs.yml ]; then
    if command -v python3 >/dev/null 2>&1; then
        if python3 -c 'import yaml' 2>/dev/null; then
            # MkDocs configs use PyYAML "!!python/name:" / "!!python/object"
            # tags (e.g. the pymdownx.superfences custom fence that enables
            # Mermaid rendering). safe_load rejects those legitimate tags, so
            # validate with a SafeLoader extended to treat the python-specific
            # tag families as opaque. This still catches real syntax errors
            # (residual conflict markers, indentation slips) without requiring
            # mkdocs itself to be importable here.
            if ! python3 - docs/site/mkdocs.yml 2>&1 <<'PY'
import sys
import yaml


class MkDocsLoader(yaml.SafeLoader):
    """SafeLoader that tolerates MkDocs/pymdownx python tags."""


def _ignore_python_tag(loader, suffix, node):
    return None


for _prefix in (
    "tag:yaml.org,2002:python/name:",
    "tag:yaml.org,2002:python/object:",
    "tag:yaml.org,2002:python/object/apply:",
):
    MkDocsLoader.add_multi_constructor(_prefix, _ignore_python_tag)

with open(sys.argv[1], encoding="utf-8") as fh:
    yaml.load(fh, Loader=MkDocsLoader)
PY
            then
                echo "FAIL: docs/site/mkdocs.yml is not valid YAML (see error above)."
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
    echo "SKIP: docs/site/mkdocs.yml not present"
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
# Matches codecov.yml's 75% patch threshold. The script approximates the
# same semantics locally so we catch gaps before push instead of learning
# about them from a failing codecov check.
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
if COVER_OUT="$COVER_OUT" PATCH_COVERAGE_THRESHOLD=75 \
    PATCH_COVERAGE_EXCLUDE="*_templ.go cmd/stillwater/main.go scripts/" \
    bash "$PATCH_COVERAGE_HELPER"; then
  :
else
  exit 1
fi

echo ""
echo "=== Per-package coverage floor ==="
# Enforce the one-way coverage ratchet. Each internal/ package must stay at
# or above the floor recorded in testdata/coverage-floor.json. Reuses the
# coverage profile generated by the test step above ($COVER_OUT) so no
# second test run is needed.
#
# coverage-floor.sh uses exit codes 0|1|2. Collapse non-zero to 1 here,
# consistent with how patch-coverage.sh exits are handled above.
if bash "$SCRIPT_DIR/coverage-floor.sh" --cover "$COVER_OUT"; then
  :
else
  exit 1
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
# Builds the binary (re-uses cached build if present), starts a temporary
# injected instance, drives the coverage matrix, and asserts that every
# covered surface communicates provider failures instead of silently
# returning empty data.  Hard-fail on non-zero exit from the script.
SMOKE_FAILURE_SCRIPT="$SCRIPT_DIR/smoke-provider-failure.sh"
if [ ! -x "$SMOKE_FAILURE_SCRIPT" ]; then
  echo "pre-push-gate: smoke-provider-failure.sh not found or not executable in scripts/" >&2
  exit 1
fi
if ! bash "$SMOKE_FAILURE_SCRIPT" 2>&1; then
  echo ""
  echo "FAIL: provider failure smoke test reported failures (see output above)."
  exit 1
fi
echo "OK"

echo ""
echo "=== Accessibility (axe-core) ==="
# Opt-in only. `make test-a11y` builds the binary, boots an ephemeral server,
# installs npm deps, downloads a Chromium browser, and runs the Playwright +
# @axe-core/playwright rendered-contrast smoke tests -- minutes of work plus a
# one-time browser download. That cost is inappropriate for every push, so the
# check is gated behind RUN_A11Y and SKIPS by default. CI runs the same
# target unconditionally in its dedicated a11y-test job, so default-skipping
# here only trades local speed for the CI gate, never removes coverage.
# Self-contained block (no shared state with other steps) to minimize merge
# conflicts with sibling branches that also append to this gate.
# Accept common truthy values (1/true/yes/on), case-insensitive, so
# contributors can opt in with whichever convention they reach for; anything
# else (incl. unset/empty) skips. ${RUN_A11Y:-} keeps `set -u` happy.
case "$(printf '%s' "${RUN_A11Y:-}" | tr '[:upper:]' '[:lower:]')" in
  1 | true | yes | on)
    if ! make test-a11y; then
      echo ""
      echo "FAIL: accessibility (axe-core) smoke tests reported failures (see output above)."
      exit 1
    fi
    echo "OK"
    ;;
  *)
    echo "a11y: skipped (set RUN_A11Y=1 to run; also accepts true/yes/on)"
    ;;
esac

echo ""
echo "All hard checks passed. Proceed with /pr-review-toolkit:review-pr."
