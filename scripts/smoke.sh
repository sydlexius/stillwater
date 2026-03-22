#!/usr/bin/env bash
# smoke.sh -- API smoke test suite for Stillwater
#
# Usage:
#   bash scripts/smoke.sh [--full] [--roundtrip] [--music-path PATH ...]
#
# Environment:
#   SW_USER        -- admin username (default: admin)
#   SW_PASS        -- admin password (default: admin)
#   SW_BASE        -- base URL       (default: http://localhost:1973)
#   SW_MUSIC_PATH  -- colon-separated music path(s) for roundtrip (optional)
#   EMBY_URL       -- Emby server URL (used by --roundtrip)
#   EMBY_API_KEY   -- Emby API key    (used by --roundtrip)
#   JELLYFIN_URL   -- Jellyfin server URL (used by --roundtrip)
#   JELLYFIN_API_KEY -- Jellyfin API key  (used by --roundtrip)
#
# --full        enables Tier 4 destructive/stateful checks (off by default)
# --roundtrip   enables Tier 5 NFO roundtrip checks (off by default)
# --music-path  music directory for roundtrip artist discovery (repeatable)
#
# Music path precedence (for roundtrip artist filtering):
#   1. --music-path CLI arguments (highest)
#   2. SW_MUSIC_PATH environment variable (colon-separated)
#   3. Accept any artist with a filesystem path (current default fallback)

set -euo pipefail

SW_USER="${SW_USER:-admin}"
SW_PASS="${SW_PASS:-admin}"
SW_BASE="${SW_BASE:-http://localhost:1973}"

FULL=0
ROUNDTRIP=0
MUSIC_PATHS=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --full) FULL=1; shift ;;
    --roundtrip) ROUNDTRIP=1; shift ;;
    --music-path)
      if [[ -z "${2:-}" ]]; then
        echo "ERROR: --music-path requires a value"
        exit 1
      fi
      if [[ "${2:-}" == --* ]]; then
        echo "ERROR: --music-path value looks like a flag: $2"
        exit 1
      fi
      MUSIC_PATHS+=("$2")
      shift 2
      ;;
    *)
      echo "ERROR: unknown argument: $1"
      echo "Usage: bash scripts/smoke.sh [--full] [--roundtrip] [--music-path PATH ...]"
      exit 1
      ;;
  esac
done

# If no --music-path CLI args, fall back to SW_MUSIC_PATH env var (colon-separated).
# Filter out empty segments from leading/trailing colons or consecutive colons.
if [[ ${#MUSIC_PATHS[@]} -eq 0 && -n "${SW_MUSIC_PATH:-}" ]]; then
  IFS=':' read -ra _raw_paths <<< "$SW_MUSIC_PATH"
  for _p in "${_raw_paths[@]}"; do
    [[ -n "$_p" ]] && MUSIC_PATHS+=("$_p")
  done
fi

# path_under_music_dirs checks whether a given path is under one of the
# configured music directories. Returns 0 (true) if MUSIC_PATHS is empty
# (no filtering) or if the path starts with any entry in MUSIC_PATHS.
path_under_music_dirs() {
  local candidate="$1"
  if [[ ${#MUSIC_PATHS[@]} -eq 0 ]]; then
    return 0  # no filter configured -- accept any path
  fi
  for mp in "${MUSIC_PATHS[@]}"; do
    # Normalize: ensure music path ends with / for prefix matching so that
    # /music does not match /music2, but does match /music/artist.
    local normalized="${mp%/}/"
    if [[ "$candidate/" == "$normalized"* || "$candidate" == "${mp%/}" ]]; then
      return 0
    fi
  done
  return 1
}

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
#
# Curl failure convention: Tier 3-5 curl calls guard against transport
# failures so that a curl error records a FAIL rather than aborting the
# script under set -e. Three guard patterns are used:
#   || code="000"      -- status-only calls (feeds assert_status)
#   || resp=$'\n000'   -- body+status calls (feeds sed/tail + assert_status)
#   || variable=""     -- body-only calls (feeds assert_json_exists or manual checks)
# Status "000" means curl itself failed (DNS, connection refused, timeout),
# not an HTTP response. Tier 1 calls are left unguarded intentionally -- if
# the server is unreachable during core checks, the entire test run is
# unreliable and immediate abort is correct. Tier 2 guards data-modifying
# push/delete/re-push calls and the platform-ids discovery GETs that gate
# them; Tier 2 assertion GETs (platform-ids status check, platform-state)
# remain unguarded. Tier 5 roundtrip discovery calls are also guarded.

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
# Returns sentinel JSON with ._error on failure:
#   HTTP_REQUEST_FAILED  -- curl could not complete the request
#   HTTP_<status>        -- non-200 response from the platform
#   PARSE_ERROR          -- response body is not valid JSON
#   ITEM_NOT_FOUND       -- Items array is empty or null
# Always returns exit code 0; callers must check ._error in the output JSON.
platform_get_item() {
  local url="$1" key="$2" item_id="$3"
  local resp body status parsed

  if ! resp=$(curl -s -w 'HTTPSTATUS:%{http_code}' \
    "${url}/Items?api_key=${key}&Ids=${item_id}&Fields=Overview,ProviderIds,PremiereDate,EndDate,Genres,Tags,TagItems"); then
    echo '{"_error":"HTTP_REQUEST_FAILED","status":0}'
    return 0
  fi

  body=${resp%HTTPSTATUS:*}
  status=${resp##*HTTPSTATUS:}

  if [[ "$status" != "200" ]]; then
    echo "{\"_error\":\"HTTP_${status}\",\"status\":${status}}"
    return 0
  fi

  if ! parsed=$(jq '.Items[0]' 2>/dev/null <<<"$body"); then
    echo '{"_error":"PARSE_ERROR","status":200}'
    return 0
  fi

  if [[ -z "$parsed" || "$parsed" == "null" ]]; then
    echo '{"_error":"ITEM_NOT_FOUND","status":200}'
    return 0
  fi

  echo "$parsed"
}

# Fetch an item, modify Overview, and POST it back.
# GET omits TagItems from Fields because this function only mutates Overview;
# unspecified fields are preserved by the platform on POST.
# POST body strips read-only fields that cause 400 errors on Jellyfin
# (ServerId, ImageBlurHashes, etc.) while keeping fields Emby requires.
# Returns the POST HTTP status code, or a sentinel if something fails:
#   GET stage:  CURL_FAILED, GET_FAILED_<status>, GET_PARSE_ERROR, GET_EMPTY_ITEMS
#   POST stage: UPDATE_BODY_PARSE_ERROR, UPDATE_BODY_EMPTY, POST_CURL_FAILED
# Always returns exit code 0.
platform_modify_overview() {
  local url="$1" key="$2" item_id="$3" new_overview="$4"
  local resp body status

  if ! resp=$(curl -s -w 'HTTPSTATUS:%{http_code}' \
    "${url}/Items?api_key=${key}&Ids=${item_id}&Fields=Overview,ProviderIds,PremiereDate,EndDate,Genres,Tags"); then
    echo "CURL_FAILED"
    return 0
  fi

  body=${resp%HTTPSTATUS:*}
  status=${resp##*HTTPSTATUS:}

  if [[ "$status" != "200" ]]; then
    echo "GET_FAILED_${status}"
    return 0
  fi

  local first_item
  if ! first_item=$(echo "$body" | jq '.Items[0]' 2>/dev/null); then
    echo "GET_PARSE_ERROR"
    return 0
  fi

  if [[ -z "$first_item" || "$first_item" == "null" ]]; then
    echo "GET_EMPTY_ITEMS"
    return 0
  fi

  local update_body
  if ! update_body=$(echo "$first_item" | jq --arg bio "$new_overview" \
    'del(.ServerId, .ImageBlurHashes, .ImageTags, .BackdropImageTags, .LocationType, .MediaType, .ChannelId) | .Overview = $bio'); then
    echo "UPDATE_BODY_PARSE_ERROR"
    return 0
  fi

  if [[ -z "$update_body" || "$update_body" == "null" ]]; then
    echo "UPDATE_BODY_EMPTY"
    return 0
  fi

  local post_status
  if ! post_status=$(curl -s -o /dev/null -w "%{http_code}" \
    -X POST "${url}/Items/${item_id}?api_key=${key}" \
    -H "Content-Type: application/json" \
    -d "$update_body"); then
    echo "POST_CURL_FAILED"
    return 0
  fi
  echo "$post_status"
}

# Portable mtime helper: GNU stat uses -c %Y, BSD/macOS uses -f %m.
# Returns "MISSING" if the file does not exist, "ERROR" on other stat failures.
# Always returns exit code 0; callers check the output string.
get_mtime() {
  local path="$1"
  if [[ ! -e "$path" ]]; then
    echo "MISSING"
    return 0
  fi
  local mtime
  if stat --version &>/dev/null; then
    mtime=$(stat -c %Y "$path" 2>/dev/null) || { echo "ERROR"; return 0; }
  else
    mtime=$(stat -f %m "$path" 2>/dev/null) || { echo "ERROR"; return 0; }
  fi
  if [[ -z "$mtime" ]]; then echo "ERROR"; return 0; fi
  echo "$mtime"
}

# Poll for an NFO file's mtime to change (indicates platform wrote it).
# Returns 0 if mtime changed within the timeout, 1 if it did not.
# When original_mtime is "MISSING", any file creation counts as a change.
# Defaults: timeout=30s, interval=3s.
wait_for_nfo_change() {
  local nfo_path="$1" original_mtime="$2" timeout="${3:-30}" interval="${4:-3}"
  local elapsed=0
  while [[ $elapsed -lt $timeout ]]; do
    sleep "$interval"
    elapsed=$((elapsed + interval))
    local current_mtime
    current_mtime=$(get_mtime "$nfo_path")
    if [[ "$current_mtime" == "ERROR" ]]; then
      continue  # stat failed this cycle; retry
    fi
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
      "$SW_BASE/api/v1/auth/tokens/$TOKEN_ID") || code="CURL_FAILED"
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
[[ -n "$CONN_LIDARR" ]] && echo "  Lidarr connection: $CONN_LIDARR"

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

# Push images to Emby -- push/delete/re-push cycle for all 4 image types.
# Requires a stored platform ID for this artist on the Emby connection.
if [[ -n "$CONN_EMBY" ]]; then
  emby_pid_resp=$(curl -s "${AUTH[@]}" \
    "$SW_BASE/api/v1/artists/$ARTIST_ID/platform-ids") || emby_pid_resp=""
  if [[ -z "$emby_pid_resp" ]]; then
    echo "[SKIP] Emby image push -- platform-ids API call failed"
  else
  emby_platform_id=$(echo "$emby_pid_resp" | \
    jq -r ".[] | select(.connection_id==\"$CONN_EMBY\") | .platform_artist_id" 2>/dev/null || echo "")
  if [[ -z "$emby_platform_id" || "$emby_platform_id" == "null" ]]; then
    echo "[SKIP] Emby image push -- no stored platform ID for this artist"
  else
    # Push all 4 image types at once.
    push_emby_resp=$(curl -s -w "\n%{http_code}" "${AUTH[@]}" \
      -X POST "$SW_BASE/api/v1/artists/$ARTIST_ID/push/images" \
      -H "Content-Type: application/json" \
      -d "{\"connection_id\":\"$CONN_EMBY\",\"image_types\":[\"thumb\",\"fanart\",\"logo\",\"banner\"]}") || push_emby_resp=$'\n000'
    push_emby_body=$(echo "$push_emby_resp" | sed '$d')
    push_emby_code=$(echo "$push_emby_resp" | tail -n 1)
    assert_status "POST push/images all types (Emby)" "200" "$push_emby_code"
    if [[ "$push_emby_code" == "200" ]]; then
      emby_err_count=$(echo "$push_emby_body" | jq -r '.errors | length' 2>/dev/null || echo "0")
      if [[ "$emby_err_count" != "0" && "$emby_err_count" != "null" ]]; then
        emby_errors=$(echo "$push_emby_body" | jq -r '.errors[]?' 2>/dev/null || echo "")
        echo "[FAIL] POST push/images all types (Emby) -- upload errors: $emby_errors"
        FAIL=$((FAIL + 1))
        FAILURES+=("POST push/images all types (Emby) -- upload errors: $emby_errors")
      else
        echo "[PASS] POST push/images all types (Emby) -- 200 no errors"
        PASS=$((PASS + 1))
      fi
    fi

    # Delete banner from Emby.
    del_emby_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
      -X DELETE "$SW_BASE/api/v1/artists/$ARTIST_ID/push/images/banner" \
      -H "Content-Type: application/json" \
      -d "{\"connection_id\":\"$CONN_EMBY\"}") || del_emby_code="000"
    assert_status_in "DELETE push/images/banner (Emby)" "$del_emby_code" 200 204

    # Re-push banner to restore. Retry once if the first attempt fails,
    # since a successful delete with a failed re-push leaves the banner
    # deleted on the platform.
    repush_emby_resp=$(curl -s -w "\n%{http_code}" "${AUTH[@]}" \
      -X POST "$SW_BASE/api/v1/artists/$ARTIST_ID/push/images" \
      -H "Content-Type: application/json" \
      -d "{\"connection_id\":\"$CONN_EMBY\",\"image_types\":[\"banner\"]}") || repush_emby_resp=$'\n000'
    repush_emby_body=$(echo "$repush_emby_resp" | sed '$d')
    repush_emby_code=$(echo "$repush_emby_resp" | tail -n 1)
    if [[ "$repush_emby_code" != "200" && ("$del_emby_code" == "200" || "$del_emby_code" == "204") ]]; then
      echo "[WARN] Emby banner re-push failed (HTTP $repush_emby_code), retrying..."
      sleep 2
      repush_emby_resp=$(curl -s -w "\n%{http_code}" "${AUTH[@]}" \
        -X POST "$SW_BASE/api/v1/artists/$ARTIST_ID/push/images" \
        -H "Content-Type: application/json" \
        -d "{\"connection_id\":\"$CONN_EMBY\",\"image_types\":[\"banner\"]}") || repush_emby_resp=$'\n000'
      repush_emby_body=$(echo "$repush_emby_resp" | sed '$d')
      repush_emby_code=$(echo "$repush_emby_resp" | tail -n 1)
    fi
    assert_status "POST push/images banner re-push (Emby)" "200" "$repush_emby_code"
    if [[ "$repush_emby_code" == "200" ]]; then
      repush_err=$(echo "$repush_emby_body" | jq -r '.errors | length' 2>/dev/null || echo "0")
      if [[ "$repush_err" != "0" && "$repush_err" != "null" ]]; then
        echo "[FAIL] POST push/images banner re-push (Emby) -- errors: $(echo "$repush_emby_body" | jq -r '.errors[]?' 2>/dev/null)"
        FAIL=$((FAIL + 1))
        FAILURES+=("POST push/images banner re-push (Emby) -- errors")
      else
        echo "[PASS] POST push/images banner re-push (Emby) -- 200 no errors"
        PASS=$((PASS + 1))
      fi
    fi
  fi
  fi  # emby_pid_resp guard
else
  echo "[SKIP] Emby image push -- no Emby connection found"
fi

# Push images to Jellyfin -- push/delete/re-push cycle for all 4 image types.
# Requires a stored platform ID for this artist on the Jellyfin connection.
if [[ -n "$CONN_JELLYFIN" ]]; then
  jf_pid_resp=$(curl -s "${AUTH[@]}" \
    "$SW_BASE/api/v1/artists/$ARTIST_ID/platform-ids") || jf_pid_resp=""
  if [[ -z "$jf_pid_resp" ]]; then
    echo "[SKIP] Jellyfin image push -- platform-ids API call failed"
    echo "[SKIP] POST push metadata (Jellyfin) -- platform-ids API call failed"
  else
  jf_platform_id=$(echo "$jf_pid_resp" | \
    jq -r ".[] | select(.connection_id==\"$CONN_JELLYFIN\") | .platform_artist_id" 2>/dev/null || echo "")
  if [[ -z "$jf_platform_id" || "$jf_platform_id" == "null" ]]; then
    echo "[SKIP] Jellyfin image push -- no stored platform ID for this artist"
    echo "[SKIP] POST push metadata (Jellyfin) -- no stored platform ID for this artist"
  else
    # Push all 4 image types at once.
    push_jf_resp=$(curl -s -w "\n%{http_code}" "${AUTH[@]}" \
      -X POST "$SW_BASE/api/v1/artists/$ARTIST_ID/push/images" \
      -H "Content-Type: application/json" \
      -d "{\"connection_id\":\"$CONN_JELLYFIN\",\"image_types\":[\"thumb\",\"fanart\",\"logo\",\"banner\"]}") || push_jf_resp=$'\n000'
    push_jf_body=$(echo "$push_jf_resp" | sed '$d')
    push_jf_code=$(echo "$push_jf_resp" | tail -n 1)
    assert_status_in "POST push/images all types (Jellyfin)" "$push_jf_code" 200 204
    if [[ "$push_jf_code" == "200" ]]; then
      jf_err_count=$(echo "$push_jf_body" | jq -r '.errors | length' 2>/dev/null || echo "0")
      if [[ "$jf_err_count" != "0" && "$jf_err_count" != "null" ]]; then
        jf_errors=$(echo "$push_jf_body" | jq -r '.errors[]?' 2>/dev/null || echo "")
        echo "[FAIL] POST push/images all types (Jellyfin) -- upload errors: $jf_errors"
        FAIL=$((FAIL + 1))
        FAILURES+=("POST push/images all types (Jellyfin) -- upload errors: $jf_errors")
      else
        echo "[PASS] POST push/images all types (Jellyfin) -- 200 no errors"
        PASS=$((PASS + 1))
      fi
    fi

    # Delete banner from Jellyfin.
    del_jf_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
      -X DELETE "$SW_BASE/api/v1/artists/$ARTIST_ID/push/images/banner" \
      -H "Content-Type: application/json" \
      -d "{\"connection_id\":\"$CONN_JELLYFIN\"}") || del_jf_code="000"
    assert_status_in "DELETE push/images/banner (Jellyfin)" "$del_jf_code" 200 204

    # Re-push banner to restore. Retry once if the first attempt fails,
    # since a successful delete with a failed re-push leaves the banner
    # deleted on the platform.
    repush_jf_resp=$(curl -s -w "\n%{http_code}" "${AUTH[@]}" \
      -X POST "$SW_BASE/api/v1/artists/$ARTIST_ID/push/images" \
      -H "Content-Type: application/json" \
      -d "{\"connection_id\":\"$CONN_JELLYFIN\",\"image_types\":[\"banner\"]}") || repush_jf_resp=$'\n000'
    repush_jf_body=$(echo "$repush_jf_resp" | sed '$d')
    repush_jf_code=$(echo "$repush_jf_resp" | tail -n 1)
    if [[ "$repush_jf_code" != "200" && "$repush_jf_code" != "204" && ("$del_jf_code" == "200" || "$del_jf_code" == "204") ]]; then
      echo "[WARN] Jellyfin banner re-push failed (HTTP $repush_jf_code), retrying..."
      sleep 2
      repush_jf_resp=$(curl -s -w "\n%{http_code}" "${AUTH[@]}" \
        -X POST "$SW_BASE/api/v1/artists/$ARTIST_ID/push/images" \
        -H "Content-Type: application/json" \
        -d "{\"connection_id\":\"$CONN_JELLYFIN\",\"image_types\":[\"banner\"]}") || repush_jf_resp=$'\n000'
      repush_jf_body=$(echo "$repush_jf_resp" | sed '$d')
      repush_jf_code=$(echo "$repush_jf_resp" | tail -n 1)
    fi
    assert_status_in "POST push/images banner re-push (Jellyfin)" "$repush_jf_code" 200 204
    if [[ "$repush_jf_code" == "200" ]]; then
      repush_jf_err=$(echo "$repush_jf_body" | jq -r '.errors | length' 2>/dev/null || echo "0")
      if [[ "$repush_jf_err" != "0" && "$repush_jf_err" != "null" ]]; then
        echo "[FAIL] POST push/images banner re-push (Jellyfin) -- errors: $(echo "$repush_jf_body" | jq -r '.errors[]?' 2>/dev/null)"
        FAIL=$((FAIL + 1))
        FAILURES+=("POST push/images banner re-push (Jellyfin) -- errors")
      else
        echo "[PASS] POST push/images banner re-push (Jellyfin) -- 200 no errors"
        PASS=$((PASS + 1))
      fi
    fi

    # Push metadata to Jellyfin
    push_meta_resp=$(curl -s -w "\n%{http_code}" "${AUTH[@]}" \
      -X POST "$SW_BASE/api/v1/artists/$ARTIST_ID/push" \
      -H "Content-Type: application/json" \
      -d "{\"connection_id\":\"$CONN_JELLYFIN\"}") || push_meta_resp=$'\n000'
    push_meta_code=$(echo "$push_meta_resp" | tail -n 1)
    assert_status_in "POST push metadata (Jellyfin)" "$push_meta_code" 200 204
  fi
  fi  # jf_pid_resp guard
else
  echo "[SKIP] Jellyfin image push -- no Jellyfin connection found"
fi

echo ""

# ---------------------------------------------------------------------------
# Tier 3: Feature Coverage
# ---------------------------------------------------------------------------

echo "--- Tier 3: Feature Coverage ---"
echo ""

nfo_diff_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/artists/$ARTIST_ID/nfo/diff") || nfo_diff_code="000"
assert_status_in "GET /api/v1/artists/$ARTIST_ID/nfo/diff" "$nfo_diff_code" 200 422

nfo_conflict_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/artists/$ARTIST_ID/nfo/conflict") || nfo_conflict_code="000"
# 422 is returned when the artist's library has no filesystem path configured; treat as non-fatal
assert_status_in "GET /api/v1/artists/$ARTIST_ID/nfo/conflict" "$nfo_conflict_code" 200 422

nfo_snaps_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/artists/$ARTIST_ID/nfo/snapshots") || nfo_snaps_code="000"
assert_status "GET /api/v1/artists/$ARTIST_ID/nfo/snapshots" "200" "$nfo_snaps_code"

health_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/artists/$ARTIST_ID/health") || health_code="000"
assert_status "GET /api/v1/artists/$ARTIST_ID/health" "200" "$health_code"

dupes_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/artists/duplicates") || dupes_code="000"
assert_status "GET /api/v1/artists/duplicates" "200" "$dupes_code"

aliases_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/artists/$ARTIST_ID/aliases") || aliases_code="000"
assert_status "GET /api/v1/artists/$ARTIST_ID/aliases" "200" "$aliases_code"

libraries_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/libraries") || libraries_code="000"
assert_status "GET /api/v1/libraries" "200" "$libraries_code"

if [[ -n "$CONN_EMBY" ]]; then
  disc_emby_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
    "$SW_BASE/api/v1/connections/$CONN_EMBY/libraries") || disc_emby_code="000"
  assert_status_in "GET /api/v1/connections/$CONN_EMBY/libraries (Emby discover)" "$disc_emby_code" 200 409 502 503
else
  echo "[SKIP] Emby library discover -- no Emby connection found"
fi

if [[ -n "$CONN_JELLYFIN" ]]; then
  disc_jf_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
    "$SW_BASE/api/v1/connections/$CONN_JELLYFIN/libraries") || disc_jf_code="000"
  assert_status_in "GET /api/v1/connections/$CONN_JELLYFIN/libraries (Jellyfin discover)" "$disc_jf_code" 200 409 502 503
else
  echo "[SKIP] Jellyfin library discover -- no Jellyfin connection found"
fi

rules_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/rules") || rules_code="000"
assert_status "GET /api/v1/rules" "200" "$rules_code"

# Fanart list endpoint -- verifies multi-backdrop support is accessible
fanart_list_resp=$(curl -s -w "\n%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/artists/$ARTIST_ID/images/fanart/list") || fanart_list_resp=$'\n000'
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
fanart_count_resp=$(curl -s "${AUTH[@]}" "$SW_BASE/api/v1/artists/$ARTIST_ID") || fanart_count_resp=""
if [[ -z "$fanart_count_resp" ]]; then
  echo "[FAIL]   artist detail fanart_count -- curl transport failure (server unreachable)"
  FAIL=$((FAIL + 1))
  FAILURES+=("artist detail fanart_count -- curl transport failure")
else
  assert_json_exists "  artist detail has fanart_count field" ".artist.fanart_count" "$fanart_count_resp"
fi

notif_counts_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/notifications/counts") || notif_counts_code="000"
assert_status "GET /api/v1/notifications/counts" "200" "$notif_counts_code"

notif_badge_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/notifications/badge") || notif_badge_code="000"
assert_status "GET /api/v1/notifications/badge" "200" "$notif_badge_code"

# Fix-undo route (renamed from /notifications/undo/{undoId} to /fix-undo/{undoId})
fix_undo_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  -X POST "$SW_BASE/api/v1/fix-undo/nonexistent-undo-id") || fix_undo_code="000"
assert_status_in "POST /api/v1/fix-undo/{undoId} (nonexistent)" "$fix_undo_code" 404 400 410

report_health_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/reports/health") || report_health_code="000"
assert_status "GET /api/v1/reports/health" "200" "$report_health_code"

report_compliance_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/reports/compliance") || report_compliance_code="000"
assert_status "GET /api/v1/reports/compliance" "200" "$report_compliance_code"

bulk_jobs_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/bulk/jobs") || bulk_jobs_code="000"
assert_status "GET /api/v1/bulk/jobs" "200" "$bulk_jobs_code"

scanner_status_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/scanner/status") || scanner_status_code="000"
assert_status "GET /api/v1/scanner/status" "200" "$scanner_status_code"

providers_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/providers") || providers_code="000"
assert_status "GET /api/v1/providers" "200" "$providers_code"

priorities_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/providers/priorities") || priorities_code="000"
assert_status "GET /api/v1/providers/priorities" "200" "$priorities_code"

backup_hist_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/settings/backup/history") || backup_hist_code="000"
assert_status "GET /api/v1/settings/backup/history" "200" "$backup_hist_code"

logging_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/settings/logging") || logging_code="000"
assert_status "GET /api/v1/settings/logging" "200" "$logging_code"

maint_status_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/settings/maintenance/status") || maint_status_code="000"
assert_status "GET /api/v1/settings/maintenance/status" "200" "$maint_status_code"

webhooks_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/webhooks") || webhooks_code="000"
assert_status "GET /api/v1/webhooks" "200" "$webhooks_code"

tokens_list_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
  "$SW_BASE/api/v1/auth/tokens") || tokens_list_code="000"
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
    -d '{"url":"https://upload.wikimedia.org/wikipedia/en/a/aa/A-ha_band_2015.jpg","type":"thumb"}') || fetch_img_code="000"
  assert_status_in "POST /api/v1/artists/$ARTIST_ID/images/fetch (--full)" "$fetch_img_code" 200 422

  # Populate from Emby or Jellyfin and verify the response includes the expected fields.
  # This exercises the multi-backdrop download path.
  if [[ -n "$LIBRARY_ID" && -n "$LIB_CONN_ID" && "$LIB_CONN_ID" != "null" ]]; then
    pop_resp=$(curl -s -w "\n%{http_code}" "${AUTH[@]}" \
      -X POST "$SW_BASE/api/v1/connections/$LIB_CONN_ID/libraries/$LIBRARY_ID/populate") || pop_resp=$'\n000'
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
    -X POST "$SW_BASE/api/v1/settings/backup") || backup_code="000"
  assert_status "POST /api/v1/settings/backup (--full)" "200" "$backup_code"

  echo ""
fi

# ---------------------------------------------------------------------------
# Tier 5: NFO Roundtrip (opt-in with --roundtrip)
# ---------------------------------------------------------------------------

if [[ "$ROUNDTRIP" -eq 1 ]]; then
  echo "--- Tier 5: NFO Roundtrip (--roundtrip) ---"
  echo ""
  if [[ ${#MUSIC_PATHS[@]} -gt 0 ]]; then
    echo "  Music path filter: ${MUSIC_PATHS[*]}"
  fi

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
    rt_detail=$(curl -s "${AUTH[@]}" "$SW_BASE/api/v1/artists/$ARTIST_ID") || rt_detail=""
    if [[ -z "$rt_detail" ]]; then
      echo "[WARN] Roundtrip artist detail -- transport failure, falling back to artist scan"
    fi
    rt_path=$(echo "$rt_detail" | jq -r '.artist.path // empty' 2>/dev/null || true)
    if [[ -n "$rt_path" ]] && path_under_music_dirs "$rt_path"; then
      RT_ARTIST_ID="$ARTIST_ID"
      RT_ARTIST_PATH="$rt_path"
      RT_ARTIST_NAME=$(echo "$rt_detail" | jq -r '.artist.name // empty' 2>/dev/null || true)
    fi
  fi

  # If a-ha does not have a path, scan the first 50 artists for one that does.
  if [[ -z "$RT_ARTIST_ID" ]]; then
    rt_list=$(curl -s "${AUTH[@]}" "$SW_BASE/api/v1/artists?page_size=50") || rt_list=""
    if [[ -z "$rt_list" ]]; then
      echo "[WARN] Roundtrip artist list -- transport failure, no artists to scan"
    fi
    rt_ids=$(echo "$rt_list" | jq -r '.artists[].id // empty' 2>/dev/null || true)
    for candidate_id in $rt_ids; do
      candidate_detail=$(curl -s "${AUTH[@]}" "$SW_BASE/api/v1/artists/$candidate_id") || candidate_detail=""
      candidate_path=$(echo "$candidate_detail" | jq -r '.artist.path // empty' 2>/dev/null || true)
      if [[ -n "$candidate_path" ]] && path_under_music_dirs "$candidate_path"; then
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
    rt_plat_ids=$(curl -s "${AUTH[@]}" "$SW_BASE/api/v1/artists/$RT_ARTIST_ID/platform-ids") || rt_plat_ids=""
    if [[ -z "$rt_plat_ids" ]]; then
      echo "[WARN] Roundtrip platform-ids -- transport failure, platform tests may be skipped"
    fi
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
    clobber_resp=$(curl -s "${AUTH[@]}" "$SW_BASE/api/v1/connections/clobber-check") || clobber_resp=""
    if [[ -z "$clobber_resp" ]]; then
      echo "[WARN] Roundtrip clobber-check -- transport failure, NFO writer detection skipped"
    fi
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
    sw_artist_orig=$(curl -s "${AUTH[@]}" "$SW_BASE/api/v1/artists/$RT_ARTIST_ID") || sw_artist_orig=""
    if ! echo "$sw_artist_orig" | jq -e '.artist.id' >/dev/null 2>&1; then
      echo "[FAIL] Roundtrip -- failed to fetch original artist data (invalid API response)"
      FAIL=$((FAIL + 1))
      FAILURES+=("Roundtrip -- failed to fetch original artist data for restore")
      RT_SETUP_OK=0
    else
      RT_SETUP_OK=1
    fi

    if [[ "$RT_SETUP_OK" -eq 1 ]]; then

    RT_ARTIST_TYPE=$(echo "$sw_artist_orig" | jq -r '.artist.type // "group"' 2>/dev/null || echo "group")
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
    # not JSON arrays. The server-side extractFieldValue handler reads the
    # value as a plain string.
    RT_GENRES="Synth-Pop, New Wave"
    RT_STYLES="Electronic, Scandinavian"
    RT_MOODS="Melancholic, Uplifting"

    # Write test values to Stillwater via the field-edit API.
    # Self-reports failures via FAIL counter; callers use || true to avoid set -e abort.
    sw_patch() {
      local field="$1" value="$2"
      local code
      if ! code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
        -X PATCH "$SW_BASE/api/v1/artists/$RT_ARTIST_ID/fields/$field" \
        -H "Content-Type: application/json" \
        -d "{\"value\":$value}"); then
        echo "[FAIL] sw_patch $field (curl failed)"
        FAIL=$((FAIL + 1))
        FAILURES+=("sw_patch $field -- curl command failed")
        return 1
      fi
      if [[ "$code" != "200" && "$code" != "204" ]]; then
        echo "[FAIL] sw_patch $field (HTTP $code)"
        FAIL=$((FAIL + 1))
        FAILURES+=("sw_patch $field failed (HTTP $code)")
        return 1
      fi
      return 0
    }

    echo "  Setting synthetic test data on Stillwater artist..."
    pre_setup_fails=$FAIL
    sw_patch "biography" "$(jq -n --arg v "$RT_BIO" '$v')" || true
    sw_patch "formed" "$(jq -n --arg v "$RT_FORMED" '$v')" || true
    sw_patch "disbanded" "$(jq -n --arg v "$RT_DISBANDED" '$v')" || true
    sw_patch "genres" "$(jq -n --arg v "$RT_GENRES" '$v')" || true
    sw_patch "styles" "$(jq -n --arg v "$RT_STYLES" '$v')" || true
    sw_patch "moods" "$(jq -n --arg v "$RT_MOODS" '$v')" || true

    if [[ $FAIL -gt $pre_setup_fails ]]; then
      echo "[SKIP] Roundtrip Direction 1 -- setup patches failed; results would be unreliable"
    else

    # Re-read artist to get the canonical values Stillwater will push.
    sw_artist=$(curl -s "${AUTH[@]}" "$SW_BASE/api/v1/artists/$RT_ARTIST_ID") || sw_artist=""
    if ! echo "$sw_artist" | jq -e '.artist.id' >/dev/null 2>&1; then
      echo "[FAIL] Roundtrip -- failed to re-read artist after patching test values"
      FAIL=$((FAIL + 1))
      FAILURES+=("Roundtrip -- failed to re-read artist after patching test values")
      # Use synthetic values as expected baseline so assertions fail clearly
      # rather than comparing empty strings against platform data.
      sw_bio="$RT_BIO"
      sw_mbid=""
    else
      sw_bio=$(echo "$sw_artist" | jq -r '.artist.biography // empty' 2>/dev/null || true)
      sw_mbid=$(echo "$sw_artist" | jq -r '.artist.musicbrainz_id // empty' 2>/dev/null || true)
    fi

    # Validate lockdata protection in the artist's NFO file.
    # After patching fields, Stillwater should have written the NFO with
    # <lockdata>true</lockdata> to prevent Emby/Jellyfin from overwriting it.
    if [[ -n "$RT_ARTIST_PATH" && -f "$RT_ARTIST_PATH/artist.nfo" ]]; then
      if grep -q '<lockdata>true</lockdata>' "$RT_ARTIST_PATH/artist.nfo"; then
        echo "[PASS] NFO lockdata -- artist.nfo contains <lockdata>true</lockdata>"
        PASS=$((PASS + 1))
      else
        echo "[FAIL] NFO lockdata -- artist.nfo missing <lockdata>true</lockdata>"
        FAIL=$((FAIL + 1))
        FAILURES+=("NFO lockdata -- artist.nfo missing <lockdata>true</lockdata>")
      fi
    else
      echo "[SKIP] NFO lockdata -- artist has no filesystem path or no NFO file"
    fi

    # --- Direction 1 per-platform verification function ---
    # Pushes metadata from Stillwater to a single platform, verifies the pushed
    # fields match, then returns.
    verify_push_to_platform() {
      local platform="$1" platform_url="$2" platform_key="$3" platform_item_id="$4" conn_id="$5"

      # Push metadata from Stillwater to the platform.
      local push_code
      if ! push_code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" \
        -X POST "$SW_BASE/api/v1/artists/$RT_ARTIST_ID/push" \
        -H "Content-Type: application/json" \
        -d "{\"connection_id\":\"$conn_id\"}"); then
        echo "[FAIL] $platform Direction 1 -- push curl command failed"
        FAIL=$((FAIL + 1))
        FAILURES+=("$platform Direction 1 -- push curl command failed")
        return
      fi
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

      # Check for sentinel error from platform_get_item.
      local p_error
      p_error=$(echo "$p_item" | jq -r '._error // empty' 2>/dev/null || true)
      if [[ -n "$p_error" ]]; then
        echo "[FAIL] $platform Direction 1 -- platform_get_item failed: $p_error"
        FAIL=$((FAIL + 1))
        FAILURES+=("$platform Direction 1 -- platform_get_item failed: $p_error")
        return
      fi

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

      # Formed/Disbanded -> PremiereDate/EndDate (group types only).
      # Non-group artists use Born/Died which are not standard Items API fields.
      if [[ "$RT_ARTIST_TYPE" != "group" && "$RT_ARTIST_TYPE" != "orchestra" && "$RT_ARTIST_TYPE" != "choir" ]]; then
        echo "[SKIP] $platform roundtrip formed/disbanded -- $RT_ARTIST_TYPE type (uses born/died, not PremiereDate/EndDate)"
      else
        local p_date_short="${p_date:0:10}"
        assert_json_value "$platform roundtrip formed->PremiereDate" "$RT_FORMED" "$p_date_short"

        local p_end_short="${p_end_date:0:10}"
        assert_json_value "$platform roundtrip disbanded->EndDate" "$RT_DISBANDED" "$p_end_short"
      fi

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
      verify_push_to_platform "Emby" "$EMBY_URL" "$EMBY_API_KEY" "$RT_EMBY_PLATFORM_ID" "$CONN_EMBY"
    fi

    # --- Jellyfin Direction 1 ---
    if [[ -z "${JELLYFIN_URL:-}" || -z "${JELLYFIN_API_KEY:-}" ]]; then
      echo "[SKIP] Jellyfin Direction 1 -- JELLYFIN_URL or JELLYFIN_API_KEY not set"
    elif [[ -z "$RT_JELLYFIN_PLATFORM_ID" ]]; then
      echo "[SKIP] Jellyfin Direction 1 -- no Jellyfin platform ID for roundtrip artist"
    elif ! curl -sf "${JELLYFIN_URL}/System/Info?api_key=${JELLYFIN_API_KEY}" >/dev/null 2>&1; then
      echo "[SKIP] Jellyfin Direction 1 -- Jellyfin unreachable at $JELLYFIN_URL"
    else
      verify_push_to_platform "Jellyfin" "$JELLYFIN_URL" "$JELLYFIN_API_KEY" "$RT_JELLYFIN_PLATFORM_ID" "$CONN_JELLYFIN"
    fi

    fi  # pre_setup_fails guard

    # Restore original values on Stillwater artist.
    echo "  Restoring original artist data on Stillwater..."
    pre_restore_fails=$FAIL
    sw_patch "biography" "$(jq -n --arg v "$orig_bio" '$v')" || true
    if [[ -n "$orig_formed" ]]; then
      sw_patch "formed" "$(jq -n --arg v "$orig_formed" '$v')" || true
    else
      sw_patch "formed" '""' || true
    fi
    if [[ -n "$orig_disbanded" ]]; then
      sw_patch "disbanded" "$(jq -n --arg v "$orig_disbanded" '$v')" || true
    else
      sw_patch "disbanded" '""' || true
    fi
    sw_patch "genres" "$(jq -n --arg v "$orig_genres" '$v')" || true
    sw_patch "styles" "$(jq -n --arg v "$orig_styles" '$v')" || true
    sw_patch "moods" "$(jq -n --arg v "$orig_moods" '$v')" || true
    restore_fail_count=$((FAIL - pre_restore_fails))
    if [[ $restore_fail_count -gt 0 ]]; then
      echo "[FAIL] $restore_fail_count restore patches failed -- artist data may have test values"
    fi

    # Each sw_patch triggers fire-and-forget background pushes to Emby/Jellyfin
    # on the server side. Wait for them to settle before
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

    # --- Direction 2 per-platform verification function ---
    # Modifies biography on the platform, waits for NFO write, verifies
    # diff/conflict endpoints, then restores the original biography.
    # Args: platform platform_url platform_key platform_item_id [timeout=30] [interval=3]
    verify_nfo_from_platform() {
      local platform="$1" platform_url="$2" platform_key="$3" platform_item_id="$4"
      local d2_timeout="${5:-30}" d2_interval="${6:-3}"

      local nfo_path="${RT_ARTIST_PATH}/artist.nfo"
      local original_mtime
      original_mtime=$(get_mtime "$nfo_path")

      if [[ "$original_mtime" == "ERROR" ]]; then
        echo "[FAIL] $platform Direction 2 -- stat failed on NFO file"
        FAIL=$((FAIL + 1))
        FAILURES+=("$platform Direction 2 -- stat failed on $nfo_path")
        return
      fi

      if [[ "$original_mtime" == "MISSING" ]]; then
        echo "[INFO] $platform Direction 2 -- NFO file does not exist yet; will detect creation"
      fi

      # Fetch current biography from the platform.
      local original_item d2_error original_bio
      original_item=$(platform_get_item "$platform_url" "$platform_key" "$platform_item_id")
      d2_error=$(echo "$original_item" | jq -r '._error // empty' 2>/dev/null || true)
      if [[ -n "$d2_error" ]]; then
        echo "[FAIL] $platform Direction 2 -- platform_get_item failed: $d2_error"
        FAIL=$((FAIL + 1))
        FAILURES+=("$platform Direction 2 -- platform_get_item failed: $d2_error")
        return
      fi

      original_bio=$(echo "$original_item" | jq -r '.Overview // empty' 2>/dev/null || true)

      # Modify biography on the platform (append marker).
      local modified_bio="${original_bio} [roundtrip-test]"
      local update_code
      update_code=$(platform_modify_overview "$platform_url" "$platform_key" "$platform_item_id" "$modified_bio")
      if [[ "$update_code" != "204" && "$update_code" != "200" ]]; then
        echo "[FAIL] $platform Direction 2 -- failed to update biography (HTTP $update_code)"
        FAIL=$((FAIL + 1))
        FAILURES+=("$platform Direction 2 -- failed to update biography (HTTP $update_code)")
        return
      fi

      # Emby does not write NFO immediately after POST /Items/{id}. Trigger a
      # metadata refresh with ReplaceAllMetadata=true to force the NFO write.
      if [[ "$platform" == "Emby" ]]; then
        local refresh_code
        if ! refresh_code=$(curl -s -o /dev/null -w "%{http_code}" -X POST \
          "${platform_url}/Items/${platform_item_id}/Refresh?api_key=${platform_key}&ReplaceAllMetadata=true&ReplaceAllImages=false"); then
          echo "[WARN] $platform Direction 2 -- metadata refresh curl failed"
        elif [[ "$refresh_code" != "204" && "$refresh_code" != "200" ]]; then
          echo "[WARN] $platform Direction 2 -- metadata refresh returned HTTP $refresh_code"
        fi
      fi

      # Poll for NFO mtime change.
      if wait_for_nfo_change "$nfo_path" "$original_mtime" "$d2_timeout" "$d2_interval"; then
        echo "[PASS] $platform Direction 2 -- NFO write detected (mtime changed)"
        PASS=$((PASS + 1))

        # Verify Stillwater can parse the externally-modified NFO.
        local diff_resp has_diff bio_status
        diff_resp=$(curl -s "${AUTH[@]}" "$SW_BASE/api/v1/artists/$RT_ARTIST_ID/nfo/diff") || diff_resp=""
        has_diff=$(echo "$diff_resp" | jq -r '.has_diff' 2>/dev/null || echo "false")
        bio_status=$(echo "$diff_resp" | jq -r '.fields[] | select(.field=="Biography") | .status' 2>/dev/null || echo "")
        assert_json_value "NFO diff has_diff after $platform write" "true" "$has_diff"
        if [[ -n "$bio_status" ]]; then
          # Accept "changed" or "added" -- the status depends on whether the
          # NFO previously had a biography field.
          if [[ "$bio_status" == "changed" || "$bio_status" == "added" ]]; then
            echo "[PASS] NFO diff biography status after $platform write -- $bio_status"
            PASS=$((PASS + 1))
          else
            echo "[FAIL] NFO diff biography status after $platform write -- expected changed or added, got $bio_status"
            FAIL=$((FAIL + 1))
            FAILURES+=("NFO diff biography status after $platform write -- expected changed or added, got $bio_status")
          fi
        else
          echo "[SKIP] NFO diff biography status -- biography field not in diff output"
        fi

        # Check conflict endpoint detects the external modification.
        local conflict_resp has_conflict
        conflict_resp=$(curl -s "${AUTH[@]}" "$SW_BASE/api/v1/artists/$RT_ARTIST_ID/nfo/conflict") || conflict_resp=""
        has_conflict=$(echo "$conflict_resp" | jq -r '.has_conflict' 2>/dev/null || echo "false")
        assert_json_value "NFO conflict detected after $platform write" "true" "$has_conflict"
      else
        echo "[FAIL] $platform Direction 2 -- no NFO mtime change within ${d2_timeout}s"
        FAIL=$((FAIL + 1))
        FAILURES+=("$platform Direction 2 -- no NFO mtime change within ${d2_timeout}s")
      fi

      # Restore original biography on the platform.
      local restore_code
      restore_code=$(platform_modify_overview "$platform_url" "$platform_key" "$platform_item_id" "$original_bio")
      if [[ "$restore_code" == "204" || "$restore_code" == "200" ]]; then
        echo "[INFO] Restored original biography on $platform"
      else
        echo "[FAIL] $platform Direction 2 -- failed to restore biography (HTTP $restore_code); artist may have test marker"
        FAIL=$((FAIL + 1))
        FAILURES+=("$platform Direction 2 restore failed (HTTP $restore_code)")
      fi
    }

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
    elif [[ -z "$RT_ARTIST_PATH" || ! -d "$RT_ARTIST_PATH" ]]; then
      echo "[SKIP] Emby Direction 2 -- artist path not accessible locally"
    else
      # Emby refresh can take 45-60s; use 60s timeout with 5s interval.
      verify_nfo_from_platform "Emby" "$EMBY_URL" "$EMBY_API_KEY" "$RT_EMBY_PLATFORM_ID" 60 5
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
    elif [[ -z "$RT_ARTIST_PATH" || ! -d "$RT_ARTIST_PATH" ]]; then
      echo "[SKIP] Jellyfin Direction 2 -- artist path not accessible locally"
    else
      # Jellyfin writes NFO immediately; default 30s/3s is sufficient.
      verify_nfo_from_platform "Jellyfin" "$JELLYFIN_URL" "$JELLYFIN_API_KEY" "$RT_JELLYFIN_PLATFORM_ID"
    fi

    echo ""

    fi  # RT_SETUP_OK guard
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
