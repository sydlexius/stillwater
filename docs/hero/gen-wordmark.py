#!/usr/bin/env python3
"""Generate docs/img/stillwater-wordmark.svg: the word "Stillwater" in Oleo
Script, converted to OUTLINED PATHS (no font dependency at render time - the
same fontTools SVGPathPen glyph pipeline #2251 used for the favicon 'S'). Fill
#2563eb, transparent background, tight viewBox. Glyphs are laid out at their
hmtx advance widths (Oleo's connecting script is designed to join at the
baseline via advances), so the outlines match the outro wordmark exactly (same
vendored TTF).

Usage:
  python3 docs/hero/gen-wordmark.py                       # writes docs/img/stillwater-wordmark.svg
  python3 docs/hero/gen-wordmark.py <font.ttf> <out.svg>  # override paths

Requires fontTools (pip install fonttools).
"""
import os
import sys
from fontTools.ttLib import TTFont
from fontTools.pens.svgPathPen import SVGPathPen
from fontTools.pens.boundsPen import BoundsPen
from fontTools.pens.transformPen import TransformPen

REPO = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
FONT = sys.argv[1] if len(sys.argv) > 1 else os.path.join(REPO, "docs/hero/captions/public/fonts/OleoScript-Regular.ttf")
OUT = sys.argv[2] if len(sys.argv) > 2 else os.path.join(REPO, "docs/img/stillwater-wordmark.svg")
WORD = "Stillwater"
BLUE = "#2563eb"

font = TTFont(FONT)
cmap = font.getBestCmap()
gs = font.getGlyphSet()
hmtx = font["hmtx"]

x = 0
parts = []
minx = miny = 1e18
maxx = maxy = -1e18
for ch in WORD:
    gname = cmap[ord(ch)]
    adv = hmtx[gname][0]
    shift = (1, 0, 0, 1, x, 0)
    sp = SVGPathPen(gs)
    gs[gname].draw(TransformPen(sp, shift))
    d = sp.getCommands()
    if d.strip():
        parts.append(d)
    bp = BoundsPen(gs)
    gs[gname].draw(TransformPen(bp, shift))
    if bp.bounds:
        x0, y0, x1, y1 = bp.bounds
        minx, miny = min(minx, x0), min(miny, y0)
        maxx, maxy = max(maxx, x1), max(maxy, y1)
    x += adv

W = maxx - minx
H = maxy - miny
pad = round(max(W, H) * 0.03)  # small margin so Oleo swashes never clip
vb_w, vb_h = W + 2 * pad, H + 2 * pad
# font units are y-up; SVG is y-down -> scale(1,-1) and place tight bounds at (pad,pad).
transform = f"translate({pad - minx:.3f} {maxy + pad:.3f}) scale(1 -1)"

svg = (
    f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {vb_w:.2f} {vb_h:.2f}" '
    f'role="img" aria-label="Stillwater">\n'
    f'  <title>Stillwater</title>\n'
    f'  <path transform="{transform}" d="{" ".join(parts)}" fill="{BLUE}"/>\n'
    f'</svg>\n'
)
os.makedirs(os.path.dirname(OUT), exist_ok=True)
open(OUT, "w").write(svg)
print(f"wrote {OUT}  (viewBox 0 0 {vb_w:.2f} {vb_h:.2f})")
