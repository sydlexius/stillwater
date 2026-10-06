// seed-extrafanart-migration.js - fixtures for the extrafanart migration page
// (#3179), /reports/extrafanart-migration.
//
// `make test-a11y` boots against a brand new EMPTY database and EMPTY library,
// so with no fixture the page renders its empty state: no plan table, no run
// button. A scan of that proves nothing about the surface this issue ships. The
// gap is the absent DATA, so this builds it inside the harness.
//
// The page is NOT cache-backed: every load runs a fresh preview over the
// artists' directories, so a per-spec seed in beforeAll is enough.
//
// CLEANUP IS PART OF THE FIXTURE. Every spec shares one server and runs in
// alphabetical order, so artists, a library row and files left behind here would
// become part of the library that later specs scan (the same debris problem
// seed-platform-backdrop-duplicates.js documents for connection rows). Each
// spec calls cleanupExtraFanartFixture in afterAll, and every seed call begins
// with it, so a retry or a second browser project always starts from nothing.

import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { BASE_URL, apiFetch, ensureLibrary, runScan, artistIdsByName } from './api.js';

const TINY_JPEG_BASE64 =
  '/9j/4AAQSkZJRgABAQEAYABgAAD/2wBDAAMCAgICAgMCAgIDAwMDBAYEBAQEBAgGBgUGCQgKCgkICQkKDA8MCgsOCwkJDRENDg8QEBEQCgwSExIQEw8QEBD/2wBDAQMDAwQDBAgEBAgQCwkLEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBD/wAARCAABAAEDASIAAhEBAxEB/8QAFQABAQAAAAAAAAAAAAAAAAAAAAj/xAAUEAEAAAAAAAAAAAAAAAAAAAAA/8QAFQEBAQAAAAAAAAAAAAAAAAAAAAX/xAAUEQEAAAAAAAAAAAAAAAAAAAAA/9oADAMBAAIRAxEAPwCdABmX/9k=';

export const FILES_PER_ARTIST = 2;

// The plan-rendering fixture (extrafanart-migration.spec.js).
export const PLAN_FIXTURE = {
  libraryName: 'a11y extrafanart-migration fixture',
  dirName: 'sw-a11y-extrafanart-migration',
  artists: ['Extra Fixture One', 'Extra Fixture Two'],
  // Scanned in like the others, then its folder is removed, so the page reports
  // it as skipped (a share that is not mounted).
  goneArtist: 'Extra Fixture Gone',
};

function fixtureDir(fx) {
  return path.join(os.tmpdir(), `${fx.dirName}-${process.env.SW_PORT || 'default'}`);
}

/**
 * cleanupExtraFanartFixture removes everything seedExtraFanartFixture created:
 * the library row WITH its artists (through the API the seed used), then the
 * temp directory. Throws on a failed API delete: swallowing it would reproduce the
 * leak this exists to stop.
 */
export async function cleanupExtraFanartFixture(request, fx) {
  // try/finally: the rm must run even when an API delete throws, or the files
  // are left behind. The delete error is still rethrown.
  try {
    const list = await request.fetch(`${BASE_URL}/api/v1/libraries`);
    if (list.ok()) {
      const body = await list.json();
      const libs = Array.isArray(body) ? body : (body.libraries || []);
      for (const lib of libs.filter(l => l.name === fx.libraryName)) {
        const resp = await apiFetch(request, 'DELETE', `/api/v1/libraries/${lib.id}?deleteArtists=true`);
        if (!resp.ok()) {
          throw new Error(`cleanup: deleting library ${lib.id} failed: ${resp.status()} ${await resp.text()}`);
        }
      }
    }
  } finally {
    const dir = fixtureDir(fx);
    fs.rmSync(dir, { recursive: true, force: true });
  }
}

/**
 * seedExtraFanartFixture builds the artists, each with a root fanart.jpg and
 * FILES_PER_ARTIST DISTINCT files under extrafanart/, scans them in, and
 * returns { dir, ids }. It starts by cleaning up any earlier run.
 *
 * It asserts the fixture's defining property before returning: every
 * extrafanart/ directory exists on disk with exactly FILES_PER_ARTIST files,
 * and every artist exists in the database. Distinct bytes per file matter: the
 * engine treats byte-identical files as copies to leave in place, which would
 * turn "will move" rows into "identical copy" rows.
 */
export async function seedExtraFanartFixture(request, fx) {
  await cleanupExtraFanartFixture(request, fx);
  const dir = fixtureDir(fx);
  const jpeg = Buffer.from(TINY_JPEG_BASE64, 'base64');

  const withGone = fx.goneArtist ? [...fx.artists, fx.goneArtist] : fx.artists;
  for (const name of withGone) {
    const artistDir = path.join(dir, name);
    const extraDir = path.join(artistDir, 'extrafanart');
    fs.mkdirSync(extraDir, { recursive: true });
    fs.writeFileSync(
      path.join(artistDir, 'artist.nfo'),
      `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>\n<artist><name>${name}</name></artist>\n`,
    );
    fs.writeFileSync(path.join(artistDir, 'fanart.jpg'), jpeg);
    for (let i = 0; i < FILES_PER_ARTIST; i++) {
      // A JPEG decoder stops at the end-of-image marker, so trailing bytes keep
      // the file valid while making its content (and sha256) unique.
      fs.writeFileSync(
        path.join(extraDir, `extra${i}.jpg`),
        Buffer.concat([jpeg, Buffer.from(`${name}-${i}`)]),
      );
    }
  }

  await ensureLibrary(request, fx.libraryName, dir);
  await runScan(request);

  // The scan has read it; now the folder disappears.
  if (fx.goneArtist) {
    fs.rmSync(path.join(dir, fx.goneArtist), { recursive: true, force: true });
    if (fs.existsSync(path.join(dir, fx.goneArtist))) {
      throw new Error(`seed: ${fx.goneArtist} folder still exists after removal`);
    }
  }

  // Precondition, asserted from the filesystem and the database rather than
  // trusted from the writes above.
  for (const name of fx.artists) {
    const files = fs.readdirSync(path.join(dir, name, 'extrafanart'));
    if (files.length !== FILES_PER_ARTIST) {
      throw new Error(`seed: ${name}/extrafanart holds ${files.length} files, want ${FILES_PER_ARTIST}`);
    }
  }
  const ids = await artistIdsByName(request, withGone);
  const missing = withGone.filter(n => !ids.has(n));
  if (missing.length) {
    throw new Error(`seed: scan did not create fixture artists: ${missing.join(', ')}`);
  }
  return { dir, ids };
}
