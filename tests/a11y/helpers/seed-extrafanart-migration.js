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
import crypto from 'node:crypto';
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

// ---- The RUN fixture (extrafanart-migration-run.spec.js) ----
//
// A run MOVES files, so it never touches the shared server: the spec boots its
// own (helpers/base-path-server.js) and these helpers talk to THAT server with
// plain fetch. One artist migrates cleanly; the other's folder is made
// read-only AFTER the scan, so its files cannot be renamed out of extrafanart/
// and the run ends partial (207).
export const RUN_FIXTURE = {
  libraryName: 'a11y extrafanart-run fixture',
  okArtist: 'Run Fixture Ok',
  lockedArtist: 'Run Fixture Locked',
};

export function serverFetch(server, method, urlPath, body, extraHeaders = {}) {
  return fetch(`${server.baseURL}${urlPath}`, {
    method,
    headers: {
      ...extraHeaders,
      'Content-Type': 'application/json',
      'X-CSRF-Token': server.csrfToken,
      Cookie: `session=${server.sessionCookie}; csrf_token=${server.csrfToken}`,
    },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
}

/** extraFanartFiles lists an artist's extrafanart/ file names ([] once the folder is gone). */
export function extraFanartFiles(libDir, artist) {
  const dir = path.join(libDir, artist, 'extrafanart');
  return fs.existsSync(dir) ? fs.readdirSync(dir).sort() : [];
}

/**
 * cleanupExtraFanartRunFixture unlocks the read-only folder FIRST (a locked
 * folder cannot be emptied, so the rm below, and the server's own temp-dir
 * removal, would fail), then removes the library row with its artists and the
 * files. Safe to call when nothing was seeded.
 */
export async function cleanupExtraFanartRunFixture(server, libDir) {
  try {
    const locked = path.join(libDir, RUN_FIXTURE.lockedArtist);
    if (fs.existsSync(locked)) fs.chmodSync(locked, 0o755);
    const libs = await (await serverFetch(server, 'GET', '/api/v1/libraries')).json();
    for (const lib of (Array.isArray(libs) ? libs : (libs.libraries || [])).filter(l => l.name === RUN_FIXTURE.libraryName)) {
      const resp = await serverFetch(server, 'DELETE', `/api/v1/libraries/${lib.id}?deleteArtists=true`);
      if (!resp.ok) throw new Error(`cleanup: deleting library ${lib.id} failed: ${resp.status} ${await resp.text()}`);
    }
  } finally {
    fs.rmSync(libDir, { recursive: true, force: true });
  }
}

/**
 * seedExtraFanartRunFixture rebuilds the run fixture from nothing under libDir
 * and proves its defining properties before returning: both artists hold
 * FILES_PER_ARTIST files in extrafanart/, the server's own PREVIEW plans every
 * one of them with no problems, and the locked folder really refuses a write.
 * As root the permission bits bind nothing, so the fixture cannot exist: that
 * throws, loudly, rather than letting a spec pass against a run that fails nothing.
 */
export async function seedExtraFanartRunFixture(server, libDir) {
  await cleanupExtraFanartRunFixture(server, libDir);
  const jpeg = Buffer.from(TINY_JPEG_BASE64, 'base64');
  const names = [RUN_FIXTURE.okArtist, RUN_FIXTURE.lockedArtist];
  for (const name of names) {
    const extraDir = path.join(libDir, name, 'extrafanart');
    fs.mkdirSync(extraDir, { recursive: true });
    fs.writeFileSync(path.join(libDir, name, 'artist.nfo'),
      `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>\n<artist><name>${name}</name></artist>\n`);
    fs.writeFileSync(path.join(libDir, name, 'fanart.jpg'), jpeg);
    for (let i = 0; i < FILES_PER_ARTIST; i++) {
      fs.writeFileSync(path.join(extraDir, `extra${i}.jpg`), Buffer.concat([jpeg, Buffer.from(`${name}-${i}`)]));
    }
  }
  const created = await serverFetch(server, 'POST', '/api/v1/libraries', { name: RUN_FIXTURE.libraryName, path: libDir, type: 'regular' });
  if (!created.ok) throw new Error(`seed: creating the run library failed: ${created.status} ${await created.text()}`);
  const scan = await serverFetch(server, 'POST', '/api/v1/scanner/run');
  if (!scan.ok) throw new Error(`seed: scanner run failed: ${scan.status} ${await scan.text()}`);
  const deadline = Date.now() + 60_000;
  for (;;) {
    const st = await (await serverFetch(server, 'GET', '/api/v1/scanner/status')).json();
    if (st.status === 'completed' || st.status === 'idle') break;
    if (Date.now() > deadline) throw new Error('seed: scan did not complete within 60s');
    await new Promise(r => setTimeout(r, 250));
  }

  const preview = await (await serverFetch(server, 'POST', '/api/v1/reports/extrafanart-migration', { dry_run: true })).json();
  const want = names.length * FILES_PER_ARTIST;
  if (preview.status !== 'planned' || preview.planned !== want || preview.problems !== 0) {
    throw new Error(`seed: want a clean plan of ${want} files, got ${JSON.stringify(preview)}`);
  }

  const locked = path.join(libDir, RUN_FIXTURE.lockedArtist);
  fs.chmodSync(locked, 0o555);
  let writable = true;
  try { fs.writeFileSync(path.join(locked, 'probe.tmp'), 'x'); } catch { writable = false; }
  if (writable) {
    fs.rmSync(path.join(locked, 'probe.tmp'), { force: true });
    throw new Error('seed: the locked artist folder still accepts writes (running as root?). '
      + 'The unmovable-folder fixture cannot be built, so the run spec cannot prove a partial run.');
  }
}

/**
 * inventory lists every file under libDir as { rel, sha }: the content hash is
 * what a move preserves (the name changes), so it is what "nothing was deleted"
 * is measured by.
 */
export function inventory(libDir) {
  const out = [];
  const walk = (dir) => {
    for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
      const full = path.join(dir, e.name);
      if (e.isDirectory()) walk(full);
      else out.push({ rel: path.relative(libDir, full), sha: crypto.createHash('sha256').update(fs.readFileSync(full)).digest('hex') });
    }
  };
  walk(libDir);
  return out.sort((a, b) => a.rel.localeCompare(b.rel));
}

/** missingContent names the files of `before` whose bytes are in no file of `after`. */
export function missingContent(before, after) {
  const have = new Set(after.map((f) => f.sha));
  return before.filter((f) => !have.has(f.sha)).map((f) => f.rel);
}

/** unlockRunFixture makes the read-only artist folder writable, for the test that needs a fully clean run. */
export function unlockRunFixture(libDir) {
  fs.chmodSync(path.join(libDir, RUN_FIXTURE.lockedArtist), 0o755);
}
