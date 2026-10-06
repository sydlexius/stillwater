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
    // A cleared value: its popover item shows the "empty" placeholder.
    if (!(await find('moods', 'manual', ''))) {
      await put(request, id, 'moods', ['Calm'], 'operator');
      const clear = await apiFetch(request, 'DELETE', `/api/v1/artists/${id}/fields/moods`);
      if (!clear.ok()) throw new Error(`seed: clear failed: ${clear.status()}`);
    }
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

test('an ordinary hx-confirm message is not tagged with the page language', async ({ page }) => {
  await page.goto(`/activity?artist_id=${artistId}`);
  const undo = page.locator(`#activity-change-${rows.bioNew.id} button[hx-post$="/revert"]`);
  // Precondition: a real hx-confirm trigger whose only lang ancestor is <html>.
  await expect(undo).toHaveCount(1);
  expect(await undo.evaluate((el) => el.hasAttribute('hx-confirm') && el.closest('[lang]') === document.documentElement)).toBe(true);
  await undo.click();
  await expect(page.locator('#confirm-modal')).toBeVisible();
  await expect(page.locator('#confirm-modal-message')).not.toBeEmpty();
  await expect(page.locator('#confirm-modal-message')).not.toHaveAttribute('lang', /.*/);
  await page.locator('#confirm-modal-cancel').click();
});

test('the value-source help opens from the keyboard and says what it means', async ({ page }) => {
  await page.goto(`/activity?artist_id=${artistId}`);
  const btn = page.locator('#help-activity-value-source button');
  await btn.focus();
  await page.keyboard.press('Enter');
  await expect(page.locator('#help-activity-value-source-popover')).toContainText('Value source not recorded means');
});

// Slice 4b: the per-field prior-values popover (artist detail, edit mode) names
// where each value came from. This is the surface where an operator restages an
// old value, so the label sits on the line the operator reads before choosing.
for (const theme of ['light', 'dark']) {
  test(`the prior-values popover shows the value source (${theme})`, async ({ page }) => {
    await page.goto(`/artists/${artistId}`);
    await applyTheme(expect, page, theme);
    await page.locator('body').press('e');
    await expect(page.locator('.sw-next-artist-detail'), 'precondition: edit mode').toHaveClass(/is-editing/);

    const field = async (name, want, absent) => {
      const trigger = page.locator(`#field-${name}-${artistId} button[aria-haspopup="true"][aria-controls^="ctx-panel-fh-"]`);
      await expect(trigger, `${name}: clock trigger must exist`).toHaveCount(1);
      await trigger.click();
      const panel = page.locator(`#ctx-panel-fh-${name}-${artistId}`);
      await expect(panel).toBeVisible();
      const label = panel.getByText(want, { exact: false });
      await expect(label.first(), `${name}: value label`).toBeVisible();
      if (absent) await expect(panel).not.toContainText(absent);
      // Measured as rendered, on top of the axe scan below, so a regression
      // names the node and the ratio instead of an axe selector.
      expect.soft(await renderedContrast(page, label.first()), `${name} value label`).toBeGreaterThanOrEqual(4.5);
      return panel;
    };

    // Biography: two operator edits and an undo. The undo row carries no label.
    const bio = await field('biography', 'Value: set by a user');
    // The Undo row's value is a restore: it must carry no value label at all.
    const undoItem = bio.locator('button[role="menuitem"]').filter({ hasText: 'Revert' });
    await expect(undoItem, 'precondition: the Revert item exists').toHaveCount(1);
    await expect(undoItem).not.toContainText('Value');
    // axe is scoped to each popover: the edit-mode page has unrelated findings
    // of its own (definition lists, an unlabeled textarea, low-contrast text in
    // the members section) outside this change.
    const scan = async (name) => {
      const r = await buildAxeBuilder(page).include(`#ctx-panel-fh-${name}-${artistId}`).analyze();
      expect(r.violations, `${name} popover: ${formatViolations(r.violations)}`).toEqual([]);
    };
    await scan('biography');
    await page.keyboard.press('Escape');

    await field('genres', 'Value: from Last.fm', 'set by a user');
    await scan('genres');
    await page.keyboard.press('Escape');
    await field('styles', 'Value source not recorded', 'set by a user');
    await scan('styles');
    await page.keyboard.press('Escape');

    // The "empty" placeholder of a cleared value, measured as rendered.
    const moods = await field('moods', 'Value: set by a user');
    const empty = moods.locator('em', { hasText: 'empty' });
    await expect(empty, 'precondition: the cleared value shows the empty placeholder').toHaveCount(1);
    const emptyContrast = await renderedContrast(page, empty);
    console.log(`CONTRAST ${theme} empty ${emptyContrast.toFixed(2)}`);
    expect(emptyContrast, 'empty placeholder').toBeGreaterThanOrEqual(4.5);
    await scan('moods');
  });
}

// applyTheme must not lose to the page's own preference load: with the saved
// preferences delayed, the page re-applies the server's theme AFTER a naive
// apply. The theme set here must still hold once that load has settled.
test('applyTheme holds when the preference load resolves late', async ({ page }) => {
  await page.route('**/api/v1/preferences', async (route) => {
    await new Promise((r) => setTimeout(r, 1200));
    await route.continue();
  });
  await page.goto('/activity');
  await applyTheme(expect, page, 'light');
  await page.waitForTimeout(2500);
  expect(await page.evaluate(() => document.documentElement.classList.contains('dark')), 'html is dark after the late load').toBe(false);
});

// A long unbreakable value label wraps inside the popover instead of widening
// the panel. The DOM text stands in for a 90-character token. Desktop panel only:
// below the breakpoint the history opens as a sheet, which is not this panel.
test('a long value label wraps inside the popover at 1280px', async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.goto(`/artists/${artistId}`);
  await page.locator('body').press('e');
  await expect(page.locator('.sw-next-artist-detail'), 'precondition: edit mode').toHaveClass(/is-editing/);
  await page.locator(`#field-biography-${artistId} button[aria-controls^="ctx-panel-fh-"]`).click();
  const panel = page.locator(`#ctx-panel-fh-biography-${artistId}`);
  await expect(panel).toBeVisible();
  await panel.locator('button[role="menuitem"] span.text-xs').first().evaluate((el, t) => { el.textContent = t; }, `Value: from ${'x'.repeat(90)}`);
  const box = await panel.boundingBox();
  expect(box.x, 'panel left edge').toBeGreaterThanOrEqual(0);
  expect(box.width, 'panel width with a 90-character token').toBeLessThanOrEqual(260);
});
