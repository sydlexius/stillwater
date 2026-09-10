// seed-google-images-link.js - fixture for google-images-link.spec.js
// (#3223). Scans one artist whose name contains "&", the AC's explicit
// encoding case, since the affordance's query is built from data.Artist.Name
// and there is no artist-create endpoint (see seed-blast-radius.js).
//
// #3223 review round 2, F3: the original fixture wrote no fanart file at
// all, so the artist scanned with FanartCount=0 and the indexed
// (fanartIdx >= 0) Actions-menu branch -- reachable only via
// ?type=fanart&index=N for an N within range -- could never render. The
// spec's "indexed backdrop slot" test papered over that with a conditional
// `test.skip` when the link's count was 0, which the repo's CLAUDE.md
// forbids (a skip that reports green forever while verifying nothing). The
// fix is the fixture, not a softened assertion: write a real fanart file so
// index=0 is always in range, and assert that precondition (FanartCount >= 1)
// here before the spec ever navigates, so a regression in the seeder itself
// fails loudly at seed time instead of surfacing as a mysterious skip three
// files away.

import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { ensureLibrary, runScan, artistIdsByName, BASE_URL } from './api.js';

const FIXTURE_LIBRARY_NAME = 'a11y google-images-link fixture';
export const AMPERSAND_ARTIST = 'Fixture & Sons';

// A minimal but genuinely decodable 1x1 JPEG (not a placeholder byte), so
// the scanner's probeImageFile (internal/scanner/scanner.go) succeeds rather
// than silently swallowing a decode error -- this fixture wants a realistic
// "one backdrop exists" artist, not merely a file with the right name.
const TINY_JPEG_BASE64 =
  '/9j/4AAQSkZJRgABAQEAYABgAAD/2wBDAAMCAgICAgMCAgIDAwMDBAYEBAQEBAgGBgUGCQgKCgkICQkKDA8MCgsOCwkJDRENDg8QEBEQCgwSExIQEw8QEBD/2wBDAQMDAwQDBAgEBAgQCwkLEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBD/wAARCAABAAEDASIAAhEBAxEB/8QAFQABAQAAAAAAAAAAAAAAAAAAAAj/xAAUEAEAAAAAAAAAAAAAAAAAAAAA/8QAFQEBAQAAAAAAAAAAAAAAAAAAAAX/xAAUEQEAAAAAAAAAAAAAAAAAAAAA/9oADAMBAAIRAxEAPwCdABmX/9k=';

/**
 * seedGoogleImagesLinkArtist scans one artist directory named "Fixture &
 * Sons" carrying a single real fanart file, and returns its id.
 *
 * Guarantees (and asserts, before returning) FanartExists=true and
 * FanartCount>=1, since the indexed backdrop-slot Actions menu branch this
 * fixture also exercises only renders when at least one backdrop slot
 * exists.
 */
export async function seedGoogleImagesLinkArtist(request) {
  const dir = path.join(os.tmpdir(), `sw-a11y-google-images-link-${process.env.SW_PORT || 'default'}`);
  const artistDir = path.join(dir, AMPERSAND_ARTIST);
  fs.mkdirSync(artistDir, { recursive: true });
  // "&" must be escaped as "&amp;" in the NFO's XML; the parser decodes it
  // back to the literal "&" the assertions compare against.
  fs.writeFileSync(
    path.join(artistDir, 'artist.nfo'),
    `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>\n<artist><name>${AMPERSAND_ARTIST.replace(/&/g, '&amp;')}</name></artist>\n`,
  );
  fs.writeFileSync(path.join(artistDir, 'fanart.jpg'), Buffer.from(TINY_JPEG_BASE64, 'base64'));

  await ensureLibrary(request, FIXTURE_LIBRARY_NAME, dir);
  await runScan(request);

  const id = (await artistIdsByName(request, [AMPERSAND_ARTIST])).get(AMPERSAND_ARTIST);
  if (!id) throw new Error('seed: google-images-link fixture artist did not appear after scan');

  // Assert the precondition the indexed-slot test depends on BEFORE handing
  // the id back, rather than letting a seeding regression surface three
  // files away as an unexplained element-not-found.
  //
  // GET /api/v1/artists/{id} wraps the artist under an "artist" key
  // (handleGetArtist, internal/api/handlers_artist.go) rather than
  // returning it flat like the list endpoint artistIdsByName reads above --
  // unwrap it here.
  const resp = await request.fetch(`${BASE_URL}/api/v1/artists/${id}`);
  if (!resp.ok()) {
    throw new Error(`seed: fetching fixture artist ${id} failed: ${resp.status()}`);
  }
  const { artist: a } = await resp.json();
  if (!a) {
    throw new Error(`seed: GET /api/v1/artists/${id} response had no "artist" key`);
  }
  if (!a.fanart_exists || a.fanart_count < 1) {
    throw new Error(
      `seed: google-images-link fixture artist has fanart_exists=${a.fanart_exists}, `
      + `fanart_count=${a.fanart_count} -- want fanart_exists=true, fanart_count>=1 `
      + '(the indexed backdrop-slot Actions menu requires at least one backdrop to exist)',
    );
  }

  return id;
}
