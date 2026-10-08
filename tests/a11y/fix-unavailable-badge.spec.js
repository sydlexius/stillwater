// fix-unavailable-badge.spec.js - the "Fix unavailable" badge must meet
// WCAG AA contrast (4.5:1) in both themes on every surface that renders it
// (#3469): the dashboard Action Queue and the artist page's findings list.
//
// The harness boots an EMPTY database, so the spec seeds its own unfixable
// finding (helpers/seed-unfixable-finding.js) and proves the badge is present
// before scanning. There is no conditional skip: an absent badge means the
// fixture is wrong, and the test fails.
//
// The artist field popover's copy of the badge (.sw-ff-pop-unfixable) only
// renders inside an opened field popover, so it is not reachable from this
// fixture; it uses the same --swd-ink-3 token as the findings-list copy, which
// IS scanned here (the token pairing itself is covered by token-pairings.spec.js).
import { test, expect } from 'playwright/test';

import { disableTransitions } from './helpers/settle.js';
import { buildAxeBuilder, formatViolations, applyTheme, restorePersistedTheme, renderedContrast } from './helpers/axe.js';
import { seedUnfixableFinding, raiseFieldFinding } from './helpers/seed-unfixable-finding.js';

let artistId;
let fieldArtistId;

test.beforeAll(async ({ request }) => {
  ({ artistId, fieldArtistId } = await seedUnfixableFinding(request));
});

test.beforeEach(async ({ page }) => {
  await disableTransitions(page);
});

test.afterEach(async ({ page }) => {
  await restorePersistedTheme(page);
});

// Each surface: where to go, and the badge locator on it. The text match is the
// rendered label itself, so a template that drops the badge fails here.
const surfaces = [
  { name: 'dashboard Action Queue', url: () => '/next/', badge: '#action-queue [data-sw-fix-unavailable]' },
  { name: 'artist findings list', url: () => `/next/artists/${artistId}`, badge: '.sw-next-finding-unfixable' },
];

for (const theme of ['dark', 'light']) {
  for (const s of surfaces) {
    test(`${s.name} badge passes color-contrast (${theme})`, async ({ page }) => {
      await page.goto(s.url());
      await page.waitForLoadState('load');
      await applyTheme(expect, page, theme);
      const badge = page.locator(s.badge).first(); // the dashboard queue also lists the field-finding artist
      await expect(badge, `no "Fix unavailable" badge on the ${s.name}; the fixture did not land`).toBeVisible({ timeout: 15_000 });
      await expect(badge).toBeVisible();
      await expect(badge).toContainText('Fix unavailable');

      const results = await buildAxeBuilder(page).include(s.badge).withRules(['color-contrast']).analyze();
      // The scan must have actually evaluated the badge (it is in passes when it
      // meets AA); log the measured ratio so a run shows the number, not just green.
      const evaluated = [...results.passes, ...results.violations].find(r => r.id === 'color-contrast');
      expect(evaluated, 'axe did not evaluate color-contrast on the badge').toBeTruthy();
      console.log(`CONTRAST ${s.name} ${theme}: ${evaluated.nodes.map(n => n.any[0]?.data?.contrastRatio).join(',')}`);
      expect(
        results.violations,
        `${s.name} (${theme}) contrast violations:\n${formatViolations(results.violations)}`,
      ).toHaveLength(0);
    });
  }
}

// The Dismiss button on the same card shares the badge's gray-100 / gray-700
// hover fill. axe never evaluates :hover, so measure the hovered text from
// rendered pixels (#3469). Preconditions are asserted, never skipped: the
// button must exist and the hover must actually change its background.
for (const theme of ['dark', 'light']) {
  test(`Action Queue Dismiss button hover passes contrast (${theme})`, async ({ page }) => {
    await page.goto('/next/');
    await page.waitForLoadState('load');
    await applyTheme(expect, page, theme);
    const btns = page.locator('#action-queue button[hx-post$="/dismiss"]');
    await expect(btns.first(), 'no Dismiss button on the Action Queue; the fixture did not land').toBeVisible({ timeout: 15_000 });
    const btn = btns.first();
    const bg = () => btn.evaluate((el) => getComputedStyle(el).backgroundColor);
    await page.mouse.move(0, 0);
    const resting = await bg();
    await btn.hover();
    const hovered = await bg();
    expect(hovered, 'hover did not change the Dismiss background; the measurement would not be of the hover state').not.toBe(resting);
    const ratio = await renderedContrast(page, btn);
    console.log(`CONTRAST Dismiss hover ${theme}: ${ratio}`);
    expect(ratio, `Dismiss hover contrast (${theme})`).toBeGreaterThanOrEqual(4.5);
  });
}

// The artist field popover's copy (.sw-ff-pop-unfixable, #3469). It sits in a
// popover that is closed until its chip is clicked, and only a field-tagged,
// unfixable finding renders it (seedUnfixableFieldFinding). Count it before
// opening so a fixture regression is a failing assertion, not an empty scan.
for (const theme of ['dark', 'light']) {
  test(`field popover badge passes contrast (${theme})`, async ({ page, request }) => {
    await raiseFieldFinding(request, fieldArtistId);
    await page.goto(`/next/artists/${fieldArtistId}`);
    await page.waitForLoadState('load');
    await applyTheme(expect, page, theme);
    const badge = page.locator('.sw-ff-pop-unfixable');
    expect(await badge.count(), 'no field-popover "Fix unavailable" badge; the fixture did not land').toBeGreaterThanOrEqual(1);
    const host = page.locator('span[data-context-menu]').filter({ has: badge }).first();
    await host.locator('.sw-field-chip').click();
    const open = host.locator('.sw-ff-pop-unfixable');
    await expect(open).toBeVisible();
    await expect(open).toContainText('Fix unavailable');
    const results = await buildAxeBuilder(page).include('.sw-ff-pop-unfixable').withRules(['color-contrast']).analyze();
    expect(results.violations, `popover badge (${theme}) violations:\n${formatViolations(results.violations)}`).toHaveLength(0);
    const ratio = await renderedContrast(page, open);
    console.log(`CONTRAST field popover badge ${theme}: ${ratio}`);
    expect(ratio, `field popover badge contrast (${theme})`).toBeGreaterThanOrEqual(4.5);
  });
}
