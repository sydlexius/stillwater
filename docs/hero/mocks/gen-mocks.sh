#!/usr/bin/env bash
#
# gen-mocks.sh - regenerate the deterministic route-mock fixtures nav-clips.mjs serves
# during the #1756 hero capture.
#
# WHY MOCKS
#   The live image + metadata endpoints hit real external providers (Fanart.tv,
#   Spotify, Deezer, Discogs, TheAudioDB) using the operator's real API keys and
#   return COPYRIGHTED images. The README hero must show ONLY public-domain
#   content and must be deterministic/regenerable. nav-clips.mjs therefore intercepts
#   these endpoints (Playwright page.route) and replies with the fixtures this
#   script produces - no network, no keys, only PD content.
#
# OUTPUTS (committed - small, text/data-URI, fully PD)
#   images-search-thumb.html   candidate-grid fragment for GET .../images/search
#                              and .../images/websearch (real templ card markup;
#                              candidates are data-URI variants of the local PD
#                              Bach portrait, so the grid loads offline).
#   refresh-metadata.html      the "Metadata Refreshed" fragment for
#                              POST .../refresh (captured once from the live
#                              server against the PD fixture; PD-safe).
#
# The card markup mirrors web/templates/image_search.templ (components.ImageCard)
# as rendered by the running server; if that template changes materially,
# re-capture from a live PD fixture and re-run.
#
# USAGE
#   docs/hero/mocks/gen-mocks.sh          # uses defaults below
#   PORT=1991 HERO_ARTIST=<uuid> FIXTURE_LIB=<dir> docs/hero/mocks/gen-mocks.sh
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FIXTURE_LIB="${FIXTURE_LIB:-/tmp/hero-1756/fixture/library}"
PORT="${PORT:-1991}"
# Bach is the "hero artist" driven through the artist-detail take.
HERO_ARTIST="${HERO_ARTIST:-c14e15f5-4ff4-4415-b2ea-75de8cb4be57}"
# Committed public-domain candidate portraits for the Manage-artwork modal grid.
CAND_DIR="${CAND_DIR:-$HERE/candidates}"

command -v magick >/dev/null || { echo "FATAL: ImageMagick (magick) not found" >&2; exit 1; }
[ -d "$CAND_DIR" ] || { echo "FATAL: candidate dir not found: $CAND_DIR" >&2; exit 1; }

TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT

# --- Candidate grid: committed PUBLIC-DOMAIN Bach portraits, each badged as if
# returned by a DIFFERENT provider, with varied likes + resolutions, so the grid
# demonstrates how Stillwater PULLS candidates from multiple sources and SORTS
# them. Distinctness is not required (real searches surface the same portrait from
# several providers - c01/c02 are the same Haussmann from two sources). Every
# image is pure PD (no attribution burden). Provenance: CANDIDATE-SOURCES.md.
#
# entry: "file|Provider label|provider_id|reported-WxH|likes"  (likes/dims vary so
# the Sort-by control is meaningful; reported dims mimic provider source sizes).
CANDS=(
  "c01.jpg|Fanart.tv|fanarttv|1000x1314|142"
  "c02.jpg|TheAudioDB|theaudiodb|1000x1298|118"
  "c04.jpg|Deezer|deezer|800x975|76"
  "c08.jpg|Wikipedia|wikipedia|564x767|54"
  "c09.jpg|Discogs|discogs|500x645|21"
)

# data-URI helper: resize to a lean thumbnail so the committed mock stays small.
datauri() { magick "$1" -resize 340x -strip -quality 82 "$TMP/du.jpg"; printf 'data:image/jpeg;base64,%s' "$(base64 < "$TMP/du.jpg" | tr -d '\n')"; }

# One faithful ImageCard, parameterized. Mirrors components.ImageCard output.
card() { # <data-uri> <provider-label> <provider-id> <wxh> <likes>
  local uri="$1" src="$2" pid="$3" wh="$4" likes="$5"
  cat <<HTML
<div class="relative rounded-lg border border-gray-200 dark:border-gray-700 overflow-hidden group hover:border-blue-400 dark:hover:border-blue-500 transition-colors" data-img-url="$uri" data-img-type="thumb" data-img-source="$pid" data-img-width="${wh%x*}" data-img-height="${wh#*x}" data-img-likes="$likes" data-img-area="0"><div class="aspect-square flex items-center justify-center overflow-hidden bg-gray-100 dark:bg-gray-900"><img src="$uri" alt="thumb from $src" class="max-w-full max-h-full object-contain" loading="lazy"></div><div class="p-2 space-y-1"><div class="flex items-center gap-1 flex-wrap"><span class="inline-flex items-center rounded-full px-2 py-0.5 text-xs font-medium bg-gray-100 dark:bg-gray-700 text-gray-800 dark:text-gray-200">$src</span> <span class="inline-flex items-center rounded-full bg-gray-100 dark:bg-gray-700 px-2 py-0.5 text-xs font-medium text-gray-600 dark:text-gray-400">Thumbnail</span></div><div class="flex items-center gap-2 text-xs text-gray-500 dark:text-gray-400"><span class="font-mono">$wh</span> <span aria-hidden="true">&middot;</span> <span>$likes likes</span></div><button type="button" class="image-card-save w-full mt-1 inline-flex items-center justify-center gap-1.5 text-sm px-2 py-2 rounded bg-blue-600 text-white hover:bg-blue-700 transition-colors" hx-post="/api/v1/artists/$HERO_ARTIST/images/fetch" hx-swap="none" hx-confirm="Save this image?"><svg class="h-4 w-4" fill="none" viewBox="0 0 24 24" stroke-width="1.5" stroke="currentColor"><path stroke-linecap="round" stroke-linejoin="round" d="M3 16.5v2.25A2.25 2.25 0 0 0 5.25 21h13.5A2.25 2.25 0 0 0 21 18.75V16.5M16.5 12 12 16.5m0 0L7.5 12m4.5 4.5V3"></path></svg>Save</button></div></div>
HTML
}

OUT="$HERE/images-search-thumb.html"
{
  echo '<div class="flex items-center justify-between mb-3"><span class="text-xs text-gray-500 dark:text-gray-400">Sort by</span><select aria-label="Sort by" class="text-xs rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-700 text-gray-700 dark:text-gray-300 px-2 py-1"><option value="likes" selected>Likes</option><option value="resolution">Resolution</option></select></div>'
  echo '<div class="image-sort-grid grid grid-cols-2 sm:grid-cols-3 gap-3">'
  for entry in "${CANDS[@]}"; do
    IFS='|' read -r file label pid wh likes <<< "$entry"
    img="$CAND_DIR/$file"
    [ -f "$img" ] || { echo "FATAL: candidate image missing: $img" >&2; exit 1; }
    card "$(datauri "$img")" "$label" "$pid" "$wh" "$likes"
  done
  echo '</div>'
} > "$OUT"
echo "wrote $OUT ($(wc -c <"$OUT") bytes, ${#CANDS[@]} PD data-URI candidates across ${#CANDS[@]} providers)"

# --- Capture the live /refresh fragment if the server is reachable; else keep
#     the committed copy. The fragment is PD (fixture data) and deterministic. ---
REFRESH_OUT="$HERE/refresh-metadata.html"
if curl -sf "http://127.0.0.1:$PORT/api/v1/health" >/dev/null 2>&1; then
  CJ="$TMP/cj"; curl -s -c "$CJ" "http://127.0.0.1:$PORT/api/v1/health" >/dev/null
  CSRF="$(grep csrf_token "$CJ" | awk '{print $7}')"
  curl -s -b "$CJ" -c "$CJ" -X POST "http://127.0.0.1:$PORT/api/v1/auth/login" \
    -H "Content-Type: application/json" -H "X-CSRF-Token: $CSRF" \
    -d '{"username":"herofixture-admin","password":"herofixture-pw"}' >/dev/null
  if curl -s -b "$CJ" -X POST "http://127.0.0.1:$PORT/api/v1/artists/$HERO_ARTIST/refresh" \
      -H "X-CSRF-Token: $CSRF" -H "HX-Request: true" -o "$TMP/refresh.html" \
      && [ -s "$TMP/refresh.html" ] && grep -q "Metadata Refreshed" "$TMP/refresh.html"; then
    cp "$TMP/refresh.html" "$REFRESH_OUT"
    echo "wrote $REFRESH_OUT (captured live, $(wc -c <"$REFRESH_OUT") bytes)"
  else
    echo "WARN: live /refresh capture failed; keeping existing $REFRESH_OUT" >&2
  fi
else
  echo "NOTE: server :$PORT not reachable; keeping existing $REFRESH_OUT" >&2
fi

echo "Mock generation complete."
