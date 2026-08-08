// backdrop-perceptual.spec.js - rendered-evidence checks for #2716's
// perceptual-duplicate reporting on /reports/backdrop-duplicates.
//
// This is a UAT-support spec, not a permanent tier member: it asserts that the
// new notice, stat tile, and perceptual-only row badge actually render, that
// they carry the intended amber "declined by design" treatment rather than an
// error treatment, and that the page is axe-clean in BOTH themes with the new
// elements present. Source inspection cannot establish any of that.
//
// Theme switching goes through applyTheme (the app's own preference path),
// never a raw classList.add -- see contrast.spec.js for why that distinction
// produced a false contrast failure in #2872.

import { test, expect } from 'playwright/test';
import { buildAxeBuilder, formatViolations, applyTheme } from './helpers/axe.js';
import { disableTransitions } from './helpers/settle.js';

const PAGE = '/reports/backdrop-duplicates';

async function gotoReport(page) {
  await page.goto(PAGE);
  await page.waitForSelector('#backdrop-duplicates-table', { timeout: 15_000 });
  await disableTransitions(page);
}

test('perceptual notice, tile, and badge render on the live page', async ({ page }) => {
  await gotoReport(page);

  // Selector MATCH COUNTS, not mere presence: a duplicated notice or a
  // duplicated id is its own defect and a presence check cannot see it.
  await expect(page.locator('#backdrop-duplicates-perceptual-notice')).toHaveCount(1);
  await expect(page.locator('#backdrop-duplicates-perceptual-slots')).toHaveCount(1);

  // The fixture library has exactly one perceptual-only artist, so exactly one
  // row may carry the badge.
  await expect(page.locator('[data-sw-perceptual-only-badge]')).toHaveCount(1);

  // The badge must sit on the row whose exact column is 0 -- the false-clean
  // row. Asserting the badge exists somewhere would pass even if it landed on
  // the wrong artist.
  const badgedRow = page.locator('tr', { has: page.locator('[data-sw-perceptual-only-badge]') });
  await expect(badgedRow.locator('td').nth(1)).toHaveText('0');
  await expect(badgedRow.locator('td').nth(2)).not.toHaveText('0');
});

test('perceptual notice uses the amber declined treatment, not an error treatment', async ({ page }) => {
  await gotoReport(page);

  const notice = page.locator('#backdrop-duplicates-perceptual-notice');
  const partial = page.locator('#backdrop-duplicates-partial-notice');

  // getComputedStyle on the LIVE element. The claim is "same non-alarming
  // amber treatment as the existing partial-scan notice", and only the
  // rendered border color can establish it. When the partial notice is absent
  // (no scan errors in this fixture) fall back to asserting the amber value
  // directly rather than silently skipping the check.
  const borderColor = await notice.evaluate((el) => getComputedStyle(el).borderLeftColor);
  const partialCount = await partial.count();
  if (partialCount === 1) {
    const partialBorder = await partial.evaluate((el) => getComputedStyle(el).borderLeftColor);
    expect(borderColor, 'perceptual notice must match the partial-scan notice treatment').toBe(partialBorder);
  }

  // Compare against the resolved --color-amber-500 token rather than a
  // hard-coded literal. Tailwind v4 emits oklch(), so an rgb() string equality
  // fails on NOTATION while the rendered colour is correct -- a false alarm
  // that would send the next reader hunting a styling bug that is not there.
  // Both sides are read from the live document and normalised by the same
  // engine, so the check still fails loudly if the accent becomes a red/error
  // token, which is the thing worth catching: it would misrepresent "declined
  // by design" as a failure.
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
  expect(
    borderColor,
    `border-left-color was ${borderColor}, expected the amber-500 token (${amberToken})`,
  ).toBe(amberToken);

  // role="alert" so the notice is announced, matching the partial-scan notice.
  await expect(notice).toHaveAttribute('role', 'alert');
});

test('report page is axe-clean in dark mode with the perceptual elements present', async ({ page }) => {
  await page.emulateMedia({ colorScheme: 'dark' });
  await gotoReport(page);
  await applyTheme(expect, page, 'dark');

  // Precondition: the elements under test are actually on the page. Without
  // this, a clean scan of a page missing the new markup would read as a pass.
  await expect(page.locator('#backdrop-duplicates-perceptual-notice')).toHaveCount(1);
  await expect(page.locator('[data-sw-perceptual-only-badge]')).toHaveCount(1);

  const results = await buildAxeBuilder(page).analyze();
  expect(
    results.violations,
    `dark-mode a11y violations:\n${formatViolations(results.violations)}`,
  ).toHaveLength(0);
});

test('report page is axe-clean in light mode with the perceptual elements present', async ({ page }) => {
  await page.emulateMedia({ colorScheme: 'light' });
  await gotoReport(page);
  await applyTheme(expect, page, 'light');

  await expect(page.locator('#backdrop-duplicates-perceptual-notice')).toHaveCount(1);
  await expect(page.locator('[data-sw-perceptual-only-badge]')).toHaveCount(1);

  const results = await buildAxeBuilder(page).analyze();
  expect(
    results.violations,
    `light-mode a11y violations:\n${formatViolations(results.violations)}`,
  ).toHaveLength(0);
});
