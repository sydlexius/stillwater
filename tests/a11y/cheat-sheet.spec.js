// cheat-sheet.spec.js - Playwright a11y scan for the keyboard-shortcut cheat
// sheet modal (#1775). Matches the contrast.spec.js structure and rule set.
//
// Surface covered:
//   Cheat-sheet modal -- opened via '?' on /next/ (dark mode forced), then
//   scanned full-page (NOT scoped to the modal subtree). Scoping axe to a
//   modal subtree hides violations in the surrounding chrome, so the full-page
//   scan is required per the hostile-review spec.
//
// Dark mode is forced via colorScheme: 'dark' on the test context so the scan
// exercises the darker glass surface (--sw-glass-bg dark default) rather than
// whatever the OS default happens to be.
//
// Auth: reuses the same setupAndLogin pattern as contrast.spec.js.

import { test, expect } from 'playwright/test';
import AxeBuilder from '@axe-core/playwright';

import { disableTransitions } from './helpers/settle.js';

// Auth: a single login happens once in global-setup.js; the session is loaded
// into every test context via `use.storageState` (playwright.config.js).

// Force dark mode: prefers-color-scheme: dark media feature so the themeInit
// script resolves to dark on first paint (matching next/'s dark default).
test.use({ colorScheme: 'dark' });

// Disable CSS transitions/animations so axe reads SETTLED colors (see
// helpers/settle.js): a synchronous color-contrast read taken during a
// `transition-colors` window can sample a blended mid-transition color and
// report a false failure. Test-measurement only; production is unchanged.
test.beforeEach(async ({ page }) => {
  await disableTransitions(page);
});

// ---------------------------------------------------------------------------
// Helper: same axe rule set as contrast.spec.js.
// ---------------------------------------------------------------------------
function buildAxeBuilder(page) {
  return new AxeBuilder({ page })
    .withTags(['wcag2a', 'wcag2aa', 'best-practice'])
    .disableRules([
      // Same exemption as contrast.spec.js: structural check, not a concern
      // for rendered smoke (Playwright provides lang via context).
      'html-has-lang',
    ]);
}

// ---------------------------------------------------------------------------
// Cheat-sheet modal: open via '?' then full-page scan.
// ---------------------------------------------------------------------------

test('cheat-sheet modal passes full-page a11y scan (dark mode)', async ({ page }) => {
  await page.goto('/next/');
  // 'networkidle' never settles while the SSE event stream is live.
  await page.waitForLoadState('load');
  // Wait for the dashboard content so keyboard.js and the cheat sheet modal
  // script have both finished executing before we press '?'.
  await page.waitForSelector('.sw-next-header-strip', { timeout: 10_000 });

  // Open the cheat sheet via the '?' keyboard shortcut.
  // '?' is Shift+/ on US keyboards; Playwright dispatches e.key === '?' which
  // is what keyboard.js listens for (shiftKey is intentionally not guarded).
  await page.keyboard.press('?');

  // Wait for the modal to become visible (the .hidden class is removed).
  await page.waitForSelector('#cheat-sheet-modal:not(.hidden)', { timeout: 5_000 });

  // Full-page scan -- do NOT scope to #cheat-sheet-modal.
  // A scoped scan hides violations in the surrounding chrome (contrast.spec.js
  // comment; hostile-review spec requirement for this surface).
  const results = await buildAxeBuilder(page).analyze();
  expect(
    results.violations,
    `Cheat-sheet modal a11y violations:\n${formatViolations(results.violations)}`,
  ).toHaveLength(0);
});

// ---------------------------------------------------------------------------
// Helper: format violations for assertion messages (matches contrast.spec.js).
// ---------------------------------------------------------------------------
function formatViolations(violations) {
  if (!violations.length) return '(none)';
  return violations.map(v =>
    `  [${v.impact}] ${v.id}: ${v.description}\n` +
    v.nodes.slice(0, 2).map(n => `    target: ${JSON.stringify(n.target)}`).join('\n'),
  ).join('\n');
}
