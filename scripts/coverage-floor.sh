#!/usr/bin/env bash
# coverage-floor.sh -- per-package coverage ratchet for internal/ packages.
#
# Enforces a one-way ratchet: each package's coverage may not drop below the
# floor stored in testdata/coverage-floor.json. A PR that degrades any
# package's total coverage fails this gate even if patch coverage passes.
#
# Written to run under Bash 3.2 (macOS system /bin/bash) as well as Bash 4+.
# No `declare -A`, no `readarray`, no `mapfile`. Associative-style operations
# use awk. The `env bash` shebang picks up a newer shell when one is installed
# but the script is safe on stock macOS.
#
# Usage:
#   coverage-floor.sh [--cover <profile>] [--floor <json>] [--bump <pkg> ...]
#                     [--lower <pkg> ...]
#
# Flags:
#   --cover <path>   Coverage profile to evaluate (default: $COVER_OUT if set,
#                    otherwise a temporary profile is generated via `go test`).
#   --floor <path>   Path to the floor JSON file (default: testdata/coverage-floor.json
#                    relative to the repo root, resolved via git rev-parse).
#   --bump <pkg>     Update the floor entry for <pkg> to the value found in the
#                    current profile, then exit 0. One-way ratchet: only raises
#                    floors, never lowers them. Repeat the flag for multiple
#                    packages (e.g. --bump internal/api --bump internal/image).
#   --lower <pkg>    Lower the floor entry for <pkg> to the current measured
#                    value, then exit 0. Symmetric counterpart to --bump: only
#                    lowers floors, never raises them. Intended for dead-code-
#                    removal PRs that legitimately reduce coverage ratios.
#                    Guards: refuses if current >= existing floor (use --bump),
#                    and errors if the package has no existing floor entry.
#                    After running --lower, also update the matching target in
#                    codecov.yml manually to keep the two files in sync.
#
# Floor lowering policy:
#   Use --lower only when ALL of the following are true:
#     1. The PR removes dead code (code with zero coverage, confirmed by
#        inspecting the coverage profile before removal).
#     2. The removal is intentional, not an accidental regression.
#     3. The PR description explains what dead code was removed and why.
#   After running --lower, commit both testdata/coverage-floor.json and the
#   corresponding codecov.yml target update in the same commit, with a message
#   of the form:
#     chore(ci): lower coverage floor for internal/<pkg> after dead code removal
#   Do NOT use --lower to paper over a genuine test-coverage regression.
#
# Inputs (environment, all optional):
#   COVER_OUT        Path to an existing coverage profile. Skips test run if set
#                    and the file is non-empty. Overridden by --cover flag.
#
# Exit codes:
#   0   All packages meet or exceed their floor (or --bump/--lower succeeded).
#   1   One or more packages are below their floor.
#   2   Configuration error (missing profile, unreadable floor file, etc.).
#
# Exclusions:
#   - *_templ.go (generated template output)
#   - *_test.go  (test code; not measured as "production" statements)
#   Only internal/ packages are evaluated -- cmd/, web/, and other top-level
#   directories are not included.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# ---------------------------------------------------------------------------
# Parse flags
# ---------------------------------------------------------------------------
cover_arg=""
floor_arg=""
bump_pkgs=""    # space-separated list of packages to bump
lower_pkgs=""   # space-separated list of packages to lower

while [ $# -gt 0 ]; do
  case "$1" in
    --cover)
      cover_arg="$2"
      shift 2
      ;;
    --floor)
      floor_arg="$2"
      shift 2
      ;;
    --bump)
      bump_pkgs="${bump_pkgs:+$bump_pkgs }$2"
      shift 2
      ;;
    --lower)
      lower_pkgs="${lower_pkgs:+$lower_pkgs }$2"
      shift 2
      ;;
    *)
      echo "coverage-floor: unknown flag: $1" >&2
      echo "Usage: $0 [--cover <profile>] [--floor <json>] [--bump <pkg> ...] [--lower <pkg> ...]" >&2
      exit 2
      ;;
  esac
done

# ---------------------------------------------------------------------------
# Resolve paths
# ---------------------------------------------------------------------------

# Repo root: used to locate go.mod, testdata/coverage-floor.json, and as the
# working directory for `go test` when we generate a fresh profile.
repo_root="$(git rev-parse --show-toplevel 2>/dev/null || echo "$PWD")"

# Floor file
if [ -n "$floor_arg" ]; then
  FLOOR_FILE="$floor_arg"
else
  FLOOR_FILE="$repo_root/testdata/coverage-floor.json"
fi

if [ ! -f "$FLOOR_FILE" ]; then
  echo "coverage-floor: floor file not found: $FLOOR_FILE" >&2
  echo "  Generate it with: $0 --bump <all packages>" >&2
  exit 2
fi

# Coverage profile: prefer explicit --cover, then $COVER_OUT env, then generate.
if [ -n "$cover_arg" ]; then
  COVER_OUT="$cover_arg"
elif [ -n "${COVER_OUT:-}" ] && [ -s "${COVER_OUT:-}" ]; then
  : # use as-is
else
  # Generate a fresh profile. Write it to the run-dir if run-paths.sh is
  # available; otherwise fall back to a temp path.
  if [ -f "$SCRIPT_DIR/lib/run-paths.sh" ]; then
    # shellcheck source=scripts/lib/run-paths.sh
    . "$SCRIPT_DIR/lib/run-paths.sh"
    COVER_OUT="$SW_RUN_DIR/floor-cover.out"
  else
    COVER_OUT="/tmp/coverage-floor-$$.out"
  fi
  echo "coverage-floor: no coverage profile supplied; running go test to generate one..."
  (cd "$repo_root" && go test -count=1 -covermode=atomic \
    -coverprofile="$COVER_OUT" ./internal/...) || {
    echo "coverage-floor: go test failed; cannot compute coverage." >&2
    exit 2
  }
fi

if [ ! -s "$COVER_OUT" ]; then
  echo "coverage-floor: coverage profile not found or empty: $COVER_OUT" >&2
  exit 2
fi

# ---------------------------------------------------------------------------
# Read the Go module name from go.mod
# ---------------------------------------------------------------------------
module=$(awk '/^module /{print $2; exit}' "$repo_root/go.mod")
if [ -z "$module" ]; then
  echo "coverage-floor: could not read module name from $repo_root/go.mod" >&2
  exit 2
fi

# ---------------------------------------------------------------------------
# Compute per-package coverage from the profile (awk, Bash-3.2 compatible)
#
# Output: one line per internal/ package:
#   <pkg> <covered_stmts> <total_stmts>
# ---------------------------------------------------------------------------
pkg_stats=$(awk -v mod="${module}/" '
  NR == 1 { next }   # skip "mode: ..."
  {
    # Field 1: module/path/file.go:sLine.sCol,eLine.eCol
    # Field 2: nstmts
    # Field 3: count (hit count)
    split($1, parts, ":")
    filepath = parts[1]
    nstmts = $2 + 0
    count  = $3 + 0

    mlen = length(mod)
    if (substr(filepath, 1, mlen) != mod) next
    rel = substr(filepath, mlen + 1)

    # Only internal/ packages
    if (substr(rel, 1, 9) != "internal/") next

    # Exclude generated templates and test files
    if (rel ~ /_templ\.go$/) next
    if (rel ~ /_test\.go$/)  next

    # Derive package path: everything up to (not including) the last component
    n = split(rel, p, "/")
    pkg = ""
    for (i = 1; i < n; i++) {
      pkg = (pkg == "") ? p[i] : pkg "/" p[i]
    }

    total[pkg]   += nstmts
    covered[pkg] += (count > 0) ? nstmts : 0
  }
  END {
    for (pkg in total) {
      print pkg, covered[pkg]+0, total[pkg]+0
    }
  }
' "$COVER_OUT" | sort)

# ---------------------------------------------------------------------------
# --bump mode: update floor entries and exit
# ---------------------------------------------------------------------------
if [ -n "$bump_pkgs" ]; then
  # For each package to bump, find its current coverage from pkg_stats and
  # update (raise only) the floor JSON entry. Uses awk to parse and rewrite
  # the JSON in-place; no jq dependency required.
  for pkg in $bump_pkgs; do
    # Find the current percentage for this package
    current=$(printf '%s\n' "$pkg_stats" | awk -v p="$pkg" '
      $1 == p {
        t = $3 + 0
        c = $2 + 0
        pct = (t > 0) ? int(c * 100 / t) : 0
        print pct
        exit
      }
    ')
    if [ -z "$current" ]; then
      echo "coverage-floor: package not found in profile: $pkg" >&2
      exit 2
    fi

    # Read the existing floor for this package (0 if not yet present)
    existing=$(awk -v p="$pkg" '
      $0 ~ "\"" p "\"" {
        match($0, /[0-9]+/)
        print substr($0, RSTART, RLENGTH)
        exit
      }
    ' "$FLOOR_FILE")
    existing=${existing:-0}

    if [ "$current" -le "$existing" ]; then
      echo "coverage-floor: --bump $pkg: current ${current}% <= floor ${existing}%; no update."
    else
      echo "coverage-floor: --bump $pkg: raising floor from ${existing}% to ${current}%"
      # Rewrite the JSON entry. Handle two cases:
      #   1. Entry already exists -- update the integer value.
      #   2. Entry does not exist -- insert a new line (before the closing '}').
      # Both transforms use a single awk pass to avoid sed -i portability issues
      # between macOS (BSD sed) and Linux (GNU sed).
      updated=$(awk -v pkg="$pkg" -v val="$current" '
        {
          # Pattern for existing entry: "internal/pkg": <number>
          entry = "\"" pkg "\":"
          if (index($0, entry) > 0) {
            # Replace the integer after the colon, preserving trailing comma if any
            match($0, /[0-9]+/)
            line = substr($0, 1, RSTART - 1) val substr($0, RSTART + RLENGTH)
            print line
            found = 1
            next
          }
          # Closing brace: if entry not found yet, insert before it
          if (/^}$/ && !found) {
            # Determine whether there are already entries (need a comma)
            if (had_entry) {
              print ","
            }
            printf "  \"%s\": %d\n", pkg, val
          }
          print
          if (!/^[[:space:]]*[{]?[[:space:]]*$/ && !/^}$/) had_entry = 1
        }
      ' "$FLOOR_FILE")
      tmp_floor="${FLOOR_FILE}.tmp.$$"
      printf '%s\n' "$updated" > "$tmp_floor"
      mv "$tmp_floor" "$FLOOR_FILE"
    fi
  done
  echo "coverage-floor: floor file updated: $FLOOR_FILE"
  exit 0
fi

# ---------------------------------------------------------------------------
# --lower mode: lower a floor entry to the current measured value and exit.
#
# Symmetric counterpart to --bump but in the opposite direction. Intended for
# dead-code-removal PRs where removing uncovered code lowers the coverage ratio
# even though no tests were deleted. Guards:
#   - Package must already have a floor entry (cannot lower what does not exist).
#   - Refuses to lower if current measured coverage >= existing floor (use --bump).
#   - Sets the floor to the current measured value exactly (not an arbitrary target).
# After running --lower, update the matching target in codecov.yml by hand to
# keep the two files in sync.
# ---------------------------------------------------------------------------
if [ -n "$lower_pkgs" ]; then
  for pkg in $lower_pkgs; do
    # Find the current percentage for this package
    current=$(printf '%s\n' "$pkg_stats" | awk -v p="$pkg" '
      $1 == p {
        t = $3 + 0
        c = $2 + 0
        pct = (t > 0) ? int(c * 100 / t) : 0
        print pct
        exit
      }
    ')
    if [ -z "$current" ]; then
      echo "coverage-floor: package not found in profile: $pkg" >&2
      exit 2
    fi

    # Read the existing floor for this package; error if absent (cannot lower
    # a package that has no floor entry -- add one with --bump first).
    existing=$(awk -v p="$pkg" '
      $0 ~ "\"" p "\"" {
        match($0, /[0-9]+/)
        print substr($0, RSTART, RLENGTH)
        exit
      }
    ' "$FLOOR_FILE")
    if [ -z "$existing" ]; then
      echo "coverage-floor: --lower $pkg: no existing floor entry; use --bump to create one." >&2
      exit 2
    fi

    if [ "$current" -ge "$existing" ]; then
      echo "coverage-floor: --lower $pkg: current ${current}% >= floor ${existing}%; no update (use --bump to raise)."
    else
      echo "coverage-floor: --lower $pkg: floor ${existing}% -> ${current}% (dead code removal)"
      # Rewrite the existing JSON entry in-place. Unlike --bump, --lower will
      # never need to INSERT a new entry (we error above if it is absent), so
      # only the "entry already exists" branch fires here. The awk pass is
      # kept structurally identical to --bump for maintainability.
      updated=$(awk -v pkg="$pkg" -v val="$current" '
        {
          entry = "\"" pkg "\":"
          if (index($0, entry) > 0) {
            match($0, /[0-9]+/)
            line = substr($0, 1, RSTART - 1) val substr($0, RSTART + RLENGTH)
            print line
            found = 1
            next
          }
          if (/^}$/ && !found) {
            if (had_entry) {
              print ","
            }
            printf "  \"%s\": %d\n", pkg, val
          }
          print
          if (!/^[[:space:]]*[{]?[[:space:]]*$/ && !/^}$/) had_entry = 1
        }
      ' "$FLOOR_FILE")
      tmp_floor="${FLOOR_FILE}.tmp.$$"
      printf '%s\n' "$updated" > "$tmp_floor"
      mv "$tmp_floor" "$FLOOR_FILE"
    fi
  done
  echo "coverage-floor: floor file updated: $FLOOR_FILE"
  echo "coverage-floor: reminder -- update the matching target(s) in codecov.yml to stay in sync."
  exit 0
fi

# ---------------------------------------------------------------------------
# Check mode: compare each package against its floor
# ---------------------------------------------------------------------------
failures=""
printf "Per-package coverage vs floor:\n"
printf "  %-50s %7s %7s %7s\n" "package" "current" "floor" "delta"
printf "  %-50s %7s %7s %7s\n" "-------" "-------" "-----" "-----"

# Read all floor entries from the JSON file using awk (no jq, no gawk).
# Each JSON line has the form:  "internal/foo/bar": 75
# Output: <pkg> <floor_pct>
floor_entries=$(awk '
  /"internal\// {
    # Portable extraction: strip leading whitespace and the opening quote,
    # then split on quote to get the key, then parse the integer value.
    line = $0
    sub(/^[[:space:]]*"/, "", line)
    n = split(line, kv, "\"")
    key = kv[1]
    # kv[2] is the closing quote; the value follows ": " after it
    rest = substr(line, length(kv[1]) + 2)   # skip key + closing quote
    sub(/^[[:space:]]*:[[:space:]]*/, "", rest)
    sub(/[^0-9].*/, "", rest)
    val = rest + 0
    if (key ~ /^internal\// && val != "") print key, val
  }
' "$FLOOR_FILE")

if [ -z "$floor_entries" ]; then
  echo "coverage-floor: no entries found in $FLOOR_FILE" >&2
  exit 2
fi

fail_count=0

# Iterate over floor entries and compare to current profile
while IFS= read -r fline; do
  [ -z "$fline" ] && continue
  floor_pkg=$(printf '%s' "$fline" | awk '{print $1}')
  floor_pct=$(printf '%s' "$fline" | awk '{print $2}')

  # Look up current coverage for this package
  current_line=$(printf '%s\n' "$pkg_stats" | awk -v p="$floor_pkg" '$1 == p {print; exit}')
  if [ -z "$current_line" ]; then
    # Package not found in profile: treat as 0% (all statements missed)
    current_pct=0
  else
    c=$(printf '%s' "$current_line" | awk '{print $2}')
    t=$(printf '%s' "$current_line" | awk '{print $3}')
    current_pct=$(awk -v c="$c" -v t="$t" 'BEGIN { printf "%d", (t > 0) ? int(c * 100 / t) : 0 }')
  fi

  delta=$(( current_pct - floor_pct ))
  delta_str=$(awk -v d="$delta" 'BEGIN { printf "%+d", d }')

  if [ "$current_pct" -lt "$floor_pct" ]; then
    printf "  FAIL %-46s %6d%% %6d%% %6s%%\n" \
      "$floor_pkg" "$current_pct" "$floor_pct" "$delta_str"
    fail_count=$(( fail_count + 1 ))
    failures="${failures}${failures:+, }${floor_pkg} (${current_pct}% < floor ${floor_pct}%)"
  else
    printf "  ok   %-46s %6d%% %6d%% %6s%%\n" \
      "$floor_pkg" "$current_pct" "$floor_pct" "$delta_str"
  fi
done <<EOF
$floor_entries
EOF

echo ""
if [ "$fail_count" -gt 0 ]; then
  echo "FAIL: ${fail_count} package(s) below coverage floor:"
  # Print each failure on its own line for readability
  printf '%s\n' "$failures" | tr ',' '\n' | sed 's/^ /  /'
  echo ""
  echo "To investigate: run 'go test -coverprofile=cov.out ./internal/<pkg>/...' and"
  echo "check which functions lost coverage."
  echo "To permanently raise a floor after adding tests:"
  echo "  bash $0 --bump <pkg>"
  exit 1
fi

echo "OK: all packages at or above their coverage floor."
