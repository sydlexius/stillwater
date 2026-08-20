// filter-flyout-contrast.spec.js - the open-filter-flyout axe scan (#3093).
//
// WHY THIS FILE EXISTS AND WHY IT TARGETS COMPLIANCE
//
// The neutral filter chip's label ink was slate-500 in light mode, which at the
// chip's 12px/400 measures 4.31:1 against the flyout's light panel. AA requires
// 4.5:1 for normal-weight text at that size, so axe reports it as a SERIOUS
// color-contrast violation on every neutral chip.
//
// It went unseen for as long as it did because a CLOSED flyout is
// visibility:hidden by design (the panel slides out and is then removed from
// paint, find-in-page and the tab order), and axe does not measure a hidden
// subtree. Every existing full-page scan in this tier ran against panes whose
// flyouts were shut, so all of them were green over the defect.
//
// The scan targets the COMPLIANCE flyout deliberately. `.sw-filter-item` is a
// shared component used by every filter flyout in the app (artists, dashboard,
// activity, logs, compliance), and the compliance one predates this work, so a
// scan there proves the fix on a surface that is not new. It also means this
// spec keeps its value if any single pane is later reworked.
//
// Runs on BOTH engines (firefox-a11y authoritative, chromium-a11y for
// compatibility) and BOTH themes: contrast is a rendered-color property, so
// each theme is a separate measurement rather than an inference from the other.
// Dark is measured rather than assumed passing -- its token value clears AA on
// the dark panel unaided, and this asserts that rather than trusting it.

import { test, expect } from 'playwright/test';

import { disableTransitions } from './helpers/settle.js';
import { buildAxeBuilder, formatViolations, applyTheme, restorePersistedTheme } from './helpers/axe.js';

// The reports workspace defaults to the compliance report, whose pane carries
// the filter trigger and flyout.
const PANE_URL = '/reports';

test.beforeEach(async ({ page }) => {
  await disableTransitions(page);
});

// Tests here exercise the real theme-switch path, which persists server-side.
// Spec files run in alphabetical order, so an unrestored light theme would
// become the starting state for every later file.
test.afterEach(async ({ page }) => {
  await restorePersistedTheme(page);
});

for (const theme of ['dark', 'light']) {
  test(`the open compliance filter flyout passes a full-page a11y scan (${theme} theme)`, async ({ page }) => {
    if (theme === 'dark') {
      // Satisfies the 'system' preference branch in preferences.js.
      await page.emulateMedia({ colorScheme: 'dark' });
    }
    await page.goto(PANE_URL);
    await page.waitForLoadState('load');
    await page.waitForSelector('#compliance-filter-trigger', { timeout: 10_000 });

    // The theme is applied through the app's OWN path, never by forcing a
    // .dark class. Forcing the class skips the --sw-glass-bg recompute and can
    // scan a half-applied theme: the identical shortcut in contrast.spec.js
    // produced false violations at ratios as bad as 1.07:1 against a surface no
    // user ever sees (#2872).
    await applyTheme(expect, page, theme);

    await page.locator('#compliance-filter-trigger').click();
    await page.waitForSelector('#compliance-filter-flyout:not([inert])', { timeout: 5000 });

    // Preconditions. A hidden or empty panel would make the scan below report a
    // meaningless green: axe skips subtrees it considers invisible, so "0
    // violations" over a closed flyout says nothing about the chips inside it.
    const panel = await page.evaluate(() => {
      const el = document.getElementById('compliance-filter-flyout');
      if (!el) return { open: false, chips: 0 };
      const cs = getComputedStyle(el);
      return {
        open: cs.display !== 'none' && cs.visibility !== 'hidden',
        chips: el.querySelectorAll('.sw-filter-item').length,
      };
    });
    expect(panel.open, 'the compliance filter flyout is not visibly open; the scan would measure a hidden '
      + 'subtree and report a green that proves nothing').toBe(true);
    expect(panel.chips, 'the open flyout holds no .sw-filter-item chips, so the ink under test is not on '
      + 'screen and this scan is vacuous').toBeGreaterThan(0);

    // At least one chip must be in the NEUTRAL state, which is the state whose
    // ink this fix changes. A panel showing only selected (blue) chips would
    // pass without ever rendering the color under test.
    const neutral = await page.evaluate(() => document.querySelectorAll(
      '#compliance-filter-flyout .sw-filter-item:not(.include):not([data-filter-state="include"]):not([data-filter-state="exclude"])',
    ).length);
    expect(neutral, 'no NEUTRAL chip is rendered in the open flyout; the neutral ink is the color this scan '
      + 'exists to measure').toBeGreaterThan(0);

    const results = await buildAxeBuilder(page).analyze();
    expect(
      results.violations,
      `Compliance filter flyout ${theme}-theme a11y violations:\n${formatViolations(results.violations)}`,
    ).toHaveLength(0);
  });
}
