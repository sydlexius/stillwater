// seed-merge-extrafanart.js - fixture for merge-extrafanart-report.spec.js
// (#3180): two near-duplicate artists whose extrafanart/ folders hold files, so
// the duplicates page offers a merge and the merge has something to report.
//
// `make test-a11y` boots an EMPTY database and library, so the fixture is built
// inside the harness against the server the spec just started. A merge MOVES and
// DELETES files, so the spec uses its own throwaway server
// (helpers/base-path-server.js) and these helpers talk to THAT server with plain
// fetch (serverFetch from seed-extrafanart-migration.js).

import fs from 'node:fs';
import path from 'node:path';

import { serverFetch } from './seed-extrafanart-migration.js';

const TINY_JPEG_BASE64 =
  '/9j/4AAQSkZJRgABAQEAYABgAAD/2wBDAAMCAgICAgMCAgIDAwMDBAYEBAQEBAgGBgUGCQgKCgkICQkKDA8MCgsOCwkJDRENDg8QEBEQCgwSExIQEw8QEBD/2wBDAQMDAwQDBAgEBAgQCwkLEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBD/wAARCAABAAEDASIAAhEBAxEB/8QAFQABAQAAAAAAAAAAAAAAAAAAAAj/xAAUEAEAAAAAAAAAAAAAAAAAAAAA/8QAFQEBAQAAAAAAAAAAAAAAAAAAAAX/xAAUEQEAAAAAAAAAAAAAAAAAAAAA/9oADAMBAAIRAxEAPwCdABmX/9k=';

export const MERGE_FIXTURE = {
  libraryName: 'a11y merge-extrafanart fixture',
  // HOSTILE on purpose: markup and a quote. Any code path that builds the report
  // with innerHTML would run the onerror handler or add an element.
  artistName: 'Merge <img src=x onerror="window.__pwn=1"> "Q" Fixture',
  dirA: 'Merge Report Fixture',
  dirB: 'Merge Report Fixture (copy)',
};

// Images the survivor will hold after the merge, whichever side survives: A has
// 3, B has 3 (one byte-identical to an A file, one sharing a NAME with an A
// file but different bytes), so both survive as separate files = 6. B also holds
// a dotfile image the merge does not carry over and the report does not count.
export const EXPECTED_IMAGES = 6;

const xmlEscape = (v) => v.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
const jpeg = () => Buffer.from(TINY_JPEG_BASE64, 'base64');
const bytes = (tag) => Buffer.concat([jpeg(), Buffer.from(tag)]);

/** cleanupMergeFixture removes the library (with its artists) and the files. Safe when nothing was seeded. */
export async function cleanupMergeFixture(server, libDir) {
  try {
    const libs = await (await serverFetch(server, 'GET', '/api/v1/libraries')).json();
    for (const lib of (Array.isArray(libs) ? libs : (libs.libraries || [])).filter(l => l.name === MERGE_FIXTURE.libraryName)) {
      const resp = await serverFetch(server, 'DELETE', `/api/v1/libraries/${lib.id}?deleteArtists=true`);
      if (!resp.ok) throw new Error(`cleanup: deleting library ${lib.id} failed: ${resp.status} ${await resp.text()}`);
    }
  } finally {
    fs.rmSync(libDir, { recursive: true, force: true });
  }
}

/**
 * seedMergeFixture rebuilds the two-artist fixture under libDir, scans it, and
 * proves its defining properties before returning: both directories hold the
 * claimed extrafanart/ files (including the byte-identical pair and the name
 * collision), and the duplicates report lists exactly one group.
 * withExtraFanart=false builds the same pair with NO extrafanart/ folders, for
 * the silent case.
 */
export async function seedMergeFixture(server, libDir, withExtraFanart = true, withSymlink = false) {
  await cleanupMergeFixture(server, libDir);
  const dirs = { [MERGE_FIXTURE.dirA]: {}, [MERGE_FIXTURE.dirB]: {} };
  if (withExtraFanart) {
    dirs[MERGE_FIXTURE.dirA] = { 'one.jpg': bytes('a-one'), 'same.jpg': bytes('shared'), 'clash.jpg': bytes('a-clash') };
    dirs[MERGE_FIXTURE.dirB] = {
      'two.jpg': bytes('b-two'), 'same-copy.jpg': bytes('shared'), 'clash.jpg': bytes('b-clash'), '.hidden.jpg': bytes('dot'),
    };
  }
  for (const [dir, files] of Object.entries(dirs)) {
    const root = path.join(libDir, dir);
    fs.mkdirSync(root, { recursive: true });
    fs.writeFileSync(path.join(root, 'artist.nfo'),
      `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>\n<artist><name>${xmlEscape(MERGE_FIXTURE.artistName)}</name></artist>\n`);
    for (const [name, data] of Object.entries(files)) {
      const target = path.join(root, 'extrafanart', name);
      fs.mkdirSync(path.dirname(target), { recursive: true });
      fs.writeFileSync(target, data);
    }
  }
  if (withSymlink) {
    // A top-level symlink in BOTH folders: whichever one is the loser, the merge
    // skips it and says so in `warnings` (no product hook needed), with no report.
    for (const dir of Object.keys(dirs)) {
      fs.symlinkSync(path.join(libDir, dir, 'artist.nfo'), path.join(libDir, dir, 'linked.nfo'));
      if (!fs.lstatSync(path.join(libDir, dir, 'linked.nfo')).isSymbolicLink()) throw new Error(`seed: ${dir}/linked.nfo is not a symlink`);
    }
  }
  const created = await serverFetch(server, 'POST', '/api/v1/libraries', { name: MERGE_FIXTURE.libraryName, path: libDir, type: 'regular' });
  if (!created.ok) throw new Error(`seed: creating the merge library failed: ${created.status} ${await created.text()}`);
  const scan = await serverFetch(server, 'POST', '/api/v1/scanner/run');
  if (!scan.ok) throw new Error(`seed: scanner run failed: ${scan.status} ${await scan.text()}`);
  const deadline = Date.now() + 60_000;
  for (;;) {
    const st = await (await serverFetch(server, 'GET', '/api/v1/scanner/status')).json();
    if (st.status === 'completed' || st.status === 'idle') break;
    if (Date.now() > deadline) throw new Error('seed: scan did not complete within 60s');
    await new Promise(r => setTimeout(r, 250));
  }

  // Defining properties, asserted before the exercise.
  const want = withExtraFanart ? { [MERGE_FIXTURE.dirA]: 3, [MERGE_FIXTURE.dirB]: 4 } : { [MERGE_FIXTURE.dirA]: 0, [MERGE_FIXTURE.dirB]: 0 };
  for (const [dir, n] of Object.entries(want)) {
    const extra = path.join(libDir, dir, 'extrafanart');
    const have = fs.existsSync(extra) ? fs.readdirSync(extra).length : 0;
    if (have !== n) throw new Error(`seed: ${dir}/extrafanart holds ${have} files, want ${n}`);
  }
  const page = await serverFetch(server, 'GET', '/reports/duplicates');
  const html = await page.text();
  const groups = (html.match(/<div[^>]*data-duplicate-group[ >]/g) || []).length;
  if (groups !== 1) throw new Error(`seed: want exactly 1 duplicate group on /reports/duplicates, found ${groups}`);
}
