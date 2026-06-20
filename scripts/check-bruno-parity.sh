#!/usr/bin/env bash
#
# check-bruno-parity.sh -- API-route vs Bruno-request parity guard.
#
# Purpose
#   Flag API routes registered in internal/api/router.go that have NO
#   corresponding request in the Bruno collection (api/bruno/**/*.bru). The
#   intent mirrors the fuzz-matrix drift guard (.github/workflows/fuzz.yml +
#   the "Fuzz matrix drift check" block in scripts/pre-push-gate.sh): catch
#   silent drift -- here, a new API endpoint shipped without an accompanying
#   Bruno smoke/contract request -- and fail loudly, directing the developer
#   to either add a .bru request or explicitly ignore-list the route.
#
# How it works
#   1. ROUTES  -- extract `METHOD /path` pairs from the standard
#      `mux.Handle/HandleFunc("METHOD "+bp+"/path", ...)` registrations in
#      router.go, restricted to the /api/v1/ surface. Path params (`{id}`,
#      `{name}`, ...) normalize to a generic `{}` token.
#   2. BRUNO   -- for every request file under api/bruno/ (excluding the
#      shared collection.bru and the environments/ vars), pair its HTTP
#      method block with its `url:` line, substitute `{{apiBase}}` ->
#      `/api/v1`, strip any query string, and normalize Bruno path variables
#      (`{{artistId}}`) to the same `{}` token.
#   3. PARITY  -- a route is "covered" when at least one Bruno request of the
#      same method matches it. Matching treats a route's `{}` segment as a
#      single-segment wildcard (`[^/]+`), so a Bruno request that fills a
#      path param with a literal (e.g. /preferences/theme for the route
#      /preferences/{key}) or with a `{{var}}` both count as coverage. This
#      is why a naive `comm` set-difference is NOT sufficient here (the fuzz
#      guard can use `comm` because fuzz-target names are exact identifiers
#      with no parameter substitution); see the IMPLEMENTATION NOTE below.
#   4. IGNORE  -- routes listed in api/bruno/parity-ignore.json are exempt
#      (intentionally not Bruno-tested; each carries a documented reason).
#
# Exit codes
#   0  parity holds (every non-ignored route is covered; no stale entries).
#   1  drift: an uncovered route exists that is not ignore-listed, OR a
#      Bruno request targets a path that matches no registered route, OR a
#      structural/parse error occurred.
#
# Usage
#   bash scripts/check-bruno-parity.sh
#
# IMPLEMENTATION NOTE (plan-vs-code divergence, #1765)
#   The CR Coding Plan proposed a `comm`-based set difference after
#   normalizing path params. That works for the fuzz guard because each side
#   is a flat set of exact identifiers. Routes differ: Bruno frequently fills
#   a route's path parameter with a hardcoded literal (e.g. the route
#   /api/v1/preferences/{key} is exercised by .bru requests against
#   /preferences/theme and /preferences/font_size). Under naive normalization
#   those literals do NOT collapse to `{}`, so plain `comm` reports false
#   "missing" routes and false "stale" Bruno entries. This script therefore
#   uses a per-route wildcard match (route `{}` -> `[^/]+`) instead of a raw
#   `comm`, while preserving the guard's structure, reporting, and ignore-list
#   conventions.

set -euo pipefail

# --- locate repo root (works from any cwd, incl. a git worktree) -----------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

ROUTER="internal/api/router.go"
BRUNO_DIR="api/bruno"
IGNORE_FILE="$BRUNO_DIR/parity-ignore.json"

if [ ! -f "$ROUTER" ]; then
  echo "check-bruno-parity: router not found at $ROUTER" >&2
  exit 1
fi
if [ ! -d "$BRUNO_DIR" ]; then
  echo "check-bruno-parity: Bruno collection dir not found at $BRUNO_DIR" >&2
  exit 1
fi

WORK_DIR="$(mktemp -d)"
trap 'rm -rf "$WORK_DIR"' EXIT
routes_file="$WORK_DIR/routes.txt"
bruno_file="$WORK_DIR/bruno.txt"
ignore_file="$WORK_DIR/ignore.txt"

# --- 1. extract + normalize API routes -------------------------------------
# Matches: mux.Handle("METHOD "+bp+"/api/v1/...", ...)
#          mux.HandleFunc("METHOD "+bp+"/api/v1/...", ...)
# Captures "METHOD /api/v1/...", normalizes {param} -> {}, keeps /api/v1/ only.
grep -oE 'mux\.(Handle|HandleFunc)\("[A-Z]+ "\+bp\+"[^"]*"' "$ROUTER" \
  | sed -E 's/.*"([A-Z]+) "\+bp\+"([^"]*)".*/\1 \2/' \
  | grep -E ' /api/v1/' \
  | sed -E 's/\{[^}]*\}/{}/g' \
  | sort -u > "$routes_file"

route_count="$(wc -l < "$routes_file" | tr -d ' ')"
if [ "$route_count" -eq 0 ]; then
  echo "check-bruno-parity: extracted 0 API routes from $ROUTER -- parser likely broke" >&2
  exit 1
fi

# --- 2. extract + normalize Bruno endpoints --------------------------------
# One request per .bru file. Skip the shared collection.bru and environments/.
: > "$bruno_file"
while IFS= read -r bru; do
  # HTTP method: the verb that opens the request block, e.g. `get {`.
  # Tolerate optional leading whitespace so an indented method block (a request
  # nested inside an enclosing block) is still detected; strip that indentation
  # back off before normalizing so the captured method has no stray spaces.
  method="$(grep -ioE '^[[:space:]]*(get|post|put|patch|delete|head|options)[[:space:]]*\{' "$bru" \
    | head -n1 | sed -E 's/^[[:space:]]*//; s/[[:space:]]*\{.*//' | tr '[:lower:]' '[:upper:]')"
  # URL: first `url: {{apiBase}}...` line.
  url="$(grep -oE 'url:[[:space:]]*\{\{apiBase\}\}[^[:space:]]*' "$bru" | head -n1)"
  [ -z "$method" ] && continue
  [ -z "$url" ] && continue
  path="$(printf '%s' "$url" \
    | sed -E 's#url:[[:space:]]*\{\{apiBase\}\}#/api/v1#' \
    | sed -E 's/\?.*$//' \
    | sed -E 's/\{\{[^}]*\}\}/{}/g')"
  echo "$method $path"
done < <(find "$BRUNO_DIR" -type f -name '*.bru' \
            ! -name 'collection.bru' ! -path "$BRUNO_DIR/environments/*") \
  | sort -u > "$bruno_file"

bruno_count="$(wc -l < "$bruno_file" | tr -d ' ')"
if [ "$bruno_count" -eq 0 ]; then
  echo "check-bruno-parity: extracted 0 Bruno endpoints from $BRUNO_DIR -- parser likely broke" >&2
  exit 1
fi

# --- 3. load ignore list ----------------------------------------------------
# parity-ignore.json: a JSON array of objects, each {"route": "...",
# "reason": "..."}. Only the "route" values matter to the guard; "reason"
# documents why the route is intentionally not Bruno-tested. Parsed with a
# tolerant grep so no jq dependency is introduced (mirrors the pure-shell
# fuzz guard).
: > "$ignore_file"
if [ -f "$IGNORE_FILE" ]; then
  # `|| true`: an empty ignore list (`[]`) yields no grep match (exit 1),
  # which must not abort the run under `set -e`.
  { grep -oE '"route"[[:space:]]*:[[:space:]]*"[^"]*"' "$IGNORE_FILE" || true; } \
    | sed -E 's/.*"route"[[:space:]]*:[[:space:]]*"([^"]*)".*/\1/' \
    | sort -u > "$ignore_file"
fi
ignore_count="$(wc -l < "$ignore_file" | tr -d ' ')"

# --- 4. parity analysis -----------------------------------------------------
# A route is covered if some Bruno endpoint of the same method matches it,
# treating the route's `{}` segments as single-segment wildcards.
route_to_regex() {
  # stdin: "METHOD /api/v1/a/{}/b" -> "^METHOD /api/v1/a/[^/]+/b$"
  #   1. protect the `{}` param token with a sentinel that contains no regex
  #      metacharacters (route paths never contain '@');
  #   2. escape regex metacharacters in the remaining literal route text --
  #      the class deliberately omits '[' and ']' (which never appear in a
  #      route path and whose presence in a BSD-sed bracket expression is a
  #      parse error);
  #   3. expand the sentinel to a single-path-segment wildcard;
  #   4. anchor both ends.
  # The `&` / `$` inside the single-quoted sed scripts are sed syntax, not
  # shell expansion -- intentional, hence the disable below.
  # shellcheck disable=SC2016
  sed -E \
    -e 's/\{\}/@WILD@/g' \
    -e 's/[.$*^()+?|\\]/\\&/g' \
    -e 's/@WILD@/[^\/]+/g' \
    -e 's/^/^/' \
    -e 's/$/$/'
}

missing_file="$WORK_DIR/missing.txt"
stale_file="$WORK_DIR/stale.txt"
: > "$missing_file"
: > "$stale_file"

# Pre-build a file of route regexes for the reverse (stale) check.
route_regex_file="$WORK_DIR/route-regex.txt"
while IFS= read -r route; do
  printf '%s\n' "$route" | route_to_regex
done < "$routes_file" > "$route_regex_file"

# Forward: routes with no covering Bruno request.
while IFS= read -r route; do
  regex="$(printf '%s\n' "$route" | route_to_regex)"
  if ! grep -qE "$regex" "$bruno_file"; then
    # not covered -- is it ignore-listed?
    if ! grep -qxF "$route" "$ignore_file"; then
      echo "$route" >> "$missing_file"
    fi
  fi
done < "$routes_file"

# Reverse: Bruno requests that match no registered route (stale or typo'd).
while IFS= read -r endpoint; do
  if ! grep -qE -f "$route_regex_file" <(printf '%s\n' "$endpoint"); then
    echo "$endpoint" >> "$stale_file"
  fi
done < "$bruno_file"

# Obsolete ignore entries: an ignore-listed route that either no longer
# exists in router.go, or is now actually covered by a Bruno request. Both
# mean the entry should be removed. This is the bidirectional half of the
# guard -- the fuzz drift check likewise fails on "extra" matrix entries.
obsolete_file="$WORK_DIR/obsolete-ignore.txt"
: > "$obsolete_file"
while IFS= read -r route; do
  [ -z "$route" ] && continue
  if ! grep -qxF "$route" "$routes_file"; then
    echo "$route  (route no longer registered)" >> "$obsolete_file"
    continue
  fi
  regex="$(printf '%s\n' "$route" | route_to_regex)"
  if grep -qE "$regex" "$bruno_file"; then
    echo "$route  (now covered by a Bruno request)" >> "$obsolete_file"
  fi
done < "$ignore_file"

missing_count="$(wc -l < "$missing_file" | tr -d ' ')"
stale_count="$(wc -l < "$stale_file" | tr -d ' ')"
obsolete_count="$(wc -l < "$obsolete_file" | tr -d ' ')"

echo "API routes (/api/v1):      $route_count"
echo "Bruno requests:            $bruno_count"
echo "Ignore-listed routes:      $ignore_count"

fail=0
if [ "$missing_count" -ne 0 ]; then
  fail=1
  echo ""
  echo "FAIL: $missing_count API route(s) have no corresponding Bruno request"
  echo "      and are not listed in $IGNORE_FILE:"
  sed 's/^/    /' "$missing_file"
  echo ""
  echo "  Fix: add a Bruno request under $BRUNO_DIR/ exercising the route,"
  echo "       or add it to $IGNORE_FILE with a documented reason."
fi
if [ "$stale_count" -ne 0 ]; then
  fail=1
  echo ""
  echo "FAIL: $stale_count Bruno request(s) target a path matching no"
  echo "      registered /api/v1 route (stale or mistyped):"
  sed 's/^/    /' "$stale_file"
  echo ""
  echo "  Fix: correct the .bru url, or remove the obsolete request."
fi
if [ "$obsolete_count" -ne 0 ]; then
  fail=1
  echo ""
  echo "FAIL: $obsolete_count entr(y/ies) in $IGNORE_FILE are obsolete:"
  sed 's/^/    /' "$obsolete_file"
  echo ""
  echo "  Fix: remove the listed entr(y/ies) from $IGNORE_FILE."
fi

if [ "$fail" -ne 0 ]; then
  exit 1
fi

echo "OK: every API route is covered by a Bruno request or ignore-listed; no stale requests."
