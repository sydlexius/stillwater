// extrafanart-migration.spec.js - axe-core and rendered-contrast coverage for the
// extrafanart migration preview page (#3179), route /reports/extrafanart-migration.
//
// Runs on both projects in playwright.config.js (firefox-a11y authoritative,
// chromium-a11y compatibility). Each test scans the page WITH THE PLAN
// RENDERED, in both themes: the plan table is the surface this slice ships, and
// the empty state scans clean without it.
//
// The harness boots an empty database, so beforeAll seeds its own fixture
// (helpers/seed-extrafanart-migration.js) and every test asserts the fixture's
// defining property on the PAGE before the scan, so an absent plan fails loudly
// instead of passing on an empty page. No conditional skips. The fixture is
// removed in afterAll and re-seeded per project, so it never leaks into a later
// spec.
//
// axe reports text over the translucent glass cards as "incomplete", never as a
// violation, so the muted text is also measured with renderedContrast.

import { test, expect } from 'playwright/test';

import { disableTransitions } from './helpers/settle.js';
import {
  buildAxeBuilder, formatViolations, applyTheme, restorePersistedTheme, renderedContrast,
} from './helpers/axe.js';
import {
  seedExtraFanartFixture, cleanupExtraFanartFixture, PLAN_FIXTURE, FILES_PER_ARTIST,
} from './helpers/seed-extrafanart-migration.js';

const PAGE = '/reports/extrafanart-migration';
const ARTISTS = PLAN_FIXTURE.artists;

test.beforeAll(async ({ request }) => {
  await seedExtraFanartFixture(request, PLAN_FIXTURE);
});
test.afterAll(async ({ request }) => {
  await cleanupExtraFanartFixture(request, PLAN_FIXTURE);
});
test.beforeEach(async ({ page }) => { await disableTransitions(page); });
test.afterEach(async ({ page }) => { await restorePersistedTheme(page); });

// Loads the page and proves the plan rendered: no empty state, no error card, the
// run button (never clicked here: this spec shares the server), one "Will Move"
// row per seeded file naming its artist, and the one-way paragraph above the table.
async function gotoPlan(page) {
  await page.goto(PAGE);
  await page.waitForLoadState('load');
  await expect(page.locator('#extrafanart-migration-empty')).toHaveCount(0);
  await expect(page.locator('#extrafanart-migration-error')).toHaveCount(0);
  await expect(page.locator('#extrafanart-migration-run-button')).toHaveCount(1);
  const rows = page.locator('#extrafanart-migration-table tbody tr');
  const fixtureRows = rows.filter({ hasText: /Extra Fixture (One|Two)/ });
  await expect(fixtureRows).toHaveCount(ARTISTS.length * FILES_PER_ARTIST);
  for (const name of ARTISTS) {
    await expect(rows.filter({ hasText: name })).toHaveCount(FILES_PER_ARTIST);
  }
  await expect(fixtureRows.filter({ hasText: 'Will Move' })).toHaveCount(ARTISTS.length * FILES_PER_ARTIST);
  // The fixture artist whose folder was removed after the scan is reported as skipped.
  const skipped = page.locator('#extrafanart-migration-skipped');
  await expect(skipped).toHaveCount(1);
  await expect(skipped).toContainText('1 artist was skipped');
  const warning = page.locator('#extrafanart-migration-warning');
  await expect(warning).toHaveCount(1);
  await expect(warning).toContainText('one-way operation');
  await expect(warning).toContainText('Nothing is deleted');
  const warnBox = await warning.boundingBox();
  const tableBox = await page.locator('#extrafanart-migration-table').boundingBox();
  expect(warnBox.y, 'the one-way paragraph must sit above the table').toBeLessThan(tableBox.y);
}

for (const theme of ['dark', 'light']) {
  test(`extrafanart migration preview passes a11y scan and contrast (${theme} theme)`, async ({ page }) => {
    await page.emulateMedia({ colorScheme: theme });
    await gotoPlan(page);
    await applyTheme(expect, page, theme);

    const measured = {
      subtitle: page.locator('.sw-extrafanart-migration > div').first().locator('p'),
      skippedLine: page.locator('#extrafanart-migration-skipped'),
      warning: page.locator('#extrafanart-migration-warning'),
      statLabel: page.locator('#extrafanart-migration-body dl dt').first(),
      tableHeader: page.locator('#extrafanart-migration-table thead th').first(),
      tableCell: page.locator('#extrafanart-migration-table tbody td').nth(1),
      outcomeCell: page.locator('#extrafanart-migration-table tbody td').nth(3),
    };
    for (const [name, loc] of Object.entries(measured)) {
      expect(await loc.count(), `${name} selector matched nothing`).toBeGreaterThanOrEqual(1);
      expect.soft(await renderedContrast(page, loc.first()), name).toBeGreaterThanOrEqual(4.5);
    }

    const results = await buildAxeBuilder(page).analyze();
    expect(results.violations, formatViolations(results.violations)).toEqual([]);
  });
}
