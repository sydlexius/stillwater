#!/usr/bin/env bash
# check-tool-versions.sh -- assert the Tailwind version pinned for CI agrees
# with the version pinned for the Docker image:
#
#   * .github/actions/setup-tailwind (the CI glibc x64 pin)
#   * build/docker/Dockerfile         (the musl pins)
#
# Only the version is the shared invariant; the SHAs intentionally differ per
# libc/arch and are maintained independently. A version drift between these two
# is a silent correctness gap (CI lints with one Tailwind, the shipped image
# builds with another), so this guard fails the pre-push gate when they differ.
#
# History: this script previously also asserted that the lint/spell tool
# versions (golangci-lint, crate-ci/typos, markdownlint-cli2) matched between
# the CI side and the `rev:` pins in .pre-commit-config.yaml. Those pre-commit
# `rev:` pins were a redundant second copy: the canonical hook
# (.githooks/pre-commit, wired via core.hooksPath) and CI already run those
# tools, and Dependabot has no pre-commit ecosystem, so it could never sync the
# `rev:` side -- every github-actions tool bump landed here as drift and stalled
# the auto-merge pipeline. The redundant lint hooks were removed from
# .pre-commit-config.yaml so there is nothing left to drift; only the Tailwind
# parity (which Dependabot does not touch) remains.
#
# Usage:
#   check-tool-versions.sh          verify only (default; used by the gate)
#   check-tool-versions.sh --fix    rewrite the Dockerfile TAILWIND_VERSION to
#                                   match the CI/source side, then report
#
# Exit: 0 = aligned (or, with --fix, the drift was auto-fixed),
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

# Rewrite every TAILWIND_VERSION=vX.Y.Z token in the Dockerfile (the musl pins)
# to <newver>. The SHAs intentionally differ per libc/arch and are left alone.
fix_dockerfile_tailwind() {
  local newver="$1" file="build/docker/Dockerfile" tmp
  tmp="$(mktemp)"
  sed -E "s/TAILWIND_VERSION=v[0-9.]+/TAILWIND_VERSION=${newver}/g" "$file" >"$tmp" \
    && mv "$tmp" "$file"
}

# Mirror the CI-declared bundled markdownlint-cli2 version into the local hook's
# pin. The CI side (the action pin + its `# bundles:` declaration) is canonical:
# it is what actually gates a PR, so the hook follows it, never the reverse.
fix_hook_markdownlint() {
  local newver="$1" file=".githooks/pre-commit" tmp
  tmp="$(mktemp)"
  sed -E "s/MARKDOWNLINT_CLI2_VERSION=\"[0-9.]+\"/MARKDOWNLINT_CLI2_VERSION=\"${newver}\"/" "$file" >"$tmp" \
    && mv "$tmp" "$file" && chmod +x "$file"
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

# Go: the go.mod `go` directive and the Dockerfile golang base-image tag must
# pin the same version, so the release image builds on the same toolchain as
# CI / local dev (#2009 #7). The Dockerfile also @sha256-pins the image (the
# real reproducibility guarantee), but the tag must still read the correct
# patch for humans and for this guard to catch a go.mod bump that forgot the
# Dockerfile. No auto-fixer: bump go.mod + the Dockerfile tag together, then
# re-pin the digest.
go_mod=$(grep -oE '^go[[:space:]]+[0-9.]+' go.mod | grep -oE '[0-9.]+' | head -1 || true)
go_docker=$(grep -oE 'golang:[0-9.]+' build/docker/Dockerfile | grep -oE '[0-9.]+' | head -1 || true)
require "go" "$go_mod" "$go_docker" "go.mod" "build/docker/Dockerfile"

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

# markdownlint-cli2: CI runs DavidAnson/markdownlint-cli2-action, whose ACTION
# version (v24.x) is a DIFFERENT numbering scheme from the markdownlint-cli2 CLI
# it bundles (0.23.x). The local pre-commit hook pins the CLI by its npm version,
# so the two sides cannot be compared textually -- the action's bundled version
# is only visible inside its package.json.
#
# The `# bundles: markdownlint-cli2 X.Y.Z` comment beside the action pin in
# docs.yml is the anchor that relates them, and this check asserts the hook's pin
# matches it. Dependabot bumps the action's SHA but NOT that comment, so a
# CI-only bump lands as drift HERE and fails closed, forcing both sides to move
# together. Without this, CI silently runs a different linter major than the hook
# and the hook passes while CI fails on the same tree (#2500).
mdl_ci=$(grep -oE '^[[:space:]]*#[[:space:]]*bundles:[[:space:]]*markdownlint-cli2[[:space:]]+[0-9.]+' \
  .github/workflows/docs.yml | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1 || true)
mdl_hook=$(grep -oE 'MARKDOWNLINT_CLI2_VERSION="[0-9.]+"' .githooks/pre-commit \
  | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1 || true)
require "markdownlint-cli2" "$mdl_ci" "$mdl_hook" \
  ".github/workflows/docs.yml (bundles: comment)" ".githooks/pre-commit" \
  fix_hook_markdownlint

if [ "$errors" -gt 0 ]; then
  echo "" >&2
  echo "$errors tool-version drift error(s)." >&2
  echo "Run 'make sync-tool-versions' to mirror each canonical CI-side version into" >&2
  echo "its derived pin (Tailwind -> the Dockerfile; markdownlint-cli2 -> the" >&2
  echo "pre-commit hook), then commit the result. The 'go' pin has no auto-fixer:" >&2
  echo "bump go.mod and the Dockerfile tag together, then re-pin the digest." >&2
  exit 1
fi
if [ "$fixed" -gt 0 ]; then
  echo ""
  echo "$fixed pin(s) synced. Review and commit the changes above."
  exit 0
fi
echo "All tracked tool versions are aligned."
