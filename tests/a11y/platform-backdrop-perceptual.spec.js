// platform-backdrop-perceptual.spec.js - rendered-evidence check for #2979's
// amber left-accent fix on /reports/platform-backdrop-duplicates.
//
// `.sw-card` sets border-color on all four sides at specificity (0,1,0) and
// used to win on source order over the Tailwind `border-amber-500` utility on
// the partial-scan notice: the 4px left width landed, the colour did not.
// The fix routes the accent through `.sw-card-accent-amber`, defined after
// `.sw-card` in input.css so it wins the same way the bug did, used the right
// way round (see the class's doc comment). Source inspection cannot verify a
// cascade outcome -- this reads the LIVE computed style.
//
// Comparing against the resolved --color-amber-500 token, never a hard-coded
// rgb() literal: Tailwind v4 emits oklch(), so a literal string equality
// fails on NOTATION while the colour is correct (same pattern as
// backdrop-perceptual.spec.js's amber-treatment check).

import { test, expect } from 'playwright/test';
import { disableTransitions } from './helpers/settle.js';
import { restorePersistedTheme, applyTheme } from './helpers/axe.js';

const PAGE = '/reports/platform-backdrop-duplicates';
const NOTICE = '#platform-backdrop-duplicates-partial-notice';

// The harness boots against an EMPTY database and library, so the notice
// (rendered only when view.ScanErrors > 0) does not exist until a real scan
// failure is forced. That fixture is seeded in global-setup.js, NOT in a
// beforeAll here (#3092).
//
// WHY IT CANNOT LIVE IN THIS FILE. The report renders from a process-wide
// dupimages.Cache: one refresh computes both this report's half and the sibling
// local report's, and the lazy trigger is then locked out for 15 minutes by
// retryCooldown, stamped when the sweep STARTS and never cleared on success
// (internal/dupimages/cache.go). So the run gets ONE sweep. Any earlier spec
// that loads either report warms the cache before this fixture's fake Emby
// exists, and the sweep this fixture needs is never allowed to run -- the
// seeder then correctly reports "ScanErrors likely never went above 0" after
// its full 90s. Seeding second loses in EITHER ordering, which is why this is a
// constraint rather than a preference; global-setup.js carries the derivation.
//
// This file only READS that fixture. globalSetup throws if it never landed, so
// a failure here is a real defect in the page, never a missing fixture.

test.beforeEach(async ({ page }) => {
  await disableTransitions(page);
});

test.afterEach(async ({ page }) => {
  await restorePersistedTheme(page);
});

async function gotoReportAndAssertNotice(page) {
  await page.goto(PAGE);
  // Precondition: the fixture actually forced ScanErrors > 0. A duplicated
  // notice, or none at all, is its own defect and a bare presence check
  // cannot see it.
  await expect(page.locator(NOTICE)).toHaveCount(1);
}

test('partial-scan notice renders the amber accent, both themes', async ({ page }) => {
  await page.emulateMedia({ colorScheme: 'dark' });
  await gotoReportAndAssertNotice(page);
  await applyTheme(expect, page, 'dark');

  const notice = page.locator(NOTICE);

  const amberToken = await notice.evaluate((el) => {
    const raw = getComputedStyle(document.documentElement).getPropertyValue('--color-amber-500').trim();
    if (!raw) return '';
    // Round-trip through a throwaway element so the token and the measured
    // border-left-color are expressed in the same notation.
    const probe = document.createElement('span');
    probe.style.color = raw;
    document.body.appendChild(probe);
    const normalised = getComputedStyle(probe).color;
    probe.remove();
    return normalised;
  });
  expect(amberToken, '--color-amber-500 did not resolve, so this check would be vacuous').not.toBe('');

  const darkBorder = await notice.evaluate(el => getComputedStyle(el).borderLeftColor);
  expect(
    darkBorder,
    `dark mode: border-left-color was ${darkBorder}, expected the amber-500 token (${amberToken})`,
  ).toBe(amberToken);

  await page.emulateMedia({ colorScheme: 'light' });
  await applyTheme(expect, page, 'light');
  const lightBorder = await notice.evaluate(el => getComputedStyle(el).borderLeftColor);
  expect(
    lightBorder,
    `light mode: border-left-color was ${lightBorder}, expected the amber-500 token (${amberToken})`,
  ).toBe(amberToken);
});
