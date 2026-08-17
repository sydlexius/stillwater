// seed-blast-radius.js - fixture for the blast-radius a11y spec (#2750).
//
// WHY THIS EXISTS
//
// `make test-a11y` boots the server against a BRAND NEW EMPTY DATABASE
// (SW_DB_PATH=$TMPDIR/stillwater-a11y-$$.db in the Makefile target). The
// blast-radius pane therefore renders its empty state, and three of the
// spec's tests cannot reach their surfaces at all: the restore confirm
// dialog needs a row to act on, and the pager needs more rows than one
// page holds.
//
// Those tests do not pass vacuously in that state -- they throw, saying the
// surface is UNVERIFIED. That refusal is correct and must stay. The gap is
// the absent DATA, so the fix is a fixture, never a softened assertion.
//
// The emptiness is structural to the harness, not a property of one
// machine: seeding a long-lived UAT database would go green locally and
// still fail in CI. This seeder runs inside the harness, against whatever
// server the run just started.
//
// WHAT A "BLAST-RADIUS ROW" ACTUALLY IS
//
// The report is not a plain SELECT over metadata_changes. It ranks every
// row within its (artist_id, field) partition, newest first, and only rank
// 1 can be damage (see blastRadiusRankedCTE in internal/artist/
// sqlite_history.go). A row qualifies only when ALL of these hold:
//
//   - it is the NEWEST change for its (artist_id, field) pair
//   - old_value != ''          (the operator had a value)
//   - old_value != new_value   (something replaced it)
//   - source != 'revert'       (a recovery is not damage)
//
// So seeding takes two writes per field: one to establish the operator's
// value, a second to destroy it. A single write leaves old_value empty and
// is correctly NOT reported.
//
// THE ONE-SECOND TRAP -- the reason this file is phased
//
// created_at has SECOND granularity and the ranking tiebreak is
// `mc.id DESC`, which is a random UUID. Two writes to the same
// (artist_id, field) inside one second therefore rank in RANDOM order. When
// the establish-write wins, old_value is '' and the report shows nothing.
//
// Measured, not theorized: writing both values back-to-back produced a
// damage row in the database and `counts: {automated: 0, unknown: 0,
// total: 0}` from the report API. A seeder that ignored this would produce
// a test that passes about half the time -- a flake indistinguishable from
// a real regression, in a spec that exists to refuse false greens.
//
// Hence seedDamage runs in PHASES: every original value first, then ONE
// wait past the second boundary, then every overwrite. The cost is one
// pause for the whole fixture rather than one per row.
//
// ATTRIBUTION: BOTH BUCKETS ARE SEEDED ON PURPOSE
//
// A field edit through the API records source="manual", which the report
// classifies as attribution "unknown" (Stillwater cannot tell an operator
// edit from pre-2026-07-19 scan damage). Rewriting an artist.nfo and
// rescanning records source="scan" -> attribution "automated", because the
// scanner tags its own context (scanner.go, ContextWithSource(ctx, "scan")).
//
// Seeding both means the pane renders both badge styles and both count
// buckets, so an a11y scan sees the real surface. The automated rows come
// from running the ACTUAL SCANNER over a real NFO file, never from an
// INSERT: a hand-written row could encode a state the product cannot
// produce, and the fixture would then verify a page that never ships.

import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import {
  BASE_URL, apiFetch, ensureLibrary, runScan, artistIdsByName,
} from './api.js';

// More than one page of rows, so the pager renders. page_size is clamped
// server-side to [10, 500] by getUserPageSize, so 10 is the smallest page
// the server will honor and 11 rows is the smallest set that pages.
// Deliberately not "just enough": DAMAGED_ARTISTS below yields comfortably
// more, so a future clamp change does not silently remove the second page.
const MIN_ROWS_FOR_PAGER = 11;

// Fields carrying a plain string. Every one is in trackableFields
// (internal/artist/service.go), which is what makes the write produce a
// history row at all -- an untracked field writes nothing and would seed
// an invisible fixture.
const STRING_FIELDS = ['biography', 'origin', 'years_active', 'formed'];

// Artists seeded via the API (source="manual" -> attribution "unknown").
// Names are obviously synthetic so a fixture row is never mistaken for
// real library data, and carry no resemblance to any real artist.
const DAMAGED_ARTISTS = [
  'Aurora Fixture', 'Basalt Fixture', 'Cinder Fixture', 'Dolerite Fixture',
];

// The artist seeded through a real scan (source="scan" -> "automated").
const SCANNED_ARTIST = 'Gabbro Fixture';

/**
 * waitPastSecondBoundary sleeps until created_at is guaranteed to differ.
 *
 * Two full seconds, not one: created_at is truncated to whole seconds, so a
 * write at t=0.99s and one 1.0s later can still land in adjacent-but-equal
 * truncated values depending on where in the second the first write fell.
 * Two seconds makes the ordering unambiguous regardless of phase.
 */
function waitPastSecondBoundary() {
  return new Promise(resolve => setTimeout(resolve, 2000));
}

/**
 * ensureLibrary points a library at dir and returns its id.
 *
 * The bootstrap-seeded "Default" library points at /music, which does not
 * exist in CI. That is harmless -- the scanner logs an error for an
 * unreadable target and continues to the next one (scanner.go, the
 * os.ReadDir branch) -- but it is why this adds its OWN library rather than
 * repointing Default. On a developer machine /music may genuinely exist and
 * hold the real library; repointing or scanning it would drag real artists
 * into the fixture, and every write below targets fixture artists by name
 * so that real data is never touched.
 */
// The library name is UNIQUE in the schema, so re-seeding a server that
// already carries the fixture gets a 409. That happens whenever the suite is
// re-run against a persistent server (the normal `make test-a11y` path builds
// a fresh database each time, but a developer pointing SW_TEST_URL at a live
// instance would otherwise hit it on the second run).
const FIXTURE_LIBRARY_NAME = 'a11y blast-radius fixture';

/** setField writes one field, failing loudly rather than seeding silently. */
async function setField(request, artistId, field, value) {
  const resp = await apiFetch(
    request, 'PATCH', `/api/v1/artists/${artistId}/fields/${field}`, { value },
  );
  if (!resp.ok()) {
    throw new Error(
      `seed: writing ${field} on ${artistId} failed: ${resp.status()} ${await resp.text()}`,
    );
  }
}

/**
 * seedBlastRadius builds a fixture the blast-radius pane can render.
 *
 * Returns { rows, automated, unknown } as the REPORT sees them, read back
 * from the report API rather than counted locally -- the seeder must prove
 * the rows are visible to the query, not merely that writes returned 200.
 * Those are different claims, and only the second one is what the spec
 * needs.
 */
export async function seedBlastRadius(request) {
  // DETERMINISTIC path, not mkdtempSync. The library row is keyed by a UNIQUE
  // name, so a re-run against a server that already carries the fixture has to
  // reuse that row -- and reuse is only safe if the path still matches. A
  // random temp dir per invocation guarantees it never does, which made the
  // 409-reuse branch below unreachable and left it throwing instead. Caught in
  // review; the idempotency it was added for did not actually work.
  //
  // Scoped to the port so two servers on one machine cannot share a fixture
  // directory and scan each other's artists.
  const port = process.env.SW_PORT || new URL(BASE_URL).port || 'default';
  const dir = path.join(os.tmpdir(), `sw-a11y-fixture-${port}`);
  fs.mkdirSync(dir, { recursive: true });

  // --- the scanned artist needs its directory + NFO before the first scan.
  const scannedDir = path.join(dir, SCANNED_ARTIST);
  fs.mkdirSync(scannedDir, { recursive: true });
  const nfo = bio => `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<artist>
  <name>${SCANNED_ARTIST}</name>
  <biography>${bio}</biography>
  <genre>Ambient</genre>
</artist>
`;
  fs.writeFileSync(path.join(scannedDir, 'artist.nfo'), nfo('Operator biography, written into the NFO by hand.'));

  // The API-seeded artists exist as bare directories; the scan creates a row
  // for each. There is no artist-create endpoint, so a scan is the only way
  // to bring an artist into being.
  for (const name of DAMAGED_ARTISTS) {
    fs.mkdirSync(path.join(dir, name), { recursive: true });
  }

  await ensureLibrary(request, FIXTURE_LIBRARY_NAME, dir);
  await runScan(request);

  const ids = await artistIdsByName(request, [...DAMAGED_ARTISTS, SCANNED_ARTIST]);
  const missing = [...DAMAGED_ARTISTS, SCANNED_ARTIST].filter(n => !ids.has(n));
  if (missing.length) {
    throw new Error(`seed: scan did not create fixture artists: ${missing.join(', ')}`);
  }

  // --- PHASE 1: establish the operator's values.
  // Every write here is a first-ever population (old_value ''), which is
  // correctly NOT damage. Phase 2 is what destroys them.
  for (const name of DAMAGED_ARTISTS) {
    for (const field of STRING_FIELDS) {
      await setField(request, ids.get(name), field, `Operator value for ${name} ${field}.`);
    }
    await setField(request, ids.get(name), 'genres', ['Operator Genre']);
  }

  // --- the boundary. One wait for the whole fixture. See the comment on
  // waitPastSecondBoundary for why this cannot be skipped.
  await waitPastSecondBoundary();

  // --- PHASE 2: destroy them. Both damage classes are represented:
  // "replaced" (non-empty -> different non-empty) and "blanked"
  // (non-empty -> empty), because the pane renders a different badge for
  // each and an a11y scan should see both.
  for (const name of DAMAGED_ARTISTS) {
    for (const field of STRING_FIELDS) {
      await setField(request, ids.get(name), field, `Automated writer replaced ${field}.`);
    }
    // Blanked: an empty array clears the field.
    await setField(request, ids.get(name), 'genres', []);
  }

  // --- the automated bucket: rewrite the NFO and rescan, so the SCANNER
  // records the change with source="scan". Uses the real scan path rather
  // than a synthetic row.
  fs.writeFileSync(path.join(scannedDir, 'artist.nfo'), nfo('Overwritten by an automated refresh.'));
  await runScan(request);

  // --- verify against the REPORT, which is the thing under test.
  const resp = await request.fetch(`${BASE_URL}/api/v1/reports/blast-radius?page_size=500`);
  if (!resp.ok()) {
    throw new Error(`seed: reading back the blast-radius report failed: ${resp.status()}`);
  }
  const report = await resp.json();
  const counts = report.counts || {};
  const rows = (report.rows || []).length;

  if (rows < MIN_ROWS_FOR_PAGER) {
    throw new Error(
      `seed: produced ${rows} blast-radius rows, need at least ${MIN_ROWS_FOR_PAGER} `
      + 'for the pager to render. The fixture is not usable and the spec would '
      + 'report an absent pager as a keyboard defect.',
    );
  }
  if (!counts.automated) {
    throw new Error(
      'seed: no rows landed in the AUTOMATED attribution bucket. The scan-sourced '
      + 'fixture did not take, so the pane would render only the unknown bucket.',
    );
  }
  if (!counts.unknown) {
    throw new Error(
      'seed: no rows landed in the UNKNOWN attribution bucket. The API-sourced '
      + 'fixture did not take.',
    );
  }

  return { rows, automated: counts.automated, unknown: counts.unknown };
}
