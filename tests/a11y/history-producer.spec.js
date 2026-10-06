// history-producer.spec.js - rendered evidence for #3078 slice 4a: the Activity
// feed shows what supplied each value, beside the existing source badge.
//
// FIXTURE. The harness boots an empty database and an empty library, so this
// spec builds its own: one scan-created artist, then through the real API an
// operator edit (twice, so there is something to undo), a Last.fm edit, an edit
// that claims nothing, and an Undo. Seeding is idempotent because the spec runs
// once per browser project against the same server. Before any page is trusted,
// the producer values are asserted through the API, so a page that is wrong
// cannot be blamed on (or excused by) a fixture that never landed. Absent data
// throws; nothing here skips.

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { test, expect } from 'playwright/test';

import { buildAxeBuilder, formatViolations, applyTheme, renderedContrast } from './helpers/axe.js';
import { disableTransitions } from './helpers/settle.js';
import { STORAGE_STATE } from './global-setup.js';
import {
  BASE_URL, apiFetch, ensureLibrary, runScan, artistIdsByName,
} from './helpers/api.js';

const ARTIST = 'Producer Fixture';
const LIBRARY = 'a11y history-producer fixture';

async function put(request, id, field, value, producer) {
  const body = producer === undefined ? { value } : { value, producer };
  const resp = await apiFetch(request, 'PATCH', `/api/v1/artists/${id}/fields/${field}`, body);
  if (!resp.ok()) throw new Error(`seed: ${field} write failed: ${resp.status()} ${await resp.text()}`);
}

async function history(request, id) {
  const resp = await request.fetch(`${BASE_URL}/api/v1/artists/${id}/history?limit=100`);
  if (!resp.ok()) throw new Error(`history read failed: ${resp.status()}`);
  return (await resp.json()).changes;
}

let rows; // history rows as the API reports them
let artistId;
let libraryId;
let fixtureDir;

test.beforeAll(async ({ playwright }) => {
  const request = await playwright.request.newContext({ storageState: STORAGE_STATE });
  try {
    // A FRESH directory and library per seeding. A field write goes through to
    // the artist.nfo on disk, so a directory reused by the second browser
    // project would be scanned back in with the first project's values already
    // set: the genres PATCH becomes a no-op (no history row is written for an
    // unchanged value) and the scan row stands in for the edit.
    fixtureDir = fs.mkdtempSync(path.join(os.tmpdir(), 'sw-a11y-producer-'));
    fs.mkdirSync(path.join(fixtureDir, ARTIST), { recursive: true });
    libraryId = await ensureLibrary(request, `${LIBRARY} ${path.basename(fixtureDir)}`, fixtureDir);
    await runScan(request);
    const id = (await artistIdsByName(request, [ARTIST])).get(ARTIST);
    if (!id) throw new Error('seed: scan did not create the fixture artist');
    artistId = id;

    // Seed only what is missing, so a run that died midway (or the second
    // browser project) completes the fixture instead of failing forever.
    const find = async (f, s, nv) => (await history(request, id))
      .find((c) => c.field === f && c.source === s && (nv === undefined || c.new_value === nv));
    if (!(await find('biography', 'manual', 'Fixture biography two.'))) {
      await put(request, id, 'biography', 'Fixture biography one.', 'operator');
      await put(request, id, 'biography', 'Fixture biography two.', 'operator');
    }
    if (!(await find('genres', 'manual'))) await put(request, id, 'genres', ['Ambient'], 'provider:lastfm');
    if (!(await find('styles', 'manual'))) await put(request, id, 'styles', ['Drone']); // no claim: the legacy-equivalent '' row
    if (!(await find('biography', 'revert'))) {
      const second = await find('biography', 'manual', 'Fixture biography two.');
      const undo = await apiFetch(request, 'POST', `/api/v1/history/${second.id}/revert`);
      if (!undo.ok()) throw new Error(`seed: undo failed: ${undo.status()}`);
    }

    rows = {
      bioNew: await find('biography', 'manual', 'Fixture biography two.'),
      genres: await find('genres', 'manual'),
      styles: await find('styles', 'manual'),
      undo: await find('biography', 'revert'),
    };
    // The fixture's defining property, asserted through the API first.
    expect(rows.bioNew?.producer, 'operator edit').toBe('operator');
    expect(rows.genres?.producer, 'last.fm edit').toBe('provider:lastfm');
    expect(rows.styles?.producer, 'edit with no claim').toBe('');
    expect(rows.undo?.producer, 'undo').toBe('restore');
  } finally {
    await request.dispose();
  }
});

// The fixture must not outlive this file: a leftover library and artist show up
// in the compliance chart and flyout of every later spec (the chromium leg runs
// after the firefox leg seeded) and change what axe measures there. deleteArtists
// removes the scan-created artist and its history with the library. Loud on
// purpose: a failed delete throws instead of leaving silent debris.
test.afterAll(async ({ request }) => {
  expect(libraryId, 'beforeAll never created the fixture library').toBeTruthy();
  const resp = await apiFetch(request, 'DELETE', `/api/v1/libraries/${libraryId}?deleteArtists=true`);
  expect(resp.ok(), `fixture library delete failed: ${resp.status()} ${await resp.text()}`).toBe(true);
  const left = await artistIdsByName(request, [ARTIST]);
  expect(left.size, 'fixture artist survived the library delete').toBe(0);
  fs.rmSync(fixtureDir, { recursive: true, force: true });
});

test.beforeEach(async ({ page }) => {
  await disableTransitions(page);
});

for (const route of ['/activity', '/next/activity']) {
  for (const theme of ['light', 'dark']) {
    test(`${route} shows the value source (${theme})`, async ({ page }) => {
      // Scoped to the fixture artist: other specs add newer rows that would push
      // these off page 1 of the global feed.
      await page.goto(`${route}?artist_id=${artistId}`);
      await applyTheme(expect, page, theme);
      const row = (r) => page.locator(`#activity-change-${r.id}`);
      for (const [r, want] of [
        [rows.bioNew, 'Value: set by a user'],
        [rows.genres, 'Value: from Last.fm'],
        [rows.styles, 'Value source not recorded'],
      ]) {
        await expect(row(r)).toContainText(want);
      }
      await expect(row(rows.styles)).not.toContainText('set by a user');
      await expect(row(rows.undo)).not.toContainText('Value');

      const chip = row(rows.styles).getByText('Value source not recorded');
      expect(await renderedContrast(page, chip)).toBeGreaterThanOrEqual(4.5);

      // Rendered contrast for the rest of the text this feed draws. axe reports
      // text over the glass surface as incomplete, never as a violation, so only
      // the rendered measurement can fail it. Each selector must match first,
      // so a markup change cannot turn these into vacuous passes.
      const genres = row(rows.genres);
      await genres.locator('summary').click();
      const measured = {
        'showing counter': page.locator('#activity-showing-counter'),
        'Set to value': genres.getByText(/^Set to:/),
        'expanded Current value': genres.locator('details > div p'),
        'Changed label': row(rows.bioNew).locator('summary').getByText('Changed', { exact: true }),
        'Undo button': row(rows.bioNew).locator('button[hx-post$="/revert"]'),
      };
      for (const [name, loc] of Object.entries(measured)) {
        expect(await loc.count(), `${name} selector matched nothing`).toBeGreaterThanOrEqual(1);
        expect.soft(await renderedContrast(page, loc.first()), name).toBeGreaterThanOrEqual(4.5);
      }

      const results = await buildAxeBuilder(page).analyze();
      expect(results.violations, formatViolations(results.violations)).toEqual([]);
    });
  }
}

test('the value-source help opens from the keyboard and says what it means', async ({ page }) => {
  await page.goto(`/activity?artist_id=${artistId}`);
  const btn = page.locator('#help-activity-value-source button');
  await btn.focus();
  await page.keyboard.press('Enter');
  await expect(page.locator('#help-activity-value-source-popover')).toContainText('Value source not recorded means');
});
