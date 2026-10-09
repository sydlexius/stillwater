// contrast.spec.js - Playwright a11y smoke set.
//
// Runs axe-core via @axe-core/playwright against an ephemeral Stillwater
// server (the `make test-a11y` target boots it). This tier catches computed-
// style violations -- especially color-contrast -- that jsdom cannot detect.
//
// Surfaces covered:
//   1. Dashboard (/next/)         - stat cards always visible, no interaction
//   2. Bulk-action bar            - artists list, strip visible at page load
//   3. Artwork modal              - seeded artist, modal opened on artist detail,
//                                   all four kinds, axe scan + painted-contrast sweep.
//                                   Primary and Backdrops are measured populated;
//                                   Logo and Banner in their EMPTY state only (the
//                                   fixture seeds neither), so their populated
//                                   state is NOT measured
//   4. Prefs drawer               - open via the prefs button on any next/ page
//
// Auth: beforeAll authenticates once via the API (setup + login) and stores
// the session cookie in Playwright's storageState for all tests.
//
// a11y rules: wcag2a + wcag2aa + color-contrast are ALL enabled here (real CSS).

import { test, expect } from 'playwright/test';

import { disableTransitions } from './helpers/settle.js';
import { buildAxeBuilder, formatViolations, applyTheme, restorePersistedTheme, renderedContrast } from './helpers/axe.js';
import { assertOnlyKnownViolations } from './helpers/known-violations.js';
import { seedArtworkModal, renderImageResults } from './helpers/seed-artwork-modal.js';

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

// Restore the SERVER-SIDE theme after every test in this file. Tests that
// exercise the real toggle path persist their change, and spec files run in
// alphabetical order, so an unrestored light theme becomes the starting state
// for every later file and breaks scans that never touched the theme.
test.afterEach(async ({ page }) => {
  await restorePersistedTheme(page);
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
// The modal is hidden by default. The spec opens the detail page of a SEEDED
// artist and opens the modal via a "Manage artwork" trigger, on each of its four
// kinds (primary, logo, banner, backdrops), in both themes. Logo and Banner are
// measured in their EMPTY state only: the fixture seeds a thumb and backdrops
// but no logo or banner, so the populated Logo/Banner state is not measured.
//
// FIXTURE (#3475): `make test-a11y` boots an empty database and an empty
// library, so there is no artist to open the modal on. helpers/seed-artwork-
// modal.js boots a throwaway server holding one artist with one thumb and
// three backdrops; nothing it creates is visible to any other spec. This test used to
// look for `a[href^="/next/artists/"]` (the harness renders `/artists/<id>`),
// found nothing, and took a conditional skip, so the scan never ran. Every
// precondition below is a hard assertion: an absent surface fails the test.
// ---------------------------------------------------------------------------

// The four kinds the modal manages, with the result fragment each one's "search
// providers" action returns and the selectors that prove it rendered. Primary,
// Logo and Banner share the image-card panel (#image-results); Backdrops has its
// own grid (#fanart-search-results). The scan and the painted sweep run over ALL
// of them: a defect one tab away from Primary is still a defect in the modal.
// Note the Logo and Banner entries run against their EMPTY state (the fixture
// seeds no logo or banner); the result cards are the same shared panel as
// Primary, but the populated current-image area is not measured for them.
const KINDS = [
  { kind: 'primary', fragment: 'images', cards: '#image-results [data-img-url]', count: 5, imgs: '#image-results img',
    needles: ['"Manage artwork"', '"Backdrops"', '"42 likes"', '"Unknown size"'] },
  { kind: 'logo', fragment: 'images', cards: '#image-results [data-img-url]', count: 5, imgs: '#image-results img',
    needles: ['"Manage artwork"', '"Backdrops"', '"42 likes"', '"Unknown size"', 'image yet'] },
  { kind: 'banner', fragment: 'images', cards: '#image-results [data-img-url]', count: 5, imgs: '#image-results img',
    needles: ['"Manage artwork"', '"Backdrops"', '"42 likes"', '"Unknown size"', 'image yet'] },
  { kind: 'backdrops', fragment: 'fanart', cards: '#fanart-search-results [data-img-url]', count: 2, imgs: '#fanart-search-results img',
    needles: ['"Manage artwork"', '"Backdrops"', '"42 likes"', '"1920x1080"', '"Crop"'] },
];

test.describe('artwork modal', () => {
  let fx;

  test.beforeAll(async () => {
    test.setTimeout(120_000);
    fx = await seedArtworkModal();
  });

  test.afterAll(() => {
    fx?.server.stop();
  });

  // openModal loads the seeded artist's detail page in the given theme, opens
  // the modal on the given kind, and returns once that kind's lazy-loaded body
  // has rendered. Each kind asserts its own fixture precondition on the live
  // page, so a missing seed fails here instead of scanning the wrong surface.
  async function openModal(page, theme, kind) {
    const { server, artistId } = fx;
    await page.context().addCookies([
      { name: 'session', value: server.sessionCookie, url: server.rootURL },
      { name: 'csrf_token', value: server.csrfToken, url: server.rootURL },
    ]);
    await page.goto(`${server.baseURL}/artists/${artistId}`);
    // 'networkidle' never completes while the SSE event stream is live.
    await page.waitForLoadState('load');
    await applyTheme(expect, page, theme);

    // Fixture precondition, asserted on the page itself: the artist-detail page
    // rendered for the seeded artist and offers an artwork trigger.
    await expect(page.locator('.sw-next-artist-detail'), 'fixture: artist detail page must render').toHaveCount(1);
    const openBtn = page.locator('[data-sw-artwork-open]:visible').first();
    await expect(openBtn, 'artwork open trigger must exist').toBeVisible();
    await openBtn.click();

    const modal = page.locator('#artwork-modal');
    await expect(modal, 'artwork modal must open').toBeVisible({ timeout: 10_000 });

    // The modal opens on Primary; any other kind is reached through its real tab.
    const tab = page.locator(`.sw-artwork-kind-tab[data-artwork-kind="${kind}"]`);
    if (kind !== 'primary') {
      await tab.click();
    }
    await expect(tab, `${kind} tab must be the pressed one`).toHaveAttribute('aria-pressed', 'true');

    // The body starts as a "Loading" placeholder and is swapped for the editor;
    // scanning before that scans the placeholder, not the surface.
    await expect(page.locator('#artwork-modal-body [data-context-menu]').first(), `${kind} editor must render`).toBeVisible({ timeout: 15_000 });
    const decoded = (img) => img.evaluate((el) => el.complete && el.naturalWidth > 0);

    if (kind === 'primary') {
      // The seeded artist's own thumb: the fixture's defining property.
      const current = page.locator('#artwork-modal-body img[src*="/images/thumb/file"]').first();
      await expect(current, 'modal must show the seeded artist thumb').toBeVisible({ timeout: 15_000 });
      await expect.poll(() => decoded(current), { message: 'seeded thumb must decode (naturalWidth > 0)' }).toBe(true);
    } else if (kind === 'backdrops') {
      // The seeded backdrops. This is what makes the fanart*.png seeds load-bearing:
      // with no backdrops the slot list is empty, there is no slot image and no
      // Crop / Fetch buttons, and this fails instead of scanning an empty tab.
      const slotImg = page.locator('#artwork-modal-body img[src*="/images/fanart/0/file"]').first();
      await expect(slotImg, 'fixture: seeded backdrop must render as backdrop slot 0').toBeVisible({ timeout: 15_000 });
      await expect.poll(() => decoded(slotImg), { message: 'seeded backdrop must decode (naturalWidth > 0)' }).toBe(true);
      // Three seeded backdrops: Crop + Fetch on each slot = 6 buttons.
      await expect(
        page.locator('#artwork-modal-body .fanart-slot-action-btn'),
        'fixture: each of the three backdrop slots must offer Crop and Fetch',
      ).toHaveCount(6);
      // Move-up on slots 1 and 2, move-down on slots 0 and 1 = 4. Without this
      // a fixture that stops producing them would leave those buttons unscanned
      // and the sweep would still pass quietly.
      await expect(
        page.locator('#artwork-modal-body .fanart-move-btn'),
        'fixture: three backdrops must render four move-up / move-down buttons',
      ).toHaveCount(4);
    } else {
      // The seed has no logo or banner: the kind must show its empty state
      // (this is the only state of Logo and Banner the spec measures).
      await expect(
        page.locator('#artwork-modal-body'),
        `fixture: ${kind} must render its empty state (the artist has none)`,
      ).toContainText(/image yet/i);
    }
    return modal;
  }

  // loadResultCards runs the editor's real "search providers" action and renders
  // genuine result markup into the kind's results panel. The harness is offline,
  // so the search response is the REAL server-rendered fragment (see fixtures/
  // render-image-results) served through page.route; every other step -- the
  // menu, the HTMX request and swap, the results panel -- is the live page. The
  // fragment carries a skipped and an errored provider, so the status banner
  // above the cards is on the page too.
  async function loadResultCards(page, k) {
    const { server, artistId } = fx;
    const fragment = renderImageResults(artistId, `${server.baseURL}/api/v1/artists/${artistId}/images/thumb/file`, k.fragment);
    await page.route('**/images/search**', (route) => route.fulfill({ status: 200, contentType: 'text/html', body: fragment }));
    await page.locator('#artwork-modal-body [data-context-menu] [aria-haspopup]').first().click();
    await page.getByRole('menuitem', { name: /search|fetch/i }).first().click();
    // The renderer emits a fixed card count; fewer means the swap did not take.
    await expect(page.locator(k.cards), `${k.kind} provider result cards must render`).toHaveCount(k.count, { timeout: 10_000 });
    await expect(page.locator('#artwork-modal [data-sw-providers-skipped]'), 'provider status banner (skipped) must render').toHaveCount(1);
    await expect(page.locator('#artwork-modal [data-sw-provider-errored]'), 'provider status banner (errored) must render').toHaveCount(1);
    await expect(page.locator(k.imgs).first(), 'card images must load').toBeVisible();
    await expect.poll(
      () => page.locator(k.imgs).evaluateAll((imgs) => imgs.every((i) => i.complete && i.naturalWidth > 0)),
      { message: 'card images must decode (a failed one swaps in a different placeholder)' },
    ).toBe(true);
  }

  for (const k of KINDS) {
    for (const theme of ['dark', 'light']) {
      // axe does not score text that is clipped out of the modal's scroll
      // viewport, so one pass at the top would skip the cards. Scan at the top,
      // the middle and the bottom of the modal's scroll range instead.
      test(`artwork modal passes a11y scan (${k.kind}, ${theme})`, async ({ page }) => {
        await openModal(page, theme, k.kind);
        await loadResultCards(page, k);
        // One merged entry per (rule id, node target): a node in view at two or
        // three stops would otherwise be reported once per stop and inflate the
        // count in the failure message. Pass/fail is unchanged (empty stays empty).
        const merged = new Map();
        for (const stop of [0, 0.5, 1]) {
          await page.locator('.sw-artwork-modal-surface').first().evaluate((el, f) => {
            el.scrollTop = (el.scrollHeight - el.clientHeight) * f;
          }, stop);
          const results = await buildAxeBuilder(page).include('#artwork-modal').analyze();
          for (const v of results.violations) {
            const entry = merged.get(v.id) || { ...v, nodes: [] };
            const seen = new Set(entry.nodes.map((n) => JSON.stringify(n.target)));
            for (const n of v.nodes) {
              if (!seen.has(JSON.stringify(n.target))) {
                seen.add(JSON.stringify(n.target));
                entry.nodes.push(n);
              }
            }
            merged.set(v.id, entry);
          }
        }
        const violations = [...merged.values()];
        expect(
          violations,
          `Artwork modal a11y violations (${k.kind}, ${theme}):\n${formatViolations(violations)}`,
        ).toHaveLength(0);
      });

      // The painted sweep is the only contrast check on content axe cannot
      // score: text outside the modal's scrolled viewport (the result cards sit
      // below the fold, and axe stayed green with their meta text at about
      // 2.5:1), and text over the modal's translucent surface, which axe
      // reports as "incomplete" (never pass/fail). It scores every visible text
      // element by its PAINTED pixels (helpers/axe.js renderedContrast), so do
      // not remove it as redundant with the scan above.
      test(`artwork modal text paints at AA (${k.kind}, ${theme})`, async ({ page }) => {
        await openModal(page, theme, k.kind);
        await loadResultCards(page, k);

        // Tag every visible element that owns a text node, then score each one.
        // NOT measured: the Actions menu items (closed again by loadResultCards),
        // the Compare panel, the Crop dialog, the Fetch-from-URL dialog, the
        // conflict-gate banner, the Revert row (all display:none until a user
        // action opens them), <option> elements, every hover / focus /
        // disabled state, and the POPULATED Logo and Banner state (current image
        // on the checkered background, the dimensions/size line, enabled Crop and
        // Delete): the fixture seeds neither, so those kinds are measured empty. ':visible' plus 'owns a text node' skips them, so a
        // green result here says nothing about those surfaces.
        const names = await page.locator('#artwork-modal *:visible').evaluateAll((els) => els
          .filter((e) => [...e.childNodes].some((n) => n.nodeType === 3 && n.textContent.trim()))
          .map((e, i) => {
            e.setAttribute('data-sw-paint-probe', String(i));
            return `${e.tagName.toLowerCase()} ${JSON.stringify(e.textContent.trim().slice(0, 30))}`;
          }));
        // Precondition: the sweep must reach the header, the tab labels, the
        // provider banner and the kind's cards or slot. A count far below this
        // means a surface failed to render, not that the page is clean.
        expect(names.length, `sweep found only ${names.length} text elements`).toBeGreaterThanOrEqual(20);
        for (const needle of [...k.needles, 'Not searched', 'could not be']) {
          expect(names.some((n) => n.includes(needle)), `sweep must include ${needle}`).toBe(true);
        }

        // Floor is 5.0, not the 4.5 AA minimum: Linux Firefox in CI paints thin
        // small text about 1.0 lower than macOS, so a local 4.6 is not safe. The
        // old text-gray-500 secondary text painted 4.70 here in the light theme.
        const FLOOR = 5.0;
        const failures = [];
        let lowest = Infinity;
        let lowestName = '';
        for (let i = 0; i < names.length; i++) {
          const el = page.locator(`[data-sw-paint-probe="${i}"]`);
          await el.evaluate((e) => e.scrollIntoView({ block: 'center' }));
          const ratio = await renderedContrast(page, el);
          if (ratio < lowest) {
            lowest = ratio;
            lowestName = names[i];
          }
          if (ratio < FLOOR) failures.push(`${names[i]} ${ratio.toFixed(2)}:1`);
        }
        console.log(`CONTRAST ${k.kind} ${theme}: ${names.length} elements, lowest ${lowest.toFixed(2)} (${lowestName})`);
        expect(failures, `${failures.length} modal element(s) painted under ${FLOOR}:1 (${k.kind}, ${theme}):\n${failures.join('\n')}`).toEqual([]);
      });
    }
  }
});

// ---------------------------------------------------------------------------
// 4. Prefs drawer
// ---------------------------------------------------------------------------

test('prefs drawer passes a11y scan', async ({ page }) => {
  await page.goto('/next/');
  // 'networkidle' never completes while the SSE event stream is live.
  await page.waitForLoadState('load');

  // Open the drawer through the real trigger (the sidebar Preferences link,
  // which the Ctrl+, shortcut also routes to). Both are hard requirements: a
  // missing trigger or a drawer that never opens fails the test rather than
  // skipping it (#3475).
  const trigger = page.locator('[data-sw-prefs-trigger]:visible').first();
  await expect(trigger, 'prefs trigger must exist').toBeVisible();
  await trigger.click();

  // The drawer is lazy-mounted over HTMX on first open and is open once its
  // aria-hidden flips to "false".
  const drawer = page.locator('#sw-prefs-drawer');
  await expect(drawer, 'prefs drawer must open').toHaveAttribute('aria-hidden', 'false', { timeout: 10_000 });

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
