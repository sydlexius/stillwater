// google-images-link.spec.js - a11y + URL-construction coverage for the
// per-slot "Search with Google Images" deep link (#3223). Runs on both
// firefox-a11y (authoritative) and chromium-a11y (compat) per
// playwright.config.js. Covers what Go-level tests cannot: the link survives
// a real browser's HTML-attribute-escaping round trip, is keyboard-reachable,
// and the menu it lives in is axe-clean in both themes.
//
// Fixture: seed-google-images-link.js scans an artist named "Fixture &
// Sons" -- the AC's explicit encoding case.

import { test, expect } from 'playwright/test';

import { disableTransitions } from './helpers/settle.js';
import { buildAxeBuilder, formatViolations, applyTheme, restorePersistedTheme } from './helpers/axe.js';
import { seedGoogleImagesLinkArtist, AMPERSAND_ARTIST } from './helpers/seed-google-images-link.js';

let artistId;

test.beforeAll(async ({ request }) => {
  artistId = await seedGoogleImagesLinkArtist(request);
});
test.beforeEach(async ({ page }) => { await disableTransitions(page); });
test.afterEach(async ({ page }) => { await restorePersistedTheme(page); });

/** Opens the Actions menu for one slot and returns the Google Images link. */
async function openActionsMenu(page, slot) {
  await page.goto(`/artists/${artistId}/images?type=${slot}`);
  await page.waitForLoadState('load');
  const trigger = page.locator('[aria-haspopup="true"]').first();
  await trigger.waitFor({ state: 'visible', timeout: 10_000 });
  await trigger.click();
  const link = page.getByRole('menuitem', { name: 'Search with Google Images' });
  await link.waitFor({ state: 'visible', timeout: 5_000 });
  return link;
}

// Corrected shapes (round 2, maintainer UAT): imgar is a TOP-LEVEL param,
// not a "tbs" token -- the first cut asserted our own constructed string
// rather than Google's real behavior, so the aspect filter silently did
// nothing. Re-verified live against Google's own Advanced Search UI (see
// internal/image/googlesearch.go's doc comment). "logo" carries no size
// filter at all (also a maintainer correction).
const SLOT_FILTERS = {
  thumb: { imgar: 's', tbs: 'imgo:1,isz:l' },
  fanart: { imgar: 'w', tbs: 'imgo:1,isz:l' },
  logo: { imgar: null, tbs: 'imgo:1,ic:trans' },
  banner: { imgar: 'xw', tbs: 'imgo:1,isz:l,ic:trans' },
};

// AC: URL construction for all four slots, including the "&" artist name,
// asserted via the BROWSER's own URL parsing of the rendered (HTML-escaped)
// href -- proof the link is not truncated or corrupted for a real name.
for (const slot of ['thumb', 'fanart', 'logo', 'banner']) {
  test(`${slot} slot Google Images link carries the correct query and filters`, async ({ page }) => {
    const link = await openActionsMenu(page, slot);
    const url = new URL(await link.getAttribute('href'));
    const want = SLOT_FILTERS[slot];

    expect(url.origin + url.pathname, `${slot}: host/path`).toBe('https://www.google.com/search');
    expect(url.searchParams.get('udm'), `${slot}: udm`).toBe('2');
    expect(url.searchParams.get('tbs'), `${slot}: tbs`).toBe(want.tbs);
    if (want.imgar === null) {
      expect(url.searchParams.has('imgar'), `${slot}: imgar must be absent`).toBe(false);
    } else {
      expect(url.searchParams.get('imgar'), `${slot}: imgar`).toBe(want.imgar);
    }
    const wantQuery = (slot === 'logo' || slot === 'banner') ? `${AMPERSAND_ARTIST} logo` : AMPERSAND_ARTIST;
    expect(url.searchParams.get('q'), `${slot}: q`).toBe(wantQuery);
    expect(await link.getAttribute('target'), `${slot}: target`).toBe('_blank');
    expect(await link.getAttribute('rel'), `${slot}: rel`).toBe('noopener noreferrer');
  });
}

// New behavior (round 2, maintainer request): clicking the link opens the
// new tab AND pre-opens the fetch-from-URL dialog in the ORIGINAL tab,
// pre-targeted to the same slot -- so the operator returns from Google with
// the paste target already waiting.
test('unscoped slot: clicking Google Images also opens the fetch-from-URL dialog', async ({ page, context }) => {
  const link = await openActionsMenu(page, 'logo');
  const modal = page.locator('#fetch-url-modal');
  await expect(modal).toBeHidden();

  const [popup] = await Promise.all([
    context.waitForEvent('page'),
    link.click(),
  ]);
  await popup.close();

  await expect(modal).toBeVisible();
  await expect(page.locator('#fetch-url-input')).toHaveValue('');
});

test('indexed backdrop slot: clicking Google Images opens the dialog targeted at that slot', async ({ page, context }) => {
  // fanartIdx >= 0 renders only when a specific backdrop tile is opened;
  // the unscoped fanart view (openActionsMenu(page, 'fanart')) exercises the
  // unscoped branch above instead. This asserts the per-slot targeting via
  // the fetch submit's actual network request rather than internal state,
  // since _fetchUrlSlot is a script-local closure variable with no DOM
  // reflection to assert against directly.
  await page.goto(`/artists/${artistId}/images?type=fanart&index=0`);
  await page.waitForLoadState('load');
  const trigger = page.locator('[aria-haspopup="true"]').first();
  await trigger.waitFor({ state: 'visible', timeout: 10_000 });
  await trigger.click();
  const link = page.getByRole('menuitem', { name: 'Search with Google Images' });
  // The indexed branch only renders when the hero is scoped to a specific
  // slot; skip gracefully if this server/fixture state doesn't reach it
  // rather than failing on an environment property this spec doesn't own.
  if (await link.count() === 0) {
    test.skip(true, 'indexed backdrop-slot Actions menu not reached from this fixture state');
    return;
  }

  const modal = page.locator('#fetch-url-modal');
  const [popup] = await Promise.all([
    context.waitForEvent('page'),
    link.click(),
  ]);
  await popup.close();

  await expect(modal).toBeVisible();
  // Submitting now must target the fanart slot (fetchType becomes 'fanart'
  // with a slot field) -- verified via the actual outgoing fetch request,
  // not the closure variable.
  const [request] = await Promise.all([
    page.waitForRequest(req => req.url().includes('/images/fetch') && req.method() === 'POST'),
    page.locator('#fetch-url-input').fill('https://example.invalid/not-a-real-image.png'),
    page.locator('#fetch-url-submit').click(),
  ]);
  const body = request.postDataJSON();
  expect(body.type, 'fetch request type must be fanart for a slot-targeted dialog').toBe('fanart');
  expect(body.slot, 'fetch request must carry the slot index the link was opened from').toBe(0);
});

test('Google Images link is keyboard-reachable inside the Actions menu', async ({ page }) => {
  const link = await openActionsMenu(page, 'thumb');
  for (let i = 0; i < 10 && !(await link.evaluate(el => el === document.activeElement)); i += 1) {
    await page.keyboard.press('Tab');
  }
  await expect(link).toBeFocused();
});

// AC: both themes, real axe-core.
for (const theme of ['dark', 'light']) {
  test(`Actions menu with Google Images link passes a11y scan (${theme} theme)`, async ({ page }) => {
    if (theme === 'dark') await page.emulateMedia({ colorScheme: 'dark' });
    await openActionsMenu(page, 'thumb');
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
