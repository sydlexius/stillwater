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

# Go-parsing tool pins: golangci-lint and govulncheck do not merely COMPILE this
# module, they parse and type-check it, so each must be built with a Go at least
# as new as the `go` directive. A tool built with an older Go fails hard on a
# newer target -- golangci-lint refuses its config ("the Go language version
# (go1.26) used to build golangci-lint is lower than the targeted Go version"),
# and govulncheck panics inside its vendored x/tools SSA builder ("panic:
# unexpected expr"). Neither is a version STRING that can be compared against
# go.mod: the coupled quantity is the Go release each tool BINARY was built
# with, which is knowable only by downloading it (govulncheck's is transitive,
# buried in its own go.mod as an x/tools version).
#
# So this is a tripwire, not an equality check. TOOL_PINS_VALIDATED_FOR_GO
# records the `go` directive these pins were last confirmed working against.
# Bumping go.mod without revalidating trips it here, offline and in one second,
# instead of in CI three jobs deep. Discovered the expensive way in #3158, where
# a Dependabot base-image bump took out Lint, Build, and the vulnerability scan
# in sequence, each behind a separate red CI round.
#
# To clear a trip: bump the tool pins if needed, confirm both run clean against
# the new toolchain, then set this to the new go.mod value.
TOOL_PINS_VALIDATED_FOR_GO="1.27.0"
if [ -n "$go_mod" ] && [ "$go_mod" != "$TOOL_PINS_VALIDATED_FOR_GO" ]; then
  echo "FAIL: go-parsing tool pins were validated for go $TOOL_PINS_VALIDATED_FOR_GO but go.mod now targets $go_mod" >&2
  echo "      golangci-lint (.github/workflows/ci.yml, .github/copilot-setup-steps.yml) and" >&2
  echo "      govulncheck (.github/workflows/security.yml, scripts/pre-push-gate.sh, Makefile)" >&2
  echo "      must each be built with a Go >= the go directive, or they fail hard on this tree." >&2
  echo "      Revalidate both against go $go_mod, then set TOOL_PINS_VALIDATED_FOR_GO in $0." >&2
  errors=$((errors + 1))
else
  echo "OK: go-parsing tool pins validated for go $TOOL_PINS_VALIDATED_FOR_GO (golangci-lint, govulncheck)"
fi


if [ "$errors" -gt 0 ]; then
  echo "" >&2
  echo "$errors tool-version drift error(s)." >&2
  echo "Run 'make sync-tool-versions' to mirror the canonical CI-side Tailwind version" >&2
  echo "into the Dockerfile pin, then commit the result. The 'go' pin has no" >&2
  echo "auto-fixer: bump go.mod and the Dockerfile tag together, then re-pin the" >&2
  echo "digest." >&2
  exit 1
fi
if [ "$fixed" -gt 0 ]; then
  echo ""
  echo "$fixed pin(s) synced. Review and commit the changes above."
  exit 0
fi
echo "All tracked tool versions are aligned."
