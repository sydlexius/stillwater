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
//      The page is served from a CACHE populated by a background sweep, not
//      computed on render (#3092). The first GET finds a cold cache, renders
//      the pending notice and TRIGGERS the sweep; the report appears on a
//      later poll. Returning after one GET would hand the spec the pending
//      state, whose partial-scan notice does not exist, and every assertion
//      would fail for a reason unrelated to the code under test. Same shape as
//      seed-backdrop-duplicates.js's wait, for the same reason.
//
// SEEDED FROM global-setup.js, NOT FROM A SPEC'S beforeAll.
//
// That cache is process-wide and ONE refresh computes both this report's half
// and the sibling local report's, after which the lazy path is locked out for
// 15 minutes (retryCooldown, internal/dupimages/cache.go) -- stamped when the
// sweep STARTS and never cleared on success. So the run gets ONE sweep, and
// whichever fixture does not exist when it starts is cached as empty behind
// that cooldown. A per-spec beforeAll is therefore unwinnable rather than
// merely fragile: whichever of the two fixtures is seeded second loses, in
// EITHER ordering. global-setup.js holds the full derivation; the ordering it
// describes is a CONSTRAINT, not a preference.
//
// apiFetch/ensureLibrary/runScan/artistIdsByName are shared with the other
// a11y seeders via ./api.js (#3058/#3059); startFakeEmby stays local since it
// is single-use, exactly like startMockMusicBrainz in seed-nfo-mbid.js.

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
//
// Exported so a spec can stand up a SECOND fake on a DIFFERENT port and
// exercise what a Playwright retry does. Port 0 is the whole point: the
// changing url is what defeated handleCreateConnection's type+url dedup.
export function startFakeEmby() {
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
      // unref so this listener can never keep the Playwright coordinator alive
      // on its own. When seeded from globalSetup (#3092) it must stay reachable
      // for the WHOLE run, because a later sweep re-queries it, so nothing
      // closes it until teardown -- without unref a crashed run would hang
      // instead of exiting. A spec that DOES own its listener still closes it
      // explicitly; unref only removes the process-lifetime hold.
      server.unref();
      resolve({ server, port, close: () => new Promise(r => server.close(r)) });
    });
  });
}

/**
 * FIXTURE_CONNECTION_NAME is exported so a spec can assert on this fixture's
 * debris by the same identifier the fixture cleans up by.
 */
export { FIXTURE_CONNECTION_NAME };

/**
 * fixtureConnectionIDs returns the id of every connection carrying this
 * fixture's NAME. The name is the only stable identifier this fixture has
 * across attempts -- see deleteFixtureConnections for why the url is not.
 */
export async function fixtureConnectionIDs(request, name = FIXTURE_CONNECTION_NAME) {
  const resp = await request.fetch(`${BASE_URL}/api/v1/connections`);
  if (!resp.ok()) {
    throw new Error(`seed: listing connections failed: ${resp.status()} ${await resp.text()}`);
  }
  const body = await resp.json();
  const list = Array.isArray(body) ? body : (body.connections || []);
  return list.filter(c => c.name === name).map(c => c.id);
}

/**
 * deleteFixtureConnections removes every connection this fixture created,
 * identified by name, and returns how many it deleted.
 *
 * WHY BY NAME, AND WHY THIS EXISTS AT ALL
 *
 * startFakeEmby binds port 0, so EVERY call gets a different OS-assigned
 * port and therefore a different connection url. handleCreateConnection
 * dedupes on type+url (GetByTypeAndURL), so that dedup can never match a
 * previous attempt's row -- each attempt creates a BRAND NEW connection
 * instead of updating the last one.
 *
 * Playwright retries a failed beforeAll hook (retries: 2 in
 * playwright.config.js), and the whole a11y tier runs against ONE shared
 * ephemeral database. So a fixture that only closed its loopback listener
 * left one live Connection row per attempt behind, each pointing at a now-dead
 * server. Those rows answer "nfo check unavailable", and the app's NFO write
 * guard is fail-closed on an indeterminate check -- which is how three dead
 * fixture connections made the unrelated nfo-mbid spec fail with a 409
 * nfo_write_blocked in the same CI run. Fixture debris, not a flake.
 *
 * Deleting by NAME rather than by the id this run happens to hold is the
 * point: it also reaps rows a PREVIOUS attempt (in a killed worker) left
 * behind, which an id-scoped delete cannot see.
 */
export async function deleteFixtureConnections(request, name = FIXTURE_CONNECTION_NAME) {
  const ids = await fixtureConnectionIDs(request, name);
  for (const id of ids) {
    const resp = await apiFetch(request, 'DELETE', `/api/v1/connections/${id}`);
    // 404 means something else already removed it -- the desired end state,
    // so it is not a failure. Anything else is, and must be loud: a silently
    // swallowed delete failure reproduces the exact leak this exists to stop.
    if (!resp.ok() && resp.status() !== 404) {
      throw new Error(
        `seed: deleting fixture connection ${id} failed: ${resp.status()} ${await resp.text()}`,
      );
    }
  }
  return ids.length;
}

export async function ensureConnection(request, port, name = FIXTURE_CONNECTION_NAME) {
  // Reap any row from a previous attempt FIRST. handleCreateConnection's
  // type+url dedup cannot do it for us (the port, and so the url, differs on
  // every call), so without this a retry stacks a second connection on top of
  // the first. See deleteFixtureConnections.
  //
  // The `name` parameter exists so the hygiene spec can exercise this exact
  // reuse path under its OWN name, and so can never delete a row the real
  // fixture is depending on.
  await deleteFixtureConnections(request, name);

  const resp = await apiFetch(request, 'POST', '/api/v1/connections', {
    name,
    type: 'emby',
    url: `http://127.0.0.1:${port}`,
    api_key: 'a11y-fixture-key',
    enabled: true,
    skip_test: false,
  });
  if (resp.ok()) return (await resp.json()).id;
  throw new Error(`seed: creating fixture connection failed: ${resp.status()} ${await resp.text()}`);
}

// waitForPlatformBackdropNotice polls the live report page for the partial-scan notice
// marker. Polling is REQUIRED, not defensive: the page renders from a cached
// sweep (#3092), so the first GET returns the pending notice and merely kicks
// the sweep that produces what this fixture is waiting for.
//
// 90s to match seed-backdrop-duplicates.js: the sweep is a per-artist,
// per-connection round trip, and the lazy trigger only fires on a GET that
// finds the cache cold -- so the budget has to cover a sweep, not a render.
// Exported (#3092) so global-setup.js can seed this dataset and the sibling
// local-report dataset BEFORE polling either. See seedPlatformBackdropScanError's
// `wait` option and global-setup.js for why that ordering is a constraint.
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
 * Returns a teardown function that stops the fake server AND deletes the
 * Connection row it created. A caller that OWNS the fixture for a bounded
 * scope MUST call it (in afterAll or similar): closing the listener alone
 * leaves a live connection pointing at a dead server, which every other spec in
 * the run then has to survive. The teardown takes an optional
 * APIRequestContext so a caller can hand it the one its own afterAll hook was
 * given rather than relying on the seeding hook's.
 *
 * THE ONE CALLER THAT DOES NOT: global-setup.js seeds this fixture for the
 * WHOLE run and deliberately never invokes the teardown, because a later sweep
 * re-queries the fake Emby and the connection row must stay mapped for the
 * report to keep rendering the notice. Nothing leaks past the run: the listener
 * is unref'd (see startFakeEmby) and the database is ephemeral per invocation.
 */
// `wait`: see seedBackdropDuplicates. False lets a caller create this dataset
// before anything polls a cache-backed report, so the single sweep the run gets
// observes BOTH fixtures rather than only whichever was seeded first.
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
    // Order matters and both must happen. The Connection row is the piece
    // that outlives this worker and poisons other specs, so it gets cleaned
    // up even if closing the listener were to hang; the listener close is in
    // `finally` so a cleanup failure cannot leak it either.
    try {
      await deleteFixtureConnections(request);
    } catch (cleanupErr) {
      // Never mask the original failure -- that is the one a maintainer needs
      // to read -- but never swallow the cleanup failure silently either.
      // Guarded because `err` is only an Error by convention: a non-Error
      // throw would make this line itself throw, replacing the real failure
      // with a TypeError from the cleanup handler.
      if (err instanceof Error) {
        err.message += ` (fixture connection cleanup ALSO failed: ${cleanupErr.message})`;
      } else {
        console.error('seed: fixture connection cleanup failed:', cleanupErr);
      }
    } finally {
      await fake.close();
    }
    throw err;
  }

  return async (teardownRequest = request) => {
    try {
      await deleteFixtureConnections(teardownRequest);
    } finally {
      await fake.close();
    }
  };
}
