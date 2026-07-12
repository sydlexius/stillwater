#!/usr/bin/env node
// run.js - responsive UAT harness CLI (issue #2386).
//
// Runs the four rendered-evidence probes (layout overflow, tap-target size,
// off-screen affordances, axe-core a11y) across the milestone's three pinned
// viewports (360x740, 390x844, 768x1024), both themes (dark + light), against a
// live Stillwater server. This is the harness every mobile/UI issue in the M55
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
//     [--browsers chromium,firefox] [--pages next|legacy] \
//     [--out tests/responsive/report] \
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
// --only <page-name> restricts the run to a single page from the selected
// --pages set (matched against that page's `name`, e.g. "dashboard").
//
// --pages selects which route set to probe: 'next' (default, canonical /next/*
// UX) requires the target server to be running with SW_UX=dual or SW_UX=next --
// the default SW_UX=stable dev config 404s every /next/* route. 'legacy' probes
// the pre-existing stable-UX equivalents. The two sets are kept apples-to-apples
// (same page `name`s wherever a legacy equivalent exists) so a next-vs-legacy
// comparison never flatters one side by omitting a page.
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
import { assertPageIdentity, assertAuthenticated, waitForQuiescence, PageGuardError } from './lib/page-guards.js';
import { openAffordance } from './lib/open-state.js';
import {
  authenticateOnce, resolveFirstArtistId, describeTarget,
  readThemePreference, writeThemePreference,
} from './lib/auth.js';

const DIRNAME = path.dirname(fileURLToPath(import.meta.url));

// Page sets: /next/* is the CANONICAL UX (project memory
// project-next-ux-is-canonical) -- real mobile/UI issues should run against it
// and 'next' is the default here. 'legacy' is a fallback pointed at the
// pre-existing stable-UX equivalents.
//
// PAGE_SETS.next and PAGE_SETS.legacy are kept apples-to-apples on purpose:
// every entry with a real legacy equivalent appears in both sets under the same
// `name`, so a next-vs-legacy comparison never flatters one side by omission.
//
// Verify a route actually serves before adding it here -- and note that "serves"
// is now ENFORCED rather than assumed: assertPageIdentity (lib/page-guards.js)
// hard-fails any record whose final URL is not the requested one. A route that
// silently 302s to its legacy equivalent (as /next/reports/compliance does) can
// no longer be measured under a "next" label by accident. That guard, not the
// comment below, is what keeps this honest.
//
// TODO(#2382): once the More sheet lands, add an open-state probe for it here
// (same shape as prefs-drawer-open below) -- an interactive open/collapsed
// surface is the one thing this harness cannot exercise via a bare page load.
const PAGE_SETS = {
  next: [
    { name: 'dashboard', path: '/next/' },
    { name: 'artists-grid', path: '/next/artists?view=grid' },
    { name: 'settings', path: '/next/settings' },
    { name: 'reports', path: '/next/reports' },
    { name: 'reports-duplicates', path: '/next/reports/duplicates' },
    { name: 'reports-foreign-files', path: '/next/reports/foreign-files' },
    // /next/reports/compliance is NOT included: it redirects to the legacy
    // /reports?tab=compliance page, which has not actually been ported to
    // /next. assertPageIdentity would now hard-fail it rather than silently
    // measure legacy under a "next" label.
    { name: 'activity', path: '/next/activity' },
    { name: 'logs', path: '/next/logs' },
    { name: 'preferences', path: '/next/preferences' },
    // 'artist-detail' is appended at runtime in main() once a real artist id is
    // resolved from the target database -- never hardcode an id here, it would
    // only be valid for one particular database snapshot.
    {
      // Real markup (verified live against a SW_UX=dual instance): the sidebar
      // renders TWO [data-sw-prefs-trigger] elements (nav link + user-menu
      // link). No legacy equivalent exists (the prefs drawer is next-only UI).
      name: 'prefs-drawer-open',
      path: '/next/',
      open: {
        trigger: '[data-sw-prefs-trigger]',
        waitFor: '.sw-prefs-drawer:not([aria-hidden="true"])',
      },
    },
  ],
  legacy: [
    { name: 'dashboard', path: '/dashboard' },
    { name: 'artists', path: '/artists' },
    { name: 'settings', path: '/settings' },
    { name: 'reports', path: '/reports' },
    { name: 'reports-duplicates', path: '/reports?tab=duplicates' },
    { name: 'reports-foreign-files', path: '/reports?tab=foreign-files' },
    { name: 'activity', path: '/activity' },
    { name: 'logs', path: '/logs' },
    { name: 'preferences', path: '/preferences' },
    // 'artist-detail' is appended at runtime in main(), mirroring the next set.
  ],
};

// ARTIST_DETAIL_PATH returns each page set's route template for the
// runtime-resolved artist-detail probe (see main()).
const ARTIST_DETAIL_PATH = {
  next: id => `/next/artists/${id}`,
  legacy: id => `/artists/${id}`,
};

const ENGINES = { chromium, firefox };

// ---------------------------------------------------------------------------
// CLI
// ---------------------------------------------------------------------------

// Every flag that takes a value validates that it GOT one. The pre-fix parser
// did `opts.browsers = argv[++i].split(',')`, which threw a bare TypeError on a
// trailing `--browsers`, and `opts.url = argv[++i]`, which silently set url to
// undefined on a trailing `--url`.
function parseArgs(argv) {
  const opts = {
    url: null,
    browsers: ['chromium', 'firefox'],
    out: path.join(DIRNAME, 'report'),
    pages: 'next',
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
    else if (a === '--pages') opts.pages = value(++i, '--pages');
    else if (a === '--headed') opts.headed = true;
    else if (a === '--slow-mo') opts.slowMo = Number(value(++i, '--slow-mo'));
    else if (a === '--only') opts.only = value(++i, '--only');
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
  if (!PAGE_SETS[opts.pages]) {
    throw new Error(`--pages must be one of: ${Object.keys(PAGE_SETS).join(', ')} (got "${opts.pages}")`);
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
function buildMetadata({ opts, runId, startedAt, browserVersions, target, artist, pageSet }) {
  return {
    runId,
    startedAt: new Date(startedAt).toISOString(),
    harnessIssue: 2386,
    baseURL: opts.url,
    pageSet: opts.pages,
    pages: pageSet.map(p => ({ name: p.name, path: p.path })),
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
  console.log(`Pages: ${opts.pages}   Browsers: ${opts.browsers.join(', ')}   Run: ${runId}\n`);

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

  let pageSet = PAGE_SETS[opts.pages].slice();

  // Resolve a real artist id from the target database rather than hardcode one.
  const artist = await resolveFirstArtistId({ baseURL: opts.url, storageState });
  if (artist.id) {
    pageSet.push({ name: 'artist-detail', path: ARTIST_DETAIL_PATH[opts.pages](artist.id) });
  }

  if (opts.only) {
    pageSet = pageSet.filter(p => p.name === opts.only);
    if (pageSet.length === 0) {
      const available = [...PAGE_SETS[opts.pages].map(p => p.name), artist.id ? 'artist-detail' : null]
        .filter(Boolean);
      throw new Error(`--only "${opts.only}" matched no page in --pages ${opts.pages} (available: ${available.join(', ')})`);
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
            for (const pageDef of pageSet) {
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

  // Put the account's theme back the way we found it (see lib/auth.js). A
  // failure here is REPORTED, not swallowed -- the operator needs to know their
  // account was left on the wrong theme.
  if (originalTheme) {
    const restored = await writeThemePreference({ baseURL: opts.url, storageState, theme: originalTheme });
    if (restored) {
      console.log(`\nRestored the account's original theme preference: ${originalTheme}`);
    } else {
      console.error(
        `\nWARNING: failed to restore the account's original theme preference `
        + `("${originalTheme}"). It is currently left at "${THEMES[THEMES.length - 1]}".`
      );
    }
  }

  const metadata = buildMetadata({ opts, runId, startedAt, browserVersions, target, artist, pageSet });
  const reportPath = path.join(opts.out, `responsive-report-${runId}.json`);
  fs.writeFileSync(reportPath, JSON.stringify({ metadata, results }, null, 2));

  printSummary(results);
  console.log(`\nFull JSON report: ${reportPath}`);
  console.log(`Screenshots:      ${path.join(opts.out, 'screenshots', runId)}`);
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
  if (r.openSkipped) bits.push('open-skipped');
  if (r.openUnsettled) bits.push('OPEN-UNSETTLED');
  if (r.unsettled) bits.push('unsettled');
  bits.push(`overflow=${r.layout.hasHorizontalOverflow ? `YES(${r.layout.overflowPx}px, ${r.layout.offenderCount} root cause(s))` : 'no'}`);
  bits.push(`tap<${r.tapTargets.minPx}px=${r.tapTargets.offenderCount}/${r.tapTargets.totalInteractive}`);
  bits.push(`offscreen=${r.offscreen.offenderCount}`);
  bits.push(`axe=${r.axe.violationCount}`);
  return bits.join(' ');
}

// The pre-fix summary printed RUN COUNTS only ("Horizontal overflow: 6 run(s)"),
// which tells you nothing about whether that is a 5px subpixel seam or a 514px
// blowout. It also folded openFailed runs into the offender tallies alongside
// genuine open-state measurements, so a broken drawer's CLOSED-state numbers
// were quietly averaged into the drawer's score.
function printSummary(results) {
  console.log('\n=== Responsive UAT Harness Summary ===');

  const skipped = results.filter(r => r.skipped);
  const guardFailed = results.filter(r => r.guardFailed);
  const errored = results.filter(r => r.error);
  // A run whose open-state never materialised measured the CLOSED page. Its
  // numbers are real, but they are not the numbers this record claims to be, so
  // they are tallied separately and never mixed into the totals below.
  const suspect = results.filter(r => !r.error && !r.guardFailed && !r.skipped && (r.openFailed || r.openUnsettled));
  const ok = results.filter(r => !r.error && !r.guardFailed && !r.skipped && !r.openFailed && !r.openUnsettled);

  console.log(`Runs: ${results.length} (${ok.length} clean, ${suspect.length} open-state suspect, `
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
  section('OPEN-STATE SUSPECT (probes ran against the closed/mid-animation page)',
    suspect.map(r => `${label(r)}: ${r.openFailed || r.openUnsettled}`));

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

main().catch(err => {
  console.error(`\n${err.message || err}\n`);
  process.exitCode = 1;
});
