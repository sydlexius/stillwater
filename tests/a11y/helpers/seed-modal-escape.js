// seed-modal-escape.js - fixture for modal-escape.spec.js (#2727). Scans one
// artist so /artists/{id} renders the artist-detail page; there is no
// artist-create endpoint (see seed-blast-radius.js), and `make test-a11y`
// boots an empty database and library.

import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { ensureLibrary, runScan, artistIdsByName } from './api.js';

const FIXTURE_LIBRARY_NAME = 'a11y modal-escape fixture';
export const MODAL_ESCAPE_ARTIST = 'Modal Escape Fixture';

/** Scans one artist directory and returns its id; throws if it did not appear. */
export async function seedModalEscapeArtist(request) {
  const dir = path.join(os.tmpdir(), `sw-a11y-modal-escape-${process.env.SW_PORT || 'default'}`);
  const artistDir = path.join(dir, MODAL_ESCAPE_ARTIST);
  fs.mkdirSync(artistDir, { recursive: true });
  fs.writeFileSync(
    path.join(artistDir, 'artist.nfo'),
    `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>\n<artist><name>${MODAL_ESCAPE_ARTIST}</name></artist>\n`,
  );

  await ensureLibrary(request, FIXTURE_LIBRARY_NAME, dir);
  await runScan(request);

  const id = (await artistIdsByName(request, [MODAL_ESCAPE_ARTIST])).get(MODAL_ESCAPE_ARTIST);
  if (!id) throw new Error('seed: modal-escape fixture artist did not appear after scan');
  return id;
}
