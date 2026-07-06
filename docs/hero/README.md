# Cinematic README hero (#1756)

A scripted, regenerable walkthrough of the promoted `/next` UI for the project
README: real UI footage, a synthetic cursor with click ripples, kinetic captions,
scene-to-scene dip-to-black fades, and an Oleo Script "Stillwater" wordmark outro.

Everything here is **source**. The rendered video, the trimmed clips, the generated
clip manifest, the fixture database, and the built binary are working-directory artifacts
and are **not** committed (see the two `.gitignore` files). The final video is
GitHub-hosted (uploaded as a PR attachment, not stored in git); the committed
`hero-static.png` is the poster / click-to-play fallback the README links to.

Two principles hold throughout:

- **Only publishable content.** The real private `/music` library never appears. The
  fixture is a purpose-built public-domain classical library; every portrait is
  license-verified from Wikimedia Commons (see `PD-SOURCES.md`, and
  `mocks/candidates/CANDIDATE-SOURCES.md` for the artwork-modal grid).
- **All fakes live in the capture harness.** No application code is modified. The
  image/metadata provider endpoints (real, keyed, copyrighted sources) are
  intercepted by the recorder via Playwright `page.route()` and answered with the
  deterministic public-domain fixtures in `mocks/`. Zero external requests.

## Pipeline (current: clip-stitch + Remotion)

```
seed-fixture.sh ──▶ stillwater on :1991 ──▶ nav-clips.mjs ──▶ build-clips.mjs ──▶ remotion render HeroStitched ──▶ ffmpeg encodes
 (fixture DB +        (native, SW_UX=dual)   (per-screen         (trim+transcode;      (stitches clips +           (deliverables:
  PD portraits)                               .webm clips +       WRITES              dip-to-black fades +        hero.mp4/webm +
                                              clips.json)         clips.generated.ts)  captions + Oleo outro)      hero-static.png)
```

The scene stitching, dip-to-black fades, kinetic captions, and the wordmark outro
all live **inside** the `HeroStitched` Remotion composition (`captions/src/`); there
is no separate stitch step.

## Regenerate from a clean clone

Prereqs: Go toolchain; Node + `npm ci` in `docs/hero/captions/`; `ffmpeg` + `ffprobe`;
`fonttools` (for the wordmark); the Playwright bundled **chromium** (never
firefox/webkit - their teardown can kill a live browser). The 12 PD composer
portraits are committed under `portraits/`; only re-run `fetch-portraits.sh` to
refresh them (it re-verifies each image's Commons license and rewrites `PD-SOURCES.md`).

```bash
# 1. Seed + start the fixture server on :1991 (builds the binary if missing, seeds
#    the DB from seed/fixture.sql, copies the committed PD portraits into the
#    library, repoints paths, masks provider keys with fake encrypted values).
HERO_PORT=1991 HERO_DIR=/tmp/hero-1756 docs/hero/seed-fixture.sh
#    Admin login used by the recorder: herofixture-admin / herofixture-pw
#    Stop later with: lsof -ti:1991 -sTCP:LISTEN | xargs kill

# 2. (Optional) regenerate the route-mock fixtures (PD candidate grid + metadata
#    fragment) if you changed mocks/candidates/ or the composer data.
bash docs/hero/mocks/gen-mocks.sh

# 3. Record each screen as its own clip (bundled chromium, dark theme, synthetic
#    black cursor + click ripple, provider routes mocked to PD data-URIs).
node docs/hero/nav-clips.mjs
#    -> /tmp/hero-1756/clips/{NN-name}.webm + clips.json

# 4. Trim each clip's lead, transcode webm -> mp4, and WRITE the clip manifest.
#    THIS STEP writes captions/src/clips.generated.ts (consumed by HeroStitched).
cd docs/hero/captions && node build-clips.mjs
#    -> captions/public/clips/*.mp4 + captions/src/clips.generated.ts

# 5. Regenerate the README wordmark (outlined Oleo paths; no font dependency).
python3 docs/hero/gen-wordmark.py            # -> docs/img/stillwater-wordmark.svg

# 6. Render the stitched hero (clips + fades + captions + Oleo outro).
cd docs/hero/captions
npx remotion render src/index.ts HeroStitched out/hero-stitched.mp4

# 7. Encode the committed/hosted deliverables from the stitched master.
SRC=out/hero-stitched.mp4
#    a) hero.mp4 - two-pass, kept < 10MB so it fits GitHub's free-plan attachment cap
ffmpeg -y -i "$SRC" -c:v libx264 -preset medium -b:v 2000k -pass 1 -an -f mp4 /dev/null
ffmpeg -y -i "$SRC" -c:v libx264 -preset medium -b:v 2000k -pass 2 -an \
  -pix_fmt yuv420p -movflags +faststart ../hero.mp4
#    b) hero.webm - VP9
ffmpeg -y -i "$SRC" -c:v libvpx-vp9 -crf 33 -b:v 0 -pix_fmt yuv420p -row-mt 1 \
  -deadline good -cpu-used 5 ../hero.webm
#    c) hero-static.png - committed poster / click-to-play fallback (a dashboard frame)
ffmpeg -y -ss 2.0 -i "$SRC" -frames:v 1 -update 1 ../hero-static.png
```

## README wire-in (one-time, manual)

GitHub only renders a click-to-play, non-looping video player from an **uploaded
attachment**, not from a repo-committed file (a committed video renders as a mere
download link). So the video is hosted, not committed:

1. Drag `docs/hero/hero.mp4` into a PR/issue comment box - GitHub mints a
   `https://github.com/user-attachments/assets/<uuid>` URL.
2. Replace the `VIDEO_URL_PLACEHOLDER` token in the top-level `README.md` with that
   URL. Until then the committed `hero-static.png` renders as the poster.

## Files

| File | Role |
|---|---|
| `seed-fixture.sh` | Build the binary, seed the DB from `seed/fixture.sql`, copy PD portraits, start the fixture on `:1991`. |
| `fetch-portraits.sh` | Fetch + license-verify the 12 PD composer portraits from Wikimedia Commons; rewrites `PD-SOURCES.md`. |
| `fetch-bach-candidates.py` | Fetch + license-verify the PD Bach candidates for the artwork-modal grid. |
| `nav-clips.mjs` | Record each screen as its own clip (Playwright, chromium-only; synthetic cursor + ripples). |
| `captions/build-clips.mjs` | Trim + transcode the clips and **write `captions/src/clips.generated.ts`**. |
| `captions/src/` | Remotion project: `HeroStitched` (stitch + fades + captions), `Outro` (Oleo wordmark), `CaptionChip`, `shots`, `Root`. |
| `gen-wordmark.py` | Emit `docs/img/stillwater-wordmark.svg` (outlined Oleo paths). |
| `mocks/` | Route-mock HTML + PD candidate images (`gen-mocks.sh` regenerates the fragments). |
| `PD-SOURCES.md` / `mocks/candidates/CANDIDATE-SOURCES.md` | Per-image license + Commons provenance. |
