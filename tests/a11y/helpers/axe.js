// axe.js - the shared axe configuration and failure formatting for the a11y tier.
//
// Every spec in this tier scanned with an identical AxeBuilder config and its
// own copy of a violation formatter. The configs were byte-identical logic with
// three differently-worded comments; the formatters had ALREADY DRIFTED, with
// one printing the help URL, every node and each failure summary while the
// others truncated to two bare targets.
//
// That drift is the argument for this file. Three copies of a rule set is a
// latent inconsistency: the day someone tightens the tags or adds an exemption
// in one spec, the other two silently hold a different bar and nobody sees it,
// because a11y specs fail on CONTENT, not on configuration mismatch.
//
// Consolidated on the RICHER formatter. A truncated failure message costs a
// re-run to diagnose, and this tier's failures are often data-dependent -- the
// re-run may not even reproduce.

import AxeBuilder from '@axe-core/playwright';

// The tier's rule set. wcag2a + wcag2aa covers color-contrast (4.5:1 normal,
// 3:1 large/UI), button-name, label and the aria-* family; best-practice adds
// the landmark and region rules.
//
// html-has-lang is the one exemption: templ emits <html lang="...">, but a
// fixture loaded without the full layout would trip it, and this tier exists to
// catch RENDERED-style violations rather than structural completeness. Keeping
// the exemption here, once, means a future change to it cannot apply to some
// specs and not others.
export function buildAxeBuilder(page) {
  return new AxeBuilder({ page })
    .withTags(['wcag2a', 'wcag2aa', 'best-practice'])
    .disableRules(['html-has-lang']);
}

// formatViolations renders rule id, impact, description, help URL AND every
// node target with its failure summary, so a CI failure is actionable on its
// own. A bare count -- or a truncated node list -- tells the next reader
// nothing about what broke, which matters most here because a11y failures in
// this tier are frequently data-dependent and may not reproduce on a re-run.
export function formatViolations(violations) {
  if (!violations.length) return '(none)';
  return violations.map(v =>
    `  [${v.impact}] ${v.id}: ${v.description}\n` +
    `    help: ${v.helpUrl}\n` +
    v.nodes.map(n => `    target: ${JSON.stringify(n.target)}\n` +
      `      ${String(n.failureSummary || '').replace(/\n/g, '\n      ')}`).join('\n'),
  ).join('\n');
}

// restorePersistedTheme puts the SERVER-SIDE theme preference back to the
// app default after a test that legitimately persisted a change.
//
// Some tests must exercise the real toggle (swSidebar.cycleTheme, which calls
// swPreferences.set) because the thing under test IS that path. `set` writes to
// the server, and Playwright orders spec files alphabetically, so a persisted
// light theme in an early file silently becomes the starting state for every
// later file. That is how a dashboard scan in a "dark mode" spec ended up
// measuring a light-mode amber badge at 4.01:1 -- a real violation, reported
// against a page the test never meant to be looking at.
//
// Called from an afterEach rather than at the end of each toggling test: a
// per-test cleanup is one forgotten call away from silently reintroducing the
// leak, and the failure lands in a DIFFERENT file, where nobody looks for it.
//
// Best-effort by design. If the page is already closed (a timed-out or retried
// test), there is nothing to restore and the next test's own navigation
// re-establishes state; swallowing that is correct, not a silent failure. A
// genuine inability to reach the API still surfaces, as the next spec's theme
// assertion.
export async function restorePersistedTheme(page, theme = 'dark') {
  try {
    if (page.isClosed()) return;
    await page.evaluate((t) => {
      const api = window.swPreferences;
      if (api && typeof api.set === 'function') api.set('theme', t);
    }, theme);
  } catch {
    // Page closed or navigated mid-teardown -- see above.
  }
}

// applyTheme switches theme through the app's OWN preference path, never by
// setting the .dark class directly.
//
// The rendered theme depends on BOTH the class AND an inline --sw-glass-bg that
// preferences.js writes on :root, and that inline value's COLOUR is chosen from
// the class at write time. Setting one without the other leaves the page
// half-themed: dark text over a light glass surface, which axe correctly
// reports at ratios as bad as 1.07:1 -- a real violation of a state no user can
// reach (#2872).
//
// applySingle applies to the DOM WITHOUT persisting. That distinction is
// load-bearing: swPreferences.set writes to the server, and because Playwright
// orders spec files alphabetically, a persisted theme change in one file leaked
// into every file after it and broke unrelated scans.
//
// Throws rather than falling back to a class toggle. A fallback would silently
// reintroduce the exact half-themed state this exists to prevent, and the
// resulting failure would read as a real contrast defect rather than a broken
// helper.
export async function applyTheme(expect, page, theme) {
  const applied = await page.evaluate((t) => {
    const api = window.swPreferences;
    if (!api || typeof api.applySingle !== 'function') return false;
    api.applySingle('theme', t);
    return true;
  }, theme);

  expect(
    applied,
    'window.swPreferences.applySingle is unavailable, so the theme could not be '
    + 'applied through the app\'s own path. Setting the .dark class directly is NOT '
    + 'an acceptable fallback here (#2872): it leaves the inline --sw-glass-bg at the '
    + 'other theme\'s colour and produces false contrast violations.',
  ).toBe(true);

  // Confirm the class actually landed, so a silently-ignored key cannot let a
  // scan run against the wrong theme and report a green that means nothing.
  const isDark = await page.evaluate(() => document.documentElement.classList.contains('dark'));
  expect(isDark, `theme "${theme}" did not take effect on <html>`).toBe(theme === 'dark');
}
