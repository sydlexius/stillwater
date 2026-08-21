// seed-platform-backdrop-duplicates.js - fixture for the platform backdrop
// report's partial-scan notice (#2979).
//
// WHY THIS EXISTS
//
// `make test-a11y` boots the server against a BRAND NEW EMPTY DATABASE and an
// EMPTY library (SW_DB_PATH / SW_EMPTY_LIB in the Makefile target). The
// #platform-backdrop-duplicates-partial-notice element renders only when
// view.ScanErrors > 0 (platform_backdrop_duplicates.templ), and ScanErrors
// only increments from a REAL scan failure against a REAL platform
// connection (internal/publish/backdrop_prune.go). With no fixture the
// element does not exist and any assertion on it would be vacuous.
//
// The gap is the absent DATA, so the fix is a fixture -- the same conclusion
// seed-backdrop-duplicates.js and seed-blast-radius.js reached for the same
// harness property. No conditional skip: an absent element must FAIL loud,
// never pass silently.
//
// HOW ScanErrors > 0 IS PRODUCED, FOR REAL
//
// detectArtistPlatformDups (internal/publish/backdrop_prune.go) walks each
// artist's platform IDs, skips connections that are disabled or whose
// Status != "ok", then calls GetArtistDetail. Status becomes "ok" only after
// a real TestConnection round-trip against GET /System/Info succeeds
// (handleCreateConnection -> testConnectionDirect, or handleTestConnection).
// A definitive 404/410 from GetArtistDetail is treated as "artist absent",
// which is NOT a scan error by itself (#2692's F1) -- but when EVERY mapped
// artist on a connection is absent, the sweep's systemic-absence guard fires
// and increments ScanErrors exactly once for that connection
// (ScanPlatformBackdropDuplicates, the `t.absent == t.mapped` loop at the end
// of the sweep). That is the cheapest real path to ScanErrors > 0: one
// connection, one mapped artist, one guaranteed-absent artist.
//
// So this fixture:
//   1. Starts a loopback fake Emby HTTP server answering GET /System/Info and
//      GET /Users (so the connection test and platform-user-id resolution
//      both succeed and Status becomes "ok"), and answering 404 to every
//      artist-detail lookup (GET /Users/{uid}/Items/{platformArtistID}).
//   2. Seeds one real artist via the filesystem-scan path (there is no
//      artist-create endpoint).
//   3. Creates a real Connection through POST /api/v1/connections pointed at
//      the fake server (skip_test: false, so the real TestConnection call
//      exercises the fake).
//   4. Maps the artist's platform ID via PUT
//      /api/v1/artists/{id}/platform-ids/{connectionId} to an ID the fake
//      always 404s.
//   5. POLLS /reports/platform-backdrop-duplicates until the notice element
//      actually lands -- the precondition the spec depends on.
//
// SEEDED FROM global-setup.js, NOT FROM THE SPEC'S beforeAll.
//
// The report renders from the process-wide dupimages.Cache, whose lazy refresh
// is locked out for 15 minutes once any page load warms it, so a fixture built
// after another spec has rendered a page can never reach the cached sweep.
// global-setup.js holds the full explanation and the measurement; the ordering
// it describes is a CONSTRAINT, not a preference. The fake Emby must also stay
// listening for the whole run, because a later sweep re-queries it.

import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import http from 'node:http';
import { BASE_URL, apiFetch, ensureLibrary, runScan, artistIdsByName } from './api.js';

const FIXTURE_LIBRARY_NAME = 'a11y platform-backdrop-duplicates fixture';
const FIXTURE_ARTIST = 'Absent Platform Fixture Artist';
const FIXTURE_CONNECTION_NAME = 'a11y platform-backdrop-duplicates fake emby';
// Never resolvable by the fake -- every /Users/{uid}/Items/{id} lookup 404s.
const FIXTURE_PLATFORM_ARTIST_ID = 'nonexistent-platform-artist-id';

// startFakeEmby answers just enough of the Emby REST surface for a
// connection test + platform-user-id resolution to succeed, then 404s any
// artist-detail lookup so the sweep's systemic-absence path fires.
function startFakeEmby() {
  const server = http.createServer((req, res) => {
    const u = new URL(req.url, 'http://127.0.0.1');
    if (u.pathname === '/System/Info') {
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ ServerName: 'a11y-fake-emby', Version: '4.0.0', Id: 'fake-emby' }));
      return;
    }
    if (u.pathname === '/Users') {
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify([{ Id: 'fake-user-id', Name: 'fake' }]));
      return;
    }
    // Every artist-detail lookup (/Users/{uid}/Items/{id}) is definitively
    // absent, so backdropRedundantIndices wraps errArtistAbsentOnPlatform.
    res.writeHead(404, { 'Content-Type': 'application/json' }).end('{}');
  });
  return new Promise((resolve, reject) => {
    server.on('error', reject);
    server.listen(0, '127.0.0.1', () => {
      const { port } = server.address();
      // unref so this listener never keeps the Playwright coordinator process
      // alive on its own. The fixture is seeded in globalSetup and must stay
      // reachable for the WHOLE run (a later sweep re-queries it), so nothing
      // closes it explicitly -- it dies with the process. Without unref a
      // crashed run would hang instead of exiting.
      server.unref();
      resolve({ server, port, close: () => new Promise(r => server.close(r)) });
    });
  });
}

async function ensureConnection(request, port) {
  const resp = await apiFetch(request, 'POST', '/api/v1/connections', {
    name: FIXTURE_CONNECTION_NAME,
    type: 'emby',
    url: `http://127.0.0.1:${port}`,
    api_key: 'a11y-fixture-key',
    enabled: true,
    skip_test: false,
  });
  if (resp.ok()) return (await resp.json()).id;
  // Re-run reuse: same type+url is treated as an update, not a 409, by
  // handleCreateConnection -- so this branch only guards a genuinely
  // unexpected failure, not idempotent re-seeding.
  throw new Error(`seed: creating fixture connection failed: ${resp.status()} ${await resp.text()}`);
}

// waitForNotice polls the live report page for the partial-scan notice
// marker. Polling is REQUIRED, not defensive: the page renders from a cached
// sweep (#3092), so the first GET returns the pending notice and merely kicks
// the sweep that produces what this fixture is waiting for.
//
// 90s to match seed-backdrop-duplicates.js: the sweep is a per-artist,
// per-connection round trip, and the lazy trigger only fires on a GET that
// finds the cache cold -- so the budget has to cover a sweep, not a render.
export async function waitForPlatformBackdropNotice(request) {
  const deadline = Date.now() + 90_000;
  let last = '';
  let sawOK = false;
  let lastStatus = 0;
  while (Date.now() < deadline) {
    const resp = await request.fetch(`${BASE_URL}/reports/platform-backdrop-duplicates`);
    lastStatus = resp.status();
    if (resp.ok()) {
      sawOK = true;
      last = await resp.text();
      if (last.includes('id="platform-backdrop-duplicates-partial-notice"')) return;
    }
    await new Promise(r => setTimeout(r, 2_000));
  }

  // Name the ACTUAL failure. These three states need different fixes, and
  // reporting the wrong one sends a maintainer to the wrong place:
  //   - never a 200: the page is unreachable (auth, routing, a dead server),
  //     nothing to do with ScanErrors at all;
  //   - pending: the page rendered, but the background sweep had not landed;
  //   - unrecognised: the page rendered a report that simply lacks the notice,
  //     which is the genuine "ScanErrors stayed 0" case.
  // The original wording reported "unrecognised" for all three, because a body
  // was only ever captured on a 200 -- so a run where every poll failed blamed
  // the fixture's scan-error setup for what was really a transport failure.
  let diagnosis;
  if (!sawOK) {
    diagnosis = `the report page never returned 200 (last status ${lastStatus}), so no body was ever inspected`;
  } else if (last.includes('platform-backdrop-duplicates-unavailable-notice')) {
    diagnosis = 'the page is still PENDING -- the background sweep had not landed';
  } else {
    diagnosis = 'the page rendered a report WITHOUT the partial-scan notice, so ScanErrors likely never went above 0';
  }
  throw new Error(
    `seed: the platform backdrop duplicates partial-scan notice never rendered within 90s: ${diagnosis}. `
    + 'The spec cannot verify a surface that never rendered.',
  );
}

/**
 * seedPlatformBackdropScanError forces view.ScanErrors > 0 on
 * /reports/platform-backdrop-duplicates by mapping a real artist to a real
 * connection backed by a fake Emby server that 404s every artist lookup.
 *
 * Returns a teardown function that stops the fake server. Callers MUST call
 * it (in afterAll or similar) so the loopback listener does not leak across
 * the test run.
 */
// `wait`: see seedBackdropDuplicates. False lets a caller create this dataset
// before anything polls a cache-backed report, so the single sweep the run gets
// observes both fixtures rather than only whichever was seeded first.
export async function seedPlatformBackdropScanError(request, { wait = true } = {}) {
  const port = process.env.SW_PORT || new URL(BASE_URL).port || 'default';
  const dir = path.join(os.tmpdir(), `sw-a11y-platform-backdrop-${port}`);
  const artistDir = path.join(dir, FIXTURE_ARTIST);
  fs.mkdirSync(artistDir, { recursive: true });
  fs.writeFileSync(path.join(artistDir, 'artist.nfo'), `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<artist>
  <name>${FIXTURE_ARTIST}</name>
  <genre>Fixture</genre>
</artist>
`);

  const fake = await startFakeEmby();

  // Everything past this point can throw (a failed fetch, a bad status, a
  // notice that never renders). Any of those must still close the loopback
  // listener before propagating -- otherwise the caller never gets `fake.close`
  // back and the listener leaks for the rest of the test run.
  try {
    await ensureLibrary(request, FIXTURE_LIBRARY_NAME, dir);
    await runScan(request);
    const artistIDs = await artistIdsByName(request, [FIXTURE_ARTIST]);
    const artistID = artistIDs.get(FIXTURE_ARTIST);
    if (!artistID) {
      throw new Error(`seed: artist "${FIXTURE_ARTIST}" did not appear after scan`);
    }
    const connectionID = await ensureConnection(request, fake.port);
    const mapResp = await apiFetch(
      request, 'PUT', `/api/v1/artists/${artistID}/platform-ids/${connectionID}`,
      { platform_artist_id: FIXTURE_PLATFORM_ARTIST_ID },
    );
    if (!mapResp.ok()) {
      throw new Error(`seed: mapping platform id failed: ${mapResp.status()} ${await mapResp.text()}`);
    }

    if (wait) await waitForPlatformBackdropNotice(request);
  } catch (err) {
    await fake.close();
    throw err;
  }

  return fake.close;
}
