// seed-unfixable-finding.js - fixture for the "Fix unavailable" badge spec (#3469).
//
// `make test-a11y` boots a brand new empty database and an empty library, so
// no finding exists and the badge (rendered only for an open finding with no
// automatic fix) is absent everywhere. The gap is the absent DATA, so this
// seeder builds it inside the harness instead of the spec skipping.
//
// The finding is artist_id_mismatch: it is raised by the engine, is never
// fixable (checkArtistIDMismatch sets Fixable false), and fires when the
// artist folder name and BOTH the NFO name and sort name disagree (the check
// also accepts a folder that matches the sort name, so the sort name is set). The rule ships disabled, so
// the seeder enables it, runs it, and reads the result back before returning.
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { BASE_URL, apiFetch, ensureLibrary, runScan, artistIdsByName } from './api.js';

const FIXTURE_LIBRARY_NAME = 'a11y unfixable-finding fixture';
export const UNFIXABLE_ARTIST = 'Wholly Different Singer';
const FOLDER_NAME = 'unfixable-folder-3469';

export async function seedUnfixableFinding(request) {
  const port = process.env.SW_PORT || new URL(BASE_URL).port || 'default';
  const dir = path.join(os.tmpdir(), `sw-a11y-unfixable-${port}`);
  fs.mkdirSync(path.join(dir, FOLDER_NAME), { recursive: true });
  fs.writeFileSync(
    path.join(dir, FOLDER_NAME, 'artist.nfo'),
    `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>\n<artist><name>${UNFIXABLE_ARTIST}</name><sortname>Singer, Wholly Different</sortname></artist>\n`,
  );

  await ensureLibrary(request, FIXTURE_LIBRARY_NAME, dir);
  await runScan(request);
  const ids = await artistIdsByName(request, [UNFIXABLE_ARTIST]);
  if (!ids.has(UNFIXABLE_ARTIST)) {
    throw new Error('seed: the scan did not create the unfixable-finding artist (the NFO name was not used)');
  }
  const artistId = ids.get(UNFIXABLE_ARTIST);

  const en = await apiFetch(request, 'PUT', '/api/v1/rules/artist_id_mismatch', { enabled: true });
  if (!en.ok()) throw new Error(`seed: enabling artist_id_mismatch failed: ${en.status()} ${await en.text()}`);
  const run = await apiFetch(request, 'POST', `/api/v1/artists/${artistId}/run-rules`);
  if (!run.ok()) throw new Error(`seed: running rules failed: ${run.status()} ${await run.text()}`);

  // Assert the fixture's defining property: an OPEN, NOT-FIXABLE finding.
  const deadline = Date.now() + 30_000;
  let last = '';
  while (Date.now() < deadline) {
    const resp = await request.fetch(`${BASE_URL}/api/v1/notifications?status=open&page_size=500`);
    if (resp.ok()) {
      const body = await resp.json();
      last = JSON.stringify((body.violations||[]).filter(x=>x.artist_id===artistId).map(x=>[x.rule_id,x.fixable,x.status]));
      const list = Array.isArray(body) ? body : (body.violations || body.notifications || []);
      const v = list.find(x => x.rule_id === 'artist_id_mismatch' && x.artist_id === artistId);
      if (v) {
        if (v.fixable) throw new Error('seed: artist_id_mismatch came back fixable, so no badge would render');
        return { artistId, fieldArtistId: await seedUnfixableFieldFinding(request, dir) };
      }
    }
    await new Promise(r => setTimeout(r, 500));
  }
  throw new Error(`seed: no open artist_id_mismatch finding appeared within 60s (last list: ${last}; last run-rules: ${ran})`);
}

// seedUnfixableFieldFinding builds the data for the artist FIELD popover's copy
// of the badge (.sw-ff-pop-unfixable), which renders only for an open finding
// whose rule is tied to a field AND is not fixable. artist_id_mismatch has no
// field, so it never reaches a popover. name_language_pref does (name, sort
// name) and is unfixable whenever no localized alias exists: this artist has a
// katakana-only name and no MusicBrainz ID, so none can. Returns the artist id.
// A later scan-triggered evaluation runs without the user's language preference
// and resolves the finding again, so specs call raiseFieldFinding right before
// they navigate.
const FIELD_ARTIST = 'テストシンガー'; // katakana only
const FIELD_FOLDER = 'field-unfixable-folder-3469';

async function seedUnfixableFieldFinding(request, dir) {
  fs.mkdirSync(path.join(dir, FIELD_FOLDER), { recursive: true });
  fs.writeFileSync(
    path.join(dir, FIELD_FOLDER, 'artist.nfo'),
    `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>\n<artist><name>${FIELD_ARTIST}</name></artist>\n`,
  );
  await runScan(request);
  const ids = await artistIdsByName(request, [FIELD_ARTIST]);
  if (!ids.has(FIELD_ARTIST)) throw new Error('seed: the scan did not create the field-finding artist');
  const artistId = ids.get(FIELD_ARTIST);
  const en = await apiFetch(request, 'PUT', '/api/v1/rules/name_language_pref', { enabled: true });
  if (!en.ok()) throw new Error(`seed: enabling name_language_pref failed: ${en.status()} ${await en.text()}`);
  await raiseFieldFinding(request, artistId);
  return artistId;
}

/** raiseFieldFinding (re)runs the rules for the field-finding artist and waits for its OPEN, NOT-FIXABLE name_language_pref finding. */
export async function raiseFieldFinding(request, artistId) {
  const deadline = Date.now() + 60_000;
  let last = '';
  let ran = '';
  // Re-run the rules every few polls: a scan-triggered evaluation can land after
  // a single run and resolve the finding again (it carries no language preference).
  for (let n = 0; Date.now() < deadline; n++) {
    if (n % 6 === 0) {
      const run = await apiFetch(request, 'POST', `/api/v1/artists/${artistId}/run-rules`);
      if (!run.ok()) throw new Error(`seed: running rules failed: ${run.status()} ${await run.text()}`);
      ran = (await run.text()).slice(0, 300);
    }
    const resp = await request.fetch(`${BASE_URL}/api/v1/notifications?status=open&rule_id=name_language_pref&page_size=500`);
    if (resp.ok()) {
      const list = (await resp.json()).violations || [];
      last = JSON.stringify(list.map(x => [x.artist_id, x.fixable, x.status]));
      const v = list.find(x => x.artist_id === artistId);
      if (v) {
        if (v.fixable) throw new Error('seed: name_language_pref came back fixable, so no popover badge would render');
        return;
      }
    }
    await new Promise(r => setTimeout(r, 500));
  }
  throw new Error(`seed: no open name_language_pref finding for ${artistId} within 60s (last list: ${last}; last run-rules: ${ran})`);
}
