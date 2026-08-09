// nfo-mbid.spec.js - Playwright a11y coverage for the rule-written
// MusicBrainz ID report pane (#2809), route /reports/nfo-has-mbid.
//
// Runs on BOTH projects declared in playwright.config.js:
//   - firefox-a11y  (authoritative: Firefox is the target browser)
//   - chromium-a11y (compatibility)
//
// Surfaces covered:
//   1. Full-page axe-core scan, dark AND light theme.
//   2. The caveat band is genuinely VISIBLE -- not just present in the DOM.
//      This is the substance of #2809's reopening: a caveat existing in a
//      JSON field or hidden behind a closed disclosure does not meet the
//      "surfaced somewhere an operator can act on it" bar. A source-only or
//      selector-only check cannot prove visibility; this spec measures the
//      rendered box model and computed style.
//   3. The rail entry and reports-rail keyboard reachability.
//
// This pane is READ-ONLY end to end (no restore, no bulk selection, no write
// path) -- issue #2810 covers validating or reverting a rule-picked ID -- so
// there is no destructive-flow keyboard test here, unlike blast-radius.spec.js.

import { test, expect } from 'playwright/test';

import { disableTransitions } from './helpers/settle.js';
import { buildAxeBuilder, formatViolations, applyTheme, restorePersistedTheme } from './helpers/axe.js';

const PANE_URL = '/reports/nfo-has-mbid';

test.beforeEach(async ({ page }) => {
  await disableTransitions(page);
});

test.afterEach(async ({ page }) => {
  await restorePersistedTheme(page);
});

// gotoPane navigates and waits for the pane's own markup, mirroring
// blast-radius.spec.js's helper. 'networkidle' is never used: the SSE event
// stream keeps the connection open forever.
async function gotoPane(page) {
  await page.goto(PANE_URL);
  await page.waitForLoadState('load');
  await page.waitForSelector('#nfo-mbid-tbl', { timeout: 10_000 });
}

// ---------------------------------------------------------------------------
// 1. axe-core, full page, both themes.
// ---------------------------------------------------------------------------

test('nfo-has-mbid pane passes full-page a11y scan (dark theme)', async ({ page }) => {
  await page.emulateMedia({ colorScheme: 'dark' });
  await gotoPane(page);
  await applyTheme(expect, page, 'dark');

  const results = await buildAxeBuilder(page).analyze();
  expect(
    results.violations,
    `nfo-has-mbid dark-theme a11y violations:\n${formatViolations(results.violations)}`,
  ).toHaveLength(0);
});

test('nfo-has-mbid pane passes full-page a11y scan (light theme)', async ({ page }) => {
  await gotoPane(page);

  await page.waitForFunction(
    () => !!(window.swPreferences && window.swSidebar
      && typeof window.swSidebar.cycleTheme === 'function'),
    { timeout: 10_000 },
  );
  // applySingle, NOT set: `set` PERSISTS the preference server-side, which
  // would leak light mode into every test that runs after this file --
  // exactly the cross-file leak blast-radius.spec.js documents hitting it
  // through the same mechanism.
  const holdLight = (message) => expect.poll(
    async () => page.evaluate(() => {
      if (document.documentElement.classList.contains('dark')) {
        window.swPreferences.applySingle('theme', 'light');
        return false;
      }
      return true;
    }),
    { message, timeout: 15_000, intervals: [200] },
  ).toBe(true);

  await holdLight('page never settled on the light theme');
  await page.waitForTimeout(750);
  await holdLight('theme kept reverting to dark');

  const themeState = () => page.evaluate(() => {
    const probe = document.querySelector('tbody') || document.body;
    const bg = getComputedStyle(probe).backgroundColor;
    const cv = document.createElement('canvas');
    cv.width = cv.height = 1;
    const c = cv.getContext('2d');
    c.fillStyle = '#ffffff';
    c.fillRect(0, 0, 1, 1);
    c.fillStyle = bg;
    c.fillRect(0, 0, 1, 1);
    const [r, g, b] = Array.from(c.getImageData(0, 0, 1, 1).data);
    const f = v => { v /= 255; return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4); };
    return {
      dark: document.documentElement.classList.contains('dark'),
      bg,
      luminance: 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b),
    };
  });

  const before = await themeState();
  expect(before.dark, `theme reverted to dark before the light-theme scan: ${JSON.stringify(before)}`).toBe(false);

  const results = await buildAxeBuilder(page).analyze();

  const after = await themeState();
  expect(after.dark, `theme reverted to dark DURING the light-theme scan: ${JSON.stringify(after)}`).toBe(false);
  expect(
    after.luminance,
    `light-theme scan measured a DARK surface (bg=${after.bg}); the theme switch did not reach the rendered page`,
  ).toBeGreaterThan(0.5);

  expect(
    results.violations,
    `nfo-has-mbid light-theme a11y violations:\n${formatViolations(results.violations)}`,
  ).toHaveLength(0);
});

// ---------------------------------------------------------------------------
// 2. The caveat band is genuinely VISIBLE. THE substantive test for #2809.
//
// A static/source check can only prove the text exists SOMEWHERE in the
// response; it cannot prove an operator looking at the rendered page can
// read it without extra interaction. This measures the actual box model:
// non-zero size, not display:none/visibility:hidden, and rendered above the
// fold enough to appear without scrolling past the table -- specifically NOT
// inside a collapsed <details>, an aria-hidden container, or an
// off-screen-positioned element (all of which a naive "add the text
// somewhere in the DOM" fix could satisfy).
// ---------------------------------------------------------------------------

test('caveat band is genuinely visible, not collapsed or hidden', async ({ page }) => {
  await gotoPane(page);

  // Locate by role WITHIN the pane, matching repPaneNFOHasMBID's
  // role="note" markup. Scoped to #nfo-mbid-results' preceding sibling via
  // the page's #sw-rep-pane container rather than a bare page.getByRole:
  // the shared page footer (.sw-list-tips) also carries role="note" for an
  // unrelated keyboard-shortcuts tip, so an unscoped query matches two
  // elements and this assertion must not depend on which one axe or
  // Playwright happens to resolve first.
  const caveat = page.locator('#sw-rep-pane [role="note"]');
  await expect(caveat).toHaveCount(1);
  await expect(caveat).toBeVisible();

  const box = await caveat.boundingBox();
  if (!box) {
    throw new Error('caveat band has no bounding box; it is not actually rendered on the page');
  }
  expect(box.width, 'caveat band has zero rendered width').toBeGreaterThan(0);
  expect(box.height, 'caveat band has zero rendered height').toBeGreaterThan(0);

  // Not inside a closed <details> anywhere on the page. Playwright's
  // isVisible() already returns false for content inside a closed <details>
  // in Chromium/Firefox, so the toBeVisible() assertion above would already
  // catch this -- this second check names the SPECIFIC forbidden pattern so
  // a future regression reads as "you added a details wrapper" rather than
  // a bare "not visible".
  const insideClosedDetails = await page.evaluate(() => {
    // Scoped to the caveat band's own id, not a bare [role="note"]: the
    // shared .sw-list-tips page footer also carries role="note" for an
    // unrelated keyboard-shortcuts tip, and it happens to follow the caveat
    // band in DOM order today -- an unscoped query would silently start
    // inspecting the footer instead if the template were ever reordered.
    const el = document.querySelector('#nfo-mbid-caveats');
    if (!el) return null;
    let node = el.parentElement;
    while (node) {
      if (node.tagName === 'DETAILS' && !node.open) return true;
      node = node.parentElement;
    }
    return false;
  });
  expect(insideClosedDetails, 'caveat band is nested inside a closed <details> element').toBe(false);

  // The four substantive caveats named in the issue reopening must all be
  // present in the visible text, not merely somewhere in the markup (an
  // aria-hidden duplicate would pass a raw textContent check while being
  // invisible to a sighted operator).
  const visibleText = await caveat.innerText();
  const mustContain = [
    'automatic NFO rule fix only',      // scope
    'minimum',                          // floor
    'never overwrote an ID',            // no-prior-value
    'not been confirmed by anyone',     // not-confirmed
  ];
  for (const phrase of mustContain) {
    expect(visibleText, `caveat band visible text is missing "${phrase}"`).toContain(phrase);
  }
});

// ---------------------------------------------------------------------------
// 3. Reports rail: the entry exists and is keyboard-reachable.
// ---------------------------------------------------------------------------

test('reports rail lists and links to the nfo-has-mbid report', async ({ page }) => {
  await page.goto('/reports');
  await page.waitForSelector('#sw-rep-list', { timeout: 10_000 });

  const link = page.locator('a[href="/reports/nfo-has-mbid"]');
  await expect(link).toHaveCount(1);

  await link.focus();
  await expect(link).toBeFocused();
  await page.keyboard.press('Enter');
  await page.waitForSelector('#nfo-mbid-tbl', { timeout: 10_000 });
  await expect(page).toHaveURL(/\/reports\/nfo-has-mbid$/);
});
