#!/usr/bin/env bash
# patch-coverage.sh -- compute local patch coverage for changed Go source.
#
# Written to run under Bash 3.2 (macOS system /bin/bash) as well as Bash
# 4+. The `env bash` shebang lets PATH pick up a newer bash when one is
# installed, but the script avoids Bash-4-only builtins (no `mapfile`,
# no associative arrays) so that `bash scripts/pre-push-gate.sh` -- which
# invokes a plain `bash` -- works on a stock macOS install.
#
# Approximates codecov.yml's patch-coverage semantics:
#   - The unit is source lines. Each line inside a coverage block is
#     treated as executable; a line is covered if any block covering it
#     was hit at least once. Lines that no block touches are ignored
#     (non-executable: comments, imports, bare declarations, etc.).
#   - Block-trailing lines whose end column <= 2 are dropped because the
#     Go coverage profile's eLine.eCol points one past the closing '}',
#     so those lines contain only a brace and are not meaningful patch.
#   - *_templ.go is excluded to match the codecov.yml ignore list.
#   - *_test.go is excluded because test code is not "patch" code.
#
# This does not match codecov's numbers exactly (codecov has its own
# block-to-line projection and counts "partials" from branch analysis
# that isn't in Go's profile). It runs conservatively: if this passes,
# codecov will almost always pass too.
#
# Inputs (all via environment; all optional):
#   COVER_OUT                  path to coverage profile (default: coverage.out)
#   BASE                       diff base (default: git merge-base main HEAD)
#   PATCH_COVERAGE_THRESHOLD   required percent (default: 70)
#
# Exit status:
#   0  patch coverage meets or exceeds the threshold (or nothing to check)
#   1  patch coverage below threshold
#   2  configuration error (missing profile, unreadable go.mod, etc.)
set -euo pipefail

COVER_OUT="${COVER_OUT:-coverage.out}"
THRESHOLD="${PATCH_COVERAGE_THRESHOLD:-70}"

# Validate the threshold up front. awk's numeric coercion quietly maps a
# non-numeric string (e.g. "foo") to 0, which would turn `p < 0` into
# false and silently false-pass the gate -- matching the fail-loudly
# pattern used for BASE and COVER_OUT above.
if ! awk -v t="$THRESHOLD" 'BEGIN { exit !(t ~ /^[0-9]+(\.[0-9]+)?$/) }'; then
  echo "patch-coverage: PATCH_COVERAGE_THRESHOLD must be numeric, got: $THRESHOLD" >&2
  exit 2
fi

# Resolve BASE. Prefer an explicit env var, otherwise fall back to
# merge-base with main. Fail loudly if neither produces a usable commit
# rather than silently comparing against a bogus ref (which would skip
# every file and exit 0 -- a false pass).
if [ -z "${BASE:-}" ]; then
  if ! BASE=$(git merge-base main HEAD 2>/dev/null); then
    echo "patch-coverage: could not resolve BASE (no 'main' branch and BASE not set)." >&2
    echo "Set BASE explicitly, e.g. BASE=\$(git merge-base origin/main HEAD)." >&2
    exit 2
  fi
fi
if ! git rev-parse --verify -q "${BASE}^{commit}" >/dev/null 2>&1; then
  echo "patch-coverage: BASE does not resolve to a commit: $BASE" >&2
  exit 2
fi

if [ ! -s "$COVER_OUT" ]; then
  echo "patch-coverage: profile not found or empty at $COVER_OUT" >&2
  echo "Generate one with: go test -coverprofile=$COVER_OUT ./..." >&2
  exit 2
fi

module=$(awk '/^module /{print $2; exit}' go.mod)
if [ -z "$module" ]; then
  echo "patch-coverage: could not read module name from go.mod" >&2
  exit 2
fi

# Pathspec: Go sources that are neither templ output nor test files.
# `--diff-filter=AMR` covers Added, Modified, and Renamed files -- the
# R is essential so a rename+edit (which git reports as R, not M) cannot
# silently bypass the gate on refactor PRs.
# The while-read loop is bash-3.2 compatible; `mapfile` is bash 4+ and
# unavailable on macOS's stock /bin/bash.
changed=()
while IFS= read -r line; do
  [ -z "$line" ] && continue
  changed+=("$line")
done < <(git diff "$BASE"..HEAD --name-only --diff-filter=AMR \
  -- '*.go' ':!*_templ.go' ':!*_test.go' ':!cmd/stillwater/main.go' | sort -u)

if [ "${#changed[@]}" -eq 0 ]; then
  echo "patch-coverage: no Go source changes in scope."
  exit 0
fi

total_exec=0
total_cov=0
printf "Patch coverage by file (threshold: %s%%):\n" "$THRESHOLD"

for file in "${changed[@]}"; do
  [ -z "$file" ] && continue

  # Added/modified line numbers for this file. -U0 gives zero context, so
  # every hunk's '+' range describes only newly added lines.
  diff_lines=$(git diff -U0 "$BASE"..HEAD -- "$file" | awk '
    /^@@/ {
      if (match($0, /\+[0-9]+(,[0-9]+)?/)) {
        spec = substr($0, RSTART+1, RLENGTH-1)
        n = split(spec, p, ",")
        start = p[1]+0
        cnt = (n > 1) ? p[2]+0 : 1
        for (i = 0; i < cnt; i++) print (start+i)
      }
    }')
  [ -z "$diff_lines" ] && continue

  # Coverage blocks for this file. Profile paths are module-relative.
  cov_path="${module}/${file}"
  cov_blocks=$(awk -v pfx="${cov_path}:" '
    BEGIN { plen = length(pfx) }
    substr($1, 1, plen) == pfx {
      # Emit: "range nstmts count" (range = sLine.sCol,eLine.eCol)
      print substr($1, plen + 1), $2, $3
    }' "$COVER_OUT")

  # Validate block range format before classification. Catching a bad
  # profile here (rather than skipping the block silently inside the
  # classifier) prevents the denominator from shrinking unnoticed and
  # the gate from false-passing with inflated coverage.
  if [ -n "$cov_blocks" ]; then
    bad=$(printf '%s\n' "$cov_blocks" | awk '
      $1 !~ /^[0-9]+\.[0-9]+,[0-9]+\.[0-9]+$/ { print $1; exit }
    ')
    if [ -n "$bad" ]; then
      echo "patch-coverage: malformed block range in $file: $bad" >&2
      exit 2
    fi
  fi

  # Classify each added line in one awk pass. Order matters: feed all
  # "D" records first so the diff set is fully populated before any "B"
  # record is processed. For each block, mark every line in its range
  # as covered (cnt > 0) or uncovered (cnt == 0), preferring "covered"
  # if any block covering the line was hit. Block-end lines with a tiny
  # end column (the trailing '}' lines) are excluded from the range.
  # The `if/fi` (rather than `[ ] && printf`) matters because this group
  # is the left side of a pipe under `set -o pipefail`; a failed test
  # would propagate as a non-zero pipeline exit and set -e would kill
  # the script on the first file without coverage blocks.
  result=$({
    printf '%s\n' "$diff_lines" | sed 's/^/D /'
    if [ -n "$cov_blocks" ]; then
      printf '%s\n' "$cov_blocks" | sed 's/^/B /'
    fi
  } | awk '
    $1 == "D" { diff[$2+0] = 1; next }
    $1 == "B" {
      split($2, r, ",")
      split(r[1], sparts, ".")
      split(r[2], eparts, ".")
      sl = sparts[1]+0; el = eparts[1]+0; ecol = eparts[2]+0
      cnt = $4+0
      # Drop the trailing line when it only holds the closing "}".
      if (ecol <= 2 && el > sl) el--
      for (ln in diff) {
        l = ln+0
        if (l >= sl && l <= el) {
          if (cnt > 0) status[l] = "c"
          else if (!(l in status)) status[l] = "u"
        }
      }
    }
    END {
      covered = 0; execcount = 0
      for (l in status) { execcount++; if (status[l] == "c") covered++ }
      print execcount+0, covered+0
    }')

  file_exec=${result% *}
  file_cov=${result#* }

  if [ "$file_exec" -gt 0 ]; then
    pct=$(awk -v c="$file_cov" -v e="$file_exec" 'BEGIN{printf "%.1f", c*100/e}')
    printf "  %-55s %6s%%  (%d/%d)\n" "$file" "$pct" "$file_cov" "$file_exec"
    total_exec=$((total_exec + file_exec))
    total_cov=$((total_cov + file_cov))
  fi
done

if [ "$total_exec" -eq 0 ]; then
  echo ""
  echo "patch-coverage: no executable lines in patch scope; nothing to enforce."
  exit 0
fi

pct=$(awk -v c="$total_cov" -v e="$total_exec" 'BEGIN{printf "%.2f", c*100/e}')
echo ""
printf "Total patch coverage: %s%% (%d/%d executable lines)\n" \
  "$pct" "$total_cov" "$total_exec"

if awk -v p="$pct" -v t="$THRESHOLD" 'BEGIN{exit !(p+0 < t+0)}'; then
  echo "FAIL: patch coverage below ${THRESHOLD}% threshold."
  exit 1
fi

echo "OK"
