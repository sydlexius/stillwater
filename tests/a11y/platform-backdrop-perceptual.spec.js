// platform-backdrop-perceptual.spec.js - rendered-evidence check for #2979's
// amber left-accent fix on /reports/platform-backdrop-duplicates.
//
// `.sw-card` sets border-color on all four sides at specificity (0,1,0) and
// used to win on source order over the Tailwind `border-amber-500` utility on
// the partial-scan notice: the 4px left width landed, the colour did not.
// The fix routes the accent through `.sw-card-accent-amber`, defined after
// `.sw-card` in input.css so it wins the same way the bug did, used the right
// way round (see the class's doc comment). Source inspection cannot verify a
// cascade outcome -- this reads the LIVE computed style.
//
// Comparing against the resolved --color-amber-500 token, never a hard-coded
// rgb() literal: Tailwind v4 emits oklch(), so a literal string equality
// fails on NOTATION while the colour is correct (same pattern as
// backdrop-perceptual.spec.js's amber-treatment check).

import { test, expect } from 'playwright/test';
import { disableTransitions } from './helpers/settle.js';
import { restorePersistedTheme, applyTheme } from './helpers/axe.js';
import { seedPlatformBackdropScanError } from './helpers/seed-platform-backdrop-duplicates.js';

const PAGE = '/reports/platform-backdrop-duplicates';
const NOTICE = '#platform-backdrop-duplicates-partial-notice';

let closeFake;

// The harness boots against an EMPTY database and library, so the notice
// (rendered only when view.ScanErrors > 0) does not exist until a real scan
// failure is forced. Seeded once for the file: the fixture creates a real
// connection and runs a real scan, which the tests only read the result of.
//
// HOOK TIMEOUT. The fixture's own waitForNotice polls for up to 90s, on
// purpose: the report is served from a cache populated by a BACKGROUND sweep
// (#3092), so the first GET only finds a cold cache and kicks the sweep, and
// the budget has to cover the sweep rather than a render. A Playwright
// beforeAll hook gets the framework's DEFAULT 60s timeout -- `timeout` in
// playwright.config.js is the per-TEST budget and does not raise a hook's --
// so on any runner slower than a laptop the hook was killed at 60s, BEFORE
// the fixture could either succeed or emit its own diagnosis. That is exactly
// what CI showed on 6e6a7ae7: three "beforeAll hook timeout of 60000ms
// exceeded" failures (the attempt plus playwright.config.js's two retries)
// and not one word about which of waitForNotice's three failure states
// actually applied.
//
// 150s = the 90s poll budget plus the scan/connection setup that runs ahead
// of it (runScan alone allows 60s) plus margin. The hook must outlast the
// fixture's own budget so a slow sweep produces the fixture's message, never
// a framework timeout that names nothing. Raise the fixture's deadline and
// this number has to move with it.
//
// WHAT THIS DOES NOT FIX, DELIBERATELY. Raising the hook timeout lets the
// fixture report its own verdict; it does not make every verdict a pass.
// There is a SECOND, independent defect underneath: both duplicate reports
// render from ONE process-wide dupimages.Cache, a single refresh computes
// both halves, and the lazy trigger is then locked out by a 15-minute
// retryCooldown. So an earlier spec that loads either report warms that cache
// BEFORE this fixture's fake Emby exists, and the sweep this fixture needs is
// never allowed to run -- waitForNotice then correctly reports "ScanErrors
// likely never went above 0" after its full 90s. That is an ORDERING defect
// owned by #3092 PR 3/3, which moves both fixtures into global-setup so one
// sweep observes all the data. It is out of scope here on purpose: this file
// is PR 1/3. Run alone (or first) this spec passes; run after contrast.spec.js
// it fails on that ordering, with or without this timeout change.
test.beforeAll(async ({ request }) => {
  test.setTimeout(150_000);
  closeFake = await seedPlatformBackdropScanError(request);
});

// Teardown deletes the fixture CONNECTION as well as closing the loopback
// listener. A connection left behind points at a dead server, reports its NFO
// check as unavailable, and the app's write guard is fail-closed on an
// indeterminate check -- so the debris fails unrelated specs later in the same
// run against the same shared database (nfo-mbid's 409 nfo_write_blocked).
// It takes this hook's own `request` rather than reusing the seeding hook's,
// whose APIRequestContext belongs to a scope that has already been torn down.
test.afterAll(async ({ request }) => {
  if (closeFake) await closeFake(request);
});

test.beforeEach(async ({ page }) => {
  await disableTransitions(page);
});

test.afterEach(async ({ page }) => {
  await restorePersistedTheme(page);
});

async function gotoReportAndAssertNotice(page) {
  await page.goto(PAGE);
  // Precondition: the fixture actually forced ScanErrors > 0. A duplicated
  // notice, or none at all, is its own defect and a bare presence check
  // cannot see it.
  await expect(page.locator(NOTICE)).toHaveCount(1);
}

test('partial-scan notice renders the amber accent, both themes', async ({ page }) => {
  await page.emulateMedia({ colorScheme: 'dark' });
  await gotoReportAndAssertNotice(page);
  await applyTheme(expect, page, 'dark');

  const notice = page.locator(NOTICE);

  const amberToken = await notice.evaluate((el) => {
    const raw = getComputedStyle(document.documentElement).getPropertyValue('--color-amber-500').trim();
    if (!raw) return '';
    // Round-trip through a throwaway element so the token and the measured
    // border-left-color are expressed in the same notation.
    const probe = document.createElement('span');
    probe.style.color = raw;
    document.body.appendChild(probe);
    const normalised = getComputedStyle(probe).color;
    probe.remove();
    return normalised;
  });
  expect(amberToken, '--color-amber-500 did not resolve, so this check would be vacuous').not.toBe('');

  const darkBorder = await notice.evaluate(el => getComputedStyle(el).borderLeftColor);
  expect(
    darkBorder,
    `dark mode: border-left-color was ${darkBorder}, expected the amber-500 token (${amberToken})`,
  ).toBe(amberToken);

  await page.emulateMedia({ colorScheme: 'light' });
  await applyTheme(expect, page, 'light');
  const lightBorder = await notice.evaluate(el => getComputedStyle(el).borderLeftColor);
  expect(
    lightBorder,
    `light mode: border-left-color was ${lightBorder}, expected the amber-500 token (${amberToken})`,
  ).toBe(amberToken);
});
