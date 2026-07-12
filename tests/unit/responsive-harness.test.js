// Unit tests for the responsive UAT harness (issue #2386, fix-round-4).
//
// These cover the harness's own failure modes -- the ones a measurement tool is
// uniquely dangerous for, because its output LOOKS like evidence either way:
//
//   * a run that measured nothing must not report success (exit code)
//   * a probe must not fabricate offenders when the page has been scrolled
//   * the page list must not claim to probe a surface that no longer exists
//
// They exercise the SAME functions the harness runs (run.js's exported pure
// helpers; probe-helpers.js's predicates, which are installed into the browser
// from these very definitions via Function.toString()), not re-typed copies.

import { test, describe } from 'node:test';
import assert from 'node:assert/strict';

import { parseArgs, classifyResults, runFailureReasons, PAGES } from '../responsive/run.js';
import { putThemePreference } from '../responsive/lib/auth.js';
import { swAssertUnscrolled, swClassifyOffscreen, TOL } from '../responsive/lib/probe-helpers.js';

// A record shaped like the ones runOnePage produces.
const okRecord = (page = 'dashboard') => ({
  page, browser: 'chromium', viewport: 'mobile-360', theme: 'dark',
  layout: { hasHorizontalOverflow: false, overflowPx: 0, offenderCount: 0, offenders: [] },
  tapTargets: { minPx: 44, totalInteractive: 10, offenderCount: 0 },
  offscreen: { offenderCount: 0 },
  axe: { violationCount: 0, violations: [] },
});
const guardFailedRecord = (kind = 'bad-status') => ({
  page: 'settings', browser: 'chromium', viewport: 'mobile-360', theme: 'dark',
  guardFailed: { kind, message: `/settings returned HTTP 404 (expected 200)` },
});

describe('runFailureReasons -- the harness must not report success while doing nothing', () => {
  // THE REGRESSION. Wrong SW_UX (every /next/* 404s) or a logged-out session
  // meant EVERY record guard-failed. The pre-fix run.js wrote a full report,
  // printed the failures, and exited 0.
  test('a 100%-guard-failed run fails, naming the cause', () => {
    const results = [guardFailedRecord(), guardFailedRecord(), guardFailedRecord()];
    const reasons = runFailureReasons(results);

    assert.ok(reasons.length > 0, 'a run in which nothing was measured must fail');
    assert.match(reasons.join('\n'), /MEASURED NOTHING/);
    assert.match(reasons.join('\n'), /GUARD-FAILED/);
  });

  test('a clean run passes', () => {
    assert.deepEqual(runFailureReasons([okRecord(), okRecord('settings')]), []);
  });

  // Partial failure is still failure: a matrix where one page could not be
  // measured has a hole in it, and the caller must not read that as a pass.
  test('a mostly-clean run with one guard failure still fails', () => {
    const reasons = runFailureReasons([okRecord(), okRecord('settings'), guardFailedRecord()]);
    assert.equal(reasons.length, 1);
    assert.match(reasons[0], /1 record\(s\) GUARD-FAILED/);
  });

  test('an errored record fails the run', () => {
    const reasons = runFailureReasons([okRecord(), { page: 'logs', error: 'boom' }]);
    assert.match(reasons.join('\n'), /ERRORED/);
  });

  // A skipped artist-detail (no artists in the target database) is not a
  // harness failure -- but it is also not a measurement, so it cannot be the
  // ONLY thing in the report.
  test('skips alone do not count as measurements', () => {
    const reasons = runFailureReasons([{ page: 'artist-detail', skipped: 'no artists' }]);
    assert.match(reasons.join('\n'), /MEASURED NOTHING/);
  });

  test('classifyResults partitions every record exactly once', () => {
    const results = [
      okRecord(),
      guardFailedRecord(),
      { page: 'logs', error: 'boom' },
      { page: 'artist-detail', skipped: 'no artists' },
      { ...okRecord('prefs-drawer-open'), openBlocked: 'trigger 0x0' },
      { ...okRecord('prefs-drawer-open'), openFailed: 'clicked, never opened' },
    ];
    const c = classifyResults(results);
    assert.equal(c.ok.length, 1);
    assert.equal(c.guardFailed.length, 1);
    assert.equal(c.errored.length, 1);
    assert.equal(c.skipped.length, 1);
    assert.equal(c.openBlocked.length, 1);
    assert.equal(c.openFailure.length, 1);
    assert.equal(
      c.ok.length + c.guardFailed.length + c.errored.length + c.skipped.length
        + c.openBlocked.length + c.openFailure.length,
      results.length,
    );
  });
});

// NF1. The theme-restore failure was loud on stderr and invisible to the exit
// code -- the weaker half of the very lesson runFailureReasons exists to teach.
// A caller gating on the exit code (CI, an agent) never reads stderr, and the
// consequence is the maintainer's account left mutated on the wrong theme.
describe('runFailureReasons -- a theme this run could not put back fails the run', () => {
  test('a failed restore fails an otherwise-clean run', () => {
    const reasons = runFailureReasons(
      [okRecord(), okRecord('settings')],
      { ok: false, reason: 'PUT /api/v1/preferences/theme failed', leftAt: 'light' },
    );
    assert.equal(reasons.length, 1);
    assert.match(reasons[0], /THEME NOT RESTORED/);
    assert.match(reasons[0], /left on "light"/);
  });

  test('a successful restore does not fail the run', () => {
    assert.deepEqual(runFailureReasons([okRecord()], { ok: true, restoredTo: 'dark' }), []);
  });

  // A read that succeeded and found no theme set is not a failure to restore:
  // there was genuinely nothing to put back.
  test('an account with no theme set is not a restore failure', () => {
    assert.deepEqual(runFailureReasons([okRecord()], { ok: true, reason: 'nothing to restore' }), []);
  });

  test('defaults to ok when no restore outcome is supplied', () => {
    assert.deepEqual(runFailureReasons([okRecord()]), []);
  });
});

// NF1b. Probing that ABORTED partway is a failed run even if every record it
// managed to collect looks clean. browser.launch() / newContext() / newPage()
// sit outside runOnePage's try/catch, so a throw from any of them used to escape
// main() to main().catch() -- skipping the report, the summary, runFailureReasons
// AND the theme restore, leaving the maintainer's account mutated and the caller
// staring at a bare stack trace with no named reason.
describe('runFailureReasons -- probing that aborted partway fails the run', () => {
  test('an abort fails a run whose collected records are all clean', () => {
    const reasons = runFailureReasons(
      [okRecord(), okRecord('settings')],
      { ok: true, restoredTo: 'dark' },
      new Error('browserType.launch: Executable does not exist'),
    );
    assert.equal(reasons.length, 1, 'a clean-looking partial run must still fail');
    assert.match(reasons[0], /PROBING ABORTED before completion/);
    assert.match(reasons[0], /Executable does not exist/);
    assert.match(reasons[0], /2 record\(s\)/);
  });

  test('an abort is reported ALONGSIDE a theme it could not put back', () => {
    const reasons = runFailureReasons(
      [okRecord()],
      { ok: false, reason: 'PUT /api/v1/preferences/theme failed', leftAt: 'dark' },
      new Error('newContext failed'),
    );
    assert.equal(reasons.length, 2);
    assert.match(reasons[0], /PROBING ABORTED/);
    assert.match(reasons[1], /THEME NOT RESTORED/);
    // leftAt must name the theme ACTUALLY applied last, not THEMES[last].
    assert.match(reasons[1], /left on "dark"/);
  });

  test('an abort with nothing measured reports BOTH causes, not just one', () => {
    const reasons = runFailureReasons([], { ok: true }, new Error('boom'));
    assert.equal(reasons.length, 2);
    assert.match(reasons[0], /PROBING ABORTED/);
    assert.match(reasons[1], /MEASURED NOTHING/);
  });

  test('no abort is not a failure', () => {
    assert.deepEqual(runFailureReasons([okRecord()], { ok: true }, null), []);
  });
});

// NF1c. writeThemePreference's PUT had no try/catch while its sibling
// readThemePreference guarded all three of its arms -- and the shared comment
// above them both claims "Neither throws". A transport-level rejection
// (connection refused, DNS, timeout) escaped restoreTheme -> main() ->
// main().catch(), skipping the report, the summary and runFailureReasons: the
// harness would die on a bare stack trace precisely when it had mutated the
// account's theme and could not put it back.
//
// Returning false does not SWALLOW that: restoreTheme prints the WARNING and
// returns { ok: false, reason, leftAt }, which runFailureReasons turns into a
// NAMED non-zero exit. These assert the guard exists at all.
describe('putThemePreference -- a PUT that THROWS must not escape the harness', () => {
  const storageState = { cookies: [{ name: 'csrf_token', value: 't' }] };

  test('a transport-level rejection returns false rather than throwing', async () => {
    const ctx = { put: async () => { throw new Error('connect ECONNREFUSED'); } };
    assert.equal(await putThemePreference(ctx, { storageState, theme: 'dark' }), false);
  });

  test('a non-ok response returns false', async () => {
    const ctx = { put: async () => ({ ok: () => false, status: () => 500 }) };
    assert.equal(await putThemePreference(ctx, { storageState, theme: 'dark' }), false);
  });

  test('a successful write returns true', async () => {
    const ctx = { put: async () => ({ ok: () => true, status: () => 204 }) };
    assert.equal(await putThemePreference(ctx, { storageState, theme: 'dark' }), true);
  });

  test('the CSRF token and the theme actually reach the request', async () => {
    let seen = null;
    const ctx = { put: async (url, o) => { seen = { url, o }; return { ok: () => true }; } };
    await putThemePreference(ctx, { storageState, theme: 'light' });
    assert.equal(seen.url, '/api/v1/preferences/theme');
    assert.equal(seen.o.headers['X-CSRF-Token'], 't');
    assert.deepEqual(JSON.parse(seen.o.data), { value: 'light' });
  });
});

// NF2. The two ways an affordance fails to open are NOT the same kind of thing,
// and collapsing them was hiding a real failure behind a known app bug.
describe('open-state -- an app finding is not a harness failure, and vice versa', () => {
  const drawer = over => ({ ...okRecord('prefs-drawer-open'), ...over });

  // The trigger is 0x0 at every pinned viewport TODAY -- that is app bug #2382,
  // and it is what this harness is FOR. A harness that exits 1 on every run
  // until #2382 lands is a harness whose exit code everyone learns to ignore.
  test('a blocked (unactionable) trigger is recorded and does NOT fail the run', () => {
    const results = [okRecord(), drawer({ openBlocked: 'trigger present but not actionable (0x0)' })];
    assert.deepEqual(runFailureReasons(results), [],
      'the 0x0 trigger is an APP finding (#2382); failing the run on it would make the '
      + 'harness permanently red for a bug it is supposed to merely report');

    const c = classifyResults(results);
    assert.equal(c.openBlocked.length, 1);
    assert.equal(c.ok.length, 1, 'a blocked open-state record is never counted clean either');
  });

  test('a trigger missing from the DOM is likewise recorded, not a run failure', () => {
    assert.deepEqual(runFailureReasons([okRecord(), drawer({ openSkipped: 'trigger not in the DOM' })]), []);
  });

  // THE REGRESSION THIS GUARDS. Once #2382 makes the trigger clickable, a drawer
  // that still never opens must NOT go green. Under the old single `openFailed`
  // marker it would have: every open-state record was excluded from `ok` and
  // added no failure reason, so the run exited 0 with the one interactive probe
  // never having opened.
  test('a click that LANDED and still never opened FAILS the run', () => {
    const reasons = runFailureReasons([
      okRecord(),
      drawer({ openFailed: 'trigger WAS actionable and the click landed, but the drawer never opened' }),
    ]);
    assert.equal(reasons.length, 1);
    assert.match(reasons[0], /FAILED TO OPEN/);
    assert.match(reasons[0], /actionable/);
  });

  test('an open state that never settled FAILS the run (probes ran mid-animation)', () => {
    const reasons = runFailureReasons([okRecord(), drawer({ openUnsettled: 'geometry still changing' })]);
    assert.match(reasons.join('\n'), /FAILED TO OPEN/);
  });

  // The two must not be confusable: a blocked trigger alongside a real failure
  // still fails, and the reason names the real one.
  test('a real open failure is not masked by a blocked one', () => {
    const reasons = runFailureReasons([
      okRecord(),
      drawer({ openBlocked: 'trigger 0x0' }),
      drawer({ openFailed: 'click landed, never opened' }),
    ]);
    assert.match(reasons.join('\n'), /1 record\(s\) FAILED TO OPEN/);
  });
});

describe('the scroll-origin invariant -- probes must not fabricate offenders', () => {
  const metrics = (over = {}) => ({
    viewportWidth: 360, viewportHeight: 740,
    scrollWidth: 360, scrollHeight: 4000,
    scrollX: 0, scrollY: 0,
    ...over,
  });

  // THE REGRESSION. openAffordance() calls trigger.click(); Playwright AUTO-
  // SCROLLS the target into view. Every probe compares a viewport-relative rect
  // against a document-absolute scrollHeight -- valid only at scrollY = 0. At
  // scrollY > 0, an element near the top of the document has a NEGATIVE
  // rect.bottom, so `above` fires and it is reported as unreachable. It is not:
  // it is simply above the current scroll position.
  test('an above-the-fold element is fabricated as "unreachable" once scrolled past', () => {
    // A nav link at document y=100..140, viewed with the page scrolled to y=800.
    const scrolledPast = { left: 16, right: 200, top: 100 - 800, bottom: 140 - 800 };

    const verdict = swClassifyOffscreen(scrolledPast, metrics({ scrollY: 800 }), TOL);
    assert.equal(verdict.above, true,
      'this is the fabrication: a reachable element classified unreachable purely because '
      + 'the page was scrolled. The predicate is correct ONLY at the origin -- which is why '
      + 'the origin is now enforced rather than assumed.');
  });

  test('the same element at the scroll origin is correctly reachable', () => {
    const atOrigin = { left: 16, right: 200, top: 100, bottom: 140 };
    const verdict = swClassifyOffscreen(atOrigin, metrics(), TOL);
    assert.deepEqual(verdict, { left: false, right: false, above: false, belowDocument: false });
  });

  // Below the fold is NOT unreachable -- you scroll to it. This was 45 of the
  // old probe's 46 findings.
  test('below-the-fold is reachable; past the end of the document is not', () => {
    const belowFold = { left: 16, right: 200, top: 3495, bottom: 3535 };
    assert.equal(swClassifyOffscreen(belowFold, metrics(), TOL).belowDocument, false);

    const pastDocument = { left: 16, right: 200, top: 4200, bottom: 4240 };
    assert.equal(swClassifyOffscreen(pastDocument, metrics(), TOL).belowDocument, true);
  });

  test('genuinely off-viewport horizontally is still caught', () => {
    assert.equal(swClassifyOffscreen({ left: -80, right: -10, top: 10, bottom: 50 }, metrics(), TOL).left, true);
    assert.equal(swClassifyOffscreen({ left: 400, right: 520, top: 10, bottom: 50 }, metrics(), TOL).right, true);
  });

  // The enforcement itself. Remove the assertion (or the scroll reset that
  // satisfies it) and the fabrication above ships silently into the report.
  test('swAssertUnscrolled throws off the origin and passes on it', () => {
    assert.doesNotThrow(() => swAssertUnscrolled(metrics()));
    assert.throws(() => swAssertUnscrolled(metrics({ scrollY: 800 })), /not the \(0, 0\) origin/);
    assert.throws(() => swAssertUnscrolled(metrics({ scrollX: 40 })), /not the \(0, 0\) origin/);
    // Subpixel jitter is not a scroll.
    assert.doesNotThrow(() => swAssertUnscrolled(metrics({ scrollY: 0.4 })));
  });
});

describe('PAGES -- no fictional surfaces', () => {
  // After M55 #1757 every screen was promoted to its canonical path, and /next/X
  // is served by an internal re-dispatch to the stable handler (HTTP 200, same
  // URL, identical DOM). A page list that still probed /next/* would be labelling
  // the canonical page as a separate "next" surface that does not exist.
  test('no page probes the /next/* lane', () => {
    const next = PAGES.filter(p => p.path.startsWith('/next'));
    assert.deepEqual(next, [], 'the /next/* lane carries no screen content: probing it '
      + 'measures the canonical page under a label that implies a surface that is gone');
  });

  test('every page has a name and an absolute path', () => {
    for (const p of PAGES) {
      assert.ok(p.name, 'page needs a name');
      assert.ok(p.path.startsWith('/'), `${p.name}: path must be absolute (got "${p.path}")`);
    }
  });

  test('page names are unique (they key the report and --only)', () => {
    const names = PAGES.map(p => p.name);
    assert.equal(new Set(names).size, names.length);
  });
});

describe('parseArgs', () => {
  test('--url is required (no default -- the harness MUTATES its target)', () => {
    assert.throws(() => parseArgs([]), /--url is required/);
  });

  test('accepts a minimal valid invocation', () => {
    const opts = parseArgs(['--url', 'http://127.0.0.1:1974']);
    assert.equal(opts.url, 'http://127.0.0.1:1974');
    assert.deepEqual(opts.browsers, ['chromium', 'firefox']);
    assert.equal(opts.headed, false);
  });

  // --pages is REMOVED, not ignored: a caller passing it believes it is
  // selecting a route set. Silently handing back the canonical pages would let
  // them think they had asked for something else.
  test('--pages is rejected with an explanation, not silently ignored', () => {
    assert.throws(
      () => parseArgs(['--url', 'http://127.0.0.1:1974', '--pages', 'next']),
      /--pages has been removed/,
    );
  });

  test('a flag missing its value throws instead of reading the next flag', () => {
    assert.throws(() => parseArgs(['--url']), /--url requires a value/);
    assert.throws(() => parseArgs(['--url', 'http://x', '--browsers']), /--browsers requires a value/);
  });

  test('an unknown engine is rejected', () => {
    assert.throws(
      () => parseArgs(['--url', 'http://127.0.0.1:1974', '--browsers', 'webkit']),
      /unknown engine "webkit"/,
    );
  });

  test('a malformed --url is rejected', () => {
    assert.throws(() => parseArgs(['--url', 'not-a-url']), /not a valid URL/);
  });
});
