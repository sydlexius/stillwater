// seed-google-images-link.js - fixture for google-images-link.spec.js
// (#3223). Scans one artist whose name contains "&", the AC's explicit
// encoding case, since the affordance's query is built from data.Artist.Name
// and there is no artist-create endpoint (see seed-blast-radius.js).

import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { ensureLibrary, runScan, artistIdsByName } from './api.js';

const FIXTURE_LIBRARY_NAME = 'a11y google-images-link fixture';
export const AMPERSAND_ARTIST = 'Fixture & Sons';

/** Scans one artist directory named "Fixture & Sons" and returns its id. */
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

  await ensureLibrary(request, FIXTURE_LIBRARY_NAME, dir);
  await runScan(request);

  const id = (await artistIdsByName(request, [AMPERSAND_ARTIST])).get(AMPERSAND_ARTIST);
  if (!id) throw new Error('seed: google-images-link fixture artist did not appear after scan');
  return id;
}
