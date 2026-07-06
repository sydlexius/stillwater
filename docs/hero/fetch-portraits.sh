#!/usr/bin/env bash
#
# fetch-portraits.sh - (re)generate the committed public-domain composer
# portraits for the #1756 hero fixture, and their provenance manifest.
#
# This is the SOURCE-OF-RECORD for docs/hero/portraits/*/folder.jpg. Those JPEGs
# are committed so the fixture seeds OFFLINE and deterministically; run this only
# to refresh them (e.g. a better PD scan appears) or to re-verify licensing.
#
# LICENSE GUARD (strict): each image's license is read from the file's own
# Wikimedia Commons metadata at fetch time and must be free-to-redistribute
# (public-domain / CC0 / CC BY / CC BY-SA). Modern photos, museum scans, or
# statue images of a long-dead composer can carry their own copyright, so the
# death date is NEVER the test. Anything failing the check is SKIPPED + logged,
# never written. Accepted images + license + Commons source go to PD-SOURCES.md.
#
# USAGE
#   docs/hero/fetch-portraits.sh            # -> docs/hero/portraits/ + PD-SOURCES.md
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PORTRAITS="$HERE/portraits"
MANIFEST="$HERE/PD-SOURCES.md"
UA="StillwaterDocsFixture/1.0 (PD classical portraits for README hero #1756)"
WIDTH=600

# Auto-accept free-to-redistribute; hard-reject NC/ND/fair-use/all-rights.
PD_ALLOW='public domain|^pd|cc0|cc-zero|no restrictions|cc by|cc-by'
LIC_DENY='noncommercial|-nc|noderiv|-nd|fair use|all rights reserved'

# "Composer dir name|Wikipedia REST title". 12 public-domain composers.
ROSTER=(
  "Johann Sebastian Bach|Johann_Sebastian_Bach"
  "Wolfgang Amadeus Mozart|Wolfgang_Amadeus_Mozart"
  "Ludwig van Beethoven|Ludwig_van_Beethoven"
  "Antonio Vivaldi|Antonio_Vivaldi"
  "George Frideric Handel|George_Frideric_Handel"
  "Johannes Brahms|Johannes_Brahms"
  "Claude Debussy|Claude_Debussy"
  "Frederic Chopin|Fr%C3%A9d%C3%A9ric_Chopin"
  "Pyotr Ilyich Tchaikovsky|Pyotr_Ilyich_Tchaikovsky"
  "Franz Schubert|Franz_Schubert"
  "Joseph Haydn|Joseph_Haydn"
  "Franz Liszt|Franz_Liszt"
)

command -v magick >/dev/null || { echo "FATAL: ImageMagick (magick) not found" >&2; exit 1; }
mkdir -p "$PORTRAITS"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT

cat > "$MANIFEST" <<EOF
# Hero fixture - public-domain image provenance (#1756)

Every portrait in \`docs/hero/portraits/\` is sourced from Wikimedia Commons and
verified free-to-redistribute from the file's own Commons license metadata at
fetch time - NOT inferred from the composer's death date (a modern photo, museum
scan, or statue image of a dead composer can carry its own copyright). Regenerate
with \`docs/hero/fetch-portraits.sh\`, which re-checks each license and rejects
anything outside the free-license allowlist.

Auto-accept (LicenseShortName, case-insensitive substring): \`public domain\`,
\`PD-*\`, \`CC0\`, \`CC BY\`, \`CC BY-SA\`, \`no restrictions\`. Hard-rejected even if a
permissive token also appears: NonCommercial, NoDerivs, fair-use, all-rights-
reserved. Attribution is captured below for CC-BY(-SA) images.

| Composer | Image (slot) | License | Commons source |
|---|---|---|---|
EOF

resolve_lead_image() { # <wiki-title> -> "commons_file<TAB>original_url"
  curl -sSL -A "$UA" "https://en.wikipedia.org/api/rest_v1/page/summary/$1" | python3 -c '
import sys, json, urllib.parse, os, re
d = json.load(sys.stdin)
src = d.get("originalimage", {}).get("source", "")
name = urllib.parse.unquote(os.path.basename(urllib.parse.urlparse(src).path))
name = re.sub(r"^\d+px-", "", name)                 # strip a rendered-thumb prefix
if "/thumb/" in src:                                # and resolve thumb -> original
    src = re.sub(r"/thumb/(.+)/[^/]+$", r"/\1", src)
print(f"{name}\t{src}")
'
}

license_of() { # <commons-file> -> "LicenseShortName<TAB>Artist"
  curl -sSL -A "$UA" -G "https://commons.wikimedia.org/w/api.php" \
    --data-urlencode "action=query" --data-urlencode "format=json" \
    --data-urlencode "prop=imageinfo" --data-urlencode "iiprop=extmetadata" \
    --data-urlencode "titles=File:$1" | python3 -c '
import sys, json, re
d = json.load(sys.stdin)
page = next(iter(d.get("query", {}).get("pages", {}).values()), {})
ext = (page.get("imageinfo", [{}])[0] or {}).get("extmetadata", {}) if page.get("imageinfo") else {}
clean = lambda s: re.sub(r"<[^>]+>", "", (ext.get(s, {}) or {}).get("value", "") or "").strip()
print("\t".join([clean("LicenseShortName"), clean("Artist")[:80]]))
'
}

accepted=0; rejected=0
for entry in "${ROSTER[@]}"; do
  name="${entry%%|*}"; title="${entry##*|}"; dir="$PORTRAITS/$name"
  echo "==> $name"
  read -r file url < <(resolve_lead_image "$title" || true)
  if [ -z "${url:-}" ]; then
    echo "    SKIP: no lead image" >&2
    echo "| $name | (none) | UNRESOLVED | lead image not found |" >> "$MANIFEST"; rejected=$((rejected+1)); continue
  fi
  IFS=$'\t' read -r license artist < <(license_of "$file" || true)
  license="${license:-unknown}"; attribution="${artist:- - }"
  if echo "$license" | grep -qiE "$LIC_DENY" || ! echo "$license" | grep -qiE "$PD_ALLOW"; then
    echo "    SKIP: license '$license' not free-to-redistribute ($file)" >&2
    echo "| $name | folder.jpg | REJECTED ($license) | https://commons.wikimedia.org/wiki/File:$file |" >> "$MANIFEST"; rejected=$((rejected+1)); continue
  fi
  curl -sSL -A "$UA" -o "$TMP/src" "$url" || { echo "    SKIP: download failed" >&2; rejected=$((rejected+1)); continue; }
  mkdir -p "$dir"
  magick "$TMP/src" -auto-orient -resize "${WIDTH}x" -strip -quality 88 "$dir/folder.jpg"
  echo "    OK: $license -> $dir/folder.jpg ($(magick identify -format '%wx%h' "$dir/folder.jpg"))"
  echo "| $name | folder.jpg (thumb) | $license (by $attribution) | https://commons.wikimedia.org/wiki/File:$file |" >> "$MANIFEST"
  accepted=$((accepted+1))
done

echo
echo "Accepted: $accepted  Rejected/skipped: $rejected"
echo "Portraits: $PORTRAITS   Manifest: $MANIFEST"
[ "$rejected" -eq 0 ] || { echo "WARN: $rejected portrait(s) failed the license/fetch check - see log above." >&2; exit 1; }
