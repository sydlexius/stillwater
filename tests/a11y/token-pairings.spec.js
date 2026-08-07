// token-pairings.spec.js - the CONTRAST FLOOR over the design-token ink/surface
// ramp (#2875).
//
// WHY THIS EXISTS
//
// Every other spec in this tier scans a PAGE: it catches a bad pairing only if
// some page happens to render it, on the data that run happened to have, on the
// engine that run happened to use. That is how --swd-ink-3 shipped carrying the
// comment "slate-500 -- AA body on white": true on white (4.76:1) and false on
// the two other surfaces it was actually painted on (4.34 on --swd-bg-base,
// 4.55 on --swd-bg-elev). The token passed on the ONE surface it was designed
// against, and nobody measured the rest.
//
// So this spec does not scan a page. It enumerates the ink x surface MATRIX
// directly and asserts every admissible pairing clears its WCAG floor. A new
// surface token, a retuned ink, or a "just one step on the ramp" tweak goes red
// here immediately, on the pairing it broke, without waiting for a page to
// exhibit it.
//
// READ FROM THE RENDERED PAGE, NEVER FROM THE STYLESHEET SOURCE
//
// The values below are resolved with getComputedStyle inside the browser, at
// the branch's real CSS, through the app's own theme path. This spec must not
// re-implement the token values as literals: a test that hard-codes the hex it
// expects passes when the token changes underneath it, which is the failure it
// exists to prevent. The only literals here are the WCAG thresholds themselves.
//
// SCOPE: OPAQUE SURFACES
//
// The pairings asserted here are ink over the OPAQUE surface tokens. The
// translucent glass fills (--swd-surface, driven by the Background Opacity
// preference) composite against an arbitrary backdrop and are governed
// separately by the #1784 floor (--swd-surface-floor, 0.92 effective alpha);
// axe cannot evaluate contrast over a dynamic backdrop, and neither can this
// matrix. The composited case stays the job of the rendered page scans in
// contrast.spec.js. See the --swd-surface-floor comment in design-tokens.css.

import { test, expect } from 'playwright/test';

import { disableTransitions } from './helpers/settle.js';
import { applyTheme, restorePersistedTheme } from './helpers/axe.js';

test.beforeEach(async ({ page }) => {
  await disableTransitions(page);
});

test.afterEach(async ({ page }) => {
  await restorePersistedTheme(page);
});

// WCAG 2.1 SC 1.4.3. Normal-size body text needs 4.5:1. The quiet ink ramp is
// used at small sizes (the dashboard timestamp is 10.5px), so the large-text
// 3:1 relaxation never applies to these pairings and is deliberately not
// offered as an escape hatch here.
const AA_NORMAL = 4.5;

// The matrix. Each ink token is asserted against every surface it can legally
// be painted on, in BOTH themes -- because the failing pairing differs by
// theme: light ink is dark, so its worst surface is the DARKEST one, while dark
// ink is light, so its worst is the LIGHTEST. Checking each against only the
// intuitive extreme (white in light, black in dark) is exactly how the original
// token earned a false "AA body on white" comment.
const INKS = ['--swd-ink', '--swd-ink-2', '--swd-ink-3'];
const SURFACES = ['--swd-bg-base', '--swd-bg-raised', '--swd-bg-elev'];

// contrastMatrix resolves every ink x surface pairing IN THE BROWSER, so the
// numbers come from the same computed values the user's page renders with.
//
// getComputedStyle on a custom property returns the token's declared text, so
// the values are pushed through a real color property on a probe element and
// read back: that makes the browser normalise every admissible CSS color form
// (hex, rgb(), color-mix(), a var() chain) into resolved rgb() channels. A
// regex over the raw token text would silently misread color-mix() -- which
// the surface ramp already uses -- and a misread here reads as a passing
// ratio, not as an error.
async function contrastMatrix(page, inks, surfaces) {
  return page.evaluate(({ inks, surfaces }) => {
    const probe = document.createElement('span');
    probe.style.display = 'none';
    document.body.appendChild(probe);

    // resolve returns [r,g,b] for a custom property, or null if the token does
    // not resolve to a usable color. Null is reported to the test rather than
    // defaulted: a token that silently resolves to transparent-black would
    // otherwise produce a confident, meaningless ratio.
    const resolve = (token) => {
      probe.style.color = '';
      probe.style.color = `var(${token})`;
      const computed = getComputedStyle(probe).color;
      const m = computed.match(/^rgba?\(([^)]+)\)$/);
      if (!m) return null;
      const parts = m[1].split(/[\s,/]+/).filter(Boolean).map(Number);
      if (parts.length < 3 || parts.slice(0, 3).some(Number.isNaN)) return null;
      // A fully transparent resolution means the var() did not land.
      if (parts.length >= 4 && parts[3] === 0) return null;
      return parts.slice(0, 3);
    };

    // WCAG relative luminance + contrast ratio, per SC 1.4.3.
    const lin = (c) => {
      const s = c / 255;
      return s <= 0.04045 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4;
    };
    const lum = ([r, g, b]) => 0.2126 * lin(r) + 0.7152 * lin(g) + 0.0722 * lin(b);
    const ratio = (a, b) => {
      const la = lum(a);
      const lb = lum(b);
      return (Math.max(la, lb) + 0.05) / (Math.min(la, lb) + 0.05);
    };

    const rows = [];
    for (const ink of inks) {
      const inkRgb = resolve(ink);
      for (const surface of surfaces) {
        const surfaceRgb = resolve(surface);
        rows.push({
          ink,
          surface,
          inkRgb,
          surfaceRgb,
          ratio: inkRgb && surfaceRgb ? ratio(inkRgb, surfaceRgb) : null,
        });
      }
    }

    probe.remove();
    return rows;
  }, { inks, surfaces });
}

const hex = (rgb) => (rgb
  ? `#${rgb.map((c) => Math.round(c).toString(16).padStart(2, '0')).join('')}`
  : '(unresolved)');

const formatRows = (rows) => rows.map((r) => `    ${r.ink} ${hex(r.inkRgb)} on `
  + `${r.surface} ${hex(r.surfaceRgb)} = `
  + `${r.ratio === null ? 'UNRESOLVED' : `${r.ratio.toFixed(2)}:1`}`).join('\n');

for (const theme of ['dark', 'light']) {
  test(`ink/surface token pairings clear the AA floor (${theme} theme)`, async ({ page }) => {
    // Any next/ page serves the token stylesheet; the dashboard is the cheapest
    // that is guaranteed to exist on an empty ephemeral DB.
    await page.goto('/next/');
    await page.waitForSelector('.sw-next-header-strip', { timeout: 10_000 });

    await applyTheme(expect, page, theme);

    const rows = await contrastMatrix(page, INKS, SURFACES);

    // PRECONDITION: the matrix must actually have measured something. Without
    // this, a page that failed to load the token stylesheet yields all-null
    // ratios, the filter below finds no FAILING rows, and the test passes
    // vacuously while measuring nothing at all.
    expect(rows, 'no ink/surface pairings were measured').toHaveLength(
      INKS.length * SURFACES.length,
    );
    const unresolved = rows.filter((r) => r.ratio === null);
    expect(
      unresolved,
      `these tokens did not resolve to a color on the rendered ${theme} page, so `
      + `their contrast was never checked:\n${formatRows(unresolved)}`,
    ).toHaveLength(0);

    const failing = rows.filter((r) => r.ratio < AA_NORMAL);
    expect(
      failing,
      `ink/surface pairings below the WCAG AA ${AA_NORMAL}:1 floor (${theme} theme).\n`
      + `The quiet ink ramp is used at small sizes, so the 3:1 large-text\n`
      + `relaxation does not apply. Retune the token in design-tokens.css --\n`
      + `do NOT add a per-surface override, which leaves the next new surface\n`
      + `carrying the same defect (#2875):\n${formatRows(failing)}\n\n`
      + `Full matrix:\n${formatRows(rows)}`,
    ).toHaveLength(0);
  });
}
