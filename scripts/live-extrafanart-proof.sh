#!/usr/bin/env bash
# scripts/live-extrafanart-proof.sh -- the #3179 live-Emby proof, both sides. Local
# machine only; the maintainer runs it. Credentials and the sandbox directory come
# ONLY from the environment (never arguments):
#   SW_LIVE_EMBY_URL SW_LIVE_EMBY_API_KEY SW_LIVE_EMBY_USER_ID (the item is discovered by name)
#   SW_LIVE_EMBY_ARTIST_DIR  (host folder of that item; must hold only the sentinel)
# GREEN: after the migration, three pushes lose no file. RED: the same sequence
# WITHOUT the migration loses files on this tree (the control). A skip, a build
# failure, a setup failure or a missing marker never counts as proven.
# Exit 0 = both PROVEN; 2 = bad invocation; 3 = NOT PROVEN.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && git rev-parse --show-toplevel)"
for v in SW_LIVE_EMBY_URL SW_LIVE_EMBY_API_KEY SW_LIVE_EMBY_USER_ID SW_LIVE_EMBY_ARTIST_DIR; do
	if [ -z "${!v:-}" ]; then
		echo "NOT PROVEN: $v is not set; export it in the environment (not as an argument)" >&2
		exit 3
	fi
done
if [ -z "${SW_RUN_DIR:-}" ]; then
	# shellcheck source=lib/run-paths.sh
	. "$ROOT/scripts/lib/run-paths.sh"
fi
OUT="${SW_RUN_DIR:?run-paths.sh did not set SW_RUN_DIR}/live-proof"
mkdir -p "$OUT"
LOG="$OUT/live-3179-$(git -C "$ROOT" rev-parse --short HEAD).log"
(cd "$ROOT" && SW_PROOF_DIAG_DIR="$OUT" go test -tags integration -count=1 -p 1 -v -run 'TestLiveEmby_ExtrafanartMigration_' ./internal/publish/) >"$LOG" 2>&1 || true

BAD='--- SKIP|build failed|^panic:|test timed out'
GREEN=NOT; RED=NOT
if grep -q -- '--- PASS: TestLiveEmby_ExtrafanartMigration_ThreePushesLoseNothing ' "$LOG" &&
	! grep -qE -- "--- FAIL: TestLiveEmby_ExtrafanartMigration_ThreePushes|LIVE-PROOF-DEFECT|$BAD" "$LOG"; then GREEN=""; fi
if grep -q -- '--- PASS: TestLiveEmby_ExtrafanartMigration_ControlUnmigratedLoses ' "$LOG" &&
	grep -q 'LIVE-PROOF-LOSS' "$LOG" && ! grep -qE -- '--- SKIP|build failed|^panic:|test timed out' "$LOG"; then RED=""; fi
echo "GREEN (migrate, then 3 pushes, no file missing): ${GREEN:+NOT }PROVEN"
echo "RED (no migration, same 3 pushes, loss observed): ${RED:+NOT }PROVEN"
grep -E 'LIVE-PROOF-(COUNT|LOSS|DEFECT|SETUP)' "$LOG" || true
echo "Produced: $LOG"
[ -z "$GREEN" ] && [ -z "$RED" ] && exit 0
exit 3
