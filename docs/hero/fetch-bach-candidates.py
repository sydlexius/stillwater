#!/usr/bin/env python3
"""fetch-bach-candidates.py - source DISTINCT public-domain images of J.S. Bach
from Wikimedia Commons for the #1756 hero's image-candidate grid (screen 4).

WHY: the grid must SELL Stillwater's "search surfaces distinct candidates, you
pick the best" capability. The prior mock showed one Bach portrait cropped five
ways (reads as the same image x5). This gathers visually-distinct real PD images
of Bach (portraits, engravings, lithographs) so the grid looks like a genuine
multi-source search result.

LICENSE GUARD (strict, same policy as fetch-portraits.sh): each file's license is
read from its OWN Commons extmetadata at fetch time and must be free-to-
redistribute (public domain / CC0 / CC BY / CC BY-SA). Bach's 1750 death is NEVER
the test - a modern photo/scan/statue of a dead composer can carry its own
copyright. Anything failing is SKIPPED + logged, never written.

OUTPUT (all under docs/hero/bach-candidates/, git-ignored until the maintainer
picks the final 5): NN-slug.jpg thumbs, contact-sheet.png (labeled montage), and
CANDIDATES.md (index -> Commons title + license + source URL).

USAGE: python3 docs/hero/fetch-bach-candidates.py
Requires: python3, ImageMagick (magick). No third-party Python deps.
"""
import json
import os
import re
import subprocess
import sys
import time
import urllib.parse
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
OUT = os.path.join(HERE, "bach-candidates")
UA = "StillwaterDocsFixture/1.0 (PD Bach candidates for README hero #1756)"
API = "https://commons.wikimedia.org/w/api.php"
THUMB_W = 420
MAX_ACCEPT = 10  # give the maintainer a generous set to pick 5 from

PD_ALLOW = re.compile(r"public domain|^pd|cc0|cc-zero|no restrictions|cc by|cc-by", re.I)
LIC_DENY = re.compile(r"noncommercial|-nc|noderiv|-nd|fair use|all rights reserved", re.I)
# ImageMagick's default font lookup is broken on some hosts; point label
# rendering at a concrete system TTF. Probe a few common locations across
# macOS/Linux so the contact sheet regenerates on any host; fall back to None
# (let ImageMagick use its own default) if none are present.
def _find_font():
    for p in (
        "/System/Library/Fonts/Supplemental/Arial.ttf",
        "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
        "/usr/share/fonts/truetype/liberation/LiberationSans-Regular.ttf",
    ):
        if os.path.exists(p):
            return p
    return None


FONT = _find_font()
DL_DELAY = 0.8  # polite gap between thumbnail downloads (Commons 429s on bursts)

# File-namespace search terms + portrait categories. Union then dedupe; any
# empty/missing category is simply ignored.
SEARCHES = [
    "Johann Sebastian Bach portrait",
    "Johann Sebastian Bach engraving",
    "Johann Sebastian Bach painting",
]
CATEGORIES = [
    "Category:Portrait paintings of Johann Sebastian Bach",
    "Category:Paintings of Johann Sebastian Bach",
    "Category:Engravings of Johann Sebastian Bach",
    "Category:Portraits of Johann Sebastian Bach",
]


def api_get(params):
    params = {**params, "format": "json"}
    url = API + "?" + urllib.parse.urlencode(params)
    req = urllib.request.Request(url, headers={"User-Agent": UA})
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.load(r)


def search_titles():
    titles = []
    for term in SEARCHES:
        try:
            d = api_get({"action": "query", "list": "search", "srsearch": term,
                         "srnamespace": 6, "srlimit": 25})
            titles += [h["title"] for h in d.get("query", {}).get("search", [])]
        except Exception as e:
            print(f"    (search '{term}' failed: {e})", file=sys.stderr)
    for cat in CATEGORIES:
        try:
            d = api_get({"action": "query", "list": "categorymembers",
                         "cmtitle": cat, "cmtype": "file", "cmlimit": 30})
            titles += [m["title"] for m in d.get("query", {}).get("categorymembers", [])]
        except Exception as e:
            print(f"    (category '{cat}' failed: {e})", file=sys.stderr)
    # Dedupe preserving order.
    seen, uniq = set(), []
    for t in titles:
        if t not in seen:
            seen.add(t)
            uniq.append(t)
    return uniq


def strip_html(s):
    return re.sub(r"<[^>]+>", "", s or "").strip()


def imageinfo(titles):
    """Batch imageinfo (thumburl + license + artist + mime) for up to 50 titles."""
    out = {}
    for i in range(0, len(titles), 40):
        batch = titles[i:i + 40]
        d = api_get({"action": "query", "titles": "|".join(batch),
                     "prop": "imageinfo", "iiprop": "url|extmetadata|mime|size",
                     "iiurlwidth": THUMB_W})
        for page in d.get("query", {}).get("pages", {}).values():
            info = (page.get("imageinfo") or [None])[0]
            if not info:
                continue
            ext = info.get("extmetadata", {}) or {}
            out[page["title"]] = {
                "thumburl": info.get("thumburl"),
                "descurl": info.get("descriptionurl"),
                "mime": info.get("mime", ""),
                "license": strip_html(ext.get("LicenseShortName", {}).get("value", "")) or "unknown",
                "artist": strip_html(ext.get("Artist", {}).get("value", ""))[:80],
            }
    return out


def slug(title):
    s = re.sub(r"^File:", "", title)
    s = re.sub(r"\.[A-Za-z0-9]+$", "", s)
    return re.sub(r"[^A-Za-z0-9]+", "-", s).strip("-").lower()[:48]


def main():
    if not _which("magick"):
        print("FATAL: ImageMagick (magick) not found", file=sys.stderr)
        sys.exit(1)
    os.makedirs(OUT, exist_ok=True)
    titles = search_titles()
    print(f"candidate files found: {len(titles)}")
    info = imageinfo(titles)

    accepted, rows = [], []
    for title in titles:
        meta = info.get(title)
        if not meta or not meta["thumburl"]:
            continue
        if meta["mime"] not in ("image/jpeg", "image/png"):
            continue
        lic = meta["license"]
        if LIC_DENY.search(lic) or not PD_ALLOW.search(lic):
            print(f"    SKIP ({lic}): {title}", file=sys.stderr)
            continue
        idx = len(accepted) + 1
        fname = f"{idx:02d}-{slug(title)}.jpg"
        dest = os.path.join(OUT, fname)
        try:
            req = urllib.request.Request(meta["thumburl"], headers={"User-Agent": UA})
            with urllib.request.urlopen(req, timeout=30) as r, open(dest + ".src", "wb") as f:
                f.write(r.read())
            subprocess.run(["magick", dest + ".src", "-auto-orient", "-resize",
                            f"{THUMB_W}x", "-strip", "-quality", "85", dest], check=True)
            os.remove(dest + ".src")
        except Exception as e:
            print(f"    SKIP (download/convert failed): {title} -> {e}", file=sys.stderr)
            continue
        dims = subprocess.run(["magick", "identify", "-format", "%wx%h", dest],
                              capture_output=True, text=True).stdout.strip()
        print(f"    OK [{idx:02d}] {lic}: {fname} ({dims})")
        accepted.append((idx, fname, dims))
        rows.append((idx, fname, title, lic, meta["artist"] or "-", meta["descurl"]))
        if len(accepted) >= MAX_ACCEPT:
            break
        time.sleep(DL_DELAY)  # polite gap so Commons does not 429 on a burst

    if not accepted:
        print("NO candidates passed the license/fetch check.", file=sys.stderr)
        sys.exit(1)

    # Labeled contact sheet.
    labeled = []
    for idx, fname, _ in accepted:
        src = os.path.join(OUT, fname)
        lab = os.path.join(OUT, f".lab-{idx:02d}.png")
        font_args = ["-font", FONT] if FONT else []
        subprocess.run(["magick", src, "-resize", "260x260^", "-gravity", "center",
                        "-extent", "260x260", "-background", "#0b0f17",
                        "-gravity", "South", "-splice", "0x28",
                        *font_args, "-pointsize", "20", "-fill", "white", "-annotate", "+0+4",
                        f"#{idx}", lab], check=True)
        labeled.append(lab)
    sheet = os.path.join(OUT, "contact-sheet.png")
    # +label suppresses montage's default (broken-font) filename labels; ours are
    # already baked into each thumb above.
    subprocess.run(["magick", "montage", *labeled, "-tile", "5x", "-geometry",
                    "+8+8", "+label", "-background", "#05070b", sheet], check=True)
    for lab in labeled:
        os.remove(lab)

    md = os.path.join(OUT, "CANDIDATES.md")
    with open(md, "w") as f:
        f.write("# Distinct PD Bach candidates (#1756 screen 4)\n\n")
        f.write("All license-verified free-to-redistribute from each file's own "
                "Commons metadata. Pick 5 for the candidate grid.\n\n")
        f.write("| # | File | License | Author | Commons source |\n")
        f.write("|---|---|---|---|---|\n")
        for idx, fname, title, lic, artist, descurl in rows:
            f.write(f"| {idx} | `{fname}` | {lic} | {artist} | {descurl} |\n")
    print(f"\nAccepted {len(accepted)} candidates.")
    print(f"Contact sheet: {sheet}")
    print(f"Manifest: {md}")


def _which(cmd):
    for p in os.environ.get("PATH", "").split(os.pathsep):
        if os.path.exists(os.path.join(p, cmd)):
            return True
    return False


if __name__ == "__main__":
    main()
