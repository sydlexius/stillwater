#!/usr/bin/env bash
# smoke.sh -- API smoke test suite for Stillwater
#
# Usage:
#   bash scripts/smoke.sh [--full] [--roundtrip]
#
# Environment:
#   SW_USER        -- admin username (default: admin)
#   SW_PASS        -- admin password (default: admin)
#   SW_BASE        -- base URL       (default: http://localhost:1973)
#   EMBY_URL       -- Emby server URL (required for --roundtrip)
#   EMBY_API_KEY   -- Emby API key    (required for --roundtrip)
#   JELLYFIN_URL   -- Jellyfin server URL (required for --roundtrip)
#   JELLYFIN_API_KEY -- Jellyfin API key  (required for --roundtrip)
#
# --full      enables Tier 4 destructive/stateful checks (off by default)
# --roundtrip enables Tier 5 NFO roundtrip checks (off by default)

set -euo pipefail

SW_USER="${SW_USER:-admin}"
SW_PASS="${SW_PASS:-admin}"
SW_BASE="${SW_BASE:-http://localhost:1973}"

FULL=0
ROUNDTRIP=0
for arg in "$@"; do
  [[ "$arg" == "--full" ]] && FULL=1
  [[ "$arg" == "--roundtrip" ]] && ROUNDTRIP=1
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

# assert_json_value compares a pre-extracted value against an expected value.
# Unlike assert_json_field, the caller supplies the value directly (no jq path).
assert_json_value() {
  local label="$1"
  local expected="$2"
  local got="$3"

  if [[ "$got" == "$expected" ]]; then
    echo "[PASS] $label -- \"$got\""
    PASS=$((PASS + 1))
  else
    echo "[FAIL] $label -- expected \"$expected\", got \"$got\""
    FAIL=$((FAIL + 1))
    FAILURES+=("$label -- expected \"$expected\", got \"$got\"")
  fi
}

# Platform API helpers for direct calls to Emby/Jellyfin.
# Both Emby and Jellyfin return sparse item responses unless Fields is specified.
# The /Items/{id} path does not work for GETs on artist items; use /Items?Ids={id}
# with explicit Fields to get Overview, ProviderIds, PremiereDate etc.
platform_get_item() {
  local url="$1" key="$2" item_id="$3"
  curl -s "${url}/Items?api_key=${key}&Ids=${item_id}&Fields=Overview,ProviderIds,PremiereDate,EndDate,Genres,Tags,TagItems" | \
    jq '.Items[0]' 2>/dev/null
}

# Fetch an item, modify Overview, and POST it back. Strips read-only fields that
# cause 400 errors on Jellyfin (ServerId, ImageBlurHashes, etc.) while keeping
# fields that Emby requires (Genres, Tags, etc.).
platform_modify_overview() {
  local url="$1" key="$2" item_id="$3" new_overview="$4"
  local item
  item=$(curl -s "${url}/Items?api_key=${key}&Ids=${item_id}&Fields=Overview,ProviderIds,PremiereDate,EndDate,Genres,Tags")
  local update_body
  update_body=$(echo "$item" | jq --arg bio "$new_overview" \
    '.Items[0] | del(.ServerId, .ImageBlurHashes, .ImageTags, .BackdropImageTags, .LocationType, .MediaType, .ChannelId) | .Overview = $bio')
  curl -s -o /dev/null -w "%{http_code}" \
    -X POST "${url}/Items/${item_id}?api_key=${key}" \
    -H "Content-Type: application/json" \
    -d "$update_body"
}

# Poll for an NFO file's mtime to change (indicates platform wrote it).
wait_for_nfo_change() {
  local nfo_path="$1" original_mtime="$2" timeout="${3:-30}" interval="${4:-3}"
  local elapsed=0
  while [[ $elapsed -lt $timeout ]]; do
    sleep "$interval"
    elapsed=$((elapsed + interval))
    local current_mtime
    current_mtime=$(stat -c %Y "$nfo_path" 2>/dev/null || echo "0")
    if [[ "$current_mtime" != "$original_mtime" ]]; then
      return 0
    fi
  done
  return 1
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
# Requires a stored platform ID for this artist on the Emby connection.
if [[ -n "$CONN_EMBY" ]]; then
  emby_platform_id=$(curl -s "${AUTH[@]}" \
    "$SW_BASE/api/v1/artists/$ARTIST_ID/platform-ids" | \
    jq -r ".[] | select(.connection_id==\"$CONN_EMBY\") | .platform_artist_id" 2>/dev/null || echo "")
  if [[ -z "$emby_platform_id" || "$emby_platform_id" == "null" ]]; then
    echo "[SKIP] POST push/images (Emby) -- no stored platform ID for this artist"
  else
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
  fanart_arr_len=$(echo "$fanart_list_body" | jq 'length' 2>/dev/null || echo "")
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
    if [[ "$pop_code" == "200" ]]; then
      assert_json_exists "  populate 200 response has artists field" ".artists" "$pop_body"
      assert_json_exists "  populate 200 response has images field" ".images" "$pop_body"
      assert_json_exists "  populate 200 response has skipped field" ".skipped" "$pop_body"
    elif [[ "$pop_code" == "202" ]]; then
      assert_json_exists "  populate 202 response has library_id field" ".library_id" "$pop_body"
      assert_json_exists "  populate 202 response has status field" ".status" "$pop_body"
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
# Tier 5: NFO Roundtrip (opt-in with --roundtrip)
# ---------------------------------------------------------------------------

if [[ "$ROUNDTRIP" -eq 1 ]]; then
  echo "--- Tier 5: NFO Roundtrip (--roundtrip) ---"
  echo ""

  # -------------------------------------------------------------------------
  # Roundtrip discovery: find an artist with a filesystem path + platform IDs
  # -------------------------------------------------------------------------

  RT_ARTIST_ID=""
  RT_ARTIST_PATH=""
  RT_ARTIST_NAME=""
  RT_EMBY_PLATFORM_ID=""
  RT_JELLYFIN_PLATFORM_ID=""

  # Prefer the existing test artist (a-ha) if it qualifies.
  if [[ -n "$ARTIST_ID" && "$ARTIST_ID" != "unknown" ]]; then
    rt_detail=$(curl -s "${AUTH[@]}" "$SW_BASE/api/v1/artists/$ARTIST_ID")
    rt_path=$(echo "$rt_detail" | jq -r '.artist.path // empty' 2>/dev/null || true)
    if [[ -n "$rt_path" ]]; then
      RT_ARTIST_ID="$ARTIST_ID"
      RT_ARTIST_PATH="$rt_path"
      RT_ARTIST_NAME=$(echo "$rt_detail" | jq -r '.artist.name // empty' 2>/dev/null || true)
    fi
  fi

  # If a-ha does not have a path, scan the first 50 artists for one that does.
  if [[ -z "$RT_ARTIST_ID" ]]; then
    rt_list=$(curl -s "${AUTH[@]}" "$SW_BASE/api/v1/artists?page_size=50")
    rt_ids=$(echo "$rt_list" | jq -r '.artists[].id // empty' 2>/dev/null || true)
    for candidate_id in $rt_ids; do
      candidate_detail=$(curl -s "${AUTH[@]}" "$SW_BASE/api/v1/artists/$candidate_id")
      candidate_path=$(echo "$candidate_detail" | jq -r '.artist.path // empty' 2>/dev/null || true)
      if [[ -n "$candidate_path" ]]; then
        RT_ARTIST_ID="$candidate_id"
        RT_ARTIST_PATH="$candidate_path"
        RT_ARTIST_NAME=$(echo "$candidate_detail" | jq -r '.artist.name // empty' 2>/dev/null || true)
        break
      fi
    done
  fi

  if [[ -z "$RT_ARTIST_ID" ]]; then
    echo "[SKIP] Roundtrip -- no artist with a filesystem path found"
  else
    echo "  Roundtrip artist: $RT_ARTIST_NAME (id=$RT_ARTIST_ID)"
    echo "  Roundtrip path:   $RT_ARTIST_PATH"

    # Discover platform IDs for the roundtrip artist.
    rt_plat_ids=$(curl -s "${AUTH[@]}" "$SW_BASE/api/v1/artists/$RT_ARTIST_ID/platform-ids")
    if [[ -n "$CONN_EMBY" ]]; then
      RT_EMBY_PLATFORM_ID=$(echo "$rt_plat_ids" | \
        jq -r ".[] | select(.connection_id==\"$CONN_EMBY\") | .platform_artist_id" 2>/dev/null || echo "")
      [[ "$RT_EMBY_PLATFORM_ID" == "null" ]] && RT_EMBY_PLATFORM_ID=""
    fi
    if [[ -n "$CONN_JELLYFIN" ]]; then
      RT_JELLYFIN_PLATFORM_ID=$(echo "$rt_plat_ids" | \
        jq -r ".[] | select(.connection_id==\"$CONN_JELLYFIN\") | .platform_artist_id" 2>/dev/null || echo "")
      [[ "$RT_JELLYFIN_PLATFORM_ID" == "null" ]] && RT_JELLYFIN_PLATFORM_ID=""
    fi

    [[ -n "$RT_EMBY_PLATFORM_ID" ]] && echo "  Emby platform ID:     $RT_EMBY_PLATFORM_ID"
    [[ -n "$RT_JELLYFIN_PLATFORM_ID" ]] && echo "  Jellyfin platform ID: $RT_JELLYFIN_PLATFORM_ID"

    # Check which platforms have NFO writers enabled (for Direction 2).
    EMBY_NFO_WRITER=0
    JELLYFIN_NFO_WRITER=0
    clobber_resp=$(curl -s "${AUTH[@]}" "$SW_BASE/api/v1/connections/clobber-check")
    if [[ -n "$CONN_EMBY" ]]; then
      emby_nfo_w=$(echo "$clobber_resp" | jq -r ".risks[] | select(.connection_type==\"emby\") | .nfo_writer" 2>/dev/null || echo "false")
      [[ "$emby_nfo_w" == "true" ]] && EMBY_NFO_WRITER=1
    fi
    if [[ -n "$CONN_JELLYFIN" ]]; then
      jf_nfo_w=$(echo "$clobber_resp" | jq -r ".risks[] | select(.connection_type==\"jellyfin\") | .nfo_writer" 2>/dev/null || echo "false")
      [[ "$jf_nfo_w" == "true" ]] && JELLYFIN_NFO_WRITER=1
    fi

    echo ""

    # -------------------------------------------------------------------
    # Direction 1: Stillwater -> Platform -> Verify
    # -------------------------------------------------------------------

    echo "  -- Direction 1: Stillwater -> Platform -> Verify --"
    echo ""

    # Save original artist values so we can restore after the test.
    sw_artist_orig=$(curl -s "${AUTH[@]}" "$SW_BASE/api/v1/artists/$RT_ARTIST_ID")
    orig_bio=$(echo "$sw_artist_orig" | jq -r '.artist.biography // empty' 2>/dev/null || true)
    orig_formed=$(echo "$sw_artist_orig" | jq -r '.artist.formed // empty' 2>/dev/null || true)
    orig_disbanded=$(echo "$sw_artist_orig" | jq -r '.artist.disbanded // empty' 2>/dev/null || true)
    # Capture genres/styles/moods as comma-separated strings for the field-edit API.
    orig_genres=$(echo "$sw_artist_orig" | jq -r '[.artist.genres // [] | .[]?] | join(", ")' 2>/dev/null || true)
    orig_styles=$(echo "$sw_artist_orig" | jq -r '[.artist.styles // [] | .[]?] | join(", ")' 2>/dev/null || true)
    orig_moods=$(echo "$sw_artist_orig" | jq -r '[.artist.moods // [] | .[]?] | join(", ")' 2>/dev/null || true)

    # Define synthetic test values for all pushable fields.
    # These use a unique marker so we can verify exact roundtrip fidelity.
    RT_BIO="Roundtrip test biography [smoke-$(date +%s)]"
    RT_FORMED="1985-01-15"
    RT_DISBANDED="2010-12-04"
    # The field-edit API expects comma-separated strings for slice fields,
    # not JSON arrays. See issue #461 (extractFieldValue reads string only).
    RT_GENRES="Synth-Pop, New Wave"
    RT_STYLES="Electronic, Scandinavian"
    RT_MOODS="Melancholic, Uplifting"

    # Write test values to Stillwater via the field-edit API.
    sw_patch() {
      local field="$1" value="$2"
      curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
        -X PATCH "$SW_BASE/api/v1/artists/$RT_ARTIST_ID/fields/$field" \
        -H "Content-Type: application/json" \
        -d "{\"value\":$value}"
    }

    echo "  Setting synthetic test data on Stillwater artist..."
    sw_patch "biography" "$(jq -n --arg v "$RT_BIO" '$v')"
    sw_patch "formed" "$(jq -n --arg v "$RT_FORMED" '$v')"
    sw_patch "disbanded" "$(jq -n --arg v "$RT_DISBANDED" '$v')"
    sw_patch "genres" "$(jq -n --arg v "$RT_GENRES" '$v')"
    sw_patch "styles" "$(jq -n --arg v "$RT_STYLES" '$v')"
    sw_patch "moods" "$(jq -n --arg v "$RT_MOODS" '$v')"

    # Re-read artist to get the canonical values Stillwater will push.
    sw_artist=$(curl -s "${AUTH[@]}" "$SW_BASE/api/v1/artists/$RT_ARTIST_ID")
    sw_bio=$(echo "$sw_artist" | jq -r '.artist.biography // empty' 2>/dev/null || true)
    sw_mbid=$(echo "$sw_artist" | jq -r '.artist.musicbrainz_id // empty' 2>/dev/null || true)

    # --- Direction 1 per-platform verification function ---
    # Verifies pushed fields on a single platform, then returns.
    verify_direction1() {
      local platform="$1" platform_url="$2" platform_key="$3" platform_item_id="$4" conn_id="$5"

      # Push metadata from Stillwater to the platform.
      local push_code
      push_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
        -X POST "$SW_BASE/api/v1/artists/$RT_ARTIST_ID/push" \
        -H "Content-Type: application/json" \
        -d "{\"connection_id\":\"$conn_id\"}")
      assert_status "POST push metadata to $platform (roundtrip)" "200" "$push_code"

      # If the push failed, skip field-level assertions -- comparing stale values
      # against expected synthetic data produces misleading failures.
      if [[ "$push_code" != "200" ]]; then
        echo "[SKIP] $platform roundtrip field assertions -- push failed (HTTP $push_code)"
        return
      fi

      sleep 3

      # Fetch the artist from the platform and compare all fields.
      local p_item p_bio p_mbid p_date p_end_date p_genres p_tags
      p_item=$(platform_get_item "$platform_url" "$platform_key" "$platform_item_id")
      p_bio=$(echo "$p_item" | jq -r '.Overview // empty' 2>/dev/null || true)
      p_mbid=$(echo "$p_item" | jq -r '.ProviderIds.MusicBrainzArtist // empty' 2>/dev/null || true)
      p_date=$(echo "$p_item" | jq -r '.PremiereDate // empty' 2>/dev/null || true)
      p_end_date=$(echo "$p_item" | jq -r '.EndDate // empty' 2>/dev/null || true)
      p_genres=$(echo "$p_item" | jq -c '[.Genres[]?] // []' 2>/dev/null || echo "[]")
      # Emby uses TagItems (array of {Name, Id}) instead of Tags (flat string array).
      # Jellyfin uses Tags. Check both and merge into a unified list.
      p_tags=$(echo "$p_item" | jq -c '([.Tags[]?] + [.TagItems[]?.Name]) | unique' 2>/dev/null || echo "[]")

      # Biography
      assert_json_value "$platform roundtrip biography" "$sw_bio" "$p_bio"

      # MusicBrainz ID
      if [[ -n "$sw_mbid" ]]; then
        assert_json_value "$platform roundtrip MBID" "$sw_mbid" "$p_mbid"
      else
        echo "[SKIP] $platform roundtrip MBID -- no MBID in Stillwater"
      fi

      # Formed date -> PremiereDate (compare YYYY-MM-DD portion)
      local p_date_short="${p_date:0:10}"
      assert_json_value "$platform roundtrip formed->PremiereDate" "$RT_FORMED" "$p_date_short"

      # Disbanded date -> EndDate (compare YYYY-MM-DD portion)
      local p_end_short="${p_end_date:0:10}"
      assert_json_value "$platform roundtrip disbanded->EndDate" "$RT_DISBANDED" "$p_end_short"

      # Genres (Stillwater pushes RT_GENRES -> platform Genres)
      local has_synth has_newwave
      has_synth=$(echo "$p_genres" | jq 'map(select(. == "Synth-Pop")) | length' 2>/dev/null || echo "0")
      has_newwave=$(echo "$p_genres" | jq 'map(select(. == "New Wave")) | length' 2>/dev/null || echo "0")
      if [[ "$has_synth" -ge 1 && "$has_newwave" -ge 1 ]]; then
        echo "[PASS] $platform roundtrip genres -- Synth-Pop, New Wave present"
        PASS=$((PASS + 1))
      else
        echo "[FAIL] $platform roundtrip genres -- expected Synth-Pop + New Wave, got $p_genres"
        FAIL=$((FAIL + 1))
        FAILURES+=("$platform roundtrip genres -- expected Synth-Pop + New Wave, got $p_genres")
      fi

      # Tags (Stillwater pushes Styles + Moods -> platform Tags)
      local has_electronic has_melancholic
      has_electronic=$(echo "$p_tags" | jq 'map(select(. == "Electronic")) | length' 2>/dev/null || echo "0")
      has_melancholic=$(echo "$p_tags" | jq 'map(select(. == "Melancholic")) | length' 2>/dev/null || echo "0")
      if [[ "$has_electronic" -ge 1 && "$has_melancholic" -ge 1 ]]; then
        echo "[PASS] $platform roundtrip tags (styles+moods) -- Electronic, Melancholic present"
        PASS=$((PASS + 1))
      else
        echo "[FAIL] $platform roundtrip tags -- expected Electronic + Melancholic, got $p_tags"
        FAIL=$((FAIL + 1))
        FAILURES+=("$platform roundtrip tags -- expected Electronic + Melancholic, got $p_tags")
      fi
    }

    # --- Emby Direction 1 ---
    if [[ -z "${EMBY_URL:-}" || -z "${EMBY_API_KEY:-}" ]]; then
      echo "[SKIP] Emby Direction 1 -- EMBY_URL or EMBY_API_KEY not set"
    elif [[ -z "$RT_EMBY_PLATFORM_ID" ]]; then
      echo "[SKIP] Emby Direction 1 -- no Emby platform ID for roundtrip artist"
    elif ! curl -sf "${EMBY_URL}/System/Info?api_key=${EMBY_API_KEY}" >/dev/null 2>&1; then
      echo "[SKIP] Emby Direction 1 -- Emby unreachable at $EMBY_URL"
    else
      verify_direction1 "Emby" "$EMBY_URL" "$EMBY_API_KEY" "$RT_EMBY_PLATFORM_ID" "$CONN_EMBY"
    fi

    # --- Jellyfin Direction 1 ---
    if [[ -z "${JELLYFIN_URL:-}" || -z "${JELLYFIN_API_KEY:-}" ]]; then
      echo "[SKIP] Jellyfin Direction 1 -- JELLYFIN_URL or JELLYFIN_API_KEY not set"
    elif [[ -z "$RT_JELLYFIN_PLATFORM_ID" ]]; then
      echo "[SKIP] Jellyfin Direction 1 -- no Jellyfin platform ID for roundtrip artist"
    elif ! curl -sf "${JELLYFIN_URL}/System/Info?api_key=${JELLYFIN_API_KEY}" >/dev/null 2>&1; then
      echo "[SKIP] Jellyfin Direction 1 -- Jellyfin unreachable at $JELLYFIN_URL"
    else
      verify_direction1 "Jellyfin" "$JELLYFIN_URL" "$JELLYFIN_API_KEY" "$RT_JELLYFIN_PLATFORM_ID" "$CONN_JELLYFIN"
    fi

    # Restore original values on Stillwater artist.
    echo "  Restoring original artist data on Stillwater..."
    sw_patch "biography" "$(echo "$orig_bio" | jq -Rs '.')"
    if [[ -n "$orig_formed" ]]; then
      sw_patch "formed" "$(jq -n --arg v "$orig_formed" '$v')"
    else
      sw_patch "formed" '""'
    fi
    if [[ -n "$orig_disbanded" ]]; then
      sw_patch "disbanded" "$(jq -n --arg v "$orig_disbanded" '$v')"
    else
      sw_patch "disbanded" '""'
    fi
    sw_patch "genres" "$(jq -n --arg v "$orig_genres" '$v')"
    sw_patch "styles" "$(jq -n --arg v "$orig_styles" '$v')"
    sw_patch "moods" "$(jq -n --arg v "$orig_moods" '$v')"

    # Each sw_patch triggers asyncPushMetadataToConnections (fire-and-forget
    # goroutines that push to Emby/Jellyfin). Wait for them to settle before
    # starting Direction 2, otherwise a late async push might overwrite the
    # biography modification that Direction 2 makes on the platform.
    sleep 5
    echo ""

    # -------------------------------------------------------------------
    # Direction 2: Platform writes NFO -> Stillwater reads
    #
    # IMPORTANT: Emby and Jellyfin share the same /music directory.
    # Tests run sequentially (Emby first, then Jellyfin) with restore
    # between them to prevent one platform from clobbering the other's
    # NFO changes.
    # -------------------------------------------------------------------

    echo "  -- Direction 2: Platform writes NFO -> Stillwater reads --"
    echo ""

    # --- Emby Direction 2 ---
    if [[ -z "${EMBY_URL:-}" || -z "${EMBY_API_KEY:-}" ]]; then
      echo "[SKIP] Emby Direction 2 -- EMBY_URL or EMBY_API_KEY not set"
    elif [[ -z "$RT_EMBY_PLATFORM_ID" ]]; then
      echo "[SKIP] Emby Direction 2 -- no Emby platform ID for roundtrip artist"
    elif [[ "$EMBY_NFO_WRITER" -eq 0 ]]; then
      echo "[SKIP] Emby Direction 2 -- NFO writer not enabled on Emby"
    elif ! curl -sf "${EMBY_URL}/System/Info?api_key=${EMBY_API_KEY}" >/dev/null 2>&1; then
      echo "[SKIP] Emby Direction 2 -- Emby unreachable at $EMBY_URL"
    else
      nfo_path="${RT_ARTIST_PATH}/artist.nfo"
      original_mtime=$(stat -c %Y "$nfo_path" 2>/dev/null || echo "0")

      # Fetch current biography from Emby.
      original_item=$(platform_get_item "$EMBY_URL" "$EMBY_API_KEY" "$RT_EMBY_PLATFORM_ID")
      original_bio=$(echo "$original_item" | jq -r '.Overview // empty' 2>/dev/null || true)

      # Modify biography on Emby (append marker).
      modified_bio="${original_bio} [roundtrip-test]"
      update_code=$(platform_modify_overview "$EMBY_URL" "$EMBY_API_KEY" "$RT_EMBY_PLATFORM_ID" "$modified_bio")
      if [[ "$update_code" != "204" && "$update_code" != "200" ]]; then
        echo "[FAIL] Emby Direction 2 -- failed to update biography on Emby (HTTP $update_code)"
        FAIL=$((FAIL + 1))
        FAILURES+=("Emby Direction 2 -- failed to update biography (HTTP $update_code)")
      else
        # Emby does not write NFO immediately after POST /Items/{id}. Trigger a
        # metadata refresh with ReplaceAllMetadata=true to force the NFO write.
        curl -s -o /dev/null -X POST \
          "${EMBY_URL}/Items/${RT_EMBY_PLATFORM_ID}/Refresh?api_key=${EMBY_API_KEY}&ReplaceAllMetadata=true&ReplaceAllImages=false"

        # Poll for NFO mtime change. Emby refresh can take 45-60s, so use a
        # longer timeout than Jellyfin (which writes immediately).
        if wait_for_nfo_change "$nfo_path" "$original_mtime" 60 5; then
          echo "[PASS] Emby Direction 2 -- NFO write detected (mtime changed)"
          PASS=$((PASS + 1))

          # Verify Stillwater can parse the externally-modified NFO.
          diff_resp=$(curl -s "${AUTH[@]}" "$SW_BASE/api/v1/artists/$RT_ARTIST_ID/nfo/diff")
          has_diff=$(echo "$diff_resp" | jq -r '.has_diff' 2>/dev/null || echo "false")
          bio_status=$(echo "$diff_resp" | jq -r '.fields[] | select(.field=="Biography") | .status' 2>/dev/null || echo "")
          assert_json_value "NFO diff has_diff after Emby write" "true" "$has_diff"
          if [[ -n "$bio_status" ]]; then
            # Accept "changed" or "added" -- the status depends on whether the
            # NFO previously had a biography field.
            if [[ "$bio_status" == "changed" || "$bio_status" == "added" ]]; then
              echo "[PASS] NFO diff biography status after Emby write -- $bio_status"
              PASS=$((PASS + 1))
            else
              echo "[FAIL] NFO diff biography status after Emby write -- expected changed or added, got $bio_status"
              FAIL=$((FAIL + 1))
              FAILURES+=("NFO diff biography status after Emby write -- expected changed or added, got $bio_status")
            fi
          else
            echo "[SKIP] NFO diff biography status -- biography field not in diff output"
          fi

          # Check conflict endpoint detects the external modification.
          conflict_resp=$(curl -s "${AUTH[@]}" "$SW_BASE/api/v1/artists/$RT_ARTIST_ID/nfo/conflict")
          has_conflict=$(echo "$conflict_resp" | jq -r '.has_conflict' 2>/dev/null || echo "false")
          assert_json_value "NFO conflict detected after Emby write" "true" "$has_conflict"
        else
          echo "[FAIL] Emby Direction 2 -- no NFO mtime change within 60s"
          FAIL=$((FAIL + 1))
          FAILURES+=("Emby Direction 2 -- no NFO mtime change within 60s")
        fi

        # Restore original biography on Emby.
        restore_code=$(platform_modify_overview "$EMBY_URL" "$EMBY_API_KEY" "$RT_EMBY_PLATFORM_ID" "$original_bio")
        if [[ "$restore_code" == "204" || "$restore_code" == "200" ]]; then
          echo "[INFO] Restored original biography on Emby"
        else
          echo "[WARN] Failed to restore biography on Emby (HTTP $restore_code)"
        fi
      fi
    fi

    # Brief pause after Emby Direction 2 restore before starting Jellyfin.
    # Both platforms share /music; let the filesystem and any watchers settle.
    sleep 2

    # --- Jellyfin Direction 2 ---
    if [[ -z "${JELLYFIN_URL:-}" || -z "${JELLYFIN_API_KEY:-}" ]]; then
      echo "[SKIP] Jellyfin Direction 2 -- JELLYFIN_URL or JELLYFIN_API_KEY not set"
    elif [[ -z "$RT_JELLYFIN_PLATFORM_ID" ]]; then
      echo "[SKIP] Jellyfin Direction 2 -- no Jellyfin platform ID for roundtrip artist"
    elif [[ "$JELLYFIN_NFO_WRITER" -eq 0 ]]; then
      echo "[SKIP] Jellyfin Direction 2 -- NFO writer not enabled on Jellyfin"
    elif ! curl -sf "${JELLYFIN_URL}/System/Info?api_key=${JELLYFIN_API_KEY}" >/dev/null 2>&1; then
      echo "[SKIP] Jellyfin Direction 2 -- Jellyfin unreachable at $JELLYFIN_URL"
    else
      nfo_path="${RT_ARTIST_PATH}/artist.nfo"
      original_mtime=$(stat -c %Y "$nfo_path" 2>/dev/null || echo "0")

      # Fetch current biography from Jellyfin for later restore.
      original_item=$(platform_get_item "$JELLYFIN_URL" "$JELLYFIN_API_KEY" "$RT_JELLYFIN_PLATFORM_ID")
      original_bio=$(echo "$original_item" | jq -r '.Overview // empty' 2>/dev/null || true)

      # Modify biography on Jellyfin (append marker).
      # Use platform_modify_overview which fetches the full item and strips read-only fields.
      modified_bio="${original_bio} [roundtrip-test]"
      update_code=$(platform_modify_overview "$JELLYFIN_URL" "$JELLYFIN_API_KEY" "$RT_JELLYFIN_PLATFORM_ID" "$modified_bio")
      if [[ "$update_code" != "204" && "$update_code" != "200" ]]; then
        echo "[FAIL] Jellyfin Direction 2 -- failed to update biography on Jellyfin (HTTP $update_code)"
        FAIL=$((FAIL + 1))
        FAILURES+=("Jellyfin Direction 2 -- failed to update biography (HTTP $update_code)")
      else
        # Poll for NFO mtime change (30s timeout, 3s interval).
        if wait_for_nfo_change "$nfo_path" "$original_mtime"; then
          echo "[PASS] Jellyfin Direction 2 -- NFO write detected (mtime changed)"
          PASS=$((PASS + 1))

          # Verify Stillwater can parse the externally-modified NFO.
          diff_resp=$(curl -s "${AUTH[@]}" "$SW_BASE/api/v1/artists/$RT_ARTIST_ID/nfo/diff")
          has_diff=$(echo "$diff_resp" | jq -r '.has_diff' 2>/dev/null || echo "false")
          bio_status=$(echo "$diff_resp" | jq -r '.fields[] | select(.field=="Biography") | .status' 2>/dev/null || echo "")
          assert_json_value "NFO diff has_diff after Jellyfin write" "true" "$has_diff"
          if [[ -n "$bio_status" ]]; then
            # Accept "changed" or "added" -- the status depends on whether the
            # NFO previously had a biography field.
            if [[ "$bio_status" == "changed" || "$bio_status" == "added" ]]; then
              echo "[PASS] NFO diff biography status after Jellyfin write -- $bio_status"
              PASS=$((PASS + 1))
            else
              echo "[FAIL] NFO diff biography status after Jellyfin write -- expected changed or added, got $bio_status"
              FAIL=$((FAIL + 1))
              FAILURES+=("NFO diff biography status after Jellyfin write -- expected changed or added, got $bio_status")
            fi
          else
            echo "[SKIP] NFO diff biography status -- biography field not in diff output"
          fi

          # Check conflict endpoint detects the external modification.
          conflict_resp=$(curl -s "${AUTH[@]}" "$SW_BASE/api/v1/artists/$RT_ARTIST_ID/nfo/conflict")
          has_conflict=$(echo "$conflict_resp" | jq -r '.has_conflict' 2>/dev/null || echo "false")
          assert_json_value "NFO conflict detected after Jellyfin write" "true" "$has_conflict"
        else
          echo "[FAIL] Jellyfin Direction 2 -- no NFO mtime change within 30s"
          FAIL=$((FAIL + 1))
          FAILURES+=("Jellyfin Direction 2 -- no NFO mtime change within 30s")
        fi

        # Restore original biography on Jellyfin.
        restore_code=$(platform_modify_overview "$JELLYFIN_URL" "$JELLYFIN_API_KEY" "$RT_JELLYFIN_PLATFORM_ID" "$original_bio")
        if [[ "$restore_code" == "204" || "$restore_code" == "200" ]]; then
          echo "[INFO] Restored original biography on Jellyfin"
        else
          echo "[WARN] Failed to restore biography on Jellyfin (HTTP $restore_code)"
        fi
      fi
    fi

    echo ""
  fi
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
