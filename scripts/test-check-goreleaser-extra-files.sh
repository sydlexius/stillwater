#!/usr/bin/env bash
#
# test-check-goreleaser-extra-files.sh -- tests for
# scripts/check-goreleaser-extra-files.sh (#3034).
#
# Hermetic: every case builds its own throwaway build/docker/Dockerfile.goreleaser
# and .goreleaser.yml fixtures under a temp dir, as its own git repo (the
# check does `git rev-parse --show-toplevel` and cd's there), and never
# touches this repo's real files.
#
# Run: bash scripts/test-check-goreleaser-extra-files.sh

set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
CHECK="$REPO_ROOT/scripts/check-goreleaser-extra-files.sh"
# Strip the inherited git environment before any fixture is built: `git init`
# honours a hook-supplied GIT_DIR and would write into the MAIN repository's
# shared config instead (#3051; see the library header for the mechanism).
# shellcheck source=scripts/lib/git-clean-env.sh
. "$REPO_ROOT/scripts/lib/git-clean-env.sh"
git_clean_env_unset

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

# new_fixture <name> -- creates $WORK/<name>/build/docker/ as a throwaway
# git repo and prints the dir.
new_fixture() {
    local name="$1"
    local dir="$WORK/$name"
    mkdir -p "$dir/build/docker"
    git init -q "$dir"
    printf '%s\n' "$dir"
}

run_check() {
    local dir="$1"
    (cd "$dir" && bash "$CHECK" 2>&1)
}

# --------------------------------------------------------------------------
# Case 1: every COPY source listed in extra_files: -> PASS
# --------------------------------------------------------------------------
echo "Case 1: Dockerfile.goreleaser COPY sources all present in extra_files: -> PASS"
C1=$(new_fixture clean)
cat > "$C1/build/docker/Dockerfile.goreleaser" << 'EOF'
FROM alpine:3.24
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/stillwater /usr/local/bin/stillwater
COPY build/docker/entrypoint.sh /entrypoint.sh
COPY build/docker/healthcheck.sh /healthcheck.sh
RUN chmod +x /entrypoint.sh /healthcheck.sh
EOF
cat > "$C1/.goreleaser.yml" << 'EOF'
dockers_v2:
  - dockerfile: build/docker/Dockerfile.goreleaser
    extra_files:
      - build/docker/entrypoint.sh
      - build/docker/healthcheck.sh
EOF
if out=$(run_check "$C1"); then
    if echo "$out" | grep -q "^OK:"; then
        ok "clean mirror passes"
    else
        bad "clean mirror" "expected an OK: line" "$out"
    fi
else
    bad "clean mirror should exit 0" "$out"
fi

# --------------------------------------------------------------------------
# Case 2: THE #3034 REGRESSION -- a new COPY added to Dockerfile.goreleaser
# without a matching extra_files: entry -> FAIL
# --------------------------------------------------------------------------
echo "Case 2: COPY added without extra_files: entry (#3034 regression) -> FAIL"
C2=$(new_fixture missing)
cat > "$C2/build/docker/Dockerfile.goreleaser" << 'EOF'
FROM alpine:3.24
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/stillwater /usr/local/bin/stillwater
COPY build/docker/entrypoint.sh /entrypoint.sh
COPY build/docker/healthcheck.sh /healthcheck.sh
RUN chmod +x /entrypoint.sh /healthcheck.sh
EOF
cat > "$C2/.goreleaser.yml" << 'EOF'
dockers_v2:
  - dockerfile: build/docker/Dockerfile.goreleaser
    extra_files:
      - build/docker/entrypoint.sh
EOF
if out=$(run_check "$C2"); then
    bad "missing entry should exit non-zero" "$out"
else
    if echo "$out" | grep -q "healthcheck.sh"; then
        ok "missing entry fails and names the offending file"
    else
        bad "missing entry failed but did not name healthcheck.sh" "$out"
    fi
fi

# --------------------------------------------------------------------------
# Case 3: $TARGETPLATFORM-staged binary is correctly ignored (not a repo file)
# --------------------------------------------------------------------------
echo "Case 3: \$TARGETPLATFORM COPY is not treated as a repo file -> PASS"
C3=$(new_fixture targetplatform)
cat > "$C3/build/docker/Dockerfile.goreleaser" << 'EOF'
FROM alpine:3.24
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/stillwater /usr/local/bin/stillwater
COPY build/docker/entrypoint.sh /entrypoint.sh
EOF
cat > "$C3/.goreleaser.yml" << 'EOF'
dockers_v2:
  - dockerfile: build/docker/Dockerfile.goreleaser
    extra_files:
      - build/docker/entrypoint.sh
EOF
if out=$(run_check "$C3"); then
    ok "\$TARGETPLATFORM-staged binary ignored, passes on entrypoint.sh alone"
else
    bad "\$TARGETPLATFORM case should pass" "$out"
fi

# --------------------------------------------------------------------------
# Case 4: setup error -- missing Dockerfile.goreleaser -> exit 2
# --------------------------------------------------------------------------
echo "Case 4: missing Dockerfile.goreleaser -> setup error (exit 2)"
C4=$(new_fixture no_dockerfile)
cat > "$C4/.goreleaser.yml" << 'EOF'
dockers_v2:
  - extra_files:
      - build/docker/entrypoint.sh
EOF
set +e
out=$(run_check "$C4")
rc=$?
set -e
if [ "$rc" -eq 2 ]; then
    ok "missing Dockerfile.goreleaser exits 2"
else
    bad "expected exit 2, got $rc" "$out"
fi

echo ""
echo "=== $PASSED passed, $FAILED failed ==="
[ "$FAILED" -eq 0 ]
