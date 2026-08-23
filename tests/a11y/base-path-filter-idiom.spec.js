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
    // Observable completion, in TWO layers, because a network response and a
    // DOM swap are different events htmx fires at different times: htmx
    // resolves the request FIRST and applies the outerHTML swap AFTER, so
    // waiting only for the response (as an earlier revision of this test
    // did) can observe the OLD #blast-radius-pane node, which is still
    // attached and still visible until the swap actually runs -- a race
    // that is TIGHTER than the fixed 500ms sleep this replaced, because
    // the response resolves earlier than any fixed sleep did.
    //
    // Mark the CURRENT node before triggering the reload, then wait for
    // that marked node specifically to detach. outerHTML replaces the whole
    // element with markup the server rendered (which never carries this
    // marker), so the marked node detaching is the actual swap completing,
    // not just the request settling.
    await page.evaluate(() => document.getElementById('blast-radius-pane').setAttribute('data-swap-marker', 'pre-reload'));
    const reloadResponse = page.waitForResponse(
      (resp) => resp.url().includes('/reports/blast-radius') && resp.request().resourceType() !== 'document',
    );
    await page.selectOption('#blast-radius-sort', { index: 1 });
    await reloadResponse;
    await page.locator('#blast-radius-pane[data-swap-marker="pre-reload"]').waitFor({ state: 'detached' });
    await expect(page.locator('#blast-radius-pane')).toBeVisible();

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
    // Observable completion, in TWO layers, same reasoning as
    // blastRadiusReload's test above: the response resolving is not the
    // same event as htmx's outerHTML swap landing, and the swap runs AFTER
    // the response resolves. Mark the CURRENT #compliance-results node
    // before the click, then wait for that marked node specifically to
    // detach -- the replacement markup the server renders never carries
    // this marker, so its detachment is the actual swap completing, not
    // just the network settling.
    await page.evaluate(() => document.getElementById('compliance-results').setAttribute('data-swap-marker', 'pre-dismiss'));
    const dismissResponse = page.waitForResponse(
      (resp) => resp.url().includes('/reports') && resp.request().resourceType() !== 'document',
    );
    await page.locator('[aria-label^="Remove "]').first().click();
    await dismissResponse;
    await page.locator('#compliance-results[data-swap-marker="pre-dismiss"]').waitFor({ state: 'detached' });

    expect(dismissRequests.length, `expected exactly one dismiss reload request, got: ${JSON.stringify(dismissRequests)}`)
      .toBe(1);
    const dismissURL = new URL(dismissRequests[0]);
    expect(
      dismissURL.pathname,
      `dismiss request path was ${dismissURL.pathname}; expected exactly one ${BASE_PATH} prefix`,
    ).toBe(`${BASE_PATH}/reports`);
    expect(dismissURL.pathname.match(new RegExp(BASE_PATH, 'g')) || []).toHaveLength(1);

    // The swap must have actually landed as ONE COPY of the shell, not zero
    // and not two. These are separate failure shapes and neither assertion
    // covers the other: a swap that extracts NOTHING (e.g. a wrong-route
    // response with no #compliance-results in it, or a select that matched
    // nothing) makes the element VANISH -- dupIds alone would read [] and
    // this test would pass while the pane is gone. The toHaveCount(1) below
    // is what rules that shape out; dupIds afterward rules out the CONVERSE
    // shape (a full-page response with no fragment handler duplicating every
    // id on the page, the #3099/#3100 class of defect). Neither replaces the
    // other.
    await expect(page.locator('#compliance-results')).toHaveCount(1);
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

    // Observable completion instead of a fixed sleep: wait for the routed
    // 404 response itself (set up BEFORE the action that triggers it), then
    // for the error toast htmx:responseError renders in reaction to it --
    // that toast IS the observable side effect of "the 404 was handled",
    // which a fixed delay only approximates.
    const notFoundResponse = page.waitForResponse(
      (resp) => resp.url().includes('/reports/blast-radius') && resp.status() === 404,
    );
    await page.selectOption('#blast-radius-sort', { index: 1 });
    await notFoundResponse;
    await expect(page.locator('#error-toast-container > *').first()).toBeVisible();

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

  // #3094 FIX 5: extends AC4 to the actually-reachable silent-failure shape,
  // rather than only the 404 shape proved above.
  //
  // The chain that makes this genuinely covered end to end, MEASURED at each
  // link rather than assumed:
  //
  //   1. FIX 3 (internal/config validateBasePathCharset) refuses an
  //      unsupported SW_BASE_PATH at BOOT, so a server cannot be configured
  //      with a base path that would silently mismatch a percent-encoded
  //      window.location.pathname in the first place. This is what makes
  //      the byte-vs-percent-encoding root cause NON-reachable through
  //      config; this spec's BASE_PATH is plain ASCII precisely because
  //      that IS the only charset FIX 3 allows through.
  //
  //   2. FIX 4 (the else-branch in DismissFilterChip / DismissFilterValueChip)
  //      converts any REMAINING runtime mismatch between the base-path meta
  //      tag and location.pathname into a LOUD console.error naming both
  //      values, fired BEFORE the (still-issued) request goes out.
  //
  //   3. Because the mismatched request still goes out (FIX 4 does not
  //      abort it), it lands on a path this server's mux does not
  //      recognize, which 404s -- confirmed by measurement below -- and the
  //      EXISTING global htmx:responseError handler (layout.templ) already
  //      toasts on that, which the ORIGINAL AC4 test above already covers.
  //
  // So the reachable failure is not silent: something in the chain reports
  // it every time, and the true "silent 200 through the catch-all" shape the
  // coordinator described requires TWO independent config values to
  // disagree on the SAME server (SW_BASE_PATH set one way, the served
  // meta/location pair reflecting another) -- which FIX 3 forecloses by
  // making that server unbootable, and FIX 4 additionally logs even without
  // relying on FIX 3.
  //
  // This test drives the runtime-mismatch path directly (mutating only the
  // served meta tag's content, not location.pathname, on an otherwise
  // correctly-booted server) and asserts the FULL chain: FIX 4's
  // console.error fires BEFORE the request, the request 404s, the existing
  // responseError toast fires, and #compliance-results survives -- proving
  // the "reachable 200" story does not in fact apply once FIX 3 and FIX 4
  // are both in place, and that whatever DOES happen is reported, not
  // silent.
  test('AC4 extended: a runtime base-path mismatch is reported loudly at every link in the chain, not silently 200d', async ({ browser }) => {
    const context = await browser.newContext();
    await context.addCookies([
      { name: 'csrf_token', value: server.csrfToken, url: server.rootURL },
      { name: 'session', value: server.sessionCookie, url: server.rootURL },
    ]);
    const page = await context.newPage();

    // Mutate ONLY the served meta tag's content, leaving location.pathname
    // (BASE_PATH-prefixed, as normal) untouched -- this is the runtime shape
    // FIX 4's guard exists for: the two values the strip compares disagree,
    // for whatever reason, at the moment the dismiss script reads them.
    await page.route(`**${BASE_PATH}/reports*`, async (route) => {
      if (route.request().resourceType() !== 'document') {
        route.continue();
        return;
      }
      const resp = await route.fetch();
      const body = (await resp.text()).replace(`content="${BASE_PATH}"`, `content="${BASE_PATH}-WRONG"`);
      route.fulfill({ response: resp, body, contentType: 'text/html' });
    });

    await page.goto(`${server.baseURL}/reports?status=non_compliant`);
    await page.waitForSelector('[aria-label^="Remove "]', { timeout: 10_000 });

    // Preconditions: the mutation actually landed (meta now disagrees with
    // location), and the pane starts with real content.
    const meta = await page.evaluate(() => document.querySelector('meta[name="htmx-base-path"]')?.content);
    const loc = await page.evaluate(() => window.location.pathname);
    expect(meta, 'the meta-tag mutation did not land; the mismatch this test drives is not actually present').toBe(`${BASE_PATH}-WRONG`);
    expect(loc.startsWith(BASE_PATH), 'location.pathname is not base-path-prefixed; the fixture setup is wrong').toBe(true);
    const before = await page.evaluate(() => document.getElementById('compliance-results')?.innerHTML.length ?? 0);
    expect(before, 'the compliance pane has no content to begin with; a wipe would be undetectable').toBeGreaterThan(0);

    const consoleMessages = [];
    page.on('console', (msg) => consoleMessages.push({ type: msg.type(), text: msg.text() }));

    // Observable completion instead of a fixed sleep: wait for the
    // mismatched request's response (it lands nowhere this server
    // recognizes, so it 404s -- set up BEFORE the click), then for the
    // resulting error toast to render. FIX 4's console.error runs
    // SYNCHRONOUSLY before htmx.ajax is even called, so by the time the
    // response (and therefore the toast) arrives, the guard message is
    // already in consoleMessages -- no separate wait is needed for it.
    const mismatchedResponse = page.waitForResponse(
      (resp) => resp.request().resourceType() !== 'document' && resp.status() === 404,
    );
    await page.locator('[aria-label^="Remove "]').first().click();
    await mismatchedResponse;
    await expect(page.locator('#error-toast-container > *').first()).toBeVisible();

    // Link 1: FIX 4's guard fired, naming both the mismatched location and
    // the wrong configured value.
    const guardMessages = consoleMessages.filter((m) => /does not start with the configured base path/.test(m.text));
    expect(guardMessages.length, `FIX 4's guard did not fire for the mismatch; console messages: ${JSON.stringify(consoleMessages)}`)
      .toBeGreaterThan(0);
    expect(guardMessages[0].text).toContain(loc);
    expect(guardMessages[0].text).toContain(`${BASE_PATH}-WRONG`);

    // Link 2/3: the mismatched request still went out, landed nowhere this
    // server recognizes, 404d, and the EXISTING global responseError
    // handler toasted -- so nothing about this chain is silent end to end.
    const toastCount = await page.locator('#error-toast-container > *').count();
    expect(toastCount, 'no toast was shown for the mismatched request; the chain went silent somewhere').toBeGreaterThan(0);

    // The pane's prior content survives -- this is NOT the "silently
    // emptied" shape, because FIX 4 caused a 404 (which htmx does not swap
    // on), not a same-origin 200.
    await expect(page.locator('#compliance-results')).toHaveCount(1);

    await context.close();
  });
});
