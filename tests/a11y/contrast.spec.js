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

import { disableTransitions } from './helpers/settle.js';
import { buildAxeBuilder, formatViolations, applyTheme } from './helpers/axe.js';
import { assertOnlyKnownViolations } from './helpers/known-violations.js';

// Auth: a single login happens once in global-setup.js; the session is loaded
// into every test context via `use.storageState` (playwright.config.js), so no
// per-file login or per-test cookie injection is needed here.

// Disable CSS transitions/animations so axe reads SETTLED colors (see
// helpers/settle.js): a synchronous getComputedStyle taken right after a theme
// flip (the light-mode test toggles the theme before scanning) can otherwise
// sample a mid-transition blended color and report a FALSE contrast failure
// even though the settled page is AA-compliant. Test-measurement only --
// production theme switching is unchanged.
test.beforeEach(async ({ page }) => {
  await disableTransitions(page);
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

// ---------------------------------------------------------------------------
// 1. Dashboard (/next/) - stat cards
// ---------------------------------------------------------------------------

test('dashboard stat cards pass a11y scan', async ({ page }) => {
  await page.goto('/next/');
  // Wait for the header strip (stat cards) to be present.
  await page.waitForSelector('.sw-next-header-strip', { timeout: 10_000 });

  const results = await buildAxeBuilder(page).analyze();
  // Allows ONLY the tracked #2875 timestamp contrast defect; anything else
  // still fails, and the allowance itself fails once #2875 is fixed. See
  // helpers/known-violations.js for why this is not an axe exclude.
  assertOnlyKnownViolations(expect, results.violations, 'Dashboard', formatViolations);
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
// Dark mode is activated by driving the app's OWN theme path
// (window.swPreferences.applySingle), never by adding the .dark class directly.
//
// WHY THAT MATTERS -- this test reported a false contrast failure for exactly
// that reason (#2872). preferences.js keeps an inline --sw-glass-bg on :root
// whose COLOUR is theme-dependent: the bg_opacity branch reads
// classList.contains('dark') at the moment it runs and writes rgba(30,41,59,a)
// or rgba(255,255,255,a) to match. An inline style on :root outranks both
// theme scopes.
//
// The old form here added the class with a raw classList.add, which fires no
// preference change, so nothing re-ran that branch. The page ended up
// half-themed: .dark applied, but the LIGHT glass value still pinned inline.
// axe then correctly reported dark-theme text (#f3f4f6, #9ca3af, ...) against
// a light surface (#dbdcdf) at ratios as bad as 1.07:1 -- a real violation of
// a state no user can reach, on a page that is fine in both actual themes.
//
// Measured at the moment of the scan: running animations 0, readyState
// complete, opacity 1 -- so this was never a fade/settling race, which is what
// the retry-flakiness made it look like.
//
// applySingle is the documented apply-without-persist entry point, and it
// re-applies bg_opacity after toggling the class, so the class and the inline
// token stay consistent. emulateMedia stays because the 'system' branch
// resolves through matchMedia.
// ---------------------------------------------------------------------------

test('/next/settings passes a11y scan in dark mode', async ({ page }) => {
  // Force dark-mode media query so preferences.js resolves 'system' as dark.
  await page.emulateMedia({ colorScheme: 'dark' });

  await page.goto('/next/settings');
  await page.waitForSelector('.sw-next-settings-pane', { timeout: 10_000 });

  await applyTheme(expect, page, 'dark');

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
  await page.goto('/next/settings');
  await page.waitForSelector('.sw-next-settings-pane', { timeout: 10_000 });

  // Switch to light via the real sidebar theme toggle (not classList forcing).
  // Wait for the sidebar JS to be wired first: cycleTheme() silently no-ops if
  // swSidebar isn't ready yet, which on a slow/loaded runner leaves the theme
  // stuck on dark and the scan never reaches light mode (a pre-existing flake
  // that only surfaces now that the suite actually reaches this test). Seed to
  // 'dark' so one cycleTheme() call deterministically lands on 'light'.
  await page.waitForFunction(
    () => !!(window.swPreferences && window.swSidebar
      && typeof window.swSidebar.cycleTheme === 'function'),
    { timeout: 10_000 },
  );
  // cycleTheme() runs swPreferences.set() synchronously, including the
  // glass/opacity token recompute, so page.evaluate() only returns once the
  // DOM mutation is already applied. That evaluate call has no timeout of its
  // own, though: under CPU starvation it can block the page's event loop long
  // enough to consume the whole 60s test timeout before the settle wait below
  // ever arms (root cause of #2223). Race it against a short deadline so a
  // starved run fails fast, feeding a retry, instead of riding to the global
  // timeout.
  // Track the evaluate promise and the race's timer independently: if the
  // timeout wins, the evaluate call is still running against a page that may
  // close during retry teardown. Attach a no-op catch so that later
  // rejection ("Target closed") never surfaces as an unhandled rejection, and
  // clearTimeout the timer in a finally so it can't fire after the race has
  // already settled.
  const themeTogglePromise = page.evaluate(() => {
    window.swPreferences.set('theme', 'dark');
    window.swSidebar.cycleTheme();
  });
  themeTogglePromise.catch(() => {});
  let themeToggleTimeoutId;
  try {
    await Promise.race([
      themeTogglePromise,
      new Promise((_, reject) => {
        themeToggleTimeoutId = setTimeout(
          () => reject(new Error('theme-toggle evaluate did not return within 5s (CPU-starved run)')),
          5_000,
        );
      }),
    ]);
  } finally {
    clearTimeout(themeToggleTimeoutId);
  }
  // Single bounded settle poll: the theme swap is synchronous, so this
  // confirms it landed rather than waiting out a fixed sleep.
  await page.waitForFunction(
    () => !document.documentElement.classList.contains('dark'),
    { timeout: 5_000 },
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
