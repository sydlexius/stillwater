// websearch-ai-filter.spec.js - browser coverage for the "Filter AI images"
// switch in the Manage Artwork web image search results panel (#2310). Runs on
// firefox-a11y (authoritative) and chromium-a11y per playwright.config.js.
//
// The fixture is seed-websearch-unavailable.js: it enables DuckDuckGo and the
// harness server runs with SW_FORCE_PROVIDER_ERROR=duckduckgo, so every
// search fails deterministically and offline. The switch renders in the
// "unavailable" state too (an empty panel is exactly when an operator needs
// it), so that state is a real render of the control, not a skip. No live
// DuckDuckGo request is ever made. The harness also sets SW_AI_BLOCKLIST_URL
// empty, so the blocklist download is turned off and the panel
// deterministically shows its "turned off on this server" notice while the
// filter is on (the "not loaded yet" copy is covered by the Go handler tests).

import { test, expect } from 'playwright/test';

import { disableTransitions } from './helpers/settle.js';
import { buildAxeBuilder, formatViolations, applyTheme, restorePersistedTheme } from './helpers/axe.js';
import { seedWebSearchUnavailableArtist } from './helpers/seed-websearch-unavailable.js';
import { apiFetch } from './helpers/api.js';

let artistId;

test.beforeAll(async ({ request }) => {
  artistId = await seedWebSearchUnavailableArtist(request);
});
test.beforeEach(async ({ page, request }) => {
  await disableTransitions(page);
  // Every test starts from the default (filter on) regardless of run order.
  const resp = await apiFetch(request, 'PUT', '/api/v1/preferences/filter_ai_images', { value: 'true' });
  expect(resp.ok(), await resp.text()).toBeTruthy();
});
test.afterEach(async ({ page }) => { await restorePersistedTheme(page); });

async function triggerWebSearch(page) {
  await page.goto(`/artists/${artistId}/images?type=thumb`);
  await page.waitForLoadState('load');
  const trigger = page.locator('[aria-haspopup="true"]').first();
  await trigger.waitFor({ state: 'visible', timeout: 10_000 });
  await trigger.click();
  const item = page.getByRole('menuitem', { name: 'Web Search' });
  await item.waitFor({ state: 'visible', timeout: 5_000 });
  await item.click();
  const toggle = page.locator('#sw-ai-filter-toggle');
  await toggle.waitFor({ state: 'visible', timeout: 10_000 });
  return toggle;
}

test('filter switch is on by default and labelled', async ({ page }) => {
  const toggle = await triggerWebSearch(page);
  await expect(toggle).toHaveAttribute('role', 'switch');
  await expect(toggle).toHaveAttribute('aria-checked', 'true');
  await expect(page.getByRole('switch', { name: 'Filter AI images' })).toBeVisible();
});

test('with the download turned off, filter on says results are unfiltered; filter off drops the notice', async ({ page }) => {
  const toggle = await triggerWebSearch(page);
  const notice = page.locator('[data-sw-ai-filter-disabled]');
  await expect(notice).toBeVisible();
  await expect(notice).toContainText('turned off on this server');
  await expect(page.locator('[data-sw-ai-filter-not-loaded]')).toHaveCount(0);
  await toggle.click();
  await expect(page.locator('#sw-ai-filter-toggle')).toHaveAttribute('aria-checked', 'false', { timeout: 10_000 });
  await expect(notice).toHaveCount(0);
});

test('one click = one preference write and exactly one re-search, then unchecked', async ({ page }) => {
  const toggle = await triggerWebSearch(page);
  const errors = [];
  page.on('console', (m) => { if (m.type() === 'error') errors.push(m.text()); });

  const searches = [];
  const writes = [];
  page.on('request', (req) => {
    const u = req.url();
    if (u.includes('/images/websearch')) searches.push(req.method());
    if (u.includes('/api/v1/preferences/filter_ai_images') && req.method() === 'PUT') writes.push(u);
  });

  await toggle.click();
  await expect(page.locator('#sw-ai-filter-toggle')).toHaveAttribute('aria-checked', 'false', { timeout: 10_000 });
  await expect(page.locator('#sw-ai-filter-toggle')).toBeEnabled();
  expect(writes, 'preference PUTs').toHaveLength(1);
  expect(searches, 'web searches').toHaveLength(1);
  // A successful re-search is not reported as failed.
  await expect(page.locator('[data-sw-ai-filter-refresh-failed]')).toHaveCount(0);
  expect(errors.filter((e) => e.includes('toggleAIImageFilter')), 'toggle errors on success').toEqual([]);
});

test('a double click still produces exactly one re-search (control disabled in flight)', async ({ page }) => {
  const toggle = await triggerWebSearch(page);

  const searches = [];
  page.on('request', (req) => {
    if (req.url().includes('/images/websearch')) searches.push(req.url());
  });

  await toggle.dblclick();
  await expect(page.locator('#sw-ai-filter-toggle')).toHaveAttribute('aria-checked', 'false', { timeout: 10_000 });
  await expect(page.locator('#sw-ai-filter-toggle')).toBeEnabled();
  expect(searches, 'web searches after a double click').toHaveLength(1);
});

test('the choice survives a reload', async ({ page }) => {
  const toggle = await triggerWebSearch(page);
  await toggle.click();
  await expect(page.locator('#sw-ai-filter-toggle')).toHaveAttribute('aria-checked', 'false', { timeout: 10_000 });

  const again = await triggerWebSearch(page);
  await expect(again).toHaveAttribute('aria-checked', 'false');
});

test('keyboard toggle keeps focus on the switch after the re-render (#2310 F3)', async ({ page }) => {
  const toggle = await triggerWebSearch(page);
  await toggle.focus();
  await page.keyboard.press('Space');
  await expect(page.locator('#sw-ai-filter-toggle')).toHaveAttribute('aria-checked', 'false', { timeout: 10_000 });
  await expect(page.locator('#sw-ai-filter-toggle')).toBeEnabled();
  await expect(page.locator('#sw-ai-filter-toggle')).toBeFocused();
});

test('a failed preference write does not re-search and does not flip the switch (F4)', async ({ page }) => {
  const toggle = await triggerWebSearch(page);
  await page.route('**/api/v1/preferences/filter_ai_images', (route) => {
    if (route.request().method() === 'PUT') return route.fulfill({ status: 500, body: '{"error":"boom"}', contentType: 'application/json' });
    return route.continue();
  });
  const searches = [];
  page.on('request', (req) => { if (req.url().includes('/images/websearch')) searches.push(req.url()); });
  const errors = [];
  page.on('console', (m) => { if (m.type() === 'error') errors.push(m.text()); });

  await toggle.click();
  await expect(page.locator('#sw-ai-filter-toggle')).toBeEnabled({ timeout: 10_000 });
  await expect(page.locator('#sw-ai-filter-toggle')).toHaveAttribute('aria-checked', 'true');
  expect(searches, 'web searches after a failed write').toHaveLength(0);
  expect(errors.join('\n')).toContain('not saved');
});

test('a failed save is detected even when the cache already holds the requested value (F7)', async ({ page }) => {
  const toggle = await triggerWebSearch(page);
  // Server says true (the switch rendered checked); make this tab's cache
  // already hold 'false', the value the click will request. A value-compare
  // would read the reverted 'false' as a successful save.
  await page.evaluate(() => {
    const c = JSON.parse(sessionStorage.getItem('sw-preferences') || '{}');
    c.filter_ai_images = 'false';
    sessionStorage.setItem('sw-preferences', JSON.stringify(c));
  });
  await page.route('**/api/v1/preferences/filter_ai_images', (route) => {
    if (route.request().method() === 'PUT') return route.fulfill({ status: 500, body: '{"error":"boom"}', contentType: 'application/json' });
    return route.continue();
  });
  const searches = [];
  page.on('request', (req) => { if (req.url().includes('/images/websearch')) searches.push(req.url()); });

  await toggle.click();
  await expect(page.locator('#sw-ai-filter-toggle')).toBeEnabled({ timeout: 10_000 });
  await expect(page.locator('#sw-ai-filter-toggle')).toHaveAttribute('aria-checked', 'true');
  expect(searches, 'web searches after a failed write').toHaveLength(0);
});

// save() must report failure for every failure shape, not only an HTTP 500:
// a dropped connection, a 403, and a 2xx whose body is not JSON (#2310 F1).
for (const [name, fail] of [
  ['network error', (route) => route.abort('failed')],
  ['HTTP 403', (route) => route.fulfill({ status: 403, body: '{"error":"forbidden"}', contentType: 'application/json' })],
  ['2xx with a non-JSON body', (route) => route.fulfill({ status: 200, body: 'not json', contentType: 'application/json' })],
]) {
  test(`a failed save (${name}) does not re-search and does not flip the switch`, async ({ page }) => {
    const toggle = await triggerWebSearch(page);
    await page.route('**/api/v1/preferences/filter_ai_images', (route) => (route.request().method() === 'PUT' ? fail(route) : route.continue()));
    const searches = [];
    page.on('request', (req) => { if (req.url().includes('/images/websearch')) searches.push(req.url()); });

    await toggle.click();
    await expect(page.locator('#sw-ai-filter-toggle')).toBeEnabled({ timeout: 10_000 });
    await expect(page.locator('#sw-ai-filter-toggle')).toHaveAttribute('aria-checked', 'true');
    expect(searches, 'web searches after a failed write').toHaveLength(0);
  });
}

test('a failed re-search leaves the switch showing the SAVED value, and the next click flips back (F4)', async ({ page, request }) => {
  const toggle = await triggerWebSearch(page);
  const putBodies = [];
  page.on('request', (req) => {
    if (req.url().includes('/api/v1/preferences/filter_ai_images') && req.method() === 'PUT') putBodies.push(req.postData());
  });
  let failNext = true;
  await page.route('**/images/websearch**', (route) => {
    if (failNext) { failNext = false; return route.fulfill({ status: 500, body: 'boom' }); }
    return route.continue();
  });

  await toggle.click();
  await expect(page.locator('#sw-ai-filter-toggle')).toBeEnabled({ timeout: 10_000 });
  await expect(page.locator('#sw-ai-filter-toggle')).toHaveAttribute('aria-checked', 'false');
  const saved = await apiFetch(request, 'GET', '/api/v1/preferences/filter_ai_images');
  expect((await saved.json()).value).toBe('false');
  // The stale panel must not contradict the switch: a refresh-failed notice
  // replaces the filter notice that described the old (filter-on) results.
  await expect(page.locator('[data-sw-ai-filter-refresh-failed]')).toBeVisible();
  await expect(page.locator('[data-sw-ai-filter-refresh-failed]')).toContainText('could not be refreshed');
  await expect(page.locator('[data-sw-ai-filter-disabled]')).toHaveCount(0);

  await page.locator('#sw-ai-filter-toggle').click();
  await expect(page.locator('#sw-ai-filter-toggle')).toHaveAttribute('aria-checked', 'true', { timeout: 10_000 });
  expect(putBodies.map((b) => JSON.parse(b).value)).toEqual(['false', 'true']);
});

test('the switch renders and toggles on the generic Manage artwork layout too (#web-search-results)', async ({ page }) => {
  // No ?type= selects the generic layout, whose web search results render into
  // #web-search-results instead of #image-results.
  await page.goto(`/artists/${artistId}/images`);
  await page.waitForLoadState('load');
  await page.getByRole('button', { name: /^Extend: / }).first().click();
  const toggle = page.locator('#web-search-results #sw-ai-filter-toggle');
  await expect(toggle).toHaveAttribute('aria-checked', 'true', { timeout: 10_000 });
  await toggle.click();
  await expect(page.locator('#web-search-results #sw-ai-filter-toggle')).toHaveAttribute('aria-checked', 'false', { timeout: 10_000 });
});

for (const theme of ['dark', 'light']) {
  test(`filter switch passes a11y scan (${theme} theme)`, async ({ page }) => {
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
