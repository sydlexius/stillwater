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

// The fixture that forces ScanErrors > 0 is seeded in global-setup.js, not in a
// beforeAll here (#3092): this report renders from a shared cache that only
// sweeps once per cooldown window, so a fixture built after any page render is
// invisible to it. See global-setup.js for the ordering and why it is mandatory.
//
// This file only READS that fixture; globalSetup throws if it never landed.

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
