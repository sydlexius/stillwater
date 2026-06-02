#!/usr/bin/env bash
# check-tool-versions.sh -- assert lint/spell tool versions agree across the
# multiple entrypoints that each pin them independently:
#
#   * .githooks/pre-commit        (canonical bash hook; npx pin for markdownlint)
#   * .pre-commit-config.yaml     (pre-commit framework alternative; rev: pins)
#   * .github/workflows/ci.yml    (golangci-lint-action version:)
#   * .github/workflows/docs.yml  (crate-ci/typos SHA pin + "# vX.Y.Z" comment)
#
# It also asserts TAILWIND_VERSION parity between the composite action
# (.github/actions/setup-tailwind, the CI glibc x64 pin) and build/docker/Dockerfile
# (the musl pins); only the version is shared, the SHAs intentionally differ.
#
# A version drift between these is a silent correctness gap: a contributor's
# local hook can pass while CI fails (or vice versa) on the same tree, and
# //nolint directives can resolve differently across golangci-lint minor
# versions (#1560). This guard fails the pre-push gate when any pair drifts.
#
# Dependabot angle: Dependabot updates the CI-side pins (e.g. the crate-ci/typos
# SHA in docs.yml via its github-actions ecosystem) but has NO pre-commit
# ecosystem, so it cannot touch the matching `.pre-commit-config.yaml` rev:.
# A typos bump therefore drifts this gate and stalls the auto-merge pipeline.
# Run this script with --fix (or `make sync-tool-versions`) to mirror the
# CI-side version into the pin Dependabot cannot reach, then push the one-line
# follow-up commit onto the Dependabot branch so its required checks re-run.
#
# Usage:
#   check-tool-versions.sh          verify only (default; used by the gate)
#   check-tool-versions.sh --fix    rewrite each drifted derived pin to match
#                                   its canonical (CI/source) side, then report
#
# Exit: 0 = all aligned (or, with --fix, all drifts were auto-fixed),
#       1 = drift detected in verify mode or a pin could not be located.
set -euo pipefail

FIX=0
for arg in "$@"; do
  case "$arg" in
    --fix) FIX=1 ;;
    -h | --help)
      # Reprint the Usage block from this header.
      sed -n '/^# Usage:/,/^# *$/p' "$0" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      echo "check-tool-versions.sh: unknown argument '$arg' (try --fix or --help)" >&2
      exit 2
      ;;
  esac
done

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_ROOT"

errors=0
fixed=0

# Extract the `rev:` value that follows a given repo URL substring in
# .pre-commit-config.yaml. The pre-commit format lists `- repo: <url>` then a
# `rev:` line, so track whether we are inside the wanted repo block. Returns
# empty (handled by the caller) if the repo or its rev is absent.
precommit_rev() {
  awk -v want="$1" '
    /^[[:space:]]*-[[:space:]]*repo:/ { inrepo = (index($0, want) > 0); next }
    inrepo && /^[[:space:]]*rev:/ {
      sub(/^[[:space:]]*rev:[[:space:]]*/, "")  # drop the "rev:" key
      sub(/[[:space:]]*#.*/, "")                # drop any inline comment
      gsub(/[\042\047\r]/, "")                  # drop quotes (" and ) and CR
      sub(/[[:space:]]+$/, "")                  # drop trailing whitespace
      print
      exit
    }
  ' .pre-commit-config.yaml
}

# Rewrite the `rev:` value inside the `.pre-commit-config.yaml` block whose
# `- repo:` line contains <want>, preserving indentation and any trailing
# comment. Only the first rev: in the block is touched. Used by --fix to mirror
# a CI-side version into the pin Dependabot cannot reach.
fix_precommit_rev() {
  local want="$1" newver="$2" file=".pre-commit-config.yaml" tmp
  tmp="$(mktemp)"
  awk -v want="$want" -v newver="$newver" '
    /^[[:space:]]*-[[:space:]]*repo:/ { inrepo = (index($0, want) > 0) }
    inrepo && /^[[:space:]]*rev:/ {
      match($0, /^[[:space:]]*rev:[[:space:]]*/)   # capture "  rev: " prefix
      prefix = substr($0, 1, RLENGTH)
      rest = substr($0, RLENGTH + 1)
      comment = ""                                  # keep any trailing "# ..."
      if (match(rest, /[[:space:]]*#.*/)) { comment = substr(rest, RSTART) }
      print prefix newver comment
      inrepo = 0                                    # first rev only
      next
    }
    { print }
  ' "$file" >"$tmp" && mv "$tmp" "$file"
}

# Rewrite every TAILWIND_VERSION=vX.Y.Z token in the Dockerfile (the musl pins)
# to <newver>. The SHAs intentionally differ per libc/arch and are left alone.
fix_dockerfile_tailwind() {
  local newver="$1" file="build/docker/Dockerfile" tmp
  tmp="$(mktemp)"
  sed -E "s/TAILWIND_VERSION=v[0-9.]+/TAILWIND_VERSION=${newver}/g" "$file" >"$tmp" \
    && mv "$tmp" "$file"
}

# require <label> <a> <b> <a-source> <b-source> [fixer-fn [fixer-args...]]
# `a`/`asrc` is the canonical (CI/source) side; `b`/`bsrc` is the derived pin.
# In --fix mode a drift is repaired by calling: <fixer-fn> [fixer-args...] <a>
# which rewrites the derived pin's file to the canonical value.
require() {
  local label="$1" a="$2" b="$3" asrc="$4" bsrc="$5"
  local fixer=("${@:6}")
  if [ -z "$a" ]; then
    echo "FAIL: $label: could not locate version in $asrc" >&2
    errors=$((errors + 1))
    return
  fi
  if [ -z "$b" ]; then
    echo "FAIL: $label: could not locate version in $bsrc" >&2
    errors=$((errors + 1))
    return
  fi
  if [ "$a" != "$b" ]; then
    if [ "$FIX" -eq 1 ] && [ "${#fixer[@]}" -gt 0 ]; then
      "${fixer[@]}" "$a"
      echo "FIXED: $label $bsrc $b -> $a (mirrored from $asrc)"
      fixed=$((fixed + 1))
    else
      echo "FAIL: $label version drift: $asrc=$a vs $bsrc=$b" >&2
      errors=$((errors + 1))
    fi
  else
    echo "OK: $label $a ($asrc == $bsrc)"
  fi
}

# Each extraction ends in `|| true` so a grep miss (exit 1) does not abort the
# script under `set -e`/`pipefail`; an empty result is reported by require().

# golangci-lint: ci.yml golangci-lint-action `version:` vs pre-commit-config rev.
# Bound the scan to the golangci-lint-action step. Steps begin with a `- ` list
# item (often `- name:` with `uses:` on the next line), so reset in_block at every
# new list item and set it true on the line naming the action. `version:` is then
# read only from within that step; if the action ever drops its `version:` input,
# a later step's `version:` is not silently picked up.
gci_ci=$(awk '
  /^[[:space:]]*-[[:space:]]/ { in_block = 0 }
  /golangci\/golangci-lint-action/ { in_block = 1 }
  in_block && /^[[:space:]]*version:[[:space:]]*v[0-9]/ {
    sub(/^[[:space:]]*version:[[:space:]]*/, "")
    print
    exit
  }
' .github/workflows/ci.yml || true)
gci_pc=$(precommit_rev 'golangci/golangci-lint' || true)
require "golangci-lint" "$gci_ci" "$gci_pc" "ci.yml" ".pre-commit-config.yaml" \
  fix_precommit_rev 'golangci/golangci-lint'

# typos: docs.yml SHA-pin "# vX.Y.Z" comment vs pre-commit-config rev.
typos_ci=$(grep -E 'crate-ci/typos@' .github/workflows/docs.yml \
  | grep -oE '#[[:space:]]*v[0-9.]+' | grep -oE 'v[0-9.]+' | head -1 || true)
typos_pc=$(precommit_rev 'crate-ci/typos' || true)
require "typos" "$typos_ci" "$typos_pc" "docs.yml" ".pre-commit-config.yaml" \
  fix_precommit_rev 'crate-ci/typos'

# markdownlint-cli2: bash-hook npx pin vs pre-commit-config rev. (docs.yml uses
# the markdownlint-cli2-ACTION, whose version is independent of the bundled
# tool version, so it is intentionally not compared here.)
mdl_hook=$(grep -oE 'markdownlint-cli2@v[0-9.]+' .githooks/pre-commit \
  | grep -oE 'v[0-9.]+' | head -1 || true)
mdl_pc=$(precommit_rev 'DavidAnson/markdownlint-cli2' || true)
require "markdownlint-cli2" "$mdl_hook" "$mdl_pc" ".githooks/pre-commit" ".pre-commit-config.yaml" \
  fix_precommit_rev 'DavidAnson/markdownlint-cli2'

# Tailwind: the composite action (CI glibc x64 binary) and the Dockerfile (musl
# binaries) pin the SAME TAILWIND_VERSION but DIFFERENT SHAs. Only the version is
# a shared invariant, so assert just the version here; the SHAs intentionally
# differ per libc/arch and are maintained independently.
tw_action=$(grep -oE 'TAILWIND_VERSION:[[:space:]]*v[0-9.]+' .github/actions/setup-tailwind/action.yml \
  | grep -oE 'v[0-9.]+' | head -1 || true)
tw_docker=$(grep -oE 'TAILWIND_VERSION=v[0-9.]+' build/docker/Dockerfile \
  | grep -oE 'v[0-9.]+' | head -1 || true)
require "tailwind" "$tw_action" "$tw_docker" ".github/actions/setup-tailwind" "build/docker/Dockerfile" \
  fix_dockerfile_tailwind

if [ "$errors" -gt 0 ]; then
  echo "" >&2
  echo "$errors tool-version drift error(s)." >&2
  echo "Run 'make sync-tool-versions' to mirror the CI-side versions into the" >&2
  echo "derived pins, then commit the result. (Dependabot cannot edit pre-commit" >&2
  echo "rev: pins, so its tool bumps land here as drift -- this is the fix.)" >&2
  exit 1
fi
if [ "$fixed" -gt 0 ]; then
  echo ""
  echo "$fixed pin(s) synced. Review and commit the changes above."
  exit 0
fi
echo "All tracked tool versions are aligned."
