#!/usr/bin/env node
// run.js - responsive UAT harness CLI (issue #2386).
//
// Runs the four rendered-evidence probes (layout overflow, tap-target size,
// off-screen affordances, axe-core a11y) across the milestone's pinned
// viewports (see VIEWPORTS in lib/constants.js), both themes (dark + light),
// against a live Stillwater server. This is the harness every mobile/UI issue in the M55
// family owes its rendered evidence to -- it measures; it does not itself define
// pass/fail thresholds (those belong to the issue consuming a given page's
// report, e.g. "0 sub-44px targets on page X").
//
// ###########################################################################
// # THIS HARNESS MUTATES THE SERVER YOU POINT IT AT.                        #
// #                                                                         #
// #   * POST /api/v1/auth/setup  -- CREATES an admin account if the target  #
// #     instance has not completed setup                                    #
// #   * PUT  /api/v1/settings    -- marks onboarding complete               #
// #   * PUT  /api/v1/preferences/theme -- ~130 writes per full matrix run   #
// #     (theme forcing goes through the app's real preference-set path so   #
// #     it actually sticks; the original value is restored at the end)      #
// #                                                                         #
// # POINT IT AT A THROWAWAY UAT INSTANCE. --url is REQUIRED and has NO      #
// # default, deliberately: an earlier version defaulted to :1973 and would  #
// # therefore have rewritten the maintainer's own dev account on a bare     #
// # `npm run test:responsive`.                                              #
// ###########################################################################
//
// Usage:
//   STILLWATER_ADMIN_USER=... STILLWATER_ADMIN_PASSWORD=... \
//   node tests/responsive/run.js --url http://127.0.0.1:1974 \
//     [--browsers chromium,firefox] [--out tests/responsive/report] \
//     [--headed] [--slow-mo <ms>] [--only <page-name>]
//
// Both credential env vars are MANDATORY (see lib/auth.js): the harness will
// not fall back to the a11y tier's committed CI defaults, because against an
// instance that has not completed setup that would provision an admin account
// with a password that is public in git history.
//
// Requires an already-running Stillwater server (this harness does NOT boot
// one). It authenticates itself, so no manual login is needed.
//
// --headed launches a visible browser window instead of the default headless
// run. This is specifically for MAINTAINER TANDEM-UAT (watching a run live
// alongside the agent); solo/pre-UAT passes stay headless.
//
// --slow-mo <ms> adds Playwright's slowMo delay between actions (default 0).
// Only meaningful paired with --headed.
//
// --only <page-name> restricts the run to a single page (matched against that
// page's `name`, e.g. "dashboard").
//
// EXIT CODE: 0 only when the matrix actually measured something. A run in which
// every record guard-failed (or errored) exits 1 -- see runFailureReasons. A
// measurement harness whose entire job is to catch "reports success while doing
// nothing" must not commit that sin itself.
//
// FIREFOX CAVEAT: Playwright's Firefox engine does not support device/touch
// emulation (`isMobile` / `hasTouch` are Chromium-only). On Firefox, "mobile"
// here means VIEWPORT WIDTH ONLY; there is no synthesized touch input. Run
// Chromium when touch-specific behavior (tap vs. hover, touch-action) is what's
// under test. Firefox stays a first-class target for everything else.
//
// OUTPUT: report/responsive-report-<runId>.json, with a `metadata` header (see
// buildMetadata) and a `results` array. Screenshots land in
// report/screenshots/<runId>/ and are referenced by a path RELATIVE to the
// report directory, so an old report keeps pointing at its own images (they
// used to be overwritten by every subsequent run) and so no absolute
// /Users/<name>/... path is baked into a shareable artifact.

import { chromium, firefox } from 'playwright';
import fs from 'node:fs';
import path from 'node:path';
import { execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

import { VIEWPORTS, THEMES, TAP_TARGET_MIN_PX } from './lib/constants.js';
import { installProbeHelpers } from './lib/probe-helpers.js';
import { runLayoutProbe, runTapTargetProbe, runOffscreenProbe, runAxeProbe } from './lib/probes.js';
import { setTheme, applyTheme } from './lib/theme.js';
import {
  assertPageIdentity, assertAuthenticated, waitForQuiescence, resetScrollToOrigin, PageGuardError,
} from './lib/page-guards.js';
import { openAffordance } from './lib/open-state.js';
import {
  authenticateOnce, resolveFirstArtistId, describeTarget,
  readThemePreference, writeThemePreference,
} from './lib/auth.js';

const DIRNAME = path.dirname(fileURLToPath(import.meta.url));

// THE PAGES. One list, at the CANONICAL paths.
//
// THERE IS NO next-vs-legacy SPLIT, because there is nothing left to split.
// This harness used to ship two page sets (`--pages next|legacy`) and claim they
// gave an "apples-to-apples next-vs-legacy comparison". That claim was FICTION,
// and the two sets measured the same bytes:
//
//   * M55 #1757 promoted every next/ screen to its canonical path. No dedicated
//     /next/* screen handler remains (internal/api/router.go).
//   * /next/X is now served by nextFallback (internal/api/handlers.go), which
//     re-dispatches INTERNALLY through the mux to the stable handler:
//     `fwd.URL.Path = target; mux.ServeHTTP(w, fwd)`. HTTP 200, same URL, no
//     redirect. The ONLY markup delta is an hx-headers attribute on <main>
//     (web/templates/layout.templ) -- which none of the four probes can see.
//     "next vs legacy" was therefore a tautology: identical DOM on both sides.
//   * The `legacy` set was additionally STALE. There is no `GET /dashboard` page
//     route at all (only the /dashboard/actions + /dashboard/activity HTMX
//     fragments), so its first entry would have 404'd; duplicates and
//     foreign-files have their own canonical routes now, not `?tab=` params.
//
// Probing the canonical paths also drops the harness's SW_UX dependency
// entirely: /next/* needs SW_UX=dual|next and 404s under the dev default of
// SW_UX=stable, which is exactly the "every record guard-failed" run that used
// to exit 0 (see runFailureReasons).
//
// Verify a route actually serves before adding one here -- and "serves" is
// ENFORCED, not assumed: assertPageIdentity (lib/page-guards.js) hard-fails any
// record the SERVER did not serve at the requested path.
//
// WHAT THE GUARD CAN AND CANNOT CATCH -- do not overstate it. It reads
// goto()'s Response, so it catches a SERVER REDIRECT (3xx to a different path)
// and a non-200 status. It CANNOT catch an internal re-dispatch: nextFallback
// rewrites the path server-side and returns 200 at the original URL, which is
// indistinguishable, over HTTP, from that URL having its own handler. Nothing
// short of a markup assertion could tell those apart, and the guard does not
// attempt one. It is a redirect/status guard -- a real one, worth keeping -- and
// nothing more.
//
// TODO(#2382): once the More sheet lands, add an open-state probe for it here
// (same shape as prefs-drawer-open below) -- an interactive open/collapsed
// surface is the one thing this harness cannot exercise via a bare page load.
//
// TODO: /reports/compliance is a real canonical route (handleCompliancePage) and
// is a candidate to add. It was excluded because the OLD /next/reports/compliance
// 302'd to the legacy page; that reason is gone with the next/ lane. Confirm it
// serves 200 at /reports/compliance before adding it, per the rule above.
export const PAGES = [
  { name: 'dashboard', path: '/' },
  { name: 'artists-grid', path: '/artists?view=grid' },
  { name: 'settings', path: '/settings' },
  { name: 'reports', path: '/reports' },
  { name: 'reports-duplicates', path: '/reports/duplicates' },
  { name: 'reports-foreign-files', path: '/reports/foreign-files' },
  { name: 'activity', path: '/activity' },
  { name: 'logs', path: '/logs' },
  { name: 'preferences', path: '/preferences' },
  // 'artist-detail' is appended at runtime in main() once a real artist id is
  // resolved from the target database -- never hardcode an id here, it would
  // only be valid for one particular database snapshot.
  {
    // Real markup: the sidebar renders TWO [data-sw-prefs-trigger] elements
    // (nav link + user-menu link).
    name: 'prefs-drawer-open',
    path: '/',
    open: {
      trigger: '[data-sw-prefs-trigger]',
      waitFor: '.sw-prefs-drawer:not([aria-hidden="true"])',
    },
  },
];

// The runtime-resolved artist-detail probe's route (see main()).
const artistDetailPath = id => `/artists/${id}`;

const ENGINES = { chromium, firefox };

// ---------------------------------------------------------------------------
// CLI
// ---------------------------------------------------------------------------

// Every flag that takes a value validates that it GOT one. The pre-fix parser
// did `opts.browsers = argv[++i].split(',')`, which threw a bare TypeError on a
// trailing `--browsers`, and `opts.url = argv[++i]`, which silently set url to
// undefined on a trailing `--url`.
export function parseArgs(argv) {
  const opts = {
    url: null,
    browsers: ['chromium', 'firefox'],
    out: path.join(DIRNAME, 'report'),
    headed: false,
    slowMo: 0,
    only: null,
  };
  const value = (i, flag) => {
    const v = argv[i];
    if (v === undefined || v.startsWith('--')) {
      throw new Error(`${flag} requires a value`);
    }
    return v;
  };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a === '--url') opts.url = value(++i, '--url');
    else if (a === '--browsers') opts.browsers = value(++i, '--browsers').split(',').map(s => s.trim()).filter(Boolean);
    else if (a === '--out') opts.out = path.resolve(value(++i, '--out'));
    else if (a === '--headed') opts.headed = true;
    else if (a === '--slow-mo') opts.slowMo = Number(value(++i, '--slow-mo'));
    else if (a === '--only') opts.only = value(++i, '--only');
    // --pages is REMOVED, not silently ignored: a caller passing it believes it
    // is selecting a route set, and would otherwise get the canonical pages back
    // while thinking it had asked for something else.
    else if (a === '--pages') {
      throw new Error(
        '--pages has been removed. It selected between a "next" and a "legacy" route '
        + 'set that measured the SAME rendered page: after M55 #1757 every screen was '
        + 'promoted to its canonical path, and /next/* is now served by an internal '
        + 're-dispatch to the stable handler (HTTP 200, same URL, identical DOM). The '
        + 'harness probes the canonical paths -- see PAGES in this file.'
      );
    }
    else throw new Error(`unknown argument "${a}"`);
  }

  // --url is REQUIRED and has NO default. See the mutation banner at the top of
  // this file: a default of :1973 meant a bare `npm run test:responsive`
  // rewrote the maintainer's own dev account and flipped their theme.
  if (!opts.url) {
    throw new Error(
      '--url is required (no default). This harness MUTATES the server it is '
      + 'pointed at (creates/updates the admin account, writes theme preferences) -- '
      + 'point it at a throwaway UAT instance, e.g. --url http://127.0.0.1:1974'
    );
  }
  try {
    new URL(opts.url);
  } catch {
    throw new Error(`--url is not a valid URL: "${opts.url}"`);
  }
  if (!opts.browsers.length) throw new Error('--browsers must name at least one engine');
  for (const b of opts.browsers) {
    if (!ENGINES[b]) throw new Error(`--browsers: unknown engine "${b}" (expected chromium or firefox)`);
  }
  if (!Number.isFinite(opts.slowMo) || opts.slowMo < 0) {
    throw new Error(`--slow-mo must be a non-negative number (got "${opts.slowMo}")`);
  }
  return opts;
}

function slug(...parts) {
  return parts.join('-').replace(/[^a-z0-9-]+/gi, '_');
}

function gitDescribe() {
  const git = args => {
    try {
      return execFileSync('git', args, { cwd: DIRNAME, encoding: 'utf8' }).trim();
    } catch {
      return null;
    }
  };
  const sha = git(['rev-parse', '--short', 'HEAD']);
  const dirty = git(['status', '--porcelain']);
  return {
    commit: sha,
    branch: git(['rev-parse', '--abbrev-ref', 'HEAD']),
    // A number produced from a dirty tree is not a number anyone can reproduce.
    dirty: dirty === null ? null : dirty.length > 0,
  };
}

// buildMetadata: the header that makes a report ATTRIBUTABLE. Without it, a JSON
// file of raw counts cannot answer "which server, which UX channel, which
// commit, which database, when" -- and two reports cannot be meaningfully
// diffed. `target` carries the database-dependent context (library/connection/
// artist counts, the artist id used) that settings' and artist-detail's numbers
// are only reproducible against.
// There is no `pageSet` key: it used to record 'next' | 'legacy', a distinction
// that no longer exists in the app (see PAGES). `pages` -- the actual paths
// probed -- is the honest, and sufficient, record.
function buildMetadata({ opts, runId, startedAt, browserVersions, target, artist, pages }) {
  return {
    runId,
    startedAt: new Date(startedAt).toISOString(),
    harnessIssue: 2386,
    baseURL: opts.url,
    pages: pages.map(p => ({ name: p.name, path: p.path })),
    viewports: VIEWPORTS,
    themes: THEMES,
    tapTargetMinPx: TAP_TARGET_MIN_PX,
    browsers: browserVersions,
    harnessCommit: gitDescribe(),
    target: { ...target, artistDetailId: artist.id ?? null, artistDetailSkipReason: artist.reason ?? null },
  };
}

// ---------------------------------------------------------------------------
// One page x viewport x theme measurement
// ---------------------------------------------------------------------------
async function runOnePage(context, browserName, pageDef, viewport, theme, ctx) {
  const page = await context.newPage();
  const record = {
    page: pageDef.name, path: pageDef.path, browser: browserName,
    viewport: viewport.name, theme,
  };
  try {
    await installProbeHelpers(page);
    await setTheme(page, theme);

    const response = await page.goto(pageDef.path, { waitUntil: 'load', timeout: 30_000 });

    // GUARD 1 -- page identity. goto()'s Response used to be discarded and the
    // landing URL never recorded, so a redirect or an error page was measured
    // silently under the requested label. Asserted on response.url() (what the
    // SERVER served); page.url() is recorded alongside it for diagnostics but is
    // NOT what identity turns on -- the app history-rewrites it. See
    // assertPageIdentity.
    const identity = assertPageIdentity({ requestedPath: pageDef.path, response });
    record.status = identity.status;
    record.servedURL = identity.servedURL;
    record.finalURL = page.url();

    // GUARD 2 -- authenticated shell. A logged-out request to an app route
    // returns HTTP 200 at the SAME URL with the login page in the body, and the
    // login shell honours the theme seed, so a logged-out run used to report a
    // spotless baseline (overflow=false, tap=5/5) of a login form.
    await assertAuthenticated(page, { requestedPath: pageDef.path });

    // setTheme() only controls first paint; the app's async preference sync can
    // overwrite it moments later with the account's server-persisted value.
    // Force + verify via the real preference-set path so the theme sticks.
    await applyTheme(page, theme);

    // Let the page stop moving before measuring it. Not networkidle: the app
    // holds an open SSE stream, so the network is never idle.
    const quiescence = await waitForQuiescence(page);
    if (!quiescence.quiet) {
      record.unsettled = 'the DOM never went quiet before probing -- these numbers may '
        + 'have been taken mid-update (see waitForQuiescence in lib/page-guards.js)';
    }

    if (pageDef.open) {
      await openAffordance(page, pageDef.open, record);
    }

    // GUARD 3 -- scroll origin. Every probe compares a viewport-relative rect
    // against a document-absolute extent, which is only valid at (0, 0). The
    // harness used to merely ASSERT that in a comment while openAffordance's
    // trigger.click() had Playwright auto-scrolling the page underneath it --
    // at which point the off-screen probe reports everything scrolled past as
    // "unreachable". Normalise, and fail loudly if we cannot.
    record.scroll = await resetScrollToOrigin(page, { requestedPath: pageDef.path });

    record.layout = await runLayoutProbe(page);
    record.tapTargets = await runTapTargetProbe(page, { minPx: TAP_TARGET_MIN_PX });
    record.offscreen = await runOffscreenProbe(page);
    record.axe = await runAxeProbe(page);

    // Screenshots are namespaced per run and referenced RELATIVE to the report
    // directory: deterministic filenames used to be overwritten by every later
    // run (so an OLD report's paths silently pointed at a NEWER run's images),
    // and an absolute path baked /Users/<name>/... into any shared artifact.
    const shotRel = path.join('screenshots', ctx.runId, `${slug(pageDef.name, browserName, viewport.name, theme)}.png`);
    await page.screenshot({ path: path.join(ctx.outDir, shotRel), fullPage: true });
    record.screenshot = shotRel;
  } catch (err) {
    if (err instanceof PageGuardError) {
      // A guard failure means we measured NOTHING -- deliberately. Reporting
      // numbers for the wrong page (or for a login form) is strictly worse than
      // reporting no numbers at all.
      record.guardFailed = { kind: err.kind, message: err.message, ...err.detail };
    } else {
      record.error = String(err && err.stack || err);
    }
  } finally {
    await page.close();
  }
  return record;
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------
async function main() {
  const opts = parseArgs(process.argv.slice(2));
  // runId is the epoch-ms the run started. It names BOTH the report file and
  // the screenshot directory, which is what keeps an old report pointing at its
  // own images instead of at a later run's.
  const startedAt = Date.now();
  const runId = String(startedAt);

  console.log('=== Responsive UAT Harness (#2386) ===');
  console.log(`Target: ${opts.url}  --  THIS RUN WILL WRITE TO THIS SERVER`);
  console.log('  (admin setup + onboarding flag + ~130 theme-preference writes;');
  console.log('   the account\'s original theme is restored at the end of the run)');
  console.log(`Browsers: ${opts.browsers.join(', ')}   Run: ${runId}\n`);

  fs.mkdirSync(path.join(opts.out, 'screenshots', runId), { recursive: true });

  // Authenticate EXACTLY ONCE for the whole run (see lib/auth.js) -- the login
  // endpoint is rate-limited and a per-context login blows through that budget
  // within a couple of viewport iterations. Credentials are MANDATORY.
  const storageState = await authenticateOnce({
    baseURL: opts.url,
    adminUser: process.env.STILLWATER_ADMIN_USER,
    adminPass: process.env.STILLWATER_ADMIN_PASSWORD,
  });

  const target = await describeTarget({ baseURL: opts.url, storageState });
  const originalTheme = await readThemePreference({ baseURL: opts.url, storageState });

  let pages = PAGES.slice();

  // Resolve a real artist id from the target database rather than hardcode one.
  const artist = await resolveFirstArtistId({ baseURL: opts.url, storageState });
  if (artist.id) {
    pages.push({ name: 'artist-detail', path: artistDetailPath(artist.id) });
  }

  if (opts.only) {
    pages = pages.filter(p => p.name === opts.only);
    if (pages.length === 0) {
      const available = [...PAGES.map(p => p.name), artist.id ? 'artist-detail' : null]
        .filter(Boolean);
      throw new Error(`--only "${opts.only}" matched no page (available: ${available.join(', ')})`);
    }
  }

  const results = [];

  // A page that could not be probed gets a RECORD, not a console.warn and a
  // hole in the output. A consumer diffing baselines otherwise sees 10 pages
  // where the last run had 11, with nothing in the machine-readable report to
  // say whether the 11th was skipped or silently passed.
  if (!artist.id && (!opts.only || opts.only === 'artist-detail')) {
    console.warn(`SKIPPED artist-detail: ${artist.reason}`);
    results.push({ page: 'artist-detail', skipped: artist.reason });
  }

  const browserVersions = {};

  for (const browserName of opts.browsers) {
    const browser = await ENGINES[browserName].launch({ headless: !opts.headed, slowMo: opts.slowMo });
    browserVersions[browserName] = browser.version();
    try {
      for (const viewport of VIEWPORTS) {
        // isMobile/hasTouch are Chromium-only; Firefox throws if set. On
        // Firefox, mobile viewports are viewport-size-only (see header caveat).
        const contextOpts = {
          baseURL: opts.url,
          storageState,
          viewport: { width: viewport.width, height: viewport.height },
        };
        if (browserName === 'chromium' && viewport.width < 768) {
          contextOpts.isMobile = true;
          contextOpts.hasTouch = true;
        }

        const context = await browser.newContext(contextOpts);
        try {
          for (const theme of THEMES) {
            for (const pageDef of pages) {
              const record = await runOnePage(
                context, browserName, pageDef, viewport, theme,
                { runId, outDir: opts.out },
              );
              results.push(record);
              console.log(`[${browserName}/${viewport.name}/${theme}] ${pageDef.name}: ${summarizeRecord(record)}`);
            }
          }
        } finally {
          await context.close();
        }
      }
    } finally {
      await browser.close();
    }
  }

  const themeRestore = await restoreTheme({ opts, storageState, originalTheme });

  const metadata = buildMetadata({ opts, runId, startedAt, browserVersions, target, artist, pages });
  const reportPath = path.join(opts.out, `responsive-report-${runId}.json`);
  fs.writeFileSync(reportPath, JSON.stringify({ metadata, results, themeRestore }, null, 2));

  printSummary(results);
  console.log(`\nFull JSON report: ${reportPath}`);
  console.log(`Screenshots:      ${path.join(opts.out, 'screenshots', runId)}`);

  // THE HARNESS MUST NOT COMMIT THE SIN IT EXISTS TO CATCH. A run in which every
  // record guard-failed used to write a full report, print its failures, and
  // exit 0 -- indistinguishable, to any caller checking the exit code, from a
  // clean pass. The same applies to a theme it mutated and could not put back.
  const reasons = runFailureReasons(results, themeRestore);
  if (reasons.length) {
    console.error('\n=== RUN FAILED ===');
    for (const r of reasons) console.error(`  ${r}`);
    process.exitCode = 1;
  }
}

// restoreTheme: put the account's theme back the way we found it (see lib/auth.js).
//
// Returns the OUTCOME -- { ok: true } | { ok: false, reason, leftAt } -- rather
// than only printing it, so runFailureReasons can fail the run on it. Printing
// alone was not enough: stderr is invisible to anything gating on the exit code,
// which is the entire failure mode this harness exists to prosecute.
//
// Both halves count as a failure to restore, because they have the SAME
// consequence for the operator: the account is left on whatever theme ran last.
//   * the READ failed -- we never learned what to restore TO
//   * the WRITE failed -- we knew, and could not put it back
// A read that succeeds and reports NO theme set is not a failure: there is
// genuinely nothing to restore.
async function restoreTheme({ opts, storageState, originalTheme }) {
  const leftAt = THEMES[THEMES.length - 1];

  if (originalTheme.reason) {
    console.error(
      `\nWARNING: could not READ the account's original theme preference `
      + `(${originalTheme.reason}), so it was NOT restored. The account is left at `
      + `"${leftAt}" -- the last theme this run applied.`
    );
    return { ok: false, reason: `could not read the original theme: ${originalTheme.reason}`, leftAt };
  }

  if (!originalTheme.theme) {
    return { ok: true, reason: 'the account had no theme preference set; nothing to restore' };
  }

  const restored = await writeThemePreference({ baseURL: opts.url, storageState, theme: originalTheme.theme });
  if (!restored) {
    console.error(
      `\nWARNING: failed to WRITE the account's original theme preference `
      + `("${originalTheme.theme}"). It is currently left at "${leftAt}".`
    );
    return {
      ok: false,
      reason: `PUT /api/v1/preferences/theme failed while restoring "${originalTheme.theme}"`,
      leftAt,
    };
  }

  console.log(`\nRestored the account's original theme preference: ${originalTheme.theme}`);
  return { ok: true, restoredTo: originalTheme.theme };
}

// ---------------------------------------------------------------------------
// Reporting
// ---------------------------------------------------------------------------

function summarizeRecord(r) {
  if (r.skipped) return `SKIPPED: ${r.skipped}`;
  if (r.guardFailed) return `GUARD-FAILED (${r.guardFailed.kind}): ${r.guardFailed.message}`;
  if (r.error) return `ERROR: ${r.error.split('\n')[0]}`;
  const bits = [];
  if (r.openFailed) bits.push('OPEN-FAILED');
  if (r.openUnsettled) bits.push('OPEN-UNSETTLED');
  // Lower-case: app findings, not run failures (see classifyResults).
  if (r.openSkipped) bits.push('open-skipped');
  if (r.openBlocked) bits.push('open-blocked(app)');
  if (r.unsettled) bits.push('unsettled');
  bits.push(`overflow=${r.layout.hasHorizontalOverflow ? `YES(${r.layout.overflowPx}px, ${r.layout.offenderCount} root cause(s))` : 'no'}`);
  bits.push(`tap<${r.tapTargets.minPx}px=${r.tapTargets.offenderCount}/${r.tapTargets.totalInteractive}`);
  bits.push(`offscreen=${r.offscreen.offenderCount}`);
  bits.push(`axe=${r.axe.violationCount}`);
  return bits.join(' ');
}

// classifyResults: the ONE partition of a result set, shared by the summary and
// the exit-code decision so the two can never disagree about what happened.
//
// A run whose open-state never materialised measured the CLOSED page. Its
// numbers are real, but they are not the numbers that record CLAIMS to be, so
// Open-state records are tallied separately and never mixed into the offender
// totals: their numbers are real, but they are not the numbers the record CLAIMS.
// OPEN-STATE OUTCOMES SPLIT IN TWO, DELIBERATELY (see lib/open-state.js):
//
//   openBlocked / openSkipped -- the trigger was not actionable, or not there at
//     all. That is a finding ABOUT THE APP -- today it is bug #2382, a 0x0
//     [data-sw-prefs-trigger] at every pinned viewport. It is recorded and it does
//     NOT fail the run. A measurement tool must not go red because it FOUND the
//     defect it was pointed at; a harness that exits 1 on every run until #2382
//     lands is a harness whose exit code everyone learns to ignore.
//
//   openFailed / openUnsettled -- the click LANDED and the drawer still never
//     opened (or never stopped moving). The app's 0x0-trigger bug does not
//     explain that, and the record would claim to hold open-state numbers while
//     holding closed-state ones. REAL failure: it fails the run.
//
// This is the line that keeps the harness honest once #2382 is fixed. Under the
// old single `openFailed` marker, the moment the trigger became clickable every
// open-state record would have gone green -- even if the drawer never opened.
//
// None of the four are `ok`: in every one of them the probes measured the CLOSED
// (or mid-animation) page, so their numbers are not the numbers the record claims.
export function classifyResults(results) {
  const isOpenBlocked = r => r.openBlocked || r.openSkipped;
  const isOpenFailure = r => r.openFailed || r.openUnsettled;
  const measured = r => !r.error && !r.guardFailed && !r.skipped;

  return {
    skipped: results.filter(r => r.skipped),
    guardFailed: results.filter(r => r.guardFailed),
    errored: results.filter(r => r.error),
    // Recorded app findings. Not clean measurements, but not our failure either.
    openBlocked: results.filter(r => measured(r) && isOpenBlocked(r) && !isOpenFailure(r)),
    // Genuine open-state failures. These fail the run.
    openFailure: results.filter(r => measured(r) && isOpenFailure(r)),
    ok: results.filter(r => measured(r) && !isOpenBlocked(r) && !isOpenFailure(r)),
  };
}

// runFailureReasons: why this run must exit NON-ZERO, in words. Empty = exit 0.
//
// THE BUG THIS FIXES. The only `process.exitCode = 1` in this file was on a
// top-level throw. So a run that measured NOTHING -- wrong SW_UX and every
// /next/* 404, or a logged-out session, i.e. every single record guard-failed --
// wrote a full report, printed its failures, and exited 0. Any caller gating on
// the exit code (CI, a script, an agent) read that as a pass. This harness exists
// to catch operations that report success while doing nothing; it does not get to
// be one.
//
// Named causes, not a bare code: an operator who sees "exit 1" and no reason
// tends to re-run it.
//
// `themeRestore` is the outcome of putting the account's theme back:
// { ok: true } | { ok: false, reason, leftAt }. It is a RUN FAILURE, not just a
// stderr warning. The warning was the weaker half of the same lesson the rest of
// this function encodes: a caller gating on the exit code (CI, a script, an
// agent) never reads stderr, so a silent exit 0 tells it everything went fine
// while the maintainer's account sits mutated on the wrong theme. The harness
// mutates a real server; failing to undo that is a failure of the run.
export function runFailureReasons(results, themeRestore = { ok: true }) {
  const { ok, guardFailed, errored, openFailure } = classifyResults(results);
  const reasons = [];

  if (ok.length === 0) {
    reasons.push(
      `MEASURED NOTHING: 0 of ${results.length} record(s) produced a usable measurement. `
      + 'Nothing in this report is evidence of anything. Common causes: the target server '
      + 'is not the one you think it is, or the session is logged out (see the GUARD '
      + 'FAILURES above).'
    );
  }
  if (guardFailed.length) {
    const kinds = [...new Set(guardFailed.map(r => r.guardFailed.kind))].sort();
    reasons.push(
      `${guardFailed.length} record(s) GUARD-FAILED [${kinds.join(', ')}] -- the harness `
      + 'refused to measure them (wrong page, not logged in, or the page would not return '
      + 'to the scroll origin).'
    );
  }
  if (errored.length) {
    reasons.push(`${errored.length} record(s) ERRORED while probing.`);
  }
  // NOT openBlocked/openSkipped -- those are app findings and stay green. See
  // classifyResults for why that line is where it is.
  if (openFailure.length) {
    reasons.push(
      `${openFailure.length} record(s) FAILED TO OPEN an affordance whose trigger WAS `
      + 'actionable (the click landed and the open state never arrived, or never settled). '
      + 'Those records hold CLOSED-state numbers under an open-state label. This is not the '
      + 'app\'s 0x0-trigger bug -- that is recorded separately and does not fail the run.'
    );
  }
  if (!themeRestore.ok) {
    reasons.push(
      `THEME NOT RESTORED: the account this run authenticated as is left on `
      + `"${themeRestore.leftAt}" instead of its original theme (${themeRestore.reason}). `
      + 'This harness MUTATES the server it is pointed at and owes it a clean restore; a '
      + 'caller gating on the exit code would otherwise never learn the account was left '
      + 'dirty.'
    );
  }
  return reasons;
}

// The pre-fix summary printed RUN COUNTS only ("Horizontal overflow: 6 run(s)"),
// which tells you nothing about whether that is a 5px subpixel seam or a 514px
// blowout. It also folded openFailed runs into the offender tallies alongside
// genuine open-state measurements, so a broken drawer's CLOSED-state numbers
// were quietly averaged into the drawer's score.
function printSummary(results) {
  console.log('\n=== Responsive UAT Harness Summary ===');

  const { skipped, guardFailed, errored, openBlocked, openFailure, ok } = classifyResults(results);

  console.log(`Runs: ${results.length} (${ok.length} clean, ${openFailure.length} open-state FAILED, `
    + `${openBlocked.length} open-state blocked (app finding), `
    + `${guardFailed.length} guard-failed, ${errored.length} errored, ${skipped.length} skipped)`);

  const label = r => `[${r.browser}/${r.viewport}/${r.theme}] ${r.page}`;
  const section = (title, rows) => {
    if (!rows.length) return;
    console.log(`\n${title}:`);
    for (const line of rows) console.log(`  ${line}`);
  };

  section('SKIPPED (no record measured)', skipped.map(r => `${r.page}: ${r.skipped}`));
  section('GUARD FAILURES (refused to measure -- wrong page or not logged in)',
    guardFailed.map(r => `${label(r)}: [${r.guardFailed.kind}] ${r.guardFailed.message}`));
  section('ERRORED', errored.map(r => `${label(r)}: ${r.error.split('\n')[0]}`));
  // These FAIL the run: the trigger was actionable and it still did not open.
  section('OPEN-STATE FAILURES (the click landed and the affordance still never opened)',
    openFailure.map(r => `${label(r)}: ${r.openFailed || r.openUnsettled}`));
  // These do NOT fail the run: the trigger could not be tapped, which is the app
  // bug this harness is here to FIND (#2382 today). See classifyResults.
  section('OPEN-STATE BLOCKED -- APP FINDING, not a harness failure (probes ran against the closed page)',
    openBlocked.map(r => {
      const rect = r.openBlockedRect;
      const geom = rect && rect.width !== undefined ? ` [trigger ${rect.width}x${rect.height} at (${rect.left}, ${rect.top})]` : '';
      return `${label(r)}: ${r.openBlocked || r.openSkipped}${geom}`;
    }));

  const overflowing = ok.filter(r => r.layout.hasHorizontalOverflow);
  const tapOffenders = ok.filter(r => r.tapTargets.offenderCount > 0);
  const offscreenOffenders = ok.filter(r => r.offscreen.offenderCount > 0);
  const axeOffenders = ok.filter(r => r.axe.violationCount > 0);

  const worstBy = (rows, fn) => rows.reduce((a, b) => (fn(b) > fn(a) ? b : a), rows[0]);

  console.log('\nAcross the clean runs:');
  if (overflowing.length) {
    const w = worstBy(overflowing, r => r.layout.overflowPx);
    console.log(`  Horizontal overflow: ${overflowing.length}/${ok.length} run(s); `
      + `worst ${w.layout.overflowPx}px on ${label(w)}`);
    console.log(`    root cause: ${w.layout.offenders[0] ? w.layout.offenders[0].selector : '(none listed)'}`);
  } else {
    console.log(`  Horizontal overflow: none`);
  }
  if (tapOffenders.length) {
    const w = worstBy(tapOffenders, r => r.tapTargets.offenderCount);
    console.log(`  Sub-${TAP_TARGET_MIN_PX}px tap targets: ${tapOffenders.length}/${ok.length} run(s); `
      + `worst ${w.tapTargets.offenderCount}/${w.tapTargets.totalInteractive} on ${label(w)}`);
  } else {
    console.log(`  Sub-${TAP_TARGET_MIN_PX}px tap targets: none`);
  }
  if (offscreenOffenders.length) {
    const w = worstBy(offscreenOffenders, r => r.offscreen.offenderCount);
    console.log(`  Off-screen affordances: ${offscreenOffenders.length}/${ok.length} run(s); `
      + `worst ${w.offscreen.offenderCount} on ${label(w)}`);
  } else {
    console.log(`  Off-screen affordances: none`);
  }
  if (axeOffenders.length) {
    const w = worstBy(axeOffenders, r => r.axe.violationCount);
    const ids = [...new Set(ok.flatMap(r => r.axe.violations.map(v => v.id)))].sort();
    console.log(`  axe-core violations: ${axeOffenders.length}/${ok.length} run(s); `
      + `worst ${w.axe.violationCount} on ${label(w)}`);
    console.log(`    rules: ${ids.join(', ')}`);
  } else {
    console.log(`  axe-core violations: none`);
  }
}

// Run only when executed as a script, never on import. The unit tests
// (tests/unit/responsive-harness.test.js) import parseArgs / classifyResults /
// runFailureReasons / PAGES from this file; a bare `main()` at module scope would
// launch browsers and mutate a server the moment they did.
const isMain = process.argv[1]
  && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url);

if (isMain) {
  main().catch(err => {
    console.error(`\n${err.message || err}\n`);
    process.exitCode = 1;
  });
}
