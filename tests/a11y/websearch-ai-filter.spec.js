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

test('a transport failure of the re-search shows the refresh-failed notice', async ({ page }) => {
  const toggle = await triggerWebSearch(page);
  await page.route('**/images/websearch**', (route) => route.abort('failed'));
  await toggle.click();
  await expect(page.locator('[data-sw-ai-filter-refresh-failed]')).toBeVisible({ timeout: 10_000 });
});

test('the switch is disabled while another trigger searches the same panel', async ({ page }) => {
  await triggerWebSearch(page);
  let release;
  const held = new Promise((r) => { release = r; });
  await page.route('**/images/websearch**', async (route) => { await held; return route.continue(); });
  await page.locator('[aria-haspopup="true"]').first().click();
  await page.getByRole('menuitem', { name: 'Web Search' }).click();
  await expect(page.locator('#sw-ai-filter-toggle')).toBeDisabled();
  release();
  await expect(page.locator('#sw-ai-filter-toggle')).toBeEnabled({ timeout: 10_000 });
});

// #3288: another search can swap the panel while the switch's save is in
// flight. The save holds the PUT open, a second search (Actions menu) swaps in
// a fresh switch rendered from the pre-save server value, then the PUT is
// released. Returns the pieces both specs assert on.
async function saveWithSearchSwappedMidSave(page, { failReSearch }) {
  const toggle = await triggerWebSearch(page);
  let releasePut;
  const putHeld = new Promise((r) => { releasePut = r; });
  await page.route('**/api/v1/preferences/filter_ai_images', async (route) => {
    if (route.request().method() !== 'PUT') return route.continue();
    await putHeld;
    return route.continue();
  });
  let searches = 0;
  await page.route('**/images/websearch**', (route) => {
    searches += 1;
    // Search 1 is the other trigger (let it swap); later ones are the switch's re-search.
    if (failReSearch && searches > 1) return route.fulfill({ status: 500, body: 'boom' });
    return route.continue();
  });

  // A locator re-resolves by id, so pin the ORIGINAL node to prove it is detached later.
  await toggle.evaluate((el) => { window.__oldAIFilterSwitch = el; });
  await toggle.click();
  await expect(toggle).toBeDisabled();
  const swapped = page.waitForResponse((r) => r.url().includes('/images/websearch'));
  await page.locator('[aria-haspopup="true"]').first().click();
  await page.getByRole('menuitem', { name: 'Web Search' }).click();
  await swapped;
  // The fixture's defining property: the switch on screen is a NEW node (the
  // old button is detached) showing the server's pre-save value.
  await expect.poll(() => page.evaluate(() => window.__oldAIFilterSwitch.isConnected)).toBe(false);
  const live = page.locator('#sw-ai-filter-toggle');
  await expect(live).toHaveAttribute('aria-checked', 'true');
  return { live, releasePut };
}

test('a switch swapped in by another search mid-save stays disabled until the save finishes (#3288)', async ({ page }) => {
  const { live, releasePut } = await saveWithSearchSwappedMidSave(page, { failReSearch: false });
  await expect(live).toBeDisabled();
  const after = [];
  page.on('request', (req) => { if (req.url().includes('/images/websearch')) after.push(req.url()); });
  releasePut();
  await expect(page.locator('#sw-ai-filter-toggle')).toHaveAttribute('aria-checked', 'false', { timeout: 10_000 });
  await expect(page.locator('#sw-ai-filter-toggle')).toBeEnabled();
  // htmx silently drops a re-search issued from a detached source node.
  expect(after, 'searches after the save resolved').toHaveLength(1);
});

test('after a mid-save swap and a failed re-search, the switch shows the value the server holds (#3288)', async ({ page, request }) => {
  const { releasePut } = await saveWithSearchSwappedMidSave(page, { failReSearch: true });
  const failed = page.waitForResponse((r) => r.url().includes('/images/websearch') && r.status() === 500);
  releasePut();
  await failed;
  await expect(page.locator('[data-sw-ai-filter-refresh-failed]')).toBeVisible({ timeout: 10_000 });
  await expect(page.locator('#sw-ai-filter-toggle')).toBeEnabled({ timeout: 10_000 });
  const saved = await apiFetch(request, 'GET', '/api/v1/preferences/filter_ai_images');
  expect((await saved.json()).value).toBe('false');
  await expect(page.locator('#sw-ai-filter-toggle')).toHaveAttribute('aria-checked', 'false');
});

// #3288: the other search's response is rendered BEFORE the save (so it shows
// the old value) but lands AFTER the save resolved, while the re-search (which
// then fails) is still pending. Only the final repaint can make the switch
// show the saved value.
for (const reStatus of [500, 200]) {
test(`a stale other-search response landing after the save, re-search ${reStatus} (#3288)`, async ({ page, request }) => {
  const toggle = await triggerWebSearch(page);
  let releasePut; const putHeld = new Promise((r) => { releasePut = r; });
  let releaseOther; const otherHeld = new Promise((r) => { releaseOther = r; });
  let releaseRe; const reHeld = new Promise((r) => { releaseRe = r; });
  let n = 0;
  let otherFetched; const otherReady = new Promise((r) => { otherFetched = r; });
  await page.route('**/api/v1/preferences/filter_ai_images', async (route) => {
    if (route.request().method() !== 'PUT') return route.continue();
    await putHeld;
    return route.continue();
  });
  await page.route('**/images/websearch**', async (route) => {
    n += 1;
    if (n === 1) {
      const resp = await route.fetch(); // rendered now: filter still on
      expect(await resp.text(), 'held body is the stale render').toContain('aria-checked="true"');
      otherFetched();
      await otherHeld;
      return route.fulfill({ response: resp });
    }
    await reHeld;
    if (reStatus === 200) return route.continue();
    return route.fulfill({ status: 500, body: 'boom' });
  });
  await toggle.evaluate((el) => { window.__oldAIFilterSwitch = el; });
  await toggle.click();
  await page.locator('[aria-haspopup="true"]').first().click();
  await page.getByRole('menuitem', { name: 'Web Search' }).click();
  await otherReady;
  const putDone = page.waitForResponse((r) => r.url().includes('/preferences/filter_ai_images') && r.request().method() === 'PUT');
  releasePut();
  await putDone;
  await expect.poll(() => n).toBe(2); // the re-search is issued and held
  releaseOther(); // the stale swap lands after the save resolved
  await expect.poll(() => page.evaluate(() => window.__oldAIFilterSwitch.isConnected)).toBe(false);
  // The stale render showed the old value; the swapped-in switch is repainted
  // from the saved one at once (#3296) and held disabled while re-search runs.
  await expect(page.locator('#sw-ai-filter-toggle')).toHaveAttribute('aria-checked', 'false');
  await expect(page.locator('#sw-ai-filter-toggle')).toBeDisabled(); // re-search still pending
  releaseRe();
  await expect(page.locator('#sw-ai-filter-toggle')).toBeEnabled({ timeout: 10_000 });
  // A successful re-search is not reported as failed even though a stale swap
  // replaced the panel while it was in flight.
  await expect(page.locator('[data-sw-ai-filter-refresh-failed]')).toHaveCount(reStatus === 200 ? 0 : 1);
  const saved = await apiFetch(request, 'GET', '/api/v1/preferences/filter_ai_images');
  expect((await saved.json()).value).toBe('false');
  await expect(page.locator('#sw-ai-filter-toggle')).toHaveAttribute('aria-checked', 'false');
});
}

// #3296: same stale response, but it lands after the save AND the re-search
// have both finished, when no handler is left running. The swapped-in switch
// was rendered from the pre-save value and must still show the saved one.
test('a stale other-search response landing after save and re-search keeps the saved value (#3296)', async ({ page, request }) => {
  const toggle = await triggerWebSearch(page);
  let releasePut; const putHeld = new Promise((r) => { releasePut = r; });
  let releaseOther; const otherHeld = new Promise((r) => { releaseOther = r; });
  let otherFetched; const otherReady = new Promise((r) => { otherFetched = r; });
  let n = 0;
  await page.route('**/api/v1/preferences/filter_ai_images', async (route) => {
    if (route.request().method() !== 'PUT') return route.continue();
    await putHeld;
    return route.continue();
  });
  await page.route('**/images/websearch**', async (route) => {
    n += 1;
    if (n > 1) return route.continue(); // the switch's own re-search
    const resp = await route.fetch(); // rendered now: filter still on
    const body = await resp.text();
    expect(body, 'held body is the stale render').toContain('aria-checked="true"');
    expect(body, 'held body carries the filter-on notice').toContain('data-sw-ai-filter-disabled');
    otherFetched();
    await otherHeld;
    return route.fulfill({ response: resp });
  });
  await toggle.click();
  await page.locator('[aria-haspopup="true"]').first().click();
  await page.getByRole('menuitem', { name: 'Web Search' }).click();
  await otherReady;
  releasePut(); // save + re-search (search 2) now run to completion
  await expect.poll(() => n).toBe(2);
  await expect(page.locator('#sw-ai-filter-toggle')).toHaveAttribute('aria-checked', 'false', { timeout: 10_000 });
  await expect(page.locator('#sw-ai-filter-toggle')).toBeEnabled({ timeout: 10_000 });
  await page.locator('#sw-ai-filter-toggle').evaluate((el) => { window.__settledSwitch = el; });
  releaseOther(); // the stale swap lands with nothing in flight
  await expect.poll(() => page.evaluate(() => window.__settledSwitch.isConnected)).toBe(false);
  const saved = await apiFetch(request, 'GET', '/api/v1/preferences/filter_ai_images');
  expect((await saved.json()).value).toBe('false');
  await expect(page.locator('#sw-ai-filter-toggle')).toHaveAttribute('aria-checked', 'false');
  await expect(page.locator('#sw-ai-filter-toggle')).toBeEnabled();
  // The stale results and their filter-on notice must not sit beside a switch
  // that now says off: the panel is marked stale instead.
  await expect(page.locator('[data-sw-ai-filter-disabled]')).toHaveCount(0);
  await expect(page.locator('[data-sw-ai-filter-refresh-failed]')).toHaveCount(1);
});

// #3296 review: the repaint must only correct a response sent BEFORE the save.
// After the preference changes elsewhere, a fresh search's correct render wins.
test('a fresh search after the preference changed elsewhere is not repainted from the old save (#3296)', async ({ page, request }) => {
  const toggle = await triggerWebSearch(page);
  await toggle.click();
  await expect(page.locator('#sw-ai-filter-toggle')).toHaveAttribute('aria-checked', 'false', { timeout: 10_000 });
  await expect(page.locator('#sw-ai-filter-toggle')).toBeEnabled();
  const put = await apiFetch(request, 'PUT', '/api/v1/preferences/filter_ai_images', { value: 'true' });
  expect(put.ok(), await put.text()).toBeTruthy();
  const fresh = page.waitForResponse((r) => r.url().includes('/images/websearch'));
  await page.locator('[aria-haspopup="true"]').first().click();
  await page.getByRole('menuitem', { name: 'Web Search' }).click();
  expect(await (await fresh).text(), 'fresh render reflects the server value').toContain('aria-checked="true"');
  await expect(page.locator('#sw-ai-filter-toggle')).toHaveAttribute('aria-checked', 'true');
  await expect(page.locator('[data-sw-ai-filter-refresh-failed]')).toHaveCount(0);
});

// #3296 review: on the generic layout two Extend searches overlap BEFORE the
// page's first toggle, so neither was stamped when sent. The late one must
// still be repainted from the save, with an accurate console message.
test('an unstamped search in flight across the FIRST toggle is repainted when it lands late (#3296)', async ({ page, request }) => {
  await page.goto(`/artists/${artistId}/images`);
  await page.waitForLoadState('load');
  const ext = page.getByRole('button', { name: /^Extend: / });
  await ext.nth(0).click();
  const t = page.locator('#web-search-results #sw-ai-filter-toggle');
  await expect(t).toHaveAttribute('aria-checked', 'true', { timeout: 10_000 });
  expect(await page.evaluate(() => window.swAIFilterSyncBound), 'this is the first toggle').toBeFalsy();
  const logs = [];
  page.on('console', (m) => { if (m.type() === 'error') logs.push(m.text()); });
  const rel = [];
  let n = 0;
  await page.route('**/images/websearch**', async (route) => {
    const i = n++;
    if (i >= 2) return route.continue(); // the toggle's own re-search
    const resp = await route.fetch();
    expect(await resp.text(), 'held body is the stale render').toContain('aria-checked="true"');
    await new Promise((r) => { rel[i] = r; });
    return route.fulfill({ response: resp });
  });
  await ext.nth(0).click(); // A
  await ext.nth(1).click(); // B
  await expect.poll(() => !!rel[0] && !!rel[1], { timeout: 10_000 }).toBe(true);
  await t.evaluate((el) => { window.__a = el; });
  rel[0](); // A lands: a fresh enabled switch
  await expect.poll(() => page.evaluate(() => window.__a.isConnected)).toBe(false);
  await t.click(); // save false + re-search
  await expect.poll(() => n, { timeout: 10_000 }).toBe(3);
  await expect(t).toHaveAttribute('aria-checked', 'false', { timeout: 10_000 });
  await expect(t).toBeEnabled({ timeout: 10_000 });
  await t.evaluate((el) => { window.__s = el; });
  rel[1](); // B lands late, rendered before the save
  await expect.poll(() => page.evaluate(() => window.__s.isConnected)).toBe(false);
  const saved = await apiFetch(request, 'GET', '/api/v1/preferences/filter_ai_images');
  expect((await saved.json()).value).toBe('false');
  await expect(t).toHaveAttribute('aria-checked', 'false');
  await expect(page.locator('[data-sw-ai-filter-disabled]')).toHaveCount(0);
  expect(logs.filter((l) => l.includes('arrived late')), 'late-arrival message').toHaveLength(1);
  expect(logs.filter((l) => l.includes('re-running the web search failed')), 'no false failure message').toHaveLength(0);
});

// #3296 review: shared setup. A search A is rendered (filter on) and held; the
// switch is saved off; the preference is then set back to true elsewhere and a
// fresh search renders the switch on. Returns the pieces the specs release.
async function saveOffThenChangeElsewhere(page, request) {
  const toggle = await triggerWebSearch(page);
  return { toggle, async finish() {
    await toggle.click();
    await expect(page.locator('#sw-ai-filter-toggle')).toHaveAttribute('aria-checked', 'false', { timeout: 10_000 });
    await expect(page.locator('#sw-ai-filter-toggle')).toBeEnabled({ timeout: 10_000 });
    const put = await apiFetch(request, 'PUT', '/api/v1/preferences/filter_ai_images', { value: 'true' });
    expect(put.ok(), await put.text()).toBeTruthy();
  } };
}
async function freshSearch(page, viaAjax = false) {
  const fresh = page.waitForResponse((r) => r.url().includes('/images/websearch'));
  if (viaAjax) {
    // The Actions menu trigger is dropped while another search is in flight.
    await page.evaluate((id) => {
      window.htmx.ajax('GET', `/api/v1/artists/${id}/images/websearch?type=thumb`, { target: '#image-results', swap: 'innerHTML' });
    }, artistId);
  } else {
    await page.locator('[aria-haspopup="true"]').first().click();
    await page.getByRole('menuitem', { name: 'Web Search' }).click();
  }
  expect(await (await fresh).text(), 'fresh render reflects the server value').toContain('aria-checked="true"');
  await expect(page.locator('#sw-ai-filter-toggle')).toHaveAttribute('aria-checked', 'true');
}

test('a pre-save swap into a different element does not repaint the switch (#3296)', async ({ page, request }) => {
  const s = await saveOffThenChangeElsewhere(page, request);
  await page.evaluate(() => {
    const d = document.createElement('div');
    d.id = 'unrelated-3296';
    document.body.appendChild(d);
  });
  let release; const held = new Promise((r) => { release = r; });
  let sent; const sentP = new Promise((r) => { sent = r; });
  await page.route('**/api/v1/health', async (route) => { sent(); await held; return route.continue(); });
  await page.evaluate(() => { window.__seqBefore3296 = window.swAIFilterSaveSeq || 0; window.htmx.ajax('GET', '/api/v1/health', { target: '#unrelated-3296', swap: 'innerHTML' }); });
  await sentP;
  await s.finish();
  await freshSearch(page);
  expect(await page.evaluate(() => document.getElementById('unrelated-3296').contains(document.getElementById('sw-ai-filter-toggle'))), 'swap target does not contain the switch').toBe(false);
  // Force the baseline to disagree with the switch, so an unscoped listener would repaint.
  await page.evaluate(() => { window.swAIFilterSaved = 'false'; });
  expect(await page.evaluate(() => (window.swAIFilterSaveSeq || 0) > window.__seqBefore3296), 'health request is stamped pre-save').toBe(true);
  release();
  await expect(page.locator('#unrelated-3296')).not.toBeEmpty();
  await expect(page.locator('#sw-ai-filter-toggle')).toHaveAttribute('aria-checked', 'true');
  await expect(page.locator('[data-sw-ai-filter-refresh-failed]')).toHaveCount(0);
});

test('a held pre-save response landing after an authoritative fresh render is not repainted (#3296)', async ({ page, request }) => {
  const toggle = await triggerWebSearch(page); // search 1
  let releasePut; const putHeld = new Promise((r) => { releasePut = r; });
  let releaseA; const heldA = new Promise((r) => { releaseA = r; });
  let aReady; const aReadyP = new Promise((r) => { aReady = r; });
  let n = 0;
  await page.route('**/api/v1/preferences/filter_ai_images', async (route) => {
    if (route.request().method() !== 'PUT') return route.continue();
    await putHeld;
    return route.continue();
  });
  await page.route('**/images/websearch**', async (route) => {
    n += 1;
    if (n !== 1) return route.continue(); // 2 = the switch's re-search, 3 = fresh
    const resp = await route.fetch();
    expect(await resp.text(), 'held body is the stale render').toContain('aria-checked="true"');
    aReady();
    await heldA;
    return route.fulfill({ response: resp });
  });
  await toggle.click(); // save held
  await page.locator('[aria-haspopup="true"]').first().click();
  await page.getByRole('menuitem', { name: 'Web Search' }).click(); // A, held
  await aReadyP;
  releasePut();
  await expect.poll(() => n).toBe(2);
  await expect(page.locator('#sw-ai-filter-toggle')).toHaveAttribute('aria-checked', 'false', { timeout: 10_000 });
  await expect(page.locator('#sw-ai-filter-toggle')).toBeEnabled({ timeout: 10_000 });
  const put = await apiFetch(request, 'PUT', '/api/v1/preferences/filter_ai_images', { value: 'true' });
  expect(put.ok(), await put.text()).toBeTruthy();
  await freshSearch(page, true); // authoritative render: true
  await page.evaluate(() => { window.__fresh = document.getElementById('sw-ai-filter-toggle'); });
  releaseA();
  await expect.poll(() => page.evaluate(() => window.__fresh.isConnected)).toBe(false);
  await expect(page.locator('#sw-ai-filter-toggle')).toHaveAttribute('aria-checked', 'true');
  await expect(page.locator('[data-sw-ai-filter-refresh-failed]')).toHaveCount(0);
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
