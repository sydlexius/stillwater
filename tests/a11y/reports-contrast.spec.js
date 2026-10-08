// reports-contrast.spec.js - painted contrast of the Reports page text that
// sits on translucent glass (#3469). axe reports text over a translucent
// surface as "incomplete", never as a violation, so only the rendered
// measurement (renderedContrast, min glyph pixel) can fail these.
//   - the "N of M compliant" captions (compliance + health summaries)
//   - every rail description: inactive, active, and a hovered inactive row
// Preconditions are counted, never skipped: a missing element fails the test.
import { test, expect } from 'playwright/test';

import { disableTransitions } from './helpers/settle.js';
import { applyTheme, restorePersistedTheme, renderedContrast } from './helpers/axe.js';
import { seedUnfixableFinding } from './helpers/seed-unfixable-finding.js';

const RAIL_ITEMS = 12; // repBuiltinReports in web/templates/reports_page.templ

// The caption text needs at least one artist to exist; the seed provides two.
test.beforeAll(async ({ request }) => {
  await seedUnfixableFinding(request);
});

test.beforeEach(async ({ page }) => {
  await disableTransitions(page);
});

test.afterEach(async ({ page }) => {
  await restorePersistedTheme(page);
});

const captions = [
  { name: 'compliance', min: 2, exact: false, url: '/reports/compliance', loc: '#compliance-summary p.text-xs', text: /\d+ of \d+ compliant/ },
  { name: 'health', min: 1, exact: true, url: '/reports/health', loc: '.sw-rep-simple-pane p.text-xs', text: /\d+ of \d+ fully compliant/ },
];

for (const theme of ['dark', 'light']) {
  for (const c of captions) {
    test(`${c.name} caption painted contrast (${theme})`, async ({ page }) => {
      await page.goto(c.url);
      await page.waitForLoadState('load');
      await applyTheme(expect, page, theme);
      const caps = page.locator(c.loc).filter({ hasText: c.text });
      // /reports/compliance renders the overall card plus one per library; every
      // one is measured so a class change on any of them fails here.
      await expect(caps.first(), `no "N of M compliant" caption on ${c.url}`).toBeVisible({ timeout: 15_000 });
      const n = await caps.count();
      if (c.exact) expect(n, `caption count on ${c.url}`).toBe(c.min);
      else expect(n, `caption count on ${c.url}`).toBeGreaterThanOrEqual(c.min);
      const ratios = [];
      for (let i = 0; i < n; i++) {
        ratios.push({ i, text: (await caps.nth(i).innerText()).trim(), ratio: await renderedContrast(page, caps.nth(i)) });
      }
      console.log(`CONTRAST caption ${c.name} ${theme}: ${JSON.stringify(ratios)}`);
      for (const r of ratios) {
        expect(r.ratio, `caption #${r.i} "${r.text}" on ${c.url} (${theme})`).toBeGreaterThanOrEqual(4.5);
      }
    });
  }

  test(`rail descriptions painted contrast (${theme})`, async ({ page }) => {
    await page.goto('/reports/compliance');
    await page.waitForLoadState('load');
    await applyTheme(expect, page, theme);
    await expect(page.locator('.sw-rep-item-desc')).toHaveCount(RAIL_ITEMS);
    await expect(page.locator('.sw-rep-item')).toHaveCount(RAIL_ITEMS);
    const active = page.locator('.sw-rep-item.is-active .sw-rep-item-desc');
    await expect(active, 'expected exactly one active rail item').toHaveCount(1);
    const inactive = page.locator('.sw-rep-item:not(.is-active) .sw-rep-item-desc');
    await expect(inactive).toHaveCount(RAIL_ITEMS - 1);

    await page.mouse.move(0, 0);
    const got = { active: await renderedContrast(page, active) };
    let min = Infinity;
    for (let i = 0; i < RAIL_ITEMS - 1; i++) min = Math.min(min, await renderedContrast(page, inactive.nth(i)));
    got.inactiveMin = min;

    const row = page.locator('.sw-rep-item:not(.is-active)').nth(3);
    const rowBg = () => row.evaluate((el) => getComputedStyle(el).backgroundColor);
    const resting = await rowBg();
    await row.hover();
    expect(await rowBg(), 'hover did not change the rail row background').not.toBe(resting);
    got.hovered = await renderedContrast(page, row.locator('.sw-rep-item-desc'));
    console.log(`CONTRAST rail ${theme}: ${JSON.stringify(got)}`);
    for (const [k, v] of Object.entries(got)) {
      expect(v, `rail description (${k}) contrast, ${theme}`).toBeGreaterThanOrEqual(4.5);
    }
  });
}
