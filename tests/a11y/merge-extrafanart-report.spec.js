// merge-extrafanart-report.spec.js - a merge that leaves the survivor holding
// extrafanart/ images tells the operator, in the merge dialog they already use
// (#3180), in BOTH the dry-run preview and after the real merge. Runs on both
// projects (firefox-a11y, chromium-a11y).
//
// OWN SERVER. A merge MOVES and DELETES files, so the spec boots a throwaway
// server under a base path (so the link to the migration page is also proven to
// carry the base path). Every test re-seeds (helpers/seed-merge-extrafanart.js
// builds the pair inside the harness and asserts the fixture first).

import { test, expect } from 'playwright/test';
import path from 'node:path';

import { disableTransitions } from './helpers/settle.js';
import { buildAxeBuilder, formatViolations, applyTheme } from './helpers/axe.js';
import { startBasePathServer } from './helpers/base-path-server.js';
import { inventory, missingContent } from './helpers/seed-extrafanart-migration.js';
import { seedMergeFixture, cleanupMergeFixture, MERGE_FIXTURE, EXPECTED_IMAGES } from './helpers/seed-merge-extrafanart.js';
import fs from 'node:fs';

const BASE_PATH = '/sw-merge-extrafanart-test';
const NOTE = '#merge-extrafanart-report';

let server;
let libDir;

test.beforeAll(async () => {
  test.setTimeout(120_000);
  server = await startBasePathServer(BASE_PATH, { seed: (dir) => { libDir = path.join(dir, 'merge-library'); } });
});

test.afterAll(async () => {
  if (!server) return;
  try { await cleanupMergeFixture(server, libDir); } finally { server.stop(); }
});

async function openMerge(browser, theme = 'dark') {
  const context = await browser.newContext({ colorScheme: theme });
  await context.addCookies([{ name: 'session', value: server.sessionCookie, url: server.rootURL }]);
  const page = await context.newPage();
  await disableTransitions(page);
  await page.goto(`${server.baseURL}/reports/duplicates`);
  await applyTheme(expect, page, theme);
  await expect(page.locator('[data-merge-open]'), 'fixture: exactly one mergeable group').toHaveCount(1);
  await page.locator('[data-merge-open]').click();
  await expect(page.locator('#merge-modal')).toBeVisible();
  // The dry run ran when the survivor was chosen: Confirm is enabled only after a clean one.
  await expect(page.locator('#merge-modal-confirm')).toBeEnabled();
  return { context, page };
}

function survivorImageCount() {
  let n = 0;
  // Scan every artist folder: the merge may rename the survivor to its canonical
  // name (a hostile name sanitizes differently from the seeded folder name).
  for (const dir of fs.readdirSync(libDir)) {
    const extra = path.join(libDir, dir, 'extrafanart');
    if (fs.existsSync(extra)) n += fs.readdirSync(extra).filter(f => !f.startsWith('.') && f.endsWith('.jpg')).length;
  }
  return n;
}

for (const theme of ['dark', 'light']) test(`dry run and real merge both report the survivor extrafanart count, name the artist, link to the migration; no file is lost (${theme})`, async ({ browser }) => {
  await seedMergeFixture(server, libDir, true);
  expect(survivorImageCount(), 'precondition: images across both artists before the merge').toBe(EXPECTED_IMAGES);
  const before = inventory(libDir);
  const { context, page } = await openMerge(browser, theme);

  // Dry run: the preview carries the report, and nothing changed on disk.
  const note = page.locator(NOTE);
  await expect(note).toHaveCount(1);
  await expect(note).toContainText(`Image files that will be in the extrafanart folder for ${MERGE_FIXTURE.artistName} after this merge: ${EXPECTED_IMAGES}.`);
  const link = note.locator('a');
  await expect(link).toHaveText('Migrate Extrafanart Files');
  // The artist name is HOSTILE markup (see the seed): the note must hold the text
  // literally, with the link as its only element, and nothing may have executed.
  await expect(note.locator('*')).toHaveCount(1);
  await expect(note.locator(':scope > a')).toHaveCount(1);
  expect(await page.evaluate(() => window.__pwn), 'the artist name executed as markup').toBeUndefined();
  await expect(page.locator('#merge-modal img')).toHaveCount(0);
  await expect(link).toHaveAttribute('href', `${BASE_PATH}/reports/extrafanart-migration`);
  expect(inventory(libDir), 'a dry run must not touch the filesystem').toEqual(before);

  const results = await buildAxeBuilder(page).include(NOTE).analyze();
  expect(results.violations, formatViolations(results.violations)).toEqual([]);

  // Real merge: the dialog stays open with the "now" sentence and the same count.
  await page.locator('#merge-modal-confirm').click();
  await expect(note).toContainText(`Image files now in the extrafanart folder for ${MERGE_FIXTURE.artistName}: ${EXPECTED_IMAGES}.`);
  await expect(note.locator('a')).toHaveAttribute('href', `${BASE_PATH}/reports/extrafanart-migration`);
  await expect(page.locator('#merge-modal-cancel')).toHaveText('Close');
  await expect(note.locator('*')).toHaveCount(1);
  expect(await page.evaluate(() => window.__pwn)).toBeUndefined();

  // Done state: the choice UI is gone, focus is on Close (Confirm is disabled, so
  // focus would otherwise fall to <body>), and the result is in the live region
  // exactly once (the note itself is not a live region).
  await expect(page.locator('#merge-description')).toBeHidden();
  await expect(page.locator('#merge-survivor-fieldset')).toBeHidden();
  await expect(page.locator('#merge-preview-heading')).toBeHidden();
  await expect(page.locator('#merge-modal-cancel')).toBeFocused();
  const status = page.locator('#merge-status');
  await expect(status).toHaveAttribute('role', 'status');
  await expect(status).toHaveText(await note.innerText());
  await expect(status).toContainText(`${EXPECTED_IMAGES}`);
  expect(await note.evaluate((el) => !!el.closest('[role="status"], [role="alert"], [aria-live]')), 'the note must not also be a live region').toBe(false);
  // The dialog title is an <h3> under the page <h1> (heading-order): pre-existing
  // and unrelated to this change, so it is excluded; everything else is scanned.
  const done = await buildAxeBuilder(page).include('#merge-modal').exclude('#merge-modal-title').analyze();
  expect(done.violations, formatViolations(done.violations)).toEqual([]);

  // Nothing was lost: every pre-merge file's bytes are still on disk, except
  // the dotfile image the merge has always left behind.
  // The two deliberate losses are older merge behavior, unchanged here: the
  // dotfile image is never carried over, and the two artists' identical
  // artist.nfo are one loose file (survivor's copy wins, the loser's is deleted).
  const missing = missingContent(before, inventory(libDir));
  const unexpected = missing.filter(rel => !rel.endsWith('.hidden.jpg') && !rel.endsWith('artist.nfo'));
  expect(unexpected, 'files whose content vanished from the library').toEqual([]);
  expect(missing.filter(rel => rel.endsWith('artist.nfo')), 'exactly one of the two same-named loose files goes').toHaveLength(1);
  expect(survivorImageCount(), 'the survivor holds every image from both artists').toBe(EXPECTED_IMAGES);

  // Closing reloads the page, which no longer lists the merged group.
  await Promise.all([
    page.waitForLoadState('load'),
    page.locator('#merge-modal-cancel').click(),
  ]);
  await expect(page.locator('[data-merge-open]')).toHaveCount(0);
  await context.close();
});

test('a merge with no extrafanart images says nothing and reloads as before', async ({ browser }) => {
  await seedMergeFixture(server, libDir, false);
  const { context, page } = await openMerge(browser);
  await expect(page.locator(NOTE), 'nothing to migrate, so nothing is reported in the preview').toHaveCount(0);
  await page.locator('#merge-modal-confirm').click();
  // No report: the dialog closes itself and the page reloads to the empty list.
  await expect(page.locator('#merge-modal')).toBeHidden();
  await expect(page.locator('[data-merge-open]')).toHaveCount(0);
  await expect(page.locator(NOTE)).toHaveCount(0);
  await context.close();
});
