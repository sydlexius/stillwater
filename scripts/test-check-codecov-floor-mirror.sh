#!/usr/bin/env bash
#
# test-check-codecov-floor-mirror.sh -- tests for
# scripts/check-codecov-floor-mirror.sh (#2756).
#
# Hermetic: every case builds its own throwaway testdata/coverage-floor.json
# and codecov.yml fixtures under a temp dir and points the check at them via
# --floor/equivalent env, never touching this repo's real files.
#
# Run: bash scripts/test-check-codecov-floor-mirror.sh

set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
CHECK="$REPO_ROOT/scripts/check-codecov-floor-mirror.sh"

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

# new_fixture <name> -- creates $WORK/<name>/{testdata/coverage-floor.json,codecov.yml}
# as a throwaway git repo (the check does `git rev-parse --show-toplevel` and
# cd's there), and prints the dir.
new_fixture() {
    local name="$1"
    local dir="$WORK/$name"
    mkdir -p "$dir/testdata"
    git init -q "$dir"
    printf '%s\n' "$dir"
}

run_check() {
    local dir="$1"
    (cd "$dir" && bash "$CHECK" 2>&1)
}

# --------------------------------------------------------------------------
# Case 1: clean mirror -> PASS
# --------------------------------------------------------------------------
echo "Case 1: matching floor and codecov.yml -> PASS"
C1=$(new_fixture clean)
cat > "$C1/testdata/coverage-floor.json" << 'EOF'
{
  "internal/api": 58,
  "internal/server": 87
}
EOF
cat > "$C1/codecov.yml" << 'EOF'
coverage:
  status:
    project:
      default:
        target: auto
        threshold: 2%
      api:
        target: 58%
        paths:
          - internal/api
      server:
        target: 87%
        paths:
          - internal/server
EOF
if OUT=$(run_check "$C1"); then
    if grep -q 'OK:' <<< "$OUT"; then
        ok "exit 0 and reports OK"
    else
        bad "exit 0 but no OK message" "$OUT"
    fi
else
    bad "rejected a genuinely clean mirror" "$OUT"
fi

# --------------------------------------------------------------------------
# Case 2: the real #2756 divergence -> FAIL, names the entry
# --------------------------------------------------------------------------
echo
echo "Case 2: value divergence (server 87 vs 91) -> FAIL naming internal/server"
C2=$(new_fixture divergent)
cat > "$C2/testdata/coverage-floor.json" << 'EOF'
{
  "internal/api": 58,
  "internal/server": 87
}
EOF
cat > "$C2/codecov.yml" << 'EOF'
coverage:
  status:
    project:
      default:
        target: auto
        threshold: 2%
      api:
        target: 58%
        paths:
          - internal/api
      server:
        target: 91%
        paths:
          - internal/server
EOF
if OUT=$(run_check "$C2"); then
    bad "PASSED despite a genuine value divergence" "$OUT"
else
    if grep -q 'internal/server' <<< "$OUT" && grep -q '87' <<< "$OUT" && grep -q '91' <<< "$OUT"; then
        ok "blocked, naming the diverging package and both values"
    else
        bad "blocked, but message omits the package or one of the values" "$OUT"
    fi
fi

# --------------------------------------------------------------------------
# Case 3: multiple divergences all reported, not just the first
# --------------------------------------------------------------------------
echo
echo "Case 3: two divergences -> both reported"
C3=$(new_fixture multi-divergent)
cat > "$C3/testdata/coverage-floor.json" << 'EOF'
{
  "internal/api": 58,
  "internal/server": 87
}
EOF
cat > "$C3/codecov.yml" << 'EOF'
coverage:
  status:
    project:
      default:
        target: auto
        threshold: 2%
      api:
        target: 60%
        paths:
          - internal/api
      server:
        target: 91%
        paths:
          - internal/server
EOF
if OUT=$(run_check "$C3"); then
    bad "PASSED despite two genuine divergences" "$OUT"
else
    if grep -q 'internal/api' <<< "$OUT" && grep -q 'internal/server' <<< "$OUT"; then
        ok "both divergences reported in one run"
    else
        bad "only one divergence reported (should be both)" "$OUT"
    fi
fi

# --------------------------------------------------------------------------
# Case 4: floor entry with no codecov.yml counterpart -> FAIL
# --------------------------------------------------------------------------
echo
echo "Case 4: floor entry missing from codecov.yml -> FAIL"
C4=$(new_fixture missing-in-codecov)
cat > "$C4/testdata/coverage-floor.json" << 'EOF'
{
  "internal/api": 58,
  "internal/newpkg": 70
}
EOF
cat > "$C4/codecov.yml" << 'EOF'
coverage:
  status:
    project:
      default:
        target: auto
        threshold: 2%
      api:
        target: 58%
        paths:
          - internal/api
EOF
if OUT=$(run_check "$C4"); then
    bad "PASSED although internal/newpkg has no codecov.yml entry" "$OUT"
else
    if grep -q 'internal/newpkg' <<< "$OUT"; then
        ok "blocked, naming the floor entry missing from codecov.yml"
    else
        bad "blocked, but did not name the missing package" "$OUT"
    fi
fi

# --------------------------------------------------------------------------
# Case 5: codecov.yml entry with no floor counterpart (stale entry) -> FAIL
# --------------------------------------------------------------------------
echo
echo "Case 5: stale codecov.yml entry (no floor entry) -> FAIL"
C5=$(new_fixture stale-codecov)
cat > "$C5/testdata/coverage-floor.json" << 'EOF'
{
  "internal/api": 58
}
EOF
cat > "$C5/codecov.yml" << 'EOF'
coverage:
  status:
    project:
      default:
        target: auto
        threshold: 2%
      api:
        target: 58%
        paths:
          - internal/api
      removed:
        target: 70%
        paths:
          - internal/removedpkg
EOF
if OUT=$(run_check "$C5"); then
    bad "PASSED although codecov.yml has a stale entry with no floor counterpart" "$OUT"
else
    if grep -q 'internal/removedpkg' <<< "$OUT"; then
        ok "blocked, naming the stale codecov.yml entry"
    else
        bad "blocked, but did not name the stale entry" "$OUT"
    fi
fi

# --------------------------------------------------------------------------
# Case 6: name-mapping mismatch is irrelevant -- match is by paths:, not label
# --------------------------------------------------------------------------
# Would catch: an implementation that hardcodes a label<->path naming rule
# (e.g. s/\//_/g against a stripped "internal/" prefix) instead of reading the
# paths: line. A deliberately weird label must still match correctly.
echo
echo "Case 6: an unconventional codecov.yml label still matches via paths:"
C6=$(new_fixture weird-label)
cat > "$C6/testdata/coverage-floor.json" << 'EOF'
{
  "internal/connection/mediabrowser": 88
}
EOF
cat > "$C6/codecov.yml" << 'EOF'
coverage:
  status:
    project:
      default:
        target: auto
        threshold: 2%
      totally_unrelated_label_name:
        target: 88%
        paths:
          - internal/connection/mediabrowser
EOF
if OUT=$(run_check "$C6"); then
    ok "matched purely by paths:, regardless of the label naming convention"
else
    bad "FAILED to match an entry whose label doesn't follow the usual convention" "$OUT"
fi

# --------------------------------------------------------------------------
# Case 7: default: entry is never treated as a floor entry
# --------------------------------------------------------------------------
# Would catch: a parser that trips on `default:` (no `paths:` key) and either
# crashes or misreports it as a missing-floor-entry package.
echo
echo "Case 7: the 'default' catch-all entry does not trigger a false positive"
C7=$(new_fixture default-entry)
cat > "$C7/testdata/coverage-floor.json" << 'EOF'
{
  "internal/api": 58
}
EOF
cat > "$C7/codecov.yml" << 'EOF'
coverage:
  status:
    project:
      default:
        target: auto
        threshold: 2%
      api:
        target: 58%
        paths:
          - internal/api
EOF
if OUT=$(run_check "$C7"); then
    if grep -qi 'default' <<< "$OUT"; then
        bad "'default' catch-all leaked into the output as if it were a real entry" "$OUT"
    else
        ok "'default' catch-all correctly ignored"
    fi
else
    bad "FAILED on a clean fixture containing only the expected 'default' entry" "$OUT"
fi

# --------------------------------------------------------------------------
# Case 8: missing floor file -> setup error (exit 2), not a silent pass
# --------------------------------------------------------------------------
echo
echo "Case 8: missing testdata/coverage-floor.json -> exit 2 (setup error), not PASS"
C8=$(new_fixture missing-floor-file)
cat > "$C8/codecov.yml" << 'EOF'
coverage:
  status:
    project:
      default:
        target: auto
        threshold: 2%
EOF
set +e
OUT=$(run_check "$C8")
STATUS=$?
set -e
if [ "$STATUS" -eq 0 ]; then
    bad "PASSED with no floor file at all -- a check that passes on nothing is worse than no check" "$OUT"
elif [ "$STATUS" -eq 2 ]; then
    ok "exit 2 (setup error), distinct from exit 1 (found divergence)"
else
    bad "failed, but with exit $STATUS instead of the documented setup-error code 2" "$OUT"
fi

# --------------------------------------------------------------------------
# Case 9: missing codecov.yml -> setup error (exit 2)
# --------------------------------------------------------------------------
echo
echo "Case 9: missing codecov.yml -> exit 2 (setup error)"
C9=$(new_fixture missing-codecov-file)
cat > "$C9/testdata/coverage-floor.json" << 'EOF'
{
  "internal/api": 58
}
EOF
set +e
OUT=$(run_check "$C9")
STATUS=$?
set -e
if [ "$STATUS" -eq 2 ]; then
    ok "exit 2 (setup error) when codecov.yml is absent"
else
    bad "expected exit 2 for a missing codecov.yml, got $STATUS" "$OUT"
fi

# --------------------------------------------------------------------------
# Case 10: unparsable floor file (zero entries extracted) -> exit 2
# --------------------------------------------------------------------------
# Would catch: a floor file that is present but garbage (or a future format
# change the extractor no longer understands) being silently treated as "zero
# divergences found" instead of "could not compare at all".
echo
echo "Case 10: floor file present but unparsable -> exit 2, not a vacuous PASS"
C10=$(new_fixture garbage-floor)
echo "not json at all, just prose" > "$C10/testdata/coverage-floor.json"
cat > "$C10/codecov.yml" << 'EOF'
coverage:
  status:
    project:
      default:
        target: auto
        threshold: 2%
      api:
        target: 58%
        paths:
          - internal/api
EOF
set +e
OUT=$(run_check "$C10")
STATUS=$?
set -e
if [ "$STATUS" -eq 2 ]; then
    ok "exit 2 on an unparsable floor file, not a vacuous pass"
else
    bad "expected exit 2 on garbage input, got $STATUS (a silent pass here would be worse than no check)" "$OUT"
fi

# --------------------------------------------------------------------------
echo
echo "----------------------------------------"
echo "passed: $PASSED  failed: $FAILED"
if [ "$FAILED" -ne 0 ]; then
    exit 1
fi
echo "All codecov/floor-mirror checks behaved as specified."
