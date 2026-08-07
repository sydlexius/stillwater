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
// preference) composite against a backdrop this matrix cannot know, so they are
// out of scope here and remain the job of the rendered-page scans in
// contrast.spec.js -- axe cannot evaluate contrast over a dynamic backdrop
// either, which is why those surfaces need a rendered measurement rather than a
// computed one.
//
// Do NOT read that exclusion as "the glass surfaces are covered elsewhere by the
// #1784 floor". Only .sw-next-hero still pins --swd-surface-floor; the #1757
// PR-2 cutover removed it from .sw-stat-bubble and .sw-dash-card, so the
// dashboard and most artist-detail cards track --sw-glass-bg down to 0.85 with
// no floor at all (input.css, the .sw-dash-card block). Ink-3 does hold up on
// those composites as measured today (5.71-6.02 dark), but nothing ASSERTS it,
// and a future opacity change is not caught by this file.

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
// regex over the raw token text would silently misread a computed form, and a
// misread reads as a passing ratio rather than as an error.
//
// Coverage of that normalisation, measured rather than assumed: plain hex,
// rgb()/rgba() and a var() chain all resolve to rgb() and parse. color-mix()
// computes to `color(srgb ...)`, which this regex does NOT match -- so it
// returns null and the run fails LOUDLY as unresolved. That is the safe
// direction and no color-mix token is in the matrix today, but extend the
// parser before adding one to SURFACES.
async function contrastMatrix(page, inks, surfaces) {
  return page.evaluate(({ inks, surfaces }) => {
    const probe = document.createElement('span');
    probe.style.display = 'none';
    document.body.appendChild(probe);

    // resolve returns [r,g,b] for a custom property, or null if the token does
    // not resolve to a usable color. Null is reported to the test rather than
    // defaulted: a token that silently resolves to transparent-black would
    // otherwise produce a confident, meaningless ratio.
    const rootStyle = getComputedStyle(document.documentElement);
    const resolve = (token) => {
      // CHECK THE TOKEN IS DECLARED AT ALL, FIRST.
      //
      // `color: var(--undefined)` is invalid at computed-value time, so color
      // falls back to the INHERITED value rather than to nothing -- the probe
      // then reports the page's body text color and the pairing scores a
      // confident, meaningless ~17:1. Measured: on a page with body color
      // rgb(1,2,3), `var(--does-not-exist)` computes to exactly rgb(1, 2, 3).
      //
      // That fails in the DANGEROUS direction, and on the exact change this
      // spec exists to survive: rename --swd-ink-3, leave the old name in INKS,
      // and every pairing passes while measuring body ink against itself. (An
      // undefined SURFACE fails safe -- body ink vs body ink is ~1:1, i.e. red
      // -- so only the ink side was unguarded.)
      if (!rootStyle.getPropertyValue(token).trim()) return null;
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

    // PRECONDITION: every pairing must have RESOLVED to a real color.
    //
    // This is the check that stops a vacuous pass. If the token stylesheet did
    // not load, or a token was renamed out from under INKS/SURFACES, the
    // affected rows carry ratio === null and this fails LOUDLY naming them --
    // rather than the test reporting green having measured nothing.
    //
    // (There is deliberately no assertion on rows.length: the matrix is built
    // by an unconditional nested loop, so its length is INKS x SURFACES by
    // construction and such an assertion could never fail. A guard that cannot
    // fail is worse than no guard -- it reads as protection that is not there.)
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
