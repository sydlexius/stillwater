#!/usr/bin/env bash
# scripts/live-emby-proof.sh -- prove a live-Emby integration test fails WITHOUT
# a fix and passes WITH it (#3200). Local machine only; the maintainer runs it.
#
# Usage: live-emby-proof.sh --test <regex> --without <ref> --files <f1,f2> --expect <TestName> [--expect ...]
#   --expect names every test that must PASS with the fix (declared by the caller,
#   never derived from the files, so a deleted or renamed test cannot hide).
#   Logs and summary are labeled by the sanitized --test value, so runs do not overwrite each other.
#   --files are repo-relative integration test files copied into a private
#   `git archive <ref>` extract; the same run then repeats in the current tree.
# Credentials come ONLY from the environment (never arguments):
#   SW_LIVE_EMBY_URL SW_LIVE_EMBY_API_KEY SW_LIVE_EMBY_USER_ID SW_LIVE_EMBY_ITEM_ID
# Exit 0 = both directions PROVEN; 2 = bad invocation; 3 = NOT PROVEN (a skip, a
# build failure, an unreachable server, or a missing marker never counts).
set -euo pipefail

TEST="" REF="" FILES="" EXPECTED=()
while [ $# -gt 0 ]; do
	case "$1" in
	--test | --without | --files | --expect)
		if [ $# -lt 2 ]; then echo "live-emby-proof: $1 needs a value" >&2; exit 2; fi
		case "$1" in
		--test) TEST="$2" ;;
		--without) REF="$2" ;;
		--files) FILES="$2" ;;
		--expect) EXPECTED+=("$2") ;;
		esac
		shift 2 ;;
	*) echo "live-emby-proof: unknown argument: $1" >&2; exit 2 ;;
	esac
done
if [ -z "$TEST" ] || [ -z "$REF" ] || [ -z "$FILES" ] || [ ${#EXPECTED[@]} -eq 0 ]; then
	echo "usage: live-emby-proof.sh --test <regex> --without <ref> --files <f1,f2> --expect <TestName> [--expect ...]" >&2
	exit 2
fi
case "$REF" in
-*) echo "live-emby-proof: --without must be a ref, not an option: $REF" >&2; exit 2 ;;
esac
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && git rev-parse --show-toplevel)"
IFS=',' read -r -a FILE_LIST <<<"$FILES"
for f in "${FILE_LIST[@]}"; do
	if [ ! -f "$ROOT/$f" ]; then
		echo "live-emby-proof: --files entry is not a regular file: $f" >&2
		exit 2
	fi
done
for name in "${EXPECTED[@]}"; do
	found=0
	for f in "${FILE_LIST[@]}"; do
		if grep -qE -- "^func $name\\(" "$ROOT/$f"; then found=1; fi
	done
	if [ "$found" -eq 0 ]; then
		echo "live-emby-proof: --expect $name is not defined in --files (renamed or deleted?)" >&2
		exit 2
	fi
done
for v in SW_LIVE_EMBY_URL SW_LIVE_EMBY_API_KEY SW_LIVE_EMBY_USER_ID SW_LIVE_EMBY_ITEM_ID; do
	if [ -z "${!v:-}" ]; then
		echo "NOT PROVEN: $v is not set; export SW_LIVE_EMBY_* in the environment (not as arguments)" >&2
		exit 3
	fi
done

if ! git -C "$ROOT" rev-parse --verify --quiet "$REF^{commit}" >/dev/null; then
	echo "live-emby-proof: --without is not a commit: $REF" >&2
	exit 2
fi
# The with-fix run tests the working tree, so it must equal HEAD (simpler than a second extract).
if [ -n "$(git -C "$ROOT" status --porcelain)" ]; then
	echo "NOT PROVEN: the working tree is dirty; commit first so the with-fix run is exactly HEAD" >&2
	exit 3
fi
LABEL="$(printf %s "$TEST" | tr -c 'A-Za-z0-9' _)"
if [ -z "${SW_RUN_DIR:-}" ]; then
	# shellcheck source=lib/run-paths.sh
	. "$ROOT/scripts/lib/run-paths.sh"
fi
OUT="${SW_RUN_DIR:?run-paths.sh did not set SW_RUN_DIR}/live-proof"
mkdir -p "$OUT"

VERSION="$(curl -fsS --max-time 10 "$SW_LIVE_EMBY_URL/System/Info/Public" 2>/dev/null |
	sed -n 's/.*"Version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' || true)"
if [ -z "$VERSION" ]; then
	echo "NOT PROVEN: $SW_LIVE_EMBY_URL/System/Info/Public did not answer with a Version" >&2
	exit 3
fi

BASE_SHA="$(git -C "$ROOT" rev-parse --short "$REF")"
HEAD_SHA="$(git -C "$ROOT" rev-parse --short HEAD)"
EXTRACT="$(mktemp -d)"
trap 'rm -rf "$EXTRACT"' EXIT
git -C "$ROOT" archive "$REF" | tar -x -C "$EXTRACT"
for f in "${FILE_LIST[@]}"; do
	cp "$ROOT/$f" "$EXTRACT/$f"
done

run_in() { # run_in <dir> <label> -> log path on stdout; the test's own exit is ignored
	local log="$OUT/$2.log"
	(cd "$1" && go test -tags integration -count=1 -p 1 -v -run "$TEST" ./internal/publish/) >"$log" 2>&1 || true
	echo "$log"
}
BEFORE_LOG="$(run_in "$EXTRACT" "$LABEL-$BASE_SHA-without-fix")"
AFTER_LOG="$(run_in "$ROOT" "$LABEL-$HEAD_SHA-with-fix")"

VERDICT=0
if grep -q -- '--- FAIL' "$BEFORE_LOG" && grep -q 'LIVE-PROOF-DEFECT:' "$BEFORE_LOG" &&
	! grep -qE -- '--- SKIP|build failed|^panic:|test timed out' "$BEFORE_LOG"; then
	BEFORE_V="PROVEN (first defect: $(grep -m1 -o 'LIVE-PROOF-DEFECT:.*' "$BEFORE_LOG"))"
else
	BEFORE_V="NOT PROVEN (need --- FAIL and LIVE-PROOF-DEFECT)"
	VERDICT=3
fi
MISSING=""
for name in "${EXPECTED[@]}"; do
	grep -q -- "--- PASS: $name " "$AFTER_LOG" || MISSING="$MISSING $name"
done
if [ -z "$MISSING" ] && grep -q '^ok' "$AFTER_LOG" && ! grep -qE -- '--- SKIP|build failed|--- FAIL|LIVE-PROOF-DEFECT:|^FAIL|^panic:' "$AFTER_LOG"; then
	AFTER_V="PROVEN (passed, no skip)"
else
	AFTER_V="NOT PROVEN (need --- PASS for every expected test with no skip, build failure, or defect; missing PASS:${MISSING:- none})"
	VERDICT=3
fi

{
	echo "# Live Emby proof: $TEST"
	echo
	echo "- Emby version: $VERSION ($SW_LIVE_EMBY_URL, item $SW_LIVE_EMBY_ITEM_ID)"
	echo "- Without fix: $BASE_SHA ($REF): $BEFORE_V"
	echo "- With fix: $HEAD_SHA: $AFTER_V"
	echo "- Logs: $BEFORE_LOG , $AFTER_LOG"
	for l in "$BEFORE_LOG" "$AFTER_LOG"; do
		echo
		echo "## Counts: $(basename "$l")"
		grep -o 'LIVE-PROOF-COUNT.*' "$l" | sed 's/^/- /' || true
	done
} >"$OUT/summary-$LABEL.md"
cat "$OUT/summary-$LABEL.md"
echo "Produced: $OUT/summary-$LABEL.md $BEFORE_LOG $AFTER_LOG"
exit "$VERDICT"
