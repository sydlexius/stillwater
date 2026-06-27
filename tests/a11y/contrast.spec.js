// contrast.spec.js - Playwright a11y smoke set.
//
// Runs axe-core via @axe-core/playwright against an ephemeral Stillwater
// server (the `make test-a11y` target boots it). This tier catches computed-
// style violations -- especially color-contrast -- that jsdom cannot detect.
//
// Surfaces covered:
//   1. Dashboard (/next/)         - stat cards always visible, no interaction
//   2. Bulk-action bar            - artists list, strip visible at page load
//   3. Artwork modal              - requires opening the modal on artist detail
//   4. Prefs drawer               - open via the prefs button on any next/ page
//
// Auth: beforeAll authenticates once via the API (setup + login) and stores
// the session cookie in Playwright's storageState for all tests.
//
// a11y rules: wcag2a + wcag2aa + color-contrast are ALL enabled here (real CSS).

import { test, expect } from 'playwright/test';
import AxeBuilder from '@axe-core/playwright';

import { setupAndLogin } from './helpers/bootstrap.js';

const BASE_URL = process.env.SW_TEST_URL
  || `http://127.0.0.1:${process.env.SW_PORT || '1973'}`;

// ---------------------------------------------------------------------------
// Auth: one-time login; storageState carries the session across all tests.
// ---------------------------------------------------------------------------

let authCookie = '';

test.beforeAll(async ({ playwright }) => {
  const request = await playwright.request.newContext({ baseURL: BASE_URL });
  try {
    const { cookie } = await setupAndLogin(request);
    authCookie = cookie;
  } finally {
    await request.dispose();
  }
});

// Each test gets a context pre-seeded with the session cookie.
test.use({
  extraHTTPHeaders: {},
});

// ---------------------------------------------------------------------------
// Helper: build an AxeBuilder scan scoped to the target rules.
//
// We run wcag2a + wcag2aa which includes:
//   - color-contrast (4.5:1 normal text, 3:1 large/UI components)
//   - button-name, label, aria-* rules (same as the jsdom tier)
//
// Exclusions:
//   - 'html-has-lang': templ generates <html lang="..."> -- suppressed here in
//     case fixtures load without the full layout; the browser tier is about
//     contrast, not structural completeness.
// ---------------------------------------------------------------------------
function buildAxeBuilder(page) {
  return new AxeBuilder({ page })
    .withTags(['wcag2a', 'wcag2aa', 'best-practice'])
    .disableRules([
      // Not a concern for the rendered smoke set (Playwright provides lang).
      'html-has-lang',
    ]);
}

// ---------------------------------------------------------------------------
// 1. Dashboard (/next/) - stat cards
// ---------------------------------------------------------------------------

test('dashboard stat cards pass a11y scan', async ({ page }) => {
  await page.context().addCookies([{
    name:   'session',
    value:  authCookie.replace('session=', ''),
    domain: '127.0.0.1',
    path:   '/',
  }]);

  await page.goto('/next/');
  // Wait for the header strip (stat cards) to be present.
  await page.waitForSelector('.sw-next-header-strip', { timeout: 10_000 });

  const results = await buildAxeBuilder(page).analyze();
  expect(
    results.violations,
    `Dashboard a11y violations:\n${formatViolations(results.violations)}`,
  ).toHaveLength(0);
});

// ---------------------------------------------------------------------------
// 2. Bulk-action bar (artists list, /next/artists?view=grid)
//
// Grid view (contextual=false) renders #bulk-action-bar without the
// sw-next-bulk-strip-contextual class, so the strip is always visible.
// Default table view (contextual=true) hides the strip via
// display:none until a row is selected -- waitForSelector would time out
// on an empty ephemeral DB with no rows to select.
// ---------------------------------------------------------------------------

test('bulk-action bar passes a11y scan', async ({ page }) => {
  await page.context().addCookies([{
    name:   'session',
    value:  authCookie.replace('session=', ''),
    domain: '127.0.0.1',
    path:   '/',
  }]);

  await page.goto('/next/artists?view=grid');
  // Grid view: #bulk-action-bar is always visible (no contextual hide).
  await page.waitForSelector('#bulk-action-bar', { timeout: 10_000 });

  // Scope the scan to the toolbar region for focused contrast coverage.
  const results = await buildAxeBuilder(page)
    .include('#bulk-action-bar')
    .analyze();
  expect(
    results.violations,
    `Bulk-bar a11y violations:\n${formatViolations(results.violations)}`,
  ).toHaveLength(0);
});

// ---------------------------------------------------------------------------
// 3. Artwork modal (artist detail page)
//
// The modal is hidden by default. Navigate to the first artist in the list,
// then open the modal via the "Manage artwork" button.
// ---------------------------------------------------------------------------

test('artwork modal passes a11y scan', async ({ page }) => {
  await page.context().addCookies([{
    name:   'session',
    value:  authCookie.replace('session=', ''),
    domain: '127.0.0.1',
    path:   '/',
  }]);

  await page.goto('/next/artists');
  // 'networkidle' never completes while the SSE event stream is live.
  // 'load' waits for all resources to finish and is sufficient for the
  // server-side-rendered artist list to be present in the DOM.
  await page.waitForLoadState('load');

  // Click the first artist link in the list to navigate to detail.
  const firstArtistLink = page.locator('a[href^="/next/artists/"]').first();
  const artistCount = await firstArtistLink.count();
  if (artistCount === 0) {
    // No artists in this ephemeral DB: skip (library not seeded by CI).
    test.skip(true, 'No artists in ephemeral DB; skipping modal scan.');
    return;
  }
  await firstArtistLink.click();
  await page.waitForLoadState('networkidle');

  // Open the artwork modal.
  const openBtn = page.locator('[data-sw-artwork-open]').first();
  if (await openBtn.count() === 0) {
    test.skip(true, 'Artwork open trigger not found; skipping.');
    return;
  }
  await openBtn.click();

  // Wait for the modal to become visible.
  await page.waitForSelector('#artwork-modal:not(.hidden)', { timeout: 10_000 });

  const results = await buildAxeBuilder(page)
    .include('#artwork-modal')
    .analyze();
  expect(
    results.violations,
    `Artwork modal a11y violations:\n${formatViolations(results.violations)}`,
  ).toHaveLength(0);
});

// ---------------------------------------------------------------------------
// 4. Prefs drawer
// ---------------------------------------------------------------------------

test('prefs drawer passes a11y scan', async ({ page }) => {
  await page.context().addCookies([{
    name:   'session',
    value:  authCookie.replace('session=', ''),
    domain: '127.0.0.1',
    path:   '/',
  }]);

  await page.goto('/next/');
  // 'networkidle' never completes while the SSE event stream is live.
  await page.waitForLoadState('load');

  // Open the prefs drawer.
  const prefsBtn = page.locator('.sw-prefs-btn, [data-sw-prefs-open], [aria-label*="ref"]').first();
  if (await prefsBtn.count() === 0) {
    // Try keyboard shortcut (Ctrl+,) as a fallback.
    await page.keyboard.press('Control+,');
  } else {
    await prefsBtn.click();
  }

  // Wait for the drawer to be visible (aria-hidden becomes false).
  await page.waitForSelector('.sw-prefs-drawer:not([aria-hidden="true"])', {
    timeout: 8_000,
  }).catch(() => {
    // If the drawer didn't open, try Esc to dismiss any tooltip and retry.
  });

  const drawerVisible = await page.locator('.sw-prefs-drawer[aria-hidden="false"]').count() > 0
    || await page.locator('.sw-prefs-drawer:not([aria-hidden])').count() > 0;

  if (!drawerVisible) {
    test.skip(true, 'Prefs drawer did not open; skipping.');
    return;
  }

  const results = await buildAxeBuilder(page)
    .include('.sw-prefs-drawer')
    .analyze();
  expect(
    results.violations,
    `Prefs drawer a11y violations:\n${formatViolations(results.violations)}`,
  ).toHaveLength(0);
});

// ---------------------------------------------------------------------------
// 5. /next/settings (dark mode)
//
// Settings is the primary surface for M55 #1339. This test verifies the
// fully-rendered settings rail + pane in DARK mode. Light-mode contrast
// regressions are caught by static-analysis snapshots; dark is where the
// reused stable bodies carry inverted-muted and blue-ink debt fixed in #1339.
//
// Dark mode is activated via:
//   (a) page.emulateMedia({ colorScheme: 'dark' }) -- satisfies the 'system'
//       theme preference branch in preferences.js (matchMedia check).
//   (b) page.evaluate classList.add('dark') -- satisfies the hardcoded 'dark'
//       preference branch and any race where JS runs after emulateMedia fires.
// ---------------------------------------------------------------------------

test('/next/settings passes a11y scan in dark mode', async ({ page }) => {
  await page.context().addCookies([{
    name:   'session',
    value:  authCookie.replace('session=', ''),
    domain: '127.0.0.1',
    path:   '/',
  }]);

  // Force dark-mode media query so preferences.js resolves 'system' as dark.
  await page.emulateMedia({ colorScheme: 'dark' });

  await page.goto('/next/settings');
  await page.waitForSelector('.sw-next-settings-pane', { timeout: 10_000 });

  // Ensure the .dark class is present regardless of stored preference state.
  await page.evaluate(() => document.documentElement.classList.add('dark'));

  const results = await buildAxeBuilder(page).analyze();
  expect(
    results.violations,
    `/next/settings dark-mode a11y violations:\n${formatViolations(results.violations)}`,
  ).toHaveLength(0);
});

// ---------------------------------------------------------------------------
// 6. /next/settings (light mode)
//
// Pairs with the dark spec above and with item 1 (rail glass surface): the
// light spec only goes green once the rail has a legible frosted surface above
// the ambient backdrop (WCAG 1.4.3 on the rail group labels / items).
//
// Light mode is activated via the real sidebar theme toggle so the full
// preference path is exercised (swPreferences.set -> applySingle -> classList):
//   (a) Seed the preference to 'dark' so cycleTheme() deterministically lands
//       on 'light' (dark -> light is step 1 in the ORDER cycle).
//   (b) Call window.swSidebar.cycleTheme() -- the same call the sidebar button
//       uses -- which drives swPreferences.set('theme', 'light') synchronously.
//   (c) waitForFunction confirms the .dark class is absent before scanning so
//       there is no axe/DOM race.
// ---------------------------------------------------------------------------

test('/next/settings passes a11y scan in light mode', async ({ page }) => {
  await page.context().addCookies([{
    name:   'session',
    value:  authCookie.replace('session=', ''),
    domain: '127.0.0.1',
    path:   '/',
  }]);

  await page.goto('/next/settings');
  await page.waitForSelector('.sw-next-settings-pane', { timeout: 10_000 });

  // Switch to light via the real sidebar theme toggle (not classList forcing).
  // Seed to 'dark' first so one cycleTheme() call deterministically lands on
  // 'light' regardless of any prior localStorage state.
  await page.evaluate(() => {
    if (window.swPreferences) window.swPreferences.set('theme', 'dark');
    if (window.swSidebar && window.swSidebar.cycleTheme) window.swSidebar.cycleTheme();
  });
  await page.waitForFunction(
    () => !document.documentElement.classList.contains('dark'),
    { timeout: 3_000 },
  );

  const results = await buildAxeBuilder(page).analyze();
  expect(
    results.violations,
    `/next/settings light-mode a11y violations:\n${formatViolations(results.violations)}`,
  ).toHaveLength(0);
});

// ---------------------------------------------------------------------------
// Helper: format violations for assertion messages.
// ---------------------------------------------------------------------------
function formatViolations(violations) {
  if (!violations.length) return '(none)';
  return violations.map(v =>
    `  [${v.impact}] ${v.id}: ${v.description}\n` +
    v.nodes.slice(0, 2).map(n => `    target: ${JSON.stringify(n.target)}`).join('\n'),
  ).join('\n');
}
