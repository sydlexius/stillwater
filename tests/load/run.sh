#!/usr/bin/env bash
#
# Stillwater load testing script using vegeta.
#
# Usage:
#   bash tests/load/run.sh [smoke|read|search|all]
#
# Environment variables:
#   SW_BASE      - Base URL (default: http://localhost:1973)
#   SW_API_KEY   - API key for authentication (required)
#   SW_RATE      - Requests per second (default: 20 for smoke, 50 for others)
#   SW_DURATION  - Test duration (default: 10s for smoke, 30s for others)
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RESULTS_DIR="${SCRIPT_DIR}/results"
mkdir -p "${RESULTS_DIR}"

: "${SW_BASE:=http://localhost:1973}"
: "${SW_API_KEY:?SW_API_KEY must be set}"

MODE="${1:-smoke}"

# Validate mode early before any other setup
case "${MODE}" in
    smoke|read|search|all) ;;
    *)
        echo "Usage: $0 [smoke|read|search|all]"
        exit 1
        ;;
esac

LOAD_TEST_FAILED=0

case "${MODE}" in
    smoke)
        RATE="${SW_RATE:-20}"
        DURATION="${SW_DURATION:-10s}"
        ;;
    *)
        RATE="${SW_RATE:-50}"
        DURATION="${SW_DURATION:-30s}"
        ;;
esac

TIMESTAMP="$(date +%Y%m%d_%H%M%S)"

# Check that vegeta is installed
if ! command -v vegeta &>/dev/null; then
    echo "ERROR: vegeta not found. Install with: go install github.com/tsenart/vegeta@latest"
    exit 1
fi

# Check that the server is running
if ! curl -sf "${SW_BASE}/api/v1/health" >/dev/null 2>&1; then
    echo "ERROR: Stillwater is not responding at ${SW_BASE}/api/v1/health"
    exit 1
fi

# Verify authentication works
auth_status=$(curl -s -o /dev/null -w "%{http_code}" \
    -H "Authorization: Bearer ${SW_API_KEY}" \
    "${SW_BASE}/api/v1/artists?page=1&page_size=1")
if [ "${auth_status}" != "200" ]; then
    echo "ERROR: Authentication failed (HTTP ${auth_status}). Check SW_API_KEY."
    exit 1
fi

echo "=== Stillwater Load Test ==="
echo "Mode:     ${MODE}"
echo "Base URL: ${SW_BASE}"
echo "Rate:     ${RATE}/s"
echo "Duration: ${DURATION}"
echo ""

run_attack() {
    local name="$1"
    local targets_file="$2"
    local output="${RESULTS_DIR}/${TIMESTAMP}_${name}"

    echo "--- Running: ${name} ---"

    vegeta attack \
        -rate="${RATE}/s" \
        -duration="${DURATION}" \
        -targets="${targets_file}" \
        -header="Authorization: Bearer ${SW_API_KEY}" \
        -timeout=10s \
        | tee "${output}.bin" \
        | vegeta report \
        | tee "${output}.txt"

    # Check success rate (requires python3 for JSON parsing)
    if command -v python3 &>/dev/null; then
        success_rate=$(vegeta report -type=json < "${output}.bin" | python3 -c "import sys,json; print(json.load(sys.stdin).get('success', 0))")
        below_threshold=$(python3 -c "print(1 if float('${success_rate}') < 0.95 else 0)" 2>/dev/null || echo 1)
        if [ "${below_threshold}" -eq 1 ]; then
            echo "WARNING: Success rate ${success_rate} is below 95% threshold"
            LOAD_TEST_FAILED=1
        fi
    fi

    echo ""
    echo "Results saved to ${output}.txt"
    echo ""
}

# Generate target files based on mode.
# Vegeta target format: METHOD URL

generate_read_targets() {
    local file="${RESULTS_DIR}/targets_read.txt"
    cat > "${file}" <<EOF
GET ${SW_BASE}/api/v1/artists
GET ${SW_BASE}/api/v1/artists?page=1&page_size=50
GET ${SW_BASE}/api/v1/artists?page=1&page_size=50&sort=name&order=asc
GET ${SW_BASE}/api/v1/artists?page=1&page_size=50&sort=health_score&order=desc
GET ${SW_BASE}/api/v1/artists?page=1&page_size=50&sort=updated_at&order=desc
GET ${SW_BASE}/api/v1/artists?filter=not_excluded
GET ${SW_BASE}/api/v1/health
GET ${SW_BASE}/api/v1/rules
GET ${SW_BASE}/api/v1/notifications/counts
GET ${SW_BASE}/api/v1/reports/health
GET ${SW_BASE}/api/v1/libraries
GET ${SW_BASE}/api/v1/connections
GET ${SW_BASE}/api/v1/providers
EOF
    echo "${file}"
}

generate_search_targets() {
    local file="${RESULTS_DIR}/targets_search.txt"
    cat > "${file}" <<EOF
GET ${SW_BASE}/api/v1/artists?search=the
GET ${SW_BASE}/api/v1/artists?search=a
GET ${SW_BASE}/api/v1/artists?search=nirvana
GET ${SW_BASE}/api/v1/artists?search=beatles
GET ${SW_BASE}/api/v1/artists?search=radio
GET ${SW_BASE}/api/v1/artists?search=pink
GET ${SW_BASE}/api/v1/artists?search=led
GET ${SW_BASE}/api/v1/artists?search=queen
GET ${SW_BASE}/api/v1/artists?filter=missing_nfo
GET ${SW_BASE}/api/v1/artists?filter=missing_thumb
GET ${SW_BASE}/api/v1/artists?filter=missing_mbid
EOF
    echo "${file}"
}

generate_smoke_targets() {
    local file="${RESULTS_DIR}/targets_smoke.txt"
    cat > "${file}" <<EOF
GET ${SW_BASE}/api/v1/health
GET ${SW_BASE}/api/v1/artists?page=1&page_size=10
GET ${SW_BASE}/api/v1/rules
GET ${SW_BASE}/api/v1/notifications/counts
GET ${SW_BASE}/api/v1/libraries
EOF
    echo "${file}"
}

case "${MODE}" in
    smoke)
        targets=$(generate_smoke_targets)
        run_attack "smoke" "${targets}"
        ;;
    read)
        targets=$(generate_read_targets)
        run_attack "read" "${targets}"
        ;;
    search)
        targets=$(generate_search_targets)
        run_attack "search" "${targets}"
        ;;
    all)
        targets=$(generate_read_targets)
        run_attack "read" "${targets}"

        targets=$(generate_search_targets)
        run_attack "search" "${targets}"
        ;;
esac

echo "=== Load Test Complete ==="

if [ "${LOAD_TEST_FAILED}" -eq 1 ]; then
    echo "Some load tests had low success rates. Check results above."
    exit 1
fi
