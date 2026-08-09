// seed-nfo-mbid.js - fixture for the rule-written MusicBrainz ID a11y spec
// (#2809, the reports-workspace pane half of the issue).
//
// WHY THIS EXISTS
//
// `make test-a11y` boots the server against a BRAND NEW EMPTY DATABASE
// (SW_DB_PATH=$TMPDIR/stillwater-a11y-$$.db in the Makefile target). The
// nfo-has-mbid pane therefore renders its empty state, and the spec cannot
// reach a real row -- the current-ID cell, the "none recorded" case, or the
// caveat band rendered above real data. The gap is the absent DATA, so the
// fix is a fixture, matching the conclusion seed-blast-radius.js and
// seed-backdrop-duplicates.js already reached for the same harness property.
//
// WHAT A "RULE-WRITTEN MUSICBRAINZ ID" ROW ACTUALLY IS
//
// The nfo_has_mbid rule (internal/rule/fixers.go, MetadataFixer.fixMBID)
// fires only on an artist with NO MusicBrainz ID, searches providers by name,
// and adopts the top hit only when it clears three gates: a minimum provider
// score, a minimum name-similarity, and an ambiguity margin over the runner-up
// (internal/artist/mbidcandidate.go). A fourth gate -- album-evidence overlap
// -- fails OPEN when there is no local album source or no release-group
// fetcher wired for the candidate (internal/rule/album_gate.go), which an
// artist directory holding no album subfolders satisfies trivially.
//
// So seeding a real row (not a synthetic metadata_changes INSERT -- there is
// no artist-create endpoint and a hand-written row could encode a state the
// rule engine can never itself produce) means:
//
//   1. A scanned artist with no MusicBrainz ID in its NFO and no album
//      subdirectories (EvidenceNone -> the album gate allows).
//   2. A MusicBrainz PROVIDER MIRROR pointed at a tiny local HTTP server this
//      fixture starts, returning a single confident, unambiguous search hit
//      for that artist's name. There is no network access to the real
//      musicbrainz.org from the CI/local harness, and there does not need to
//      be: PUT /api/v1/providers/musicbrainz/mirror is the exact operator
//      path for a self-hosted mirror (docs: administering-connections.md),
//      and the SSRF guard's newMirrorClient explicitly allowlists a
//      configured mirror's own host for this reason.
//   3. Running the rule via POST /api/v1/rules/nfo_has_mbid/run, which is the
//      real MetadataFixer.fixMBID code path -- not a direct DB write -- so
//      the fixture cannot encode a state the product does not produce.
//   4. Restoring the default MusicBrainz base URL afterward, so a later spec
//      file (or a re-run against the same server) is not left pointed at a
//      now-dead mock server.
//
// A second artist with a MusicBrainz ID ALREADY SET is seeded alongside the
// first and never reaches this rule (checkNFOHasMBID short-circuits when
// MusicBrainzID != ""), so the mock server can assert its own precondition:
// exactly one row landed, not two, proving the seeded fixture did not
// over-fire.

import http from 'node:http';
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';

const BASE_URL = process.env.SW_TEST_URL
  || `http://127.0.0.1:${process.env.SW_PORT || '1973'}`;

const FIXTURE_LIBRARY_NAME = 'a11y nfo-mbid fixture';

// The artist the rule fix WILL touch: no MusicBrainz ID in its NFO, no album
// subdirectories (EvidenceNone). Named distinctly from every other a11y
// fixture artist so a cross-file collision cannot happen.
const TARGET_ARTIST = 'Quartzite Fixture';
// A confusable near-duplicate NAME the mock server also returns as a lower-
// scored hit, so BestMBIDCandidates has a real runner-up to be measured
// against (rather than an ambiguity margin that trivially passes because
// nothing else was offered).
const RUNNER_UP_NAME = 'Quartzite Fixture Tribute Band';
// The MusicBrainz ID the mock server hands back for TARGET_ARTIST and that
// the rule fix is expected to adopt.
const TARGET_MBID = 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee';

// A second, UNAFFECTED artist: already carries a MusicBrainz ID, so
// checkNFOHasMBID never flags it and the rule fixer never runs on it. Present
// so the "exactly one row" precondition check below is not vacuous -- without
// a control artist a fixture that (bug) fired on everything scanned would
// look identical to a correct one.
const CONTROL_ARTIST = 'Malachite Fixture';
const CONTROL_MBID = '11111111-2222-3333-4444-555555555555';

/**
 * apiFetch issues an authenticated, CSRF-bearing request against the
 * ephemeral server, mirroring seed-blast-radius.js's helper of the same name.
 */
async function apiFetch(request, method, url, body) {
  const state = await request.storageState();
  const csrf = (state.cookies.find(c => c.name === 'csrf_token') || {}).value || '';
  const opts = {
    headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf },
  };
  if (body !== undefined) opts.data = JSON.stringify(body);
  return request.fetch(`${BASE_URL}${url}`, { method, ...opts });
}

/**
 * startMockMusicBrainz starts a tiny local HTTP server implementing exactly
 * the one endpoint the rule fixer's search path calls:
 * GET /ws/2/artist?query=...&fmt=json&limit=25
 *
 * Always answers with TWO hits for TARGET_ARTIST's query (one high-confidence
 * exact-name match, one lower-scored near-duplicate acting as the runner-up)
 * and ZERO hits for anything else, so the fixture cannot accidentally assign
 * an ID to an artist it was not built for.
 *
 * Returns { server, port, close }.
 */
function startMockMusicBrainz() {
  const server = http.createServer((req, res) => {
    const u = new URL(req.url, 'http://127.0.0.1');
    if (!u.pathname.endsWith('/artist')) {
      res.writeHead(404).end();
      return;
    }
    const query = u.searchParams.get('query') || '';
    const artists = query.includes(TARGET_ARTIST)
      ? [
        {
          id: TARGET_MBID,
          name: TARGET_ARTIST,
          'sort-name': TARGET_ARTIST,
          type: 'Group',
          score: 100,
          country: 'US',
          'life-span': {},
          aliases: [],
          tags: [],
          genres: [],
        },
        {
          id: 'ffffffff-1111-2222-3333-444444444444',
          name: RUNNER_UP_NAME,
          'sort-name': RUNNER_UP_NAME,
          type: 'Group',
          // Scored well below TARGET_MBID's 100 and outside the ambiguity
          // margin (internal/artist/mbidcandidate.go: MBIDAmbiguityMargin),
          // so BestMBIDCandidates picks TARGET_MBID unambiguously while this
          // entry still exercises the runner-up code path.
          score: 60,
          country: 'US',
          'life-span': {},
          aliases: [],
          tags: [],
          genres: [],
        },
      ]
      : [];
    const body = JSON.stringify({ created: new Date().toISOString(), count: artists.length, offset: 0, artists });
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(body);
  });
  return new Promise((resolve, reject) => {
    server.on('error', reject);
    server.listen(0, '127.0.0.1', () => {
      const { port } = server.address();
      resolve({ server, port, close: () => new Promise(r => server.close(r)) });
    });
  });
}

/** ensureLibrary mirrors seed-blast-radius.js's helper of the same name. */
async function ensureLibrary(request, dir) {
  const resp = await apiFetch(request, 'POST', '/api/v1/libraries', {
    name: FIXTURE_LIBRARY_NAME, path: dir, type: 'regular',
  });
  if (resp.ok()) return (await resp.json()).id;
  if (resp.status() === 409) {
    const list = await request.fetch(`${BASE_URL}/api/v1/libraries`);
    if (list.ok()) {
      const body = await list.json();
      const libs = Array.isArray(body) ? body : (body.libraries || []);
      const existing = libs.find(l => l.name === FIXTURE_LIBRARY_NAME);
      if (existing && existing.path === dir) return existing.id;
      if (existing) {
        throw new Error(
          `seed: fixture library exists but points at ${existing.path}, not ${dir}. `
          + 'Delete it (or use a fresh database) before re-seeding.',
        );
      }
    }
  }
  throw new Error(`seed: creating fixture library failed: ${resp.status()} ${await resp.text()}`);
}

/** runScan mirrors seed-blast-radius.js's helper of the same name. */
async function runScan(request) {
  const resp = await apiFetch(request, 'POST', '/api/v1/scanner/run');
  if (!resp.ok()) {
    throw new Error(`seed: scanner run failed: ${resp.status()} ${await resp.text()}`);
  }
  const deadline = Date.now() + 60_000;
  while (Date.now() < deadline) {
    const st = await request.fetch(`${BASE_URL}/api/v1/scanner/status`);
    if (st.ok()) {
      const body = await st.json();
      if (body.status === 'completed' || body.status === 'idle') return;
    }
    await new Promise(r => setTimeout(r, 500));
  }
  throw new Error('seed: scan did not complete within 60s');
}

/** artistIdsByName mirrors seed-blast-radius.js's helper of the same name. */
async function artistIdsByName(request, names) {
  const resp = await request.fetch(`${BASE_URL}/api/v1/artists?page_size=500`);
  if (!resp.ok()) {
    throw new Error(`seed: listing artists failed: ${resp.status()}`);
  }
  const body = await resp.json();
  const list = Array.isArray(body) ? body : (body.artists || []);
  const out = new Map();
  for (const a of list) {
    if (names.includes(a.name)) out.set(a.name, a.id);
  }
  return out;
}

/** waitForRuleRun polls the shared single-rule/run-all status slot. */
async function waitForRuleRun(request) {
  const deadline = Date.now() + 60_000;
  while (Date.now() < deadline) {
    const resp = await request.fetch(`${BASE_URL}/api/v1/rules/run-all/status`);
    if (resp.ok()) {
      const body = await resp.json();
      if (body.status === 'idle' || body.status === 'completed' || body.status === 'failed') {
        return body;
      }
    }
    await new Promise(r => setTimeout(r, 500));
  }
  throw new Error('seed: nfo_has_mbid rule run did not finish within 60s');
}

/**
 * seedNFOMBID builds a fixture the nfo-has-mbid pane can render: one artist
 * the rule fix assigns a MusicBrainz ID to, and one control artist it never
 * touches.
 *
 * Returns { writes, artists } as the REPORT sees them, read back from the
 * report API rather than assumed from the rule run's own summary -- the
 * seeder must prove the row is visible to the QUERY this report's pane and
 * JSON/CSV surfaces share, not merely that the rule fixer claimed success.
 */
export async function seedNFOMBID(request) {
  const port = process.env.SW_PORT || new URL(BASE_URL).port || 'default';
  const dir = path.join(os.tmpdir(), `sw-a11y-nfo-mbid-fixture-${port}`);
  fs.mkdirSync(dir, { recursive: true });
  // No album subdirectories under either artist folder: the scanner's own
  // presence check is what makes checkNFOHasMBID fire (an artist directory
  // must exist to be scanned at all), and the ABSENCE of album folders is
  // what gives the album-evidence gate EvidenceNone -- allow -- rather than
  // EvidenceFound, which would require a real release-group fetch this
  // fixture's mock server does not implement.
  fs.mkdirSync(path.join(dir, TARGET_ARTIST), { recursive: true });
  fs.mkdirSync(path.join(dir, CONTROL_ARTIST), { recursive: true });

  await ensureLibrary(request, dir);
  await runScan(request);

  const ids = await artistIdsByName(request, [TARGET_ARTIST, CONTROL_ARTIST]);
  const missing = [TARGET_ARTIST, CONTROL_ARTIST].filter(n => !ids.has(n));
  if (missing.length) {
    throw new Error(`seed: scan did not create fixture artists: ${missing.join(', ')}`);
  }

  // Give the control artist a MusicBrainz ID directly through the field-edit
  // API BEFORE running the rule, so checkNFOHasMBID never flags it and the
  // rule fixer never has a reason to touch it.
  const csrfState = await request.storageState();
  const csrf = (csrfState.cookies.find(c => c.name === 'csrf_token') || {}).value || '';
  const controlResp = await request.fetch(
    `${BASE_URL}/api/v1/artists/${ids.get(CONTROL_ARTIST)}/fields/musicbrainz_id`,
    {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf },
      data: JSON.stringify({ value: CONTROL_MBID }),
    },
  );
  if (!controlResp.ok()) {
    throw new Error(`seed: setting control artist's MusicBrainz ID failed: ${controlResp.status()} ${await controlResp.text()}`);
  }

  const mock = await startMockMusicBrainz();
  let originalMirror = null;
  try {
    // Read the CURRENT mirror config before overwriting it, so it can be
    // restored afterward -- this fixture must not leave the server pointed
    // at a mock endpoint that stops existing the moment this function
    // returns, which would break any LATER spec file or manual reuse of the
    // same server. GET /api/v1/providers/{name}/config only returns field
    // verbosity, not the mirror -- the mirror lives on the list endpoint's
    // per-provider MirrorConfig (internal/provider/settings.go).
    const listResp = await request.fetch(`${BASE_URL}/api/v1/providers`);
    if (listResp.ok()) {
      const body = await listResp.json();
      const providers = Array.isArray(body.providers) ? body.providers : [];
      const mb = providers.find(p => p.name === 'musicbrainz');
      if (mb && mb.mirror) {
        originalMirror = mb.mirror;
      }
    }

    const mirrorResp = await apiFetch(request, 'PUT', '/api/v1/providers/musicbrainz/mirror', {
      base_url: `http://127.0.0.1:${mock.port}/ws/2`,
      rate_limit: 50,
    });
    if (!mirrorResp.ok()) {
      throw new Error(`seed: setting MusicBrainz mirror failed: ${mirrorResp.status()} ${await mirrorResp.text()}`);
    }

    const runResp = await apiFetch(request, 'POST', '/api/v1/rules/nfo_has_mbid/run');
    if (!runResp.ok()) {
      throw new Error(`seed: running nfo_has_mbid rule failed: ${runResp.status()} ${await runResp.text()}`);
    }
    const runStatus = await waitForRuleRun(request);
    if (runStatus.status === 'failed') {
      throw new Error(`seed: nfo_has_mbid rule run reported failure: ${JSON.stringify(runStatus)}`);
    }
  } finally {
    // Restore the default (or whatever was configured before this ran) so a
    // later spec file, or a developer re-running against a persistent
    // server, is not left pointed at a mock server this process is about to
    // tear down.
    if (originalMirror && originalMirror.base_url && originalMirror.base_url !== `http://127.0.0.1:${mock.port}/ws/2`) {
      await apiFetch(request, 'PUT', '/api/v1/providers/musicbrainz/mirror', {
        base_url: originalMirror.base_url,
        rate_limit: originalMirror.rate_limit || 10,
      });
    } else {
      await apiFetch(request, 'DELETE', '/api/v1/providers/musicbrainz/mirror');
    }
    await mock.close();
  }

  // Verify against the REPORT, which is the thing under test -- not the rule
  // run's own summary, which only claims a fix succeeded, not that the query
  // this pane shares with the JSON/CSV surfaces can see it.
  const resp = await request.fetch(`${BASE_URL}/api/v1/reports/nfo-has-mbid?page_size=500`);
  if (!resp.ok()) {
    throw new Error(`seed: reading back the nfo-has-mbid report failed: ${resp.status()}`);
  }
  const report = await resp.json();
  const rows = report.rows || [];
  const counts = report.counts || {};

  if (rows.length < 1) {
    throw new Error(
      'seed: the nfo_has_mbid rule run completed but produced no rows in the '
      + 'report. Either the mock MusicBrainz mirror was not reached, or the '
      + 'confidence/album gates declined the candidate. The fixture is not usable.',
    );
  }
  const targetRow = rows.find(r => r.artist_name === TARGET_ARTIST);
  if (!targetRow) {
    throw new Error(`seed: no report row for ${TARGET_ARTIST}; the seeded rule run did not land where expected`);
  }
  if (targetRow.current_musicbrainz_id !== TARGET_MBID) {
    throw new Error(
      `seed: ${TARGET_ARTIST}'s current MusicBrainz ID is ${targetRow.current_musicbrainz_id}, `
      + `expected the seeded mock ID ${TARGET_MBID}`,
    );
  }
  // The control artist must NEVER appear -- it already had an ID before the
  // rule ran, so checkNFOHasMBID must not have flagged it. A fixture that
  // fires the rule fixer regardless of precondition would pass every other
  // assertion here while silently proving nothing about correct scoping.
  if (rows.some(r => r.artist_name === CONTROL_ARTIST)) {
    throw new Error(
      `seed: the control artist ${CONTROL_ARTIST} appears in the report; it already `
      + 'carried a MusicBrainz ID before the rule ran and must never have been touched',
    );
  }

  return { rows: rows.length, writes: counts.writes || rows.length, artists: counts.artists || 1 };
}
