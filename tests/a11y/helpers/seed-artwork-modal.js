// seed-artwork-modal.js - throwaway server + artist for the artwork-modal scan
// (#3475).
//
// WHY THIS EXISTS
//
// `make test-a11y` boots the shared server against an EMPTY database and an
// EMPTY library, so /artists lists nothing and the Manage-artwork modal has no
// artist to open on. The scan used to take a conditional test.skip for that,
// which reported green on every run while verifying nothing. The gap is the
// absent DATA, so the fix is a fixture, never a softened assertion.
//
// WHY A SECOND SERVER, NOT THE SHARED ONE
//
// Spec files share one server and run alphabetically. An artist seeded there
// would appear in the list of every later spec (the bulk bar, the dashboard
// counts) and change what they see. This spec boots its OWN server (the
// base-path-server pattern, as seed-muted-text.js does): nothing it creates is
// visible to, or outlives, any other spec, and there is nothing to clean up
// beyond stopping the process.
//
// WHAT THE FIXTURE MUST HOLD
//
// The scan is only worth anything if the modal has images to render. The
// artist directory carries a thumb (folder.png) and THREE fanart (fanart.png,
// fanart2.png, fanart3.png) written as real PNG files, and seedArtworkModal()
// asserts, against the API, that the scan registered the thumb and all three
// fanart before handing the artist back. Three, because the Backdrops slot list
// only renders move-up / move-down buttons between slots. The fixture seeds no
// logo or banner, so those two kinds render their EMPTY state only.

import fs from 'node:fs';
import path from 'node:path';
import zlib from 'node:zlib';

import { execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

import { startBasePathServer } from './base-path-server.js';

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..', '..');

export const MODAL_ARTIST = 'Artwork Modal Fixture';
// A well-formed but synthetic MusicBrainz id. The editor only offers its
// "search providers" action for an artist that has one.
const MODAL_MBID = '00000000-0000-4000-8000-000000003475';

// CRC-32 table for the PNG chunk checksums (zlib.crc32 is not available on
// every supported Node 18+ release).
const CRC_TABLE = (() => {
  const table = new Uint32Array(256);
  for (let n = 0; n < 256; n++) {
    let c = n;
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    table[n] = c >>> 0;
  }
  return table;
})();

function crc32(buf) {
  let c = 0xffffffff;
  for (const b of buf) c = CRC_TABLE[(c ^ b) & 0xff] ^ (c >>> 8);
  return (c ^ 0xffffffff) >>> 0;
}

function chunk(type, data) {
  const len = Buffer.alloc(4);
  len.writeUInt32BE(data.length);
  const body = Buffer.concat([Buffer.from(type, 'ascii'), data]);
  const crc = Buffer.alloc(4);
  crc.writeUInt32BE(crc32(body));
  return Buffer.concat([len, body, crc]);
}

// png returns an RGB gradient PNG of w x h. A gradient (not a flat fill) so the
// image decodes as a real picture the way a user's artwork would.
function png(w, h) {
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(w, 0);
  ihdr.writeUInt32BE(h, 4);
  ihdr[8] = 8; // bit depth
  ihdr[9] = 2; // color type: RGB
  const raw = Buffer.alloc((w * 3 + 1) * h);
  for (let y = 0; y < h; y++) {
    const row = y * (w * 3 + 1);
    raw[row] = 0; // filter: none
    for (let x = 0; x < w; x++) {
      raw[row + 1 + x * 3] = Math.floor((x * 255) / w);
      raw[row + 2 + x * 3] = Math.floor((y * 255) / h);
      raw[row + 3 + x * 3] = 128;
    }
  }
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    chunk('IHDR', ihdr),
    chunk('IDAT', zlib.deflateSync(raw)),
    chunk('IEND', Buffer.alloc(0)),
  ]);
}

async function call(server, method, urlPath, body) {
  const resp = await fetch(`${server.baseURL}${urlPath}`, {
    method,
    headers: {
      'Content-Type': 'application/json',
      'X-CSRF-Token': server.csrfToken,
      Cookie: `csrf_token=${server.csrfToken}; session=${server.sessionCookie}`,
    },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (!resp.ok) throw new Error(`seed-artwork-modal: ${method} ${urlPath} -> ${resp.status} ${await resp.text()}`);
  return resp;
}

/**
 * seedArtworkModal boots a throwaway server whose library holds one artist with
 * one thumb and three backdrops (fanart) on disk, scans it, and returns
 * { server, artistId }. Throws (after stopping the server) if the fixture's
 * defining property does not hold: the artist must exist and the API must
 * report the thumb and exactly three backdrops (fanart_count === 3).
 */
export async function seedArtworkModal() {
  const server = await startBasePathServer('', {
    env: { SW_UX: 'next' },
    // Runs before the process starts, so the library already holds the artist
    // when the scan below walks it. The music dir is <tmp>/empty-library.
    seed: (tmpDir) => {
      const artistDir = path.join(tmpDir, 'empty-library', MODAL_ARTIST);
      fs.mkdirSync(artistDir, { recursive: true });
      fs.writeFileSync(
        path.join(artistDir, 'artist.nfo'),
        `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>\n<artist><name>${MODAL_ARTIST}</name><musicbrainzartistid>${MODAL_MBID}</musicbrainzartistid></artist>\n`,
      );
      fs.writeFileSync(path.join(artistDir, 'folder.png'), png(600, 600));
      // THREE backdrops, not one: the template only emits a move-up button when
      // a slot has a predecessor and a move-down button when it has a successor,
      // so a single backdrop renders neither and leaves them unscanned (#3475).
      // Three gives 2 move-up + 2 move-down buttons (slots 1,2 up; slots 0,1 down).
      for (const name of ['fanart.png', 'fanart2.png', 'fanart3.png']) {
        fs.writeFileSync(path.join(artistDir, name), png(1280, 720));
      }
    },
  });
  try {
    await call(server, 'POST', '/api/v1/scanner/run');
    const deadline = Date.now() + 60_000;
    let artist = null;
    while (Date.now() < deadline && !artist) {
      const list = await (await call(server, 'GET', '/api/v1/artists?page_size=50')).json();
      artist = (Array.isArray(list) ? list : list.artists || []).find((a) => a.name === MODAL_ARTIST) || null;
      if (!artist) await new Promise((r) => setTimeout(r, 500));
    }
    if (!artist) throw new Error('seed-artwork-modal: scan did not register the fixture artist within 60s');

    // The defining property: the artist HAS images. Without them the modal would
    // render its empty state and the scan would verify the wrong surface.
    const detail = await (await call(server, 'GET', `/api/v1/artists/${artist.id}`)).json();
    const a = detail.artist || detail;
    if (!a.thumb_exists || !a.fanart_exists || a.fanart_count !== 3) {
      throw new Error(
        `seed-artwork-modal: fixture artist lacks images (thumb_exists=${a.thumb_exists}, fanart_exists=${a.fanart_exists}, fanart_count=${a.fanart_count}, want 3)`,
      );
    }
    return { server, artistId: artist.id };
  } catch (err) {
    server.stop();
    throw err;
  }
}

// Memo for renderImageResults, keyed on every input it takes. Module-level, so it
// lives for one Playwright worker process and never outlives a run.
const renderCache = new Map();

/**
 * renderImageResults returns the real server-rendered provider-search fragment
 * (fixtures/render-image-results) with every card pointing at imageURL.
 * fragment is "images" (Primary, Logo and Banner result panel) or "fanart" (the
 * Backdrops kind's result grid). The harness is offline, so no live search can
 * produce cards; see that tool's header for why the component is rendered
 * rather than hand-copied.
 */
export function renderImageResults(artistId, imageURL, fragment = 'images') {
  // The program's output is a pure function of exactly these three arguments, and
  // within one run they do not change between tests (one seeded artist, one
  // server URL, two fragment kinds), so the 16 per-test calls collapse to two
  // `go run` spawns. Only a success is cached: a failure throws before the set
  // below, so the next call retries exactly as it did without the cache.
  const key = JSON.stringify([artistId, imageURL, fragment]);
  if (renderCache.has(key)) return renderCache.get(key);
  let out;
  try {
    out = execFileSync('go', ['run', './tests/a11y/fixtures/render-image-results', artistId, imageURL, fragment], {
      cwd: REPO_ROOT, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'],
    });
  } catch (err) {
    throw new Error(`seed-artwork-modal: rendering the ${fragment} fragment failed: ${err.stderr || err.message}`);
  }
  renderCache.set(key, out);
  return out;
}
