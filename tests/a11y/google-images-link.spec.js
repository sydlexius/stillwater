// google-images-link.spec.js - a11y + URL-construction coverage for the
// per-slot "Google Images search" deep link (#3223). Runs on both
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
  const link = page.getByRole('menuitem', { name: 'Google Images search' });
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
  const link = page.getByRole('menuitem', { name: 'Google Images search' });
  // #3223 review round 2, F3: this used to be a conditional
  // `if (count === 0) test.skip(...)`, which the repo's CLAUDE.md forbids --
  // a skip that reports green forever while verifying nothing is worse than
  // no test. seedGoogleImagesLinkArtist (seed-google-images-link.js) now
  // guarantees and asserts FanartExists/FanartCount>=1 at seed time
  // specifically so index=0 is always in range and this branch always
  // renders; a real regression here (in the seeder OR in the indexed-slot
  // rendering branch itself) must fail this test, not silently skip it.
  await expect(link, 'the indexed backdrop-slot Actions menu must render one Google Images link -- the fixture guarantees fanart_count >= 1').toHaveCount(1);

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
  //
  // #3223 review round 3, R1 (also raised independently by CodeRabbit):
  // fill() and click() used to race inside the same Promise.all -- nothing
  // guarantees fill() resolves before click() starts, so the submit could
  // fire on an empty #fetch-url-input. The handler returns early on an
  // empty url (`if (!url) return;`), so the click produces no fetch request
  // at all, and waitForRequest below would then hang until its timeout
  // instead of failing fast on the real defect. Await the fill to
  // completion FIRST, then race only waitForRequest against click (which is
  // the correct pattern: the request fires synchronously inside the click
  // handler, before click() itself resolves, so THAT race is safe).
  await page.locator('#fetch-url-input').fill('https://example.invalid/not-a-real-image.png');
  const [request] = await Promise.all([
    page.waitForRequest(req => req.url().includes('/images/fetch') && req.method() === 'POST'),
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

// #3223 review round 3, C2: the fetch-url-submit handler used to call
// r.json() unconditionally, so a non-OK response with a non-JSON or empty
// body made that Promise REJECT -- landing in the outer .catch and showing
// msgFetchFailed ("Fetch failed") instead of the code's own promised
// msgFetchUnable ("Unable to fetch image") fallback. These three cases route
// the REAL /images/fetch endpoint through page.route to a canned response
// (a real browser Response, not a mock object), which is the thing the
// dom-harness unit tests in tests/unit/image-search-fetch-slot.test.js
// cannot exercise -- those stub `window.fetch` directly; this proves the
// actual fetch()/Response machinery in a real browser behaves the same way.
test.describe('Google Images link: fetch-dialog error body parsing (#3223 review round 3, C2)', () => {
  async function openDialogAndSubmit(page, routeFulfill) {
    const link = await openActionsMenu(page, 'thumb');
    await page.route('**/api/v1/artists/*/images/fetch', route => route.fulfill(routeFulfill));

    const [popup] = await Promise.all([
      page.context().waitForEvent('page'),
      link.click(),
    ]);
    await popup.close();

    const modal = page.locator('#fetch-url-modal');
    await expect(modal).toBeVisible();
    await page.locator('#fetch-url-input').fill('https://example.invalid/not-a-real-image.png');
    await page.locator('#fetch-url-submit').click();
    // No waitForRequest here (unlike the indexed-slot test above): the
    // response is what's under test, and the route above intercepts before
    // the request reaches a real server, so waiting on the status line is
    // the meaningful synchronization point instead.
    return page.locator('#upload-status');
  }

  test('a 422 with a JSON {error: "X"} body shows X', async ({ page }) => {
    const status = await openDialogAndSubmit(page, {
      status: 422,
      contentType: 'application/json',
      body: JSON.stringify({ error: 'That link points to an SVG image, which Stillwater cannot use. Pick a PNG or JPG result instead.' }),
    });
    await expect(status).toHaveText('That link points to an SVG image, which Stillwater cannot use. Pick a PNG or JPG result instead.');
  });

  test('a 502 with an EMPTY body shows the msgFetchUnable text, not msgFetchFailed', async ({ page }) => {
    const status = await openDialogAndSubmit(page, { status: 502, contentType: 'text/plain', body: '' });
    await expect(status).toHaveText('Unable to fetch image');
  });

  test('a 502 with an HTML body does the same as an empty body', async ({ page }) => {
    const status = await openDialogAndSubmit(page, {
      status: 502,
      contentType: 'text/html',
      body: '<html><body><h1>502 Bad Gateway</h1></body></html>',
    });
    await expect(status).toHaveText('Unable to fetch image');
  });
});
