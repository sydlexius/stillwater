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
  // Reject an unsupported value BEFORE touching the page. preferences.js
  // silently ignores a key it does not recognise, so a typo ('Dark', 'lite')
  // would no-op and then fail on the class assertion below with "theme did not
  // take effect on <html>" -- a message that accuses the APP of a defect when
  // the caller passed a bad argument. Failing here names the real cause.
  expect(['dark', 'light'], `unsupported theme "${theme}"`).toContain(theme);

  // preferences.js load() runs at DOMContentLoaded and its applyAll(prefs)
  // re-applies the server's SAVED theme when the fetch resolves, which can be
  // after a theme set here. Wait for a load of our own first: it starts after
  // the page's, so it settles after it, and the theme applied below is the last
  // writer. A page without swPreferences (login, error pages) has no load to
  // wait for; the applySingle check below then fails loudly.
  await page.evaluate(() => (window.swPreferences && typeof window.swPreferences.load === 'function'
    ? window.swPreferences.load() : undefined));

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

// renderedContrast measures the contrast ratio of an element's TEXT AS
// RENDERED, from pixels. Needed where axe reports color-contrast as
// "incomplete" (never pass/fail) because the background is a translucent
// surface over an image, so a deliberate low-contrast defect scans clean (#3012).
//
// Method: screenshot the element twice, once normal and once with its text made
// invisible. Pixels that differ between the two are glyph pixels, and the second
// shot is the true background directly behind each of them. Only glyph CORE
// pixels (differing by >= 98% of the largest difference) are scored, because
// antialiased edge pixels are blends of text and background. Each core pixel is
// contrasted against its OWN background pixel and the MINIMUM is returned, so a
// border, icon or image highlight in the box can never lift the result, and one
// weak glyph pixel is not averaged away. Throws when no text pixels are found:
// a number for an element with no visible text would be a silent pass.
export async function renderedContrast(page, locator) {
  const shoot = async () => (await locator.screenshot({ animations: 'disabled' })).toString('base64');
  const withText = await shoot();
  await locator.evaluate((el) => {
    const st = document.createElement('style');
    st.id = 'sw-no-text-probe';
    st.textContent = '[data-sw-no-text], [data-sw-no-text] * { color: transparent !important; '
      + '-webkit-text-fill-color: transparent !important; text-decoration: none !important; '
      + 'text-shadow: none !important; transition: none !important; }';
    document.head.appendChild(st);
    el.setAttribute('data-sw-no-text', '');
  });
  let withoutText;
  try {
    withoutText = await shoot();
  } finally {
    await locator.evaluate((el) => {
      el.removeAttribute('data-sw-no-text');
      document.getElementById('sw-no-text-probe')?.remove();
    });
  }
  return page.evaluate(async ([a, b]) => {
    // No fetch(data:): the app's CSP connect-src blocks it.
    const load = async (data) => {
      const img = await createImageBitmap(new Blob([Uint8Array.from(atob(data), (c) => c.charCodeAt(0))], { type: 'image/png' }));
      const cv = new OffscreenCanvas(img.width, img.height);
      const ctx = cv.getContext('2d');
      ctx.drawImage(img, 0, 0);
      return ctx.getImageData(0, 0, img.width, img.height);
    };
    const [fg, bg] = [await load(a), await load(b)];
    if (fg.width !== bg.width || fg.height !== bg.height) throw new Error('renderedContrast: element size changed between shots');
    const lin = (v) => { const c = v / 255; return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4; };
    const lum = (d, i) => 0.2126 * lin(d[i]) + 0.7152 * lin(d[i + 1]) + 0.0722 * lin(d[i + 2]);
    const diffs = [];
    let max = 0;
    for (let i = 0; i < fg.data.length; i += 4) {
      const d = Math.max(Math.abs(fg.data[i] - bg.data[i]), Math.abs(fg.data[i + 1] - bg.data[i + 1]), Math.abs(fg.data[i + 2] - bg.data[i + 2]));
      diffs.push(d);
      max = Math.max(max, d);
    }
    if (max < 24) throw new Error(`renderedContrast: no glyph pixels found (max pixel difference ${max}); element has no visible text`);
    let min = Infinity;
    diffs.forEach((d, n) => {
      if (d < max * 0.98) return;
      const la = lum(fg.data, n * 4); const lb = lum(bg.data, n * 4);
      min = Math.min(min, (Math.max(la, lb) + 0.05) / (Math.min(la, lb) + 0.05));
    });
    return min;
  }, [withText, withoutText]);
}
