// base-path-filter-idiom.spec.js - end-to-end coverage for #3094.
//
// THE QUESTION THE ISSUE ASKS
//
// blastRadiusReload (web/templates/reports_page.templ) passes a root-relative
// path to htmx.ajax and relies on the global htmx:configRequest hook
// (layout.templ) to prepend SW_BASE_PATH. DismissFilterChip and
// DismissFilterValueChip (web/components/filter_flyout.templ) instead strip
// the base path from window.location.pathname before calling htmx.ajax, on
// the theory that the SAME configRequest hook would otherwise double-prefix
// it. Both cannot be correct under a non-empty base path -- one must either
// double-prefix (404) or drop the prefix (also 404, at the wrong origin
// path). Nothing before this spec ran either path with SW_BASE_PATH set to
// anything but "".
//
// WHAT THIS SPEC ESTABLISHES (measured against a real server + real htmx,
// not read from source): htmx 2.0.8's internal request builder reads
// evt.detail.path back AFTER htmx:configRequest fires (confirmed by reading
// the vendored web/static/js/htmx.min.js -- the `he()` function assigns
// `n=C.path` from the mutated event detail before opening the XHR). So the
// global hook prepends the base path on EVERY root-relative htmx.ajax call
// regardless of caller, including a caller that ALSO stripped the prefix
// from an already-prefixed path first. Both idioms independently arrive at a
// correctly single-prefixed URL:
//
//   - blastRadiusReload:      '/reports/blast-radius' + search
//                             -> hook prepends bp -> '/bp/reports/blast-radius'
//   - DismissFilterChip:      strips bp from location.pathname first
//                             -> '/artists' (bp already removed)
//                             -> hook prepends bp -> '/bp/artists'
//
// Both land on exactly ONE copy of the prefix. The chip-side stripping is
// therefore NOT redundant and NOT wrong: it is necessary precisely because
// window.location.pathname under a sub-path deployment already CONTAINS the
// base path (the browser's address bar reads /bp/artists, not /artists), and
// the dismiss handlers build their path from location.pathname while
// blastRadiusReload builds it from a hardcoded root-relative literal. Two
// different SOURCE VALUES for "the path", each needing a different
// transform to reach the same bp-free root-relative form the hook expects.
// The issue's premise that "both cannot be correct" assumed both had the
// same starting point; they do not.
//
// This spec drives both call sites against a real server booted with
// SW_BASE_PATH set (tests/a11y/helpers/base-path-server.js spins up a
// SEPARATE ephemeral instance so it cannot perturb the shared a11y server or
// any other spec's baseURL/storageState assumptions) and asserts the actual
// network request that reaches the server, not a static reading of the
// source.
//
// AC4 (no silent-failure 404): a request that resolves to a 404 under a base
// path must not render as an empty result set. Simulated via page.route so
// the assertion does not depend on constructing a real 404 condition, and
// verifies the EXISTING global htmx:responseError handler (layout.templ)
// actually fires and toasts, and that the swap target's prior content
// survives (htmx never swaps on an error response by default -- see
// Q.config.responseHandling in htmx.min.js, [45].. => swap:false).

import { test, expect } from 'playwright/test';
import { startBasePathServer } from './helpers/base-path-server.js';

const BASE_PATH = '/sw-basepath-test';

test.describe('filter reload / chip dismiss under a non-empty SW_BASE_PATH (#3094)', () => {
  /** @type {Awaited<ReturnType<typeof startBasePathServer>>} */
  let server;

  test.beforeAll(async () => {
    server = await startBasePathServer(BASE_PATH);
  });

  test.afterAll(async () => {
    if (server) server.stop();
  });

  test('blastRadiusReload issues exactly one base-path prefix, never a double or a bare root path', async ({ browser }) => {
    const context = await browser.newContext();
    await context.addCookies([
      { name: 'csrf_token', value: server.csrfToken, url: server.rootURL },
      { name: 'session', value: server.sessionCookie, url: server.rootURL },
    ]);
    const page = await context.newPage();

    const paneRequests = [];
    page.on('request', (req) => {
      if (req.url().includes('/reports/blast-radius') && req.resourceType() !== 'document') {
        paneRequests.push(req.url());
      }
    });

    await page.goto(`${server.baseURL}/reports/blast-radius`);
    await page.waitForSelector('#blast-radius-sort', { timeout: 10_000 });

    // Precondition: the meta tag actually carries the configured base path,
    // or the prefix check below would pass vacuously against an empty bp.
    const metaBasePath = await page.evaluate(
      () => document.querySelector('meta[name="htmx-base-path"]')?.content ?? null,
    );
    expect(metaBasePath, 'htmx-base-path meta tag does not carry the configured SW_BASE_PATH').toBe(BASE_PATH);

    paneRequests.length = 0;
    await page.selectOption('#blast-radius-sort', { index: 1 });
    await page.waitForTimeout(500);

    expect(paneRequests.length, `expected exactly one blast-radius reload request, got: ${JSON.stringify(paneRequests)}`)
      .toBe(1);
    const reloadURL = new URL(paneRequests[0]);
    expect(
      reloadURL.pathname,
      `reload request path was ${reloadURL.pathname}; expected exactly one ${BASE_PATH} prefix`,
    ).toBe(`${BASE_PATH}/reports/blast-radius`);
    expect(reloadURL.pathname.match(new RegExp(BASE_PATH, 'g')) || []).toHaveLength(1);

    await context.close();
  });

  test('DismissFilterChip issues exactly one base-path prefix, never a double or a bare root path', async ({ browser }) => {
    const context = await browser.newContext();
    await context.addCookies([
      { name: 'csrf_token', value: server.csrfToken, url: server.rootURL },
      { name: 'session', value: server.sessionCookie, url: server.rootURL },
    ]);
    const page = await context.newPage();

    const dismissRequests = [];
    page.on('request', (req) => {
      if (req.url().includes('/reports') && req.resourceType() !== 'document') {
        dismissRequests.push(req.url());
      }
    });

    await page.goto(`${server.baseURL}/reports?status=non_compliant`);
    await page.waitForSelector('[aria-label^="Remove "]', { timeout: 10_000 });

    // Precondition: the address bar itself is base-path-prefixed (this is
    // what makes location.pathname-based stripping necessary in the first
    // place), and there is a chip to dismiss.
    const locationPathname = await page.evaluate(() => window.location.pathname);
    expect(locationPathname, 'the browser location is not base-path-prefixed; the precondition does not hold')
      .toBe(`${BASE_PATH}/reports`);
    const chipCount = await page.locator('[aria-label^="Remove "]').count();
    expect(chipCount, 'no dismissable chip rendered for ?status=non_compliant').toBe(1);

    dismissRequests.length = 0;
    await page.locator('[aria-label^="Remove "]').first().click();
    await page.waitForTimeout(500);

    expect(dismissRequests.length, `expected exactly one dismiss reload request, got: ${JSON.stringify(dismissRequests)}`)
      .toBe(1);
    const dismissURL = new URL(dismissRequests[0]);
    expect(
      dismissURL.pathname,
      `dismiss request path was ${dismissURL.pathname}; expected exactly one ${BASE_PATH} prefix`,
    ).toBe(`${BASE_PATH}/reports`);
    expect(dismissURL.pathname.match(new RegExp(BASE_PATH, 'g')) || []).toHaveLength(1);

    // The swap must have actually landed (not a discarded/errored request):
    // no duplicated DOM ids, which is what a double-prefixed 404 rendering an
    // error page fragment, or a de-prefixed request hitting the wrong route,
    // would produce.
    const dupIds = await page.evaluate(() => {
      const ids = Array.from(document.querySelectorAll('[id]')).map((e) => e.id);
      const counts = {};
      ids.forEach((id) => { counts[id] = (counts[id] || 0) + 1; });
      return Object.entries(counts).filter(([, c]) => c > 1);
    });
    expect(dupIds, `duplicated DOM ids after dismiss: ${JSON.stringify(dupIds)}`).toEqual([]);

    await context.close();
  });

  test('AC4: a 404 under the base path does not render as an empty result set, and is reported loudly', async ({ browser }) => {
    const context = await browser.newContext();
    await context.addCookies([
      { name: 'csrf_token', value: server.csrfToken, url: server.rootURL },
      { name: 'session', value: server.sessionCookie, url: server.rootURL },
    ]);
    const page = await context.newPage();

    const consoleErrors = [];
    page.on('console', (msg) => {
      if (msg.type() === 'error') consoleErrors.push(msg.text());
    });

    await page.goto(`${server.baseURL}/reports/blast-radius`);
    await page.waitForSelector('#blast-radius-pane', { timeout: 10_000 });

    // Precondition: the pane starts with real, non-empty markup so an
    // "emptied out" regression is observable.
    const before = await page.evaluate(() => document.getElementById('blast-radius-pane')?.innerHTML.length ?? 0);
    expect(before, 'the blast-radius pane has no content to begin with; a wipe would be undetectable').toBeGreaterThan(0);

    // Force the NEXT reload request to 404, simulating the exact failure mode
    // AC4 names: a base-path idiom bug that resolves to the wrong route.
    await page.route('**/reports/blast-radius*', (route) => {
      if (route.request().resourceType() === 'document') {
        route.continue();
        return;
      }
      route.fulfill({ status: 404, contentType: 'text/plain', body: 'not found' });
    });

    await page.selectOption('#blast-radius-sort', { index: 1 });
    await page.waitForTimeout(500);

    // NOT a silent failure: the global htmx:responseError handler
    // (layout.templ) must have toasted, and the pane's previous content must
    // still be standing (htmx does not swap on a [45].. response by
    // default -- see Q.config.responseHandling in htmx.min.js).
    const toastCount = await page.locator('#error-toast-container > *').count();
    expect(toastCount, 'no toast was shown for the 404; the failure would be silent to the operator').toBeGreaterThan(0);

    const after = await page.evaluate(() => document.getElementById('blast-radius-pane')?.innerHTML.length ?? 0);
    expect(after, 'the pane rendered as empty after a 404 -- a failed request must not present as "no data"').toBeGreaterThan(0);

    await context.close();
  });
});
