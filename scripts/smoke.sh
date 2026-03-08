#!/usr/bin/env bash
# smoke.sh -- API smoke test suite for Stillwater
#
# Usage:
#   bash scripts/smoke.sh [--full]
#
# Environment:
#   SW_USER  -- admin username (default: admin)
#   SW_PASS  -- admin password (default: admin)
#   SW_BASE  -- base URL       (default: http://localhost:1973)
#
# --full enables Tier 4 destructive/stateful checks (off by default)

set -euo pipefail

SW_USER="${SW_USER:-admin}"
SW_PASS="${SW_PASS:-admin}"
SW_BASE="${SW_BASE:-http://localhost:1973}"

FULL=0
for arg in "$@"; do
  [[ "$arg" == "--full" ]] && FULL=1
done

# IDs are discovered dynamically after login; initialized empty before discovery.
ARTIST_ID=""
CONN_EMBY=""
CONN_JELLYFIN=""
CONN_LIDARR=""

PASS=0
FAIL=0
FAILURES=()

TOKEN=""
TOKEN_ID=""
COOKIE_JAR=""

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

assert_status() {
  local label="$1"
  local expected="$2"
  local got="$3"

  if [[ "$got" == "$expected" ]]; then
    echo "[PASS] $label -- $got"
    PASS=$((PASS + 1))
  else
    echo "[FAIL] $label -- expected $expected, got $got"
    FAIL=$((FAIL + 1))
    FAILURES+=("$label -- expected HTTP $expected, got $got")
  fi
}

assert_status_in() {
  local label="$1"
  local got="$2"
  shift 2
  local valid=("$@")
  for code in "${valid[@]}"; do
    if [[ "$got" == "$code" ]]; then
      echo "[PASS] $label -- $got"
      PASS=$((PASS + 1))
      return
    fi
  done
  echo "[FAIL] $label -- expected one of {${valid[*]}}, got $got"
  FAIL=$((FAIL + 1))
  FAILURES+=("$label -- expected HTTP {${valid[*]}}, got $got")
}

assert_json_field() {
  local label="$1"
  local field="$2"
  local expected="$3"
  local json="$4"

  local got
  got=$(echo "$json" | jq -r "$field" 2>/dev/null || echo "PARSE_ERROR")
  if [[ "$got" == "$expected" ]]; then
    echo "[PASS] $label -- $field=$got"
    PASS=$((PASS + 1))
  else
    echo "[FAIL] $label -- $field expected \"$expected\", got \"$got\""
    FAIL=$((FAIL + 1))
    FAILURES+=("$label -- $field expected \"$expected\", got \"$got\"")
  fi
}

assert_json_exists() {
  local label="$1"
  local field="$2"
  local json="$3"

  local got
  got=$(echo "$json" | jq -r "$field" 2>/dev/null || echo "null")
  if [[ "$got" != "null" && "$got" != "" && "$got" != "PARSE_ERROR" ]]; then
    echo "[PASS] $label -- $field present"
    PASS=$((PASS + 1))
  else
    echo "[FAIL] $label -- $field missing or null"
    FAIL=$((FAIL + 1))
    FAILURES+=("$label -- $field missing or null in response")
  fi
}

# ---------------------------------------------------------------------------
# Cleanup trap: revoke test token on exit
# ---------------------------------------------------------------------------

cleanup() {
  if [[ -n "$TOKEN" && -n "$TOKEN_ID" ]]; then
    code=$(curl -s -o /dev/null -w "%{http_code}" -X DELETE \
      -H "Authorization: Bearer $TOKEN" \
      "$SW_BASE/api/v1/auth/tokens/$TOKEN_ID")
    if [[ "$code" == "200" ]]; then
      echo ""
      echo "[cleanup] Test token $TOKEN_ID revoked."
    else
      echo ""
      echo "[cleanup] WARNING: failed to revoke token $TOKEN_ID (HTTP $code)"
    fi
  fi
  [[ -n "$COOKIE_JAR" ]] && rm -f "$COOKIE_JAR"
}
trap cleanup EXIT

echo "======================================================="
echo "  Stillwater Smoke Test"
echo "  Base: $SW_BASE"
echo "  User: $SW_USER"
echo "======================================================="
echo ""

# ---------------------------------------------------------------------------
# Tier 1: Core
# ---------------------------------------------------------------------------

echo "--- Tier 1: Core ---"
echo ""

# Create cookie jar early so the health GET can receive the csrf_token cookie
COOKIE_JAR=$(mktemp /tmp/smoke-cookies-XXXXXX)

# Health (public, no auth) -- also seeds the csrf_token cookie
resp=$(curl -s -c "$COOKIE_JAR" -w "\n%{http_code}" "$SW_BASE/api/v1/health")
body=$(echo "$resp" | sed '$d')
code=$(echo "$resp" | tail -n 1)
assert_status "GET /api/v1/health" "200" "$code"
assert_json_field "  /api/v1/health shape" ".status" "ok" "$body"

# Login (CSRF-exempt endpoint)
login_payload=$(jq -nc --arg u "$SW_USER" --arg p "$SW_PASS" '{username: $u, password: $p}')
login_resp=$(printf '%s' "$login_payload" | curl -s -c "$COOKIE_JAR" -b "$COOKIE_JAR" -w "\n%{http_code}" \
  -X POST "$SW_BASE/api/v1/auth/login" \
  -H "Content-Type: application/json" \
  -d @-)
login_body=$(echo "$login_resp" | sed '$d')
login_code=$(echo "$login_resp" | tail -n 1)
assert_status "POST /api/v1/auth/login" "200" "$login_code"

if [[ "$login_code" != "200" ]]; then
  echo ""
  echo "FATAL: login failed -- cannot proceed. Check SW_USER/SW_PASS."
  exit 1
fi

# Extract CSRF token from cookie jar (set on the health GET above)
CSRF_TOKEN=$(grep "csrf_token" "$COOKIE_JAR" | awk '{print $NF}' | tail -1 || true)
if [[ -z "$CSRF_TOKEN" ]]; then
  echo "FATAL: csrf_token cookie not found after health GET."
  exit 1
fi

# Mint API token using session cookie + CSRF token header
token_resp=$(curl -s -b "$COOKIE_JAR" -w "\n%{http_code}" \
  -X POST "$SW_BASE/api/v1/auth/tokens" \
  -H "Content-Type: application/json" \
  -H "X-CSRF-Token: $CSRF_TOKEN" \
  -d '{"name":"smoke-test","scopes":"read,write,admin"}')
token_body=$(echo "$token_resp" | sed '$d')
token_code=$(echo "$token_resp" | tail -n 1)
assert_status "POST /api/v1/auth/tokens (mint)" "201" "$token_code"

TOKEN=$(echo "$token_body" | jq -r '.token' 2>/dev/null || echo "")
TOKEN_ID=$(echo "$token_body" | jq -r '.id' 2>/dev/null || echo "")

if [[ -z "$TOKEN" || "$TOKEN" == "null" ]]; then
  echo ""
  echo "FATAL: failed to mint API token."
  exit 1
fi
echo "  Token minted: ${TOKEN:0:12}... (id=$TOKEN_ID)"

AUTH=(-H "Authorization: Bearer $TOKEN")

# Discover real IDs from the live DB (makes the script resilient to DB resets)
discover_resp=$(curl -s "${AUTH[@]}" "$SW_BASE/api/v1/artists?search=a-ha&page_size=1")
ARTIST_ID=$(echo "$discover_resp" | jq -r '.artists[0].id // empty' 2>/dev/null || true)
if [[ -z "$ARTIST_ID" ]]; then
  # Fall back to first artist in the list
  ARTIST_ID=$(curl -s "${AUTH[@]}" "$SW_BASE/api/v1/artists?page_size=1" | jq -r '.artists[0].id // empty' 2>/dev/null || true)
fi
if [[ -z "$ARTIST_ID" ]]; then
  echo "  WARNING: could not discover a test artist ID -- artist-specific checks will fail"
  ARTIST_ID="unknown"
else
  echo "  Discovered artist ID: $ARTIST_ID"
fi

conns_resp=$(curl -s "${AUTH[@]}" "$SW_BASE/api/v1/connections")
CONN_EMBY=$(echo "$conns_resp" | jq -r '.[] | select(.type=="emby") | .id' 2>/dev/null | head -1 || true)
CONN_JELLYFIN=$(echo "$conns_resp" | jq -r '.[] | select(.type=="jellyfin") | .id' 2>/dev/null | head -1 || true)
CONN_LIDARR=$(echo "$conns_resp" | jq -r '.[] | select(.type=="lidarr") | .id' 2>/dev/null | head -1 || true)

# Discover a library connected to Emby or Jellyfin for populate smoke tests
LIBRARY_ID=""
libs_resp=$(curl -s "${AUTH[@]}" "$SW_BASE/api/v1/libraries")
if [[ -n "$CONN_EMBY" ]]; then
  LIBRARY_ID=$(echo "$libs_resp" | jq -r '.[] | select(.source=="emby") | .id' 2>/dev/null | head -1 || true)
fi
if [[ -z "$LIBRARY_ID" && -n "$CONN_JELLYFIN" ]]; then
  LIBRARY_ID=$(echo "$libs_resp" | jq -r '.[] | select(.source=="jellyfin") | .id' 2>/dev/null | head -1 || true)
fi
LIB_CONN_ID=$(echo "$libs_resp" | jq -r --arg id "$LIBRARY_ID" '.[] | select(.id==$id) | .connection_id' 2>/dev/null || true)
[[ -n "$LIBRARY_ID" ]] && echo "  Platform library: $LIBRARY_ID"
[[ -n "$CONN_EMBY" ]] && echo "  Emby connection: $CONN_EMBY"
[[ -n "$CONN_JELLYFIN" ]] && echo "  Jellyfin connection: $CONN_JELLYFIN"
[[ -n "$CONN_LIDARR" ]] && echo "  Lidarr connection: $CONN_LIDARR"  # reserved for future Lidarr tests

# GET /api/v1/auth/me
me_resp=$(curl -s -w "\n%{http_code}" "${AUTH[@]}" "$SW_BASE/api/v1/auth/me")
me_body=$(echo "$me_resp" | sed '$d')
me_code=$(echo "$me_resp" | tail -n 1)
assert_status "GET /api/v1/auth/me" "200" "$me_code"
assert_json_exists "  /api/v1/auth/me has user_id" ".user_id" "$me_body"

# List artists
artists_resp=$(curl -s -w "\n%{http_code}" "${AUTH[@]}" "$SW_BASE/api/v1/artists")
artists_body=$(echo "$artists_resp" | sed '$d')
artists_code=$(echo "$artists_resp" | tail -n 1)
assert_status "GET /api/v1/artists" "200" "$artists_code"
assert_json_exists "  /api/v1/artists has artists array" ".artists" "$artists_body"
assert_json_exists "  /api/v1/artists has total" ".total" "$artists_body"

# Search artists
search_resp=$(curl -s -w "\n%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/artists?search=a-ha")
search_body=$(echo "$search_resp" | sed '$d')
search_code=$(echo "$search_resp" | tail -n 1)
assert_status "GET /api/v1/artists?search=a-ha" "200" "$search_code"
assert_json_exists "  search returns artists" ".artists" "$search_body"

# Get specific artist (a-ha)
artist_resp=$(curl -s -w "\n%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/artists/$ARTIST_ID")
artist_body=$(echo "$artist_resp" | sed '$d')
artist_code=$(echo "$artist_resp" | tail -n 1)
assert_status "GET /api/v1/artists/$ARTIST_ID (a-ha)" "200" "$artist_code"
assert_json_exists "  artist has id" ".artist.id" "$artist_body"
assert_json_exists "  artist has name" ".artist.name" "$artist_body"

# Image info
img_info_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/artists/$ARTIST_ID/images/thumb/info")
assert_status "GET /api/v1/artists/$ARTIST_ID/images/thumb/info" "200" "$img_info_code"

# Image file (200 or 404, not 500)
img_file_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/artists/$ARTIST_ID/images/thumb/file")
assert_status_in "GET /api/v1/artists/$ARTIST_ID/images/thumb/file" "$img_file_code" 200 404

# Connections list
conn_resp=$(curl -s -w "\n%{http_code}" "${AUTH[@]}" "$SW_BASE/api/v1/connections")
conn_body=$(echo "$conn_resp" | sed '$d')
conn_code=$(echo "$conn_resp" | tail -n 1)
assert_status "GET /api/v1/connections" "200" "$conn_code"

# Get Emby connection
if [[ -n "$CONN_EMBY" ]]; then
  emby_resp=$(curl -s -w "\n%{http_code}" "${AUTH[@]}" \
    "$SW_BASE/api/v1/connections/$CONN_EMBY")
  emby_body=$(echo "$emby_resp" | sed '$d')
  emby_code=$(echo "$emby_resp" | tail -n 1)
  assert_status "GET /api/v1/connections/$CONN_EMBY (Emby)" "200" "$emby_code"
  assert_json_exists "  Emby connection has id" ".id" "$emby_body"

  test_emby_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
    -X POST "$SW_BASE/api/v1/connections/$CONN_EMBY/test")
  assert_status "POST /api/v1/connections/$CONN_EMBY/test (Emby)" "200" "$test_emby_code"
else
  echo "[SKIP] Emby connection -- no Emby connection found"
fi

# Test Jellyfin connection
if [[ -n "$CONN_JELLYFIN" ]]; then
  test_jf_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
    -X POST "$SW_BASE/api/v1/connections/$CONN_JELLYFIN/test")
  assert_status "POST /api/v1/connections/$CONN_JELLYFIN/test (Jellyfin)" "200" "$test_jf_code"
else
  echo "[SKIP] Jellyfin connection test -- no Jellyfin connection found"
fi

# Settings
settings_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/settings")
assert_status "GET /api/v1/settings" "200" "$settings_code"

# Logout (uses session cookie + CSRF token; Bearer token bypass does not apply here)
logout_code=$(curl -s -o /dev/null -w "%{http_code}" -b "$COOKIE_JAR" \
  -H "X-CSRF-Token: $CSRF_TOKEN" \
  -X POST "$SW_BASE/api/v1/auth/logout")
assert_status "POST /api/v1/auth/logout" "200" "$logout_code"

echo ""

# ---------------------------------------------------------------------------
# Tier 2: Platform Integration
# ---------------------------------------------------------------------------

echo "--- Tier 2: Platform Integration ---"
echo ""

# Artist platform IDs
plat_ids_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/artists/$ARTIST_ID/platform-ids")
assert_status "GET /api/v1/artists/$ARTIST_ID/platform-ids" "200" "$plat_ids_code"

# Platform state (Emby)
if [[ -n "$CONN_EMBY" ]]; then
  ps_emby_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
    "$SW_BASE/api/v1/artists/$ARTIST_ID/platform-state?connection_id=$CONN_EMBY")
  assert_status_in "GET platform-state (Emby)" "$ps_emby_code" 200 404
else
  echo "[SKIP] platform-state (Emby) -- no Emby connection found"
fi

# Platform state (Jellyfin)
if [[ -n "$CONN_JELLYFIN" ]]; then
  ps_jf_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
    "$SW_BASE/api/v1/artists/$ARTIST_ID/platform-state?connection_id=$CONN_JELLYFIN")
  assert_status_in "GET platform-state (Jellyfin)" "$ps_jf_code" 200 404
else
  echo "[SKIP] platform-state (Jellyfin) -- no Jellyfin connection found"
fi

# Push images to Emby -- Emby expects base64-encoded body (fixed in #408)
if [[ -n "$CONN_EMBY" ]]; then
  push_emby_resp=$(curl -s -w "\n%{http_code}" "${AUTH[@]}" \
    -X POST "$SW_BASE/api/v1/artists/$ARTIST_ID/push/images" \
    -H "Content-Type: application/json" \
    -d "{\"connection_id\":\"$CONN_EMBY\",\"image_types\":[\"thumb\"]}")
  push_emby_body=$(echo "$push_emby_resp" | sed '$d')
  push_emby_code=$(echo "$push_emby_resp" | tail -n 1)
  assert_status "POST push/images (Emby)" "200" "$push_emby_code"
  if [[ "$push_emby_code" == "200" ]]; then
    emby_err_count=$(echo "$push_emby_body" | jq -r '.errors | length' 2>/dev/null || echo "0")
    if [[ "$emby_err_count" != "0" && "$emby_err_count" != "null" ]]; then
      emby_errors=$(echo "$push_emby_body" | jq -r '.errors[]?' 2>/dev/null || echo "")
      echo "[FAIL] POST push/images (Emby) -- 200 but upload errors: $emby_errors"
      FAIL=$((FAIL + 1))
      FAILURES+=("POST /api/v1/artists/$ARTIST_ID/push/images (Emby) -- upload errors: $emby_errors")
    else
      echo "[PASS] POST push/images (Emby) -- 200 no errors"
      PASS=$((PASS + 1))
    fi
  fi
else
  echo "[SKIP] POST push/images (Emby) -- no Emby connection found"
fi

# Push images to Jellyfin -- EXPECTED PASS
# Requires a stored platform ID for this artist on the Jellyfin connection.
if [[ -n "$CONN_JELLYFIN" ]]; then
  jf_platform_id=$(curl -s "${AUTH[@]}" \
    "$SW_BASE/api/v1/artists/$ARTIST_ID/platform-ids" | \
    jq -r ".[] | select(.connection_id==\"$CONN_JELLYFIN\") | .platform_artist_id" 2>/dev/null || echo "")
  if [[ -z "$jf_platform_id" || "$jf_platform_id" == "null" ]]; then
    echo "[SKIP] POST push/images (Jellyfin) -- no stored platform ID for this artist"
    echo "[SKIP] POST push metadata (Jellyfin) -- no stored platform ID for this artist"
    echo "[SKIP] DELETE push/images/thumb (Jellyfin) -- no stored platform ID for this artist"
  else
    push_jf_resp=$(curl -s -w "\n%{http_code}" "${AUTH[@]}" \
      -X POST "$SW_BASE/api/v1/artists/$ARTIST_ID/push/images" \
      -H "Content-Type: application/json" \
      -d "{\"connection_id\":\"$CONN_JELLYFIN\",\"image_types\":[\"thumb\"]}")
    push_jf_body=$(echo "$push_jf_resp" | sed '$d')
    push_jf_code=$(echo "$push_jf_resp" | tail -n 1)
    assert_status_in "POST push/images (Jellyfin)" "$push_jf_code" 200 204
    if [[ "$push_jf_code" == "200" ]]; then
      jf_err_count=$(echo "$push_jf_body" | jq -r '.errors | length' 2>/dev/null || echo "0")
      if [[ "$jf_err_count" != "0" && "$jf_err_count" != "null" ]]; then
        jf_errors=$(echo "$push_jf_body" | jq -r '.errors[]?' 2>/dev/null || echo "")
        echo "[FAIL] POST push/images (Jellyfin) -- upload errors: $jf_errors"
        FAIL=$((FAIL + 1))
        FAILURES+=("POST /api/v1/artists/$ARTIST_ID/push/images (Jellyfin) -- $jf_errors")
      else
        echo "[PASS] POST push/images (Jellyfin) -- 200 no errors"
        PASS=$((PASS + 1))
      fi
    fi

    # Push metadata to Jellyfin
    push_meta_resp=$(curl -s -w "\n%{http_code}" "${AUTH[@]}" \
      -X POST "$SW_BASE/api/v1/artists/$ARTIST_ID/push" \
      -H "Content-Type: application/json" \
      -d "{\"connection_id\":\"$CONN_JELLYFIN\"}")
    push_meta_code=$(echo "$push_meta_resp" | tail -n 1)
    assert_status_in "POST push metadata (Jellyfin)" "$push_meta_code" 200 204

    # Delete thumb from Jellyfin
    del_img_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
      -X DELETE "$SW_BASE/api/v1/artists/$ARTIST_ID/push/images/thumb" \
      -H "Content-Type: application/json" \
      -d "{\"connection_id\":\"$CONN_JELLYFIN\"}")
    assert_status_in "DELETE push/images/thumb (Jellyfin)" "$del_img_code" 200 204
  fi
else
  echo "[SKIP] Jellyfin push/delete checks -- no Jellyfin connection found"
fi

echo ""

# ---------------------------------------------------------------------------
# Tier 3: Feature Coverage
# ---------------------------------------------------------------------------

echo "--- Tier 3: Feature Coverage ---"
echo ""

nfo_diff_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/artists/$ARTIST_ID/nfo/diff")
assert_status_in "GET /api/v1/artists/$ARTIST_ID/nfo/diff" "$nfo_diff_code" 200 422

nfo_conflict_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/artists/$ARTIST_ID/nfo/conflict")
# 422 is returned when the artist's library has no filesystem path configured; treat as non-fatal
assert_status_in "GET /api/v1/artists/$ARTIST_ID/nfo/conflict" "$nfo_conflict_code" 200 422

nfo_snaps_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/artists/$ARTIST_ID/nfo/snapshots")
assert_status "GET /api/v1/artists/$ARTIST_ID/nfo/snapshots" "200" "$nfo_snaps_code"

health_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/artists/$ARTIST_ID/health")
assert_status "GET /api/v1/artists/$ARTIST_ID/health" "200" "$health_code"

dupes_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/artists/duplicates")
assert_status "GET /api/v1/artists/duplicates" "200" "$dupes_code"

aliases_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/artists/$ARTIST_ID/aliases")
assert_status "GET /api/v1/artists/$ARTIST_ID/aliases" "200" "$aliases_code"

libraries_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/libraries")
assert_status "GET /api/v1/libraries" "200" "$libraries_code"

if [[ -n "$CONN_EMBY" ]]; then
  disc_emby_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
    "$SW_BASE/api/v1/connections/$CONN_EMBY/libraries")
  assert_status_in "GET /api/v1/connections/$CONN_EMBY/libraries (Emby discover)" "$disc_emby_code" 200 409 502 503
else
  echo "[SKIP] Emby library discover -- no Emby connection found"
fi

if [[ -n "$CONN_JELLYFIN" ]]; then
  disc_jf_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
    "$SW_BASE/api/v1/connections/$CONN_JELLYFIN/libraries")
  assert_status_in "GET /api/v1/connections/$CONN_JELLYFIN/libraries (Jellyfin discover)" "$disc_jf_code" 200 409 502 503
else
  echo "[SKIP] Jellyfin library discover -- no Jellyfin connection found"
fi

rules_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/rules")
assert_status "GET /api/v1/rules" "200" "$rules_code"

# Fanart list endpoint -- verifies multi-backdrop support is accessible
fanart_list_resp=$(curl -s -w "\n%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/artists/$ARTIST_ID/images/fanart/list")
fanart_list_body=$(echo "$fanart_list_resp" | sed '$d')
fanart_list_code=$(echo "$fanart_list_resp" | tail -n 1)
assert_status "GET /api/v1/artists/$ARTIST_ID/images/fanart/list" "200" "$fanart_list_code"
if [[ "$fanart_list_code" == "200" ]]; then
  fanart_arr_len=$(echo "$fanart_list_body" | jq 'length' 2>/dev/null)
  if [[ -n "$fanart_arr_len" && "$fanart_arr_len" =~ ^[0-9]+$ ]]; then
    echo "[PASS]   fanart list returns array (length=$fanart_arr_len)"
    PASS=$((PASS + 1))
  else
    echo "[FAIL]   fanart list did not return a valid array"
    FAIL=$((FAIL + 1))
    FAILURES+=("fanart list did not return a valid array")
  fi
fi

# Artist fanart_count field in artist detail response
fanart_count_resp=$(curl -s "${AUTH[@]}" "$SW_BASE/api/v1/artists/$ARTIST_ID")
assert_json_exists "  artist detail has fanart_count field" ".artist.fanart_count" "$fanart_count_resp"

notif_counts_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/notifications/counts")
assert_status "GET /api/v1/notifications/counts" "200" "$notif_counts_code"

notif_badge_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/notifications/badge")
assert_status "GET /api/v1/notifications/badge" "200" "$notif_badge_code"

report_health_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/reports/health")
assert_status "GET /api/v1/reports/health" "200" "$report_health_code"

report_compliance_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/reports/compliance")
assert_status "GET /api/v1/reports/compliance" "200" "$report_compliance_code"

bulk_jobs_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/bulk/jobs")
assert_status "GET /api/v1/bulk/jobs" "200" "$bulk_jobs_code"

scanner_status_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/scanner/status")
assert_status "GET /api/v1/scanner/status" "200" "$scanner_status_code"

providers_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/providers")
assert_status "GET /api/v1/providers" "200" "$providers_code"

priorities_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/providers/priorities")
assert_status "GET /api/v1/providers/priorities" "200" "$priorities_code"

backup_hist_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/settings/backup/history")
assert_status "GET /api/v1/settings/backup/history" "200" "$backup_hist_code"

logging_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/settings/logging")
assert_status "GET /api/v1/settings/logging" "200" "$logging_code"

maint_status_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/settings/maintenance/status")
assert_status "GET /api/v1/settings/maintenance/status" "200" "$maint_status_code"

webhooks_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/webhooks")
assert_status "GET /api/v1/webhooks" "200" "$webhooks_code"

tokens_list_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/auth/tokens")
assert_status "GET /api/v1/auth/tokens (list)" "200" "$tokens_list_code"

echo ""

# ---------------------------------------------------------------------------
# Tier 4: Destructive/Stateful (opt-in with --full)
# ---------------------------------------------------------------------------

if [[ "$FULL" -eq 1 ]]; then
  echo "--- Tier 4: Destructive/Stateful (--full) ---"
  echo ""

  fetch_img_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
    -X POST "$SW_BASE/api/v1/artists/$ARTIST_ID/images/fetch" \
    -H "Content-Type: application/json" \
    -d '{"url":"https://upload.wikimedia.org/wikipedia/en/a/aa/A-ha_band_2015.jpg","type":"thumb"}')
  assert_status_in "POST /api/v1/artists/$ARTIST_ID/images/fetch (--full)" "$fetch_img_code" 200 422

  # Populate from Emby or Jellyfin and verify the response includes backdrop image counts.
  # This exercises the multi-backdrop download path added in #357.
  if [[ -n "$LIBRARY_ID" && -n "$LIB_CONN_ID" && "$LIB_CONN_ID" != "null" ]]; then
    pop_resp=$(curl -s -w "\n%{http_code}" "${AUTH[@]}" \
      -X POST "$SW_BASE/api/v1/connections/$LIB_CONN_ID/libraries/$LIBRARY_ID/populate")
    pop_body=$(echo "$pop_resp" | sed '$d')
    pop_code=$(echo "$pop_resp" | tail -n 1)
    assert_status_in "POST /api/v1/connections/$LIB_CONN_ID/libraries/$LIBRARY_ID/populate (--full)" "$pop_code" 200 202
    if [[ "$pop_code" == "200" || "$pop_code" == "202" ]]; then
      assert_json_exists "  populate response has artists field" ".artists" "$pop_body"
      assert_json_exists "  populate response has images field" ".images" "$pop_body"
      assert_json_exists "  populate response has skipped field" ".skipped" "$pop_body"
    fi
  else
    echo "[SKIP] POST populate (--full) -- no Emby/Jellyfin library or connection found"
  fi

  backup_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
    -X POST "$SW_BASE/api/v1/settings/backup")
  assert_status "POST /api/v1/settings/backup (--full)" "200" "$backup_code"

  echo ""
fi

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

exit 0
