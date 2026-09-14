// websearch-unavailable.spec.js - a11y + markup coverage for the web image
// search "unavailable" state (#3229). Runs on both firefox-a11y
// (authoritative) and chromium-a11y (compat) per playwright.config.js.
//
// The fixture (seed-websearch-unavailable.js) enables the DuckDuckGo web
// search provider and relies on the harness server being launched with
// SW_FORCE_PROVIDER_ERROR=duckduckgo (Makefile's test-a11y target) so every
// DuckDuckGo call fails deterministically and offline -- see that file's
// header comment for why this is the sanctioned, production-inert seam.

import { test, expect } from 'playwright/test';

import { disableTransitions } from './helpers/settle.js';
import { buildAxeBuilder, formatViolations, applyTheme, restorePersistedTheme } from './helpers/axe.js';
import { seedWebSearchUnavailableArtist } from './helpers/seed-websearch-unavailable.js';

let artistId;

test.beforeAll(async ({ request }) => {
  artistId = await seedWebSearchUnavailableArtist(request);
});
test.beforeEach(async ({ page }) => { await disableTransitions(page); });
test.afterEach(async ({ page }) => { await restorePersistedTheme(page); });

/**
 * triggerWebSearch opens the Actions menu for the thumb slot and clicks
 * "Web Search", waiting for the HTMX swap of #image-results to settle.
 */
async function triggerWebSearch(page) {
  await page.goto(`/artists/${artistId}/images?type=thumb`);
  await page.waitForLoadState('load');
  const trigger = page.locator('[aria-haspopup="true"]').first();
  await trigger.waitFor({ state: 'visible', timeout: 10_000 });
  await trigger.click();
  const webSearchItem = page.getByRole('menuitem', { name: 'Web Search' });
  await webSearchItem.waitFor({ state: 'visible', timeout: 5_000 });
  await webSearchItem.click();
  const unavailable = page.locator('[data-sw-websearch-unavailable]');
  await unavailable.waitFor({ state: 'visible', timeout: 10_000 });
  return unavailable;
}

test('web search unavailable message is visible and names DuckDuckGo', async ({ page }) => {
  const message = await triggerWebSearch(page);
  await expect(message).toContainText('DuckDuckGo');
  await expect(message).toContainText('Google Images');
  // The plain zero-result copy must never appear alongside the unavailable
  // state -- the two are mutually exclusive branches in the template.
  await expect(page.locator('#image-results')).not.toContainText('No images found from web search.');
});

// AC: both themes, real axe-core.
for (const theme of ['dark', 'light']) {
  test(`web search unavailable message passes a11y scan (${theme} theme)`, async ({ page }) => {
    if (theme === 'dark') await page.emulateMedia({ colorScheme: 'dark' });
    await triggerWebSearch(page);
    if (theme === 'dark') {
      await applyTheme(expect, page, 'dark');
    } else {
      await page.waitForFunction(() => !!window.swPreferences?.applySingle, { timeout: 10_000 });
      await page.evaluate(() => window.swPreferences.applySingle('theme', 'light'));
      await page.waitForFunction(() => !document.documentElement.classList.contains('dark'), { timeout: 5_000 });
    }

    const results = await buildAxeBuilder(page).analyze();
    expect(results.violations, formatViolations(results.violations)).toHaveLength(0);
  });
}
