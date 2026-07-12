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
