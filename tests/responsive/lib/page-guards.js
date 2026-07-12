// page-guards.js - the assertions that must hold before ANY probe is allowed
// to run against a page (issue #2386 fix-round-3).
//
// The project's cardinal sin is an operation that reports success while doing
// nothing. A measurement harness commits that sin in a specific way: it
// produces clean, plausible, PRECISE numbers for a page that is not the page
// you asked for. Two live instances of exactly that, both found by hostile
// review of this harness:
//
//   * goto('/next/reports/compliance') returns HTTP 200 -- at
//     http://.../reports?tab=compliance. The LEGACY page. The harness recorded
//     it under the label "next" without a murmur. The only thing keeping that
//     route out of the report was a code COMMENT saying "don't add this".
//
//   * A LOGGED-OUT run scores a perfect card. /next/settings while logged out
//     serves the login page -- HTTP 200, same URL. The login shell carries the
//     same first-paint theme script, so even verifyTheme(dark) PASSES on it.
//     The recorded numbers were overflow=false, tap=5/5: a spotless baseline
//     of a login form. The only thing preventing that today was the accident
//     that the login shell does not load preferences.js, which made an
//     unrelated wait time out. That is luck, not a guard.
//
// Hence: assert the page's IDENTITY and its AUTHENTICATED state, per record,
// and fail the record LOUDLY when either does not hold.
//
// WHAT THESE GUARDS CANNOT DO -- read this before trusting them with a claim.
// assertPageIdentity works from goto()'s HTTP Response. It therefore catches a
// SERVER REDIRECT (a 3xx landing on a different path, the compliance case above)
// and a non-200 status. It CANNOT detect a server that re-dispatches INTERNALLY:
// /next/X is handled by nextFallback (internal/api/handlers.go), which rewrites
// the path in-process and serves the stable handler's response at HTTP 200 under
// the ORIGINAL URL. Over HTTP that is indistinguishable from /next/X having its
// own handler, and no status/URL check can tell them apart -- only a markup
// assertion could, and these guards do not attempt one.
//
// This matters because it is exactly the claim it would be tempting to make.
// After M55 #1757 there are no per-screen next/ handlers left at all, so an
// earlier version of this comment saying the guard would catch the server
// "silently serving the legacy page" was WRONG: for a re-dispatch it would catch
// nothing, because there is nothing at the HTTP layer to catch. The harness now
// probes the canonical paths (run.js PAGES) instead, which removes the question.
// These are a redirect/status guard, an auth guard, and a scroll-origin guard.
// Real, and worth keeping. Nothing more than that.

import { AUTHED_BODY_CLASS, LOGIN_TITLE_PREFIX } from './constants.js';

// PageGuardError marks a failure of the harness's own preconditions (wrong
// page, not logged in) as distinct from a probe blowing up. run.js records it
// under `record.guardFailed` and skips every probe -- measuring the wrong page
// is worse than not measuring it.
export class PageGuardError extends Error {
  constructor(kind, message, detail) {
    super(message);
    this.name = 'PageGuardError';
    this.kind = kind;
    this.detail = detail;
  }
}

// assertPageIdentity: the page the SERVER gave us IS the page we asked for.
//
// `response` is goto()'s Response, which the pre-fix harness discarded entirely
// -- it never recorded a status and never looked at where it had landed, so a
// redirect or an error page was measured silently under the requested label.
//
// MEASURE AGAINST response.url(), NOT page.url(). This distinction is load-
// bearing and was got wrong on the first attempt at this guard:
//
//   response.url() is the URL the SERVER finally served, after any HTTP
//   redirect. page.url() is the browser's CURRENT history entry -- which the
//   app's own client-side JS rewrites. /next/ serves HTTP 200 at /next/ and
//   then history-replaces the address bar with /, so a page.url() check reports
//   "/next/ redirected to /". Worse, it does so RACILY: read immediately after
//   waitUntil:'load', Firefox had already run the rewrite and Chromium had not,
//   so the same route "redirected" on one engine and not the other. That is a
//   false positive on a cosmetic URL tidy-up, and exactly the class of
//   engine-artifact finding this whole fix-round exists to eliminate.
//
//   response.url() sees through it: /next/ -> /next/ (no server redirect),
//   while /next/reports/compliance -> /reports?tab=compliance (a real one).
//
// The browser's post-JS URL is still RECORDED by the caller, as `finalURL`, for
// diagnostics -- it is just not what identity is asserted on.
//
// Checks, in order:
//   1. a response exists at all (a null Response means no navigation happened)
//   2. status is 200 -- not 3xx, not a 4xx/5xx error page rendered at 200
//   3. the served pathname equals the requested pathname
//   4. every query param we asked for survives (catches a dropped ?view=grid /
//      ?tab=duplicates, which would silently measure a different tab of the
//      same route)
export function assertPageIdentity({ requestedPath, response }) {
  if (!response) {
    throw new PageGuardError('no-response', `no navigation response for ${requestedPath}`);
  }
  const status = response.status();
  const servedURL = response.url();
  const requested = new URL(requestedPath, 'http://placeholder.invalid');
  const served = new URL(servedURL);

  if (status !== 200) {
    throw new PageGuardError(
      'bad-status',
      `${requestedPath} returned HTTP ${status} (expected 200)`,
      { status, servedURL },
    );
  }
  if (served.pathname !== requested.pathname) {
    throw new PageGuardError(
      'redirected',
      `${requestedPath} was served from ${served.pathname} -- refusing to measure a `
      + 'different page under the requested label',
      { status, servedURL, requestedPath },
    );
  }
  for (const [k, v] of requested.searchParams) {
    if (served.searchParams.get(k) !== v) {
      throw new PageGuardError(
        'query-dropped',
        `${requestedPath} lost query param ${k}=${v} (served: ${servedURL})`,
        { status, servedURL, requestedPath },
      );
    }
  }
  return { status, servedURL };
}

// assertAuthenticated: this is the authenticated app shell, not the login page
// wearing its URL.
//
// Positive marker (AUTHED_BODY_CLASS on <body>) rather than a negative one --
// "the word Login is absent" would pass on an error page, a blank page, or a
// spinner. The title check is the belt to that suspenders: if the shell class
// is ever added to the login layout by accident, the title still catches it.
export async function assertAuthenticated(page, { requestedPath }) {
  const state = await page.evaluate((cls) => ({
    authed: document.body.classList.contains(cls),
    title: document.title,
  }), AUTHED_BODY_CLASS);

  if (!state.authed || state.title.startsWith(LOGIN_TITLE_PREFIX)) {
    throw new PageGuardError(
      'not-authenticated',
      `${requestedPath} did not render the authenticated app shell `
      + `(body.${AUTHED_BODY_CLASS} present: ${state.authed}, title: "${state.title}"). `
      + 'A logged-out run serves the login page at HTTP 200 under the SAME URL; '
      + 'refusing to report its numbers as a baseline.',
      state,
    );
  }
  return state;
}

// resetScrollToOrigin: put the page back at (0, 0) before ANY probe runs.
//
// THE BUG THIS FIXES. Every geometry probe compares a VIEWPORT-relative rect
// (getBoundingClientRect) against a DOCUMENT-absolute extent
// (documentElement.scrollHeight). Those share an origin only at scroll (0, 0).
// The harness asserted that in a COMMENT -- "the harness never scrolls before
// probing" -- and the comment was wrong: openAffordance() calls trigger.click(),
// and Playwright AUTO-SCROLLS a click target into view. At scrollY > 0 the
// off-screen probe's `above` test (rect.bottom < -1) is true for every element
// the page has scrolled PAST, so the report fills up with fabricated
// "unreachable" offenders that are in fact perfectly reachable.
//
// Normalising the scroll position (rather than offsetting every rect by scrollY)
// is the genuinely correct fix: it also makes the numbers REPRODUCIBLE. Playwright
// scrolls by however far that particular trigger happened to need, so an
// offset-based harness would measure sticky/lazy geometry from a different origin
// on every run.
//
// An invariant is only an invariant if something enforces it. This throws a
// PageGuardError when the page will not return to the origin (e.g. a future
// scroll-locked overlay), and window.__swAssertUnscrolled() throws inside each
// probe as the backstop -- neither path can silently fabricate offenders.
export async function resetScrollToOrigin(page, { requestedPath } = {}) {
  const scroll = await page.evaluate(() => new Promise(resolve => {
    // 'instant' defeats any `scroll-behavior: smooth`, which would otherwise
    // still be animating when the probes read geometry.
    window.scrollTo({ top: 0, left: 0, behavior: 'instant' });
    requestAnimationFrame(() => requestAnimationFrame(() => {
      resolve({ scrollX: window.scrollX, scrollY: window.scrollY });
    }));
  }));

  if (Math.abs(scroll.scrollX) > 0.5 || Math.abs(scroll.scrollY) > 0.5) {
    throw new PageGuardError(
      'scroll-origin',
      `${requestedPath} would not return to the scroll origin `
      + `(still at ${scroll.scrollX}, ${scroll.scrollY} after scrollTo(0, 0)). `
      + 'Every probe measures a viewport-relative rect against a document-absolute '
      + 'extent, which is only valid at (0, 0); measuring here would report elements '
      + 'scrolled PAST as unreachable. Refusing to measure.',
      scroll,
    );
  }
  return scroll;
}

// waitForQuiescence: let the page STOP MOVING before probing it.
//
// The harness navigates with waitUntil:'load' and probes immediately, so
// anything the app fills in asynchronously (HTMX swaps, the post-load
// preference sync, lazy content) can land mid-measurement. Symptom: the same
// page measured 371/376 and 372/377 tap targets across two themes on the same
// browser -- a drift that makes exact-number regressions untrustworthy.
//
// This waits for a quiet window with no DOM mutations. It does NOT use
// waitUntil:'networkidle': the app holds an open SSE stream (internal/api's
// hub), so the network is never idle and that wait would simply hang until it
// timed out on every page.
//
// A timeout here is NOT swallowed -- it returns { quiet: false } and the caller
// records it on the record, so a page that never settles is visible in the
// report rather than silently producing a mid-flight measurement.
export async function waitForQuiescence(page, { idleMs = 400, timeout = 5_000 } = {}) {
  return page.evaluate(({ idleMs, timeout }) => new Promise(resolve => {
    let timer;
    const started = Date.now();
    const observer = new MutationObserver(() => {
      clearTimeout(timer);
      timer = setTimeout(settle, idleMs);
    });
    const settle = () => {
      clearTimeout(deadline);
      observer.disconnect();
      resolve({ quiet: true, waitedMs: Date.now() - started });
    };
    const deadline = setTimeout(() => {
      clearTimeout(timer);
      observer.disconnect();
      resolve({ quiet: false, waitedMs: Date.now() - started });
    }, timeout);
    observer.observe(document.documentElement, {
      childList: true, subtree: true, attributes: true, characterData: true,
    });
    timer = setTimeout(settle, idleMs);
  }), { idleMs, timeout });
}
