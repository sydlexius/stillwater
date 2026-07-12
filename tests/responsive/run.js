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
//     [--out tests/responsive/report]
//
// Requires a running Stillwater server (this harness does NOT boot one --
// point --url at whatever instance you already have up, e.g. the dev
// server on :1973). It authenticates itself (tests/a11y/helpers/bootstrap.js)
// so no manual login is needed.
//
// --pages selects which route set to probe: 'next' (default, canonical
// /next/* UX) requires the target server to be running with SW_UX=dual or
// SW_UX=next -- the default SW_UX=stable dev config 404s every /next/*
// route. 'legacy' probes the pre-existing stable-UX equivalents, for use
// against a server not booted that way.
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
import { authenticateOnce } from './lib/auth.js';

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
const PAGE_SETS = {
  next: [
    { name: 'dashboard', path: '/next/' },
    { name: 'artists-grid', path: '/next/artists?view=grid' },
    {
      name: 'prefs-drawer-open',
      path: '/next/',
      open: {
        trigger: '.sw-prefs-btn, [data-sw-prefs-open], [aria-label*="ref"]',
        waitFor: '.sw-prefs-drawer:not([aria-hidden="true"])',
      },
    },
  ],
  legacy: [
    { name: 'dashboard', path: '/dashboard' },
    { name: 'artists', path: '/artists' },
    { name: 'settings', path: '/settings' },
  ],
};

const ENGINES = { chromium, firefox };

function parseArgs(argv) {
  const opts = {
    url: process.env.SW_TEST_URL || `http://127.0.0.1:${process.env.SW_PORT || '1973'}`,
    browsers: ['chromium', 'firefox'],
    out: path.join(DIRNAME, 'report'),
    pages: 'next',
  };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a === '--url') opts.url = argv[++i];
    else if (a === '--browsers') opts.browsers = argv[++i].split(',').map(s => s.trim());
    else if (a === '--out') opts.out = path.resolve(argv[++i]);
    else if (a === '--pages') opts.pages = argv[++i];
  }
  if (!PAGE_SETS[opts.pages]) {
    throw new Error(`--pages must be one of: ${Object.keys(PAGE_SETS).join(', ')} (got "${opts.pages}")`);
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
        await trigger.click();
        await page.waitForSelector(pageDef.open.waitFor, { timeout: 8_000 }).catch(() => {});
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

  const results = [];

  for (const browserName of opts.browsers) {
    const engine = ENGINES[browserName];
    if (!engine) {
      console.error(`Unknown browser "${browserName}" (expected chromium or firefox)`);
      process.exitCode = 1;
      continue;
    }
    const browser = await engine.launch({ headless: true });
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
            for (const pageDef of PAGE_SETS[opts.pages]) {
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
  console.log(`Runs: ${results.length} (${ok.length} completed, ${errored.length} errored)`);
  console.log(`Horizontal overflow: ${overflowing.length} run(s)`);
  console.log(`Sub-${TAP_TARGET_MIN_PX}px tap targets: ${tapOffenders.length} run(s)`);
  console.log(`Off-screen/clipped affordances: ${offscreenOffenders.length} run(s)`);
  console.log(`axe-core violations: ${axeOffenders.length} run(s)`);
}

main().catch(err => {
  console.error(err);
  process.exitCode = 1;
});
