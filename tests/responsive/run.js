#!/usr/bin/env node
// run.js - responsive UAT harness CLI (issue #2386).
//
// Runs the four rendered-evidence probes (layout overflow, tap-target size,
// off-screen/clipped affordances, axe-core a11y) across the milestone's
// three pinned viewports (360x740, 390x844, 768x1024), both themes (dark +
// light), against a live Stillwater server. This is the harness every
// mobile/UI issue in the M55 family owes its rendered evidence to -- it
// measures; it does not itself define pass/fail thresholds (those belong to
// the issue consuming a given page's report, e.g. "0 sub-44px targets on
// page X").
//
// Usage:
//   node tests/responsive/run.js [--url http://127.0.0.1:1973] \
//     [--browsers chromium,firefox] [--pages next|legacy] \
//     [--out tests/responsive/report] \
//     [--headed] [--slow-mo <ms>] [--only <page-name>]
//
// Requires a running Stillwater server (this harness does NOT boot one --
// point --url at whatever instance you already have up, e.g. the dev
// server on :1973). It authenticates itself (tests/a11y/helpers/bootstrap.js)
// so no manual login is needed.
//
// --headed launches a visible browser window instead of the default
// headless run. This is specifically for MAINTAINER TANDEM-UAT (watching a
// run live alongside the agent); solo/pre-UAT passes stay headless. Default
// is unchanged (headless: true) when --headed is absent.
//
// --slow-mo <ms> adds Playwright's slowMo delay between actions (default 0).
// Only meaningful paired with --headed -- a headed run at full speed flashes
// past too fast to watch.
//
// --only <page-name> restricts the run to a single page from the selected
// --pages set (matched against that page's `name`, e.g. "dashboard"), so a
// tandem-UAT session can focus on one screen instead of the whole matrix.
//
// --pages selects which route set to probe: 'next' (default, canonical
// /next/* UX) requires the target server to be running with SW_UX=dual or
// SW_UX=next -- the default SW_UX=stable dev config 404s every /next/*
// route. 'legacy' probes the pre-existing stable-UX equivalents, for use
// against a server not booted that way. The two sets are kept apples-to-
// apples (same page `name`s wherever a legacy equivalent exists) so a
// next-vs-legacy comparison never flatters one side by omitting a page.
//
// The artist-detail page is added to whichever set is selected at runtime,
// using the first artist id returned by GET /api/v1/artists against the
// target server -- never a hardcoded id, so this stays portable across
// databases. If the target database has no artists (or the lookup fails),
// the harness logs a warning and skips that probe rather than erroring out.
//
// FIREFOX CAVEAT: Playwright's Firefox engine does not support device/touch
// emulation (`isMobile` / `hasTouch` context options are Chromium-only --
// setting them on a firefox context throws). On Firefox, "mobile" here means
// VIEWPORT WIDTH ONLY; there is no synthesized touch input. Run Chromium
// (with isMobile/hasTouch on the two phone viewports) when touch-specific
// behavior (tap vs. hover, touch-action) is what's under test. Firefox stays
// a first-class target for everything else (layout, tap-target sizing, axe).
//
// Screenshots and the JSON/summary report land under
// tests/responsive/report/ (gitignored), never a bare/relative filename.

import { chromium, firefox } from 'playwright';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { VIEWPORTS, THEMES, TAP_TARGET_MIN_PX } from './lib/constants.js';
import { installProbeHelpers } from './lib/probe-helpers.js';
import { runLayoutProbe, runTapTargetProbe, runOffscreenProbe, runAxeProbe } from './lib/probes.js';
import { setTheme, applyTheme } from './lib/theme.js';
import { authenticateOnce, resolveFirstArtistId } from './lib/auth.js';

const DIRNAME = path.dirname(fileURLToPath(import.meta.url));

// Page sets: /next/* is the CANONICAL UX (project memory
// project-next-ux-is-canonical) -- real mobile/UI issues should run against
// it and 'next' is the default here. It requires the target server to be
// booted with SW_UX=dual (or =next); the default SW_UX=stable dev config
// 404s every /next/* route (project memory
// reference_sw_ux_dev_next_channel). 'legacy' is a fallback pointed at the
// pre-existing stable-UX equivalents, for sample-running this harness (or
// probing a legacy screen on purpose) against a server that wasn't booted
// with SW_UX=dual.
// PAGE_SETS.next and PAGE_SETS.legacy are kept apples-to-apples on purpose:
// every entry with a real legacy equivalent appears in both sets under the
// same `name`, so a next-vs-legacy comparison never flatters one side by
// omission (#2386 fix-round-2 -- an earlier version of this file had
// /next/settings missing entirely while legacy/settings was present, which
// made /next look better than legacy purely because the harness never
// visited /next's worst offender). Verify a route actually serves (curl it
// against a live SW_UX=dual instance) before adding it here -- do not add a
// route on the assumption it exists.
//
// TODO(#2382): once the More sheet lands, add an open-state probe for it
// here (same shape as prefs-drawer-open below) -- an interactive
// open/collapsed surface is the one thing this harness cannot exercise via
// a bare page load, and the More sheet is exactly that kind of surface.
const PAGE_SETS = {
  next: [
    { name: 'dashboard', path: '/next/' },
    { name: 'artists-grid', path: '/next/artists?view=grid' },
    { name: 'settings', path: '/next/settings' },
    { name: 'reports', path: '/next/reports' },
    { name: 'reports-duplicates', path: '/next/reports/duplicates' },
    { name: 'reports-foreign-files', path: '/next/reports/foreign-files' },
    // /next/reports/compliance is NOT included: it 302-redirects to the
    // legacy /reports?tab=compliance page (verified live) -- that page has
    // not actually been ported to /next yet, so probing it would silently
    // measure the legacy page under a "next" label.
    { name: 'activity', path: '/next/activity' },
    { name: 'logs', path: '/next/logs' },
    { name: 'preferences', path: '/next/preferences' },
    // 'artist-detail' is appended at runtime in main() once a real artist id
    // is resolved from the target database (see resolveFirstArtistId in
    // lib/auth.js) -- never hardcode an id here, it would only be valid for
    // one particular database snapshot.
    {
      // Real markup (verified live against a SW_UX=dual instance, #2386
      // dogfooding): the sidebar renders TWO [data-sw-prefs-trigger]
      // elements (nav link + user-menu link), not the guessed
      // .sw-prefs-btn/[data-sw-prefs-open] classes tests/a11y's prefs-drawer
      // test falls back through -- that test only runs at desktop width, so
      // the gap never surfaced there. This selector was corrected against
      // the live DOM rather than left as a copy-pasted guess. No legacy
      // equivalent exists for this entry (the prefs drawer is next-only UI).
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

function parseArgs(argv) {
  const opts = {
    url: process.env.SW_TEST_URL || `http://127.0.0.1:${process.env.SW_PORT || '1973'}`,
    browsers: ['chromium', 'firefox'],
    out: path.join(DIRNAME, 'report'),
    pages: 'next',
    headed: false,
    slowMo: 0,
    only: null,
  };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a === '--url') opts.url = argv[++i];
    else if (a === '--browsers') opts.browsers = argv[++i].split(',').map(s => s.trim());
    else if (a === '--out') opts.out = path.resolve(argv[++i]);
    else if (a === '--pages') opts.pages = argv[++i];
    else if (a === '--headed') opts.headed = true;
    else if (a === '--slow-mo') opts.slowMo = Number(argv[++i]);
    else if (a === '--only') opts.only = argv[++i];
  }
  if (!PAGE_SETS[opts.pages]) {
    throw new Error(`--pages must be one of: ${Object.keys(PAGE_SETS).join(', ')} (got "${opts.pages}")`);
  }
  if (!Number.isFinite(opts.slowMo) || opts.slowMo < 0) {
    throw new Error(`--slow-mo must be a non-negative number (got "${opts.slowMo}")`);
  }
  return opts;
}

function slug(...parts) {
  return parts.join('-').replace(/[^a-z0-9-]+/gi, '_');
}

async function runOnePage(context, browserName, pageDef, viewport, theme, outDir) {
  const page = await context.newPage();
  const record = {
    page: pageDef.name, path: pageDef.path, browser: browserName,
    viewport: viewport.name, theme,
  };
  try {
    await installProbeHelpers(page);
    await setTheme(page, theme);
    await page.goto(pageDef.path, { waitUntil: 'load', timeout: 30_000 });
    // setTheme() only controls first paint; the app's async preference sync
    // can overwrite it moments later with the account's server-persisted
    // value (see lib/theme.js applyTheme doc comment). Force + verify via
    // the real preference-set path so the theme actually sticks.
    await applyTheme(page, theme);

    if (pageDef.open) {
      const trigger = page.locator(pageDef.open.trigger).first();
      if (await trigger.count() > 0) {
        // A present-but-not-actionable trigger (zero-size, covered, or
        // otherwise unclickable) is itself a real finding worth keeping --
        // NOT a reason to let the whole viewport/theme run crash. Bound the
        // click well under Playwright's 30s action-timeout default and
        // catch: discovered live (#2386 dogfooding) that this exact trigger
        // is a 0x0 element at every one of this harness's three pinned
        // viewports on /next/, and an uncaught click timeout here took down
        // the entire browser context, silently losing every later
        // page/theme combination in that context's loop.
        try {
          await trigger.click({ timeout: 5_000 });
          await page.waitForSelector(pageDef.open.waitFor, { timeout: 8_000 }).catch(() => {});
        } catch {
          record.openFailed = 'trigger present but not actionable (zero-size/covered/unclickable) '
            + `-- probes below ran against the CLOSED-state page, not the open ${pageDef.name.replace('-open', '')}`;
        }
      } else {
        record.openSkipped = 'trigger not found';
      }
    }

    record.layout = await runLayoutProbe(page);
    record.tapTargets = await runTapTargetProbe(page, { minPx: TAP_TARGET_MIN_PX });
    record.offscreen = await runOffscreenProbe(page);
    record.axe = await runAxeProbe(page);

    const shotName = `${slug(pageDef.name, browserName, viewport.name, theme)}.png`;
    const shotPath = path.join(outDir, 'screenshots', shotName);
    await page.screenshot({ path: shotPath, fullPage: true });
    record.screenshot = shotPath;
  } catch (err) {
    record.error = String(err && err.stack || err);
  } finally {
    await page.close();
  }
  return record;
}

async function main() {
  const opts = parseArgs(process.argv.slice(2));
  fs.mkdirSync(path.join(opts.out, 'screenshots'), { recursive: true });

  // Authenticate EXACTLY ONCE for the whole run (see lib/auth.js) -- the
  // login endpoint is rate-limited and a per-context login blows through
  // that budget within a couple of viewport iterations.
  const storageState = await authenticateOnce({
    baseURL: opts.url,
    adminUser: process.env.STILLWATER_ADMIN_USER,
    adminPass: process.env.STILLWATER_ADMIN_PASSWORD,
  });

  let pageSet = PAGE_SETS[opts.pages].slice();

  // Resolve a real artist id from the target database rather than hardcode
  // one (see resolveFirstArtistId doc comment) -- this keeps the harness
  // portable across whatever database a given server instance is running.
  const artistId = await resolveFirstArtistId({ baseURL: opts.url, storageState });
  if (artistId) {
    pageSet.push({ name: 'artist-detail', path: ARTIST_DETAIL_PATH[opts.pages](artistId) });
  } else {
    console.warn(
      'No artist found in the target database (or the lookup failed) -- '
      + 'skipping the artist-detail probe. See resolveFirstArtistId in lib/auth.js.'
    );
  }

  if (opts.only) {
    pageSet = pageSet.filter(p => p.name === opts.only);
    if (pageSet.length === 0) {
      const available = [...PAGE_SETS[opts.pages].map(p => p.name), artistId ? 'artist-detail' : null]
        .filter(Boolean);
      throw new Error(`--only "${opts.only}" matched no page in --pages ${opts.pages} (available: ${available.join(', ')})`);
    }
  }

  const results = [];

  for (const browserName of opts.browsers) {
    const engine = ENGINES[browserName];
    if (!engine) {
      console.error(`Unknown browser "${browserName}" (expected chromium or firefox)`);
      process.exitCode = 1;
      continue;
    }
    const browser = await engine.launch({ headless: !opts.headed, slowMo: opts.slowMo });
    try {
      for (const viewport of VIEWPORTS) {
        // isMobile/hasTouch are Chromium-only; Firefox throws if set. On
        // Firefox, mobile viewports are viewport-size-only (see header caveat).
        const isPhone = viewport.width < 768;
        const contextOpts = {
          baseURL: opts.url,
          storageState,
          viewport: { width: viewport.width, height: viewport.height },
        };
        if (browserName === 'chromium' && isPhone) {
          contextOpts.isMobile = true;
          contextOpts.hasTouch = true;
        }

        const context = await browser.newContext(contextOpts);
        try {
          for (const theme of THEMES) {
            for (const pageDef of pageSet) {
              const record = await runOnePage(context, browserName, pageDef, viewport, theme, opts.out);
              results.push(record);
              const status = record.error ? `ERROR: ${record.error.split('\n')[0]}` : summarizeRecord(record);
              console.log(`[${browserName}/${viewport.name}/${theme}] ${pageDef.name}: ${status}`);
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

  const reportPath = path.join(opts.out, `responsive-report-${Date.now()}.json`);
  fs.writeFileSync(reportPath, JSON.stringify(results, null, 2));

  printSummary(results);
  console.log(`\nFull JSON report: ${reportPath}`);
}

function summarizeRecord(r) {
  const bits = [];
  if (r.openFailed) bits.push('OPEN-FAILED');
  if (r.openSkipped) bits.push('open-skipped');
  bits.push(`overflow=${r.layout.hasHorizontalOverflow ? `YES(${r.layout.overflowPx}px, ${r.layout.offenderCount} els)` : 'no'}`);
  bits.push(`tap<${r.tapTargets.minPx}px=${r.tapTargets.offenderCount}/${r.tapTargets.totalInteractive}`);
  bits.push(`offscreen=${r.offscreen.offenderCount}`);
  bits.push(`axe=${r.axe.violationCount}`);
  return bits.join(' ');
}

function printSummary(results) {
  console.log('\n=== Responsive UAT Harness Summary ===');
  const errored = results.filter(r => r.error);
  if (errored.length) {
    console.log(`\n${errored.length} run(s) errored:`);
    for (const r of errored) {
      console.log(`  [${r.browser}/${r.viewport}/${r.theme}] ${r.page}: ${r.error.split('\n')[0]}`);
    }
  }
  const ok = results.filter(r => !r.error);
  const overflowing = ok.filter(r => r.layout.hasHorizontalOverflow);
  const tapOffenders = ok.filter(r => r.tapTargets.offenderCount > 0);
  const offscreenOffenders = ok.filter(r => r.offscreen.offenderCount > 0);
  const axeOffenders = ok.filter(r => r.axe.violationCount > 0);
  const openFailed = ok.filter(r => r.openFailed);
  console.log(`Runs: ${results.length} (${ok.length} completed, ${errored.length} errored)`);
  console.log(`Horizontal overflow: ${overflowing.length} run(s)`);
  console.log(`Sub-${TAP_TARGET_MIN_PX}px tap targets: ${tapOffenders.length} run(s)`);
  console.log(`Off-screen/clipped affordances: ${offscreenOffenders.length} run(s)`);
  console.log(`axe-core violations: ${axeOffenders.length} run(s)`);
  if (openFailed.length) {
    console.log(`\nOPEN-STATE FAILURES (trigger present but not actionable): ${openFailed.length} run(s):`);
    for (const r of openFailed) {
      console.log(`  [${r.browser}/${r.viewport}/${r.theme}] ${r.page}: ${r.openFailed}`);
    }
  }
}

main().catch(err => {
  console.error(err);
  process.exitCode = 1;
});
