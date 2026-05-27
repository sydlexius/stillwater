#!/usr/bin/env bash
# smoke-provider-failure.sh -- provider-failure injection smoke test
#
# Starts a temporary Stillwater instance with SW_FORCE_PROVIDER_ERROR set
# to all real providers, drives each surface in the coverage matrix, and
# asserts that every covered handler communicates the failure rather than
# silently returning empty/incomplete data.
#
# Usage:
#   bash scripts/smoke-provider-failure.sh
#
# Environment (all optional):
#   SW_BINARY     -- path to a pre-built binary (default: ./stillwater)
#   SW_BASE       -- base URL for the injected instance (default: http://localhost:19730)
#   SW_USER       -- admin username (default: admin)
#   SW_PASS       -- admin password (default: testpassword123testpassword123)
#
# Exit codes:
#   0 -- all assertions passed
#   1 -- one or more assertions failed (see FAILED list in output)
#   2 -- setup/infrastructure failure (could not start binary, etc.)
#
# Output is tee'd to $SW_RUN_DIR/smoke-provider-failure.log per the
# repo convention in scripts/lib/run-paths.sh.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
. "$SCRIPT_DIR/lib/run-paths.sh"

LOG_FILE="$SW_RUN_DIR/smoke-provider-failure.log"

# Re-exec with tee so output lands in the log AND on stdout.
# Guard against infinite re-exec if tee fails to open the log.
if [[ -z "${_SPF_LOGGED:-}" ]]; then
  export _SPF_LOGGED=1
  exec > >(tee "$LOG_FILE") 2>&1
fi

echo "======================================================="
echo "  Provider Failure Smoke Test"
echo "  Log: $LOG_FILE"
echo "======================================================="
echo ""

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

SW_BINARY="${SW_BINARY:-$SCRIPT_DIR/../stillwater}"
SW_BASE="${SW_BASE:-http://localhost:19730}"
SW_USER="${SW_USER:-admin}"
SW_PASS="${SW_PASS:-testpassword123testpassword123}"

# Derive the listen port from SW_BASE so a non-default SW_BASE (e.g. a
# different port for parallel smoke runs) launches the binary on the
# matching port instead of the hard-coded 19730 the health check then
# fails to reach. The sed regex matches "http(s)://<host>:<port>" and
# returns empty for URLs without an explicit port (e.g. "http://localhost")
# or IPv6 bracketed hosts ("http://[::1]:..."), both of which fall back to
# the 19730 default on the next line -- safe for the smoke use case where
# the operator overriding SW_BASE controls the port either way.
SW_PORT="${SW_PORT:-$(printf '%s' "$SW_BASE" | sed -nE 's#^https?://[^:/]+:([0-9]+).*$#\1#p')}"
SW_PORT="${SW_PORT:-19730}"

# All real providers -- must match provider.NameXxx constants in provider.go.
ALL_PROVIDERS="musicbrainz,fanarttv,audiodb,discogs,lastfm,wikidata,duckduckgo,deezer,genius,wikipedia,spotify"

PASS=0
FAIL=0
FAILURES=()

SWPID=""
COOKIE_JAR=""
TOKEN=""
TOKEN_ID=""

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

assert_pass() {
  local label="$1"
  echo "[PASS] $label"
  PASS=$((PASS + 1))
}

assert_fail() {
  local label="$1"
  local detail="$2"
  echo "[FAIL] $label -- $detail"
  FAIL=$((FAIL + 1))
  FAILURES+=("$label -- $detail")
}

assert_body_contains() {
  local label="$1"
  local needle="$2"
  local body="$3"
  if echo "$body" | grep -qF "$needle"; then
    assert_pass "$label"
  else
    assert_fail "$label" "response did not contain: $needle"
    echo "  (first 400 chars of response:)"
    echo "  ${body:0:400}"
  fi
}

assert_status() {
  local label="$1"
  local expected="$2"
  local got="$3"
  if [[ "$got" == "$expected" ]]; then
    echo "[PASS] $label -- HTTP $got"
    PASS=$((PASS + 1))
  else
    echo "[FAIL] $label -- expected HTTP $expected, got $got"
    FAIL=$((FAIL + 1))
    FAILURES+=("$label -- expected HTTP $expected, got $got")
  fi
}

# ---------------------------------------------------------------------------
# Cleanup trap
# ---------------------------------------------------------------------------

cleanup() {
  if [[ -n "$TOKEN" && -n "$TOKEN_ID" ]]; then
    curl -s -o /dev/null -X DELETE \
      -H "Authorization: Bearer $TOKEN" \
      "$SW_BASE/api/v1/auth/tokens/$TOKEN_ID" || true
  fi
  [[ -n "$COOKIE_JAR" ]] && rm -f "$COOKIE_JAR"
  if [[ -n "$SWPID" ]]; then
    kill "$SWPID" 2>/dev/null || true
    # Give the process a moment to exit cleanly.
    local i=0
    while kill -0 "$SWPID" 2>/dev/null && [[ $i -lt 10 ]]; do
      sleep 0.3
      i=$((i + 1))
    done
    # Only SIGKILL if the PID still refers to a live process. The wait
    # loop exits either because the child is gone (and the kernel may
    # have already recycled the PID) or because the 3 s cap fired; the
    # unconditional kill -9 in the second case could otherwise terminate
    # an unrelated process that happened to inherit the recycled PID.
    if kill -0 "$SWPID" 2>/dev/null; then
      kill -9 "$SWPID" 2>/dev/null || true
    fi
  fi
  if [[ -n "${_SPF_TMPDIR:-}" && -d "$_SPF_TMPDIR" ]]; then
    rm -rf "$_SPF_TMPDIR"
  fi
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# Build binary (uses cached binary if already present)
# ---------------------------------------------------------------------------

if [[ ! -x "$SW_BINARY" ]]; then
  echo "Binary not found at $SW_BINARY -- building..."
  (cd "$SCRIPT_DIR/.." && make build) || {
    echo "FATAL: make build failed." >&2
    exit 2
  }
fi

# ---------------------------------------------------------------------------
# Start injected instance
# ---------------------------------------------------------------------------

# Spin up a temporary data directory so this run does not touch any real DB.
_SPF_TMPDIR=$(mktemp -d "$SW_RUN_DIR/spf-data.XXXXXX")

echo "Starting injected instance..."
echo "  Binary: $SW_BINARY"
echo "  Base: $SW_BASE"
echo "  Providers injected: $ALL_PROVIDERS"
echo "  Data dir: $_SPF_TMPDIR"
echo ""

SPF_LOG="$SW_RUN_DIR/spf-server.log"
# SW_DB_PATH: override the in-code default (/config/stillwater.db) so the
# binary writes its database into our temp directory instead of the
# container-oriented /config path which is read-only on host systems.
# SW_CONFIG_PATH is intentionally unset so the scaffold step is skipped;
# env-var defaults are sufficient for a smoke instance.
SW_FORCE_PROVIDER_ERROR="$ALL_PROVIDERS" \
  SW_DB_PATH="$_SPF_TMPDIR/stillwater.db" \
  SW_PORT="$SW_PORT" \
  "$SW_BINARY" >"$SPF_LOG" 2>&1 &
SWPID=$!

# Wait for the server to become healthy (up to 20 s). Bail out early if
# the child process already exited so the operator sees the boot error
# rather than 20 s of silence followed by a "did not become healthy" message.
healthy=0
for i in $(seq 1 40); do
  if ! kill -0 "$SWPID" 2>/dev/null; then
    echo "FATAL: injected instance exited before becoming healthy." >&2
    echo "  PID: $SWPID"
    echo "  Server log (last 40 lines):"
    tail -40 "$SPF_LOG" >&2 || true
    exit 2
  fi
  if curl -s "$SW_BASE/api/v1/health" | grep -q '"status":"ok"'; then
    healthy=1
    break
  fi
  sleep 0.5
done

if [[ $healthy -eq 0 ]]; then
  echo "FATAL: injected instance did not become healthy within 20 s." >&2
  echo "  PID: $SWPID"
  echo "  Server log (last 40 lines):"
  tail -40 "$SPF_LOG" >&2
  exit 2
fi
echo "Injected instance is healthy (PID $SWPID)."
echo ""

# ---------------------------------------------------------------------------
# Auth
# ---------------------------------------------------------------------------

echo "--- Auth ---"
echo ""

COOKIE_JAR="$SW_RUN_DIR/spf-cookies.jar"
: > "$COOKIE_JAR"

resp=$(curl -s -c "$COOKIE_JAR" -w "\n%{http_code}" "$SW_BASE/api/v1/health")
code=$(echo "$resp" | tail -n 1)
assert_status "GET /api/v1/health (injected instance)" "200" "$code"

# The injected instance starts with an empty database. Create the admin account
# via the OOBE setup endpoint (unauthenticated and CSRF-exempt on first run).
setup_payload=$(jq -nc --arg u "$SW_USER" --arg p "$SW_PASS" \
  '{auth_method:"local",username:$u,password:$p}')
setup_resp=$(curl -s -w "\n%{http_code}" \
  -X POST "$SW_BASE/api/v1/auth/setup" \
  -H "Content-Type: application/json" \
  -d "$setup_payload")
setup_code=$(echo "$setup_resp" | tail -n 1)
# 201 = created; 409 = already exists (acceptable if DB was somehow reused).
# Branch the PASS message so the 409 case (admin already present) does not
# get reported as a failure by assert_status's strict "got != expected" path.
if [[ "$setup_code" != "201" && "$setup_code" != "409" ]]; then
  setup_body=$(echo "$setup_resp" | sed '$d')
  echo "FATAL: POST /api/v1/auth/setup returned HTTP $setup_code" >&2
  echo "  body: ${setup_body:0:400}" >&2
  exit 2
fi
if [[ "$setup_code" == "201" ]]; then
  assert_pass "POST /api/v1/auth/setup (create admin)"
else
  assert_pass "POST /api/v1/auth/setup (admin already exists, HTTP 409)"
fi

login_payload=$(jq -nc --arg u "$SW_USER" --arg p "$SW_PASS" '{username: $u, password: $p}')
login_resp=$(printf '%s' "$login_payload" | curl -s -c "$COOKIE_JAR" -b "$COOKIE_JAR" -w "\n%{http_code}" \
  -X POST "$SW_BASE/api/v1/auth/login" \
  -H "Content-Type: application/json" \
  -d @-)
login_code=$(echo "$login_resp" | tail -n 1)
assert_status "POST /api/v1/auth/login (injected instance)" "200" "$login_code"
if [[ "$login_code" != "200" ]]; then
  echo "FATAL: login failed on injected instance." >&2
  exit 2
fi

CSRF_TOKEN=$(grep "csrf_token" "$COOKIE_JAR" | awk '{print $NF}' | tail -1 || true)
if [[ -z "$CSRF_TOKEN" ]]; then
  echo "FATAL: csrf_token cookie not found." >&2
  exit 2
fi

token_resp=$(curl -s -b "$COOKIE_JAR" -w "\n%{http_code}" \
  -X POST "$SW_BASE/api/v1/auth/tokens" \
  -H "Content-Type: application/json" \
  -H "X-CSRF-Token: $CSRF_TOKEN" \
  -d '{"name":"spf-smoke","scopes":"read,write,admin"}')
token_body=$(echo "$token_resp" | sed '$d')
token_code=$(echo "$token_resp" | tail -n 1)
assert_status "POST /api/v1/auth/tokens (mint)" "201" "$token_code"

TOKEN=$(echo "$token_body" | jq -r '.token' 2>/dev/null || echo "")
TOKEN_ID=$(echo "$token_body" | jq -r '.id' 2>/dev/null || echo "")
if [[ -z "$TOKEN" || "$TOKEN" == "null" ]]; then
  echo "FATAL: failed to mint API token on injected instance." >&2
  exit 2
fi
echo "  Token minted: ${TOKEN:0:12}... (id=$TOKEN_ID)"
echo ""

AUTH=(-H "Authorization: Bearer $TOKEN")

# ---------------------------------------------------------------------------
# Provision a test artist via library scan
# ---------------------------------------------------------------------------

echo "--- Artist Provisioning ---"
echo ""

# Artists are created by the library scanner, not directly via the API.
# Create a minimal music-directory hierarchy, register it as a library,
# and trigger a scan so the scanner creates a test artist row in the DB.
MUSIC_DIR="$_SPF_TMPDIR/music"
ARTIST_DIR="$MUSIC_DIR/Smoke Test Artist"
mkdir -p "$ARTIST_DIR"

echo "  Music dir: $MUSIC_DIR"

lib_resp=$(curl -s -w "\n%{http_code}" "${AUTH[@]}" \
  -X POST "$SW_BASE/api/v1/libraries" \
  -H "Content-Type: application/json" \
  -H "X-CSRF-Token: $CSRF_TOKEN" \
  -d "{\"name\":\"spf-smoke\",\"path\":\"$MUSIC_DIR\",\"type\":\"regular\"}")
lib_code=$(echo "$lib_resp" | tail -n 1)
lib_body=$(echo "$lib_resp" | sed '$d')
assert_status "POST /api/v1/libraries (smoke library)" "201" "$lib_code"
if [[ "$lib_code" != "201" ]]; then
  echo "FATAL: could not create smoke library: ${lib_body:0:300}" >&2
  exit 2
fi
LIB_ID=$(echo "$lib_body" | jq -r '.id // empty' 2>/dev/null || true)
echo "  Library ID: $LIB_ID"

# Trigger a full scan so the scanner discovers the smoke library and creates
# the artist row. The scanner/run endpoint takes no body and scans all
# registered libraries.
scan_resp=$(curl -s -w "\n%{http_code}" "${AUTH[@]}" \
  -X POST "$SW_BASE/api/v1/scanner/run" \
  -H "X-CSRF-Token: $CSRF_TOKEN")
scan_code=$(echo "$scan_resp" | tail -n 1)
# 202 = accepted (async scan started).
if [[ "$scan_code" != "200" && "$scan_code" != "202" ]]; then
  scan_body=$(echo "$scan_resp" | sed '$d')
  echo "FATAL: scanner/run returned HTTP $scan_code: ${scan_body:0:300}" >&2
  exit 2
fi

# Poll for the artist to appear (up to 15 s).
ARTIST_ID=""
for i in $(seq 1 30); do
  ARTIST_ID=$(curl -s "${AUTH[@]}" "$SW_BASE/api/v1/artists?page_size=1" \
    | jq -r '.artists[0].id // empty' 2>/dev/null || true)
  if [[ -n "$ARTIST_ID" ]]; then
    break
  fi
  sleep 0.5
done

if [[ -z "$ARTIST_ID" ]]; then
  echo "FATAL: no artist appeared within 15 s after scan." >&2
  exit 2
fi
echo "  Test artist ID: $ARTIST_ID"
echo ""

# ---------------------------------------------------------------------------
# Coverage matrix assertions
# ---------------------------------------------------------------------------

echo "--- Provider Failure Coverage Matrix ---"
echo ""

# ---- Row 1: Single-artist Re-identify search --------------------------------
# POST /api/v1/artists/{id}/refresh/search (HTMX fragment)
# Asserts: response body contains data-testid="providers-unreachable-banner"
# (added in PR #1664)
echo "[ Row 1: Single-artist Re-identify search ]"
search_resp=$(curl -s "${AUTH[@]}" \
  -X POST "$SW_BASE/api/v1/artists/$ARTIST_ID/refresh/search" \
  -H "Content-Type: application/json" \
  -H "X-CSRF-Token: $CSRF_TOKEN" \
  -H "HX-Request: true" \
  -d '{"query":"Smoke Test Artist"}' || echo "")
assert_body_contains \
  "POST /api/v1/artists/$ARTIST_ID/refresh/search -- providers-unreachable-banner present" \
  'data-testid="providers-unreachable-banner"' \
  "$search_resp"
echo ""

# ---- Row 2: Wizard step renderer -------------------------------------------
# GET /artists/re-identify/wizard/{sid}/step/{idx}
# Asserts: response body contains wizard error heading
# ("Could not load candidates" from en.json key
#  artists.bulk.reidentify.wizard.error.heading)
#
# To exercise this we need a wizard session. Start one by POSTing to the
# wizard start endpoint with our test artist.
echo "[ Row 2: Wizard step renderer ]"
wizard_start_resp=$(curl -s -w "\n%{http_code}" "${AUTH[@]}" \
  -X POST "$SW_BASE/api/v1/artists/re-identify/wizard" \
  -H "Content-Type: application/json" \
  -H "X-CSRF-Token: $CSRF_TOKEN" \
  -d "{\"artist_ids\":[\"$ARTIST_ID\"]}")
wizard_start_code=$(echo "$wizard_start_resp" | tail -n 1)
wizard_start_body=$(echo "$wizard_start_resp" | sed '$d')

if [[ "$wizard_start_code" == "200" || "$wizard_start_code" == "201" ]]; then
  SESSION_ID=$(echo "$wizard_start_body" | jq -r '.session_id // empty' 2>/dev/null || true)
  if [[ -n "$SESSION_ID" ]]; then
    # Wait briefly for the first step lookup to complete (it runs async).
    sleep 1
    # GET the wizard step page (full HTML page -- session cookie required)
    step_resp=$(curl -s -b "$COOKIE_JAR" \
      "$SW_BASE/artists/re-identify/wizard/$SESSION_ID/step/0" || echo "")
    assert_body_contains \
      "GET /artists/re-identify/wizard/$SESSION_ID/step/0 -- wizard error banner present" \
      "Could not load candidates" \
      "$step_resp"
  else
    assert_fail "GET wizard step -- failed to extract session_id from start response" \
      "start body: ${wizard_start_body:0:200}"
  fi
else
  # Row 2 is WARN-NOT-FAIL until the wizard start + session flow is stabilized
  # under injection. The single-artist search (Row 1) is the primary hard gate.
  # TODO: harden once session-start error handling in injection mode is verified.
  # Tracking issue: #1666 follow-up (wizard session start under full injection)
  echo "[WARN] POST wizard start returned HTTP $wizard_start_code (non-fatal; see TODO above)"
  echo "  body: ${wizard_start_body:0:200}"
fi
echo ""

# ---- Row 3: Single-artist refresh (WARN-NOT-FAIL) --------------------------
# POST /api/v1/artists/{id}/refresh
# The response or HTMX fragment must carry a warnings/failed_providers field.
# Currently SILENT -- this surface has not been fixed yet.
# TODO: flip to hard-fail once #1666 follow-up surface-fix issue lands.
echo "[ Row 3: Single-artist refresh (warn-not-fail -- surface fix pending) ]"
refresh_resp=$(curl -s -w "\n%{http_code}" "${AUTH[@]}" \
  -X POST "$SW_BASE/api/v1/artists/$ARTIST_ID/refresh" \
  -H "Content-Type: application/json" \
  -H "X-CSRF-Token: $CSRF_TOKEN" \
  -d '{}')
refresh_code=$(echo "$refresh_resp" | tail -n 1)
refresh_body=$(echo "$refresh_resp" | sed '$d')
# HTTP 200 is expected; warn if response carries no failure signal.
if [[ "$refresh_code" == "200" ]]; then
  if echo "$refresh_body" | grep -qE '"warnings"|"failed_providers"|providers-unreachable-banner'; then
    assert_pass "POST /api/v1/artists/$ARTIST_ID/refresh -- failure signal present"
  else
    echo "[WARN] POST /api/v1/artists/$ARTIST_ID/refresh -- no failure signal in response (silent failure)"
    echo "       TODO surface-fix: add warnings/failed_providers field (tracked in #1666)"
  fi
else
  echo "[WARN] POST /api/v1/artists/$ARTIST_ID/refresh returned HTTP $refresh_code (non-fatal; surface fix pending)"
fi
echo ""

# ---- Row 4: Bulk identify (WARN-NOT-FAIL) ----------------------------------
# POST /api/v1/artists/bulk/identify
# Should return per-artist status with error category distinguishing
# "no match" from "providers down". Currently does not.
# TODO: flip to hard-fail once follow-up surface-fix issue lands.
echo "[ Row 4: Bulk identify (warn-not-fail -- surface fix pending) ]"
bulk_resp=$(curl -s -w "\n%{http_code}" "${AUTH[@]}" \
  -X POST "$SW_BASE/api/v1/artists/bulk/identify" \
  -H "Content-Type: application/json" \
  -H "X-CSRF-Token: $CSRF_TOKEN" \
  -d "{\"artist_ids\":[\"$ARTIST_ID\"]}")
bulk_code=$(echo "$bulk_resp" | tail -n 1)
bulk_body=$(echo "$bulk_resp" | sed '$d')
# HTTP 200 accepted; warn if no provider-error category visible.
if [[ "$bulk_code" == "200" || "$bulk_code" == "202" ]]; then
  if echo "$bulk_body" | grep -qE '"provider_error"|"providers_unreachable"|"error_category"'; then
    assert_pass "POST /api/v1/artists/bulk/identify -- provider-error category present"
  else
    echo "[WARN] POST /api/v1/artists/bulk/identify -- no provider-error category (silent failure)"
    echo "       TODO surface-fix: distinguish 'no match' vs 'providers down' (tracked in #1666)"
  fi
else
  echo "[WARN] POST /api/v1/artists/bulk/identify returned HTTP $bulk_code (non-fatal; surface fix pending)"
fi
echo ""

# ---- Row 5: Image search (WARN-NOT-FAIL) -----------------------------------
# GET /api/v1/artists/{id}/images/search
# Should carry warning when all image providers errored. Currently silent.
# TODO: flip to hard-fail once follow-up surface-fix issue lands.
echo "[ Row 5: Image search (warn-not-fail -- surface fix pending) ]"
img_resp=$(curl -s -w "\n%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/artists/$ARTIST_ID/images/search")
img_code=$(echo "$img_resp" | tail -n 1)
img_body=$(echo "$img_resp" | sed '$d')
if [[ "$img_code" == "200" ]]; then
  if echo "$img_body" | grep -qE '"warnings"|"failed_providers"|providers-unreachable'; then
    assert_pass "GET /api/v1/artists/$ARTIST_ID/images/search -- failure signal present"
  else
    echo "[WARN] GET /api/v1/artists/$ARTIST_ID/images/search -- no failure signal (silent failure)"
    echo "       TODO surface-fix: add warnings field for all-providers-failed (tracked in #1666)"
  fi
else
  echo "[WARN] GET /api/v1/artists/$ARTIST_ID/images/search returned HTTP $img_code (non-fatal; surface fix pending)"
fi
echo ""

# ---- Row 6: Per-field provider lookup (partial -- verify) ------------------
# GET /api/v1/artists/{id}/fields/{field}/providers
# Should carry per-provider error state.
echo "[ Row 6: Per-field provider lookup ]"
field_resp=$(curl -s -w "\n%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/artists/$ARTIST_ID/fields/biography/providers")
field_code=$(echo "$field_resp" | tail -n 1)
field_body=$(echo "$field_resp" | sed '$d')
if [[ "$field_code" == "200" ]]; then
  # Each provider should report an error rather than silently return empty data.
  if echo "$field_body" | grep -qE '"error"|"failed"|"unavailable"'; then
    assert_pass "GET /api/v1/artists/$ARTIST_ID/fields/biography/providers -- per-provider error state present"
  else
    echo "[WARN] GET /api/v1/artists/$ARTIST_ID/fields/biography/providers -- no per-provider error state"
    echo "       TODO surface-fix: surface per-provider errors in field provider response (tracked in #1666)"
  fi
else
  echo "[WARN] GET /api/v1/artists/$ARTIST_ID/fields/biography/providers returned HTTP $field_code (non-fatal)"
fi
echo ""

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------

TOTAL=$((PASS + FAIL))
echo "======================================================="
echo "=== RESULTS: $PASS passed, $FAIL failed (of $TOTAL checks) ==="
echo "======================================================="

if [[ ${#FAILURES[@]} -gt 0 ]]; then
  echo ""
  echo "FAILED:"
  for f in "${FAILURES[@]}"; do
    echo "  $f"
  done
  echo ""
  exit 1
fi

echo ""
echo "All provider-failure assertions passed."
exit 0
