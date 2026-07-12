// probes.js - the four rendered-evidence measurements the harness runs per
// page x viewport x theme (issue #2386). Each probe reads live computed
// geometry/style from the page -- never static source -- per the project's
// RENDERED EVIDENCE ONLY rule.
//
// Call installProbeHelpers(page) (probe-helpers.js) once before using these.

import AxeBuilder from '@axe-core/playwright';

import { TAP_TARGET_MIN_PX } from './constants.js';

const OFFENDER_CAP = 25;

// ---------------------------------------------------------------------------
// 1. Layout probe: horizontal overflow.
//
// Reports document-level overflow (scrollWidth > clientWidth) AND every
// element whose right edge exceeds window.innerWidth, since a single wide
// element is often the root cause even when the document total looks small.
// ---------------------------------------------------------------------------
export async function runLayoutProbe(page) {
  return page.evaluate((cap) => {
    const TOLERANCE = 1; // px, avoid subpixel false positives
    const docEl = document.documentElement;
    const viewportWidth = window.innerWidth;
    const docOverflowPx = docEl.scrollWidth - docEl.clientWidth;

    const offenders = [];
    document.querySelectorAll('body *').forEach(el => {
      const style = getComputedStyle(el);
      if (style.display === 'none' || style.visibility === 'hidden') return;
      const rect = el.getBoundingClientRect();
      if (rect.width === 0 && rect.height === 0) return;
      const overflowPx = rect.right - viewportWidth;
      if (overflowPx > TOLERANCE) {
        offenders.push({
          selector: window.__swDescribeSelector(el),
          overflowPx: Math.round(overflowPx * 100) / 100,
          rect: { left: Math.round(rect.left), right: Math.round(rect.right), width: Math.round(rect.width) },
        });
      }
    });
    offenders.sort((a, b) => b.overflowPx - a.overflowPx);

    return {
      scrollWidth: docEl.scrollWidth,
      clientWidth: docEl.clientWidth,
      hasHorizontalOverflow: docOverflowPx > TOLERANCE,
      overflowPx: Math.max(0, Math.round(docOverflowPx * 100) / 100),
      offenderCount: offenders.length,
      offenders: offenders.slice(0, cap),
      offendersTruncated: Math.max(0, offenders.length - cap),
    };
  }, OFFENDER_CAP);
}

// ---------------------------------------------------------------------------
// 2. Tap-target probe: interactive elements under TAP_TARGET_MIN_PX square.
//
// minPx is an explicit param (defaults to the harness constant) so a caller
// can pass the real #2377 token once it lands without editing this file.
// ---------------------------------------------------------------------------
export async function runTapTargetProbe(page, { minPx = TAP_TARGET_MIN_PX } = {}) {
  return page.evaluate(({ minPx, cap }) => {
    const SELECTOR = 'a, button, [role="button"], input, select';
    const offenders = [];
    let total = 0;
    document.querySelectorAll(SELECTOR).forEach(el => {
      const style = getComputedStyle(el);
      if (style.display === 'none' || style.visibility === 'hidden') return;
      if (el.tagName === 'INPUT' && el.type === 'hidden') return;
      const rect = el.getBoundingClientRect();
      if (rect.width === 0 || rect.height === 0) return;
      total++;
      if (rect.width < minPx || rect.height < minPx) {
        offenders.push({
          selector: window.__swDescribeSelector(el),
          width: Math.round(rect.width * 100) / 100,
          height: Math.round(rect.height * 100) / 100,
        });
      }
    });
    return {
      minPx,
      totalInteractive: total,
      offenderCount: offenders.length,
      offenders: offenders.slice(0, cap),
      offendersTruncated: Math.max(0, offenders.length - cap),
    };
  }, { minPx, cap: OFFENDER_CAP });
}

// ---------------------------------------------------------------------------
// 3. Off-screen / clipped affordance probe.
//
// Scans fixed/absolute-positioned elements that are CURRENTLY VISIBLE (not
// display:none, visibility:hidden, aria-hidden="true", or opacity:0) and
// reports any whose box extends outside the viewport. Restricting to the
// visible set is deliberate: a closed drawer/menu is commonly
// visibility:hidden while still occupying its open-state geometry off-canvas,
// which reads as a false "off-screen" positive if measured while closed (this
// exact mistake was made in a prior manual sweep). Callers MUST open the
// affordance (click the trigger, wait for the open state) before calling this
// probe -- it only measures whatever is in the DOM at call time.
// ---------------------------------------------------------------------------
export async function runOffscreenProbe(page) {
  return page.evaluate((cap) => {
    const vw = window.innerWidth;
    const vh = window.innerHeight;
    const offenders = [];
    document.querySelectorAll('*').forEach(el => {
      const style = getComputedStyle(el);
      if (style.position !== 'fixed' && style.position !== 'absolute') return;
      if (style.display === 'none' || style.visibility === 'hidden') return;
      if (el.getAttribute('aria-hidden') === 'true') return;
      if (parseFloat(style.opacity) === 0) return;
      const rect = el.getBoundingClientRect();
      if (rect.width === 0 || rect.height === 0) return;
      const clipped = {
        left: rect.left < 0,
        right: rect.right > vw,
        top: rect.top < 0,
        bottom: rect.bottom > vh,
      };
      if (clipped.left || clipped.right || clipped.top || clipped.bottom) {
        offenders.push({
          selector: window.__swDescribeSelector(el),
          position: style.position,
          rect: {
            left: Math.round(rect.left), top: Math.round(rect.top),
            right: Math.round(rect.right), bottom: Math.round(rect.bottom),
          },
          clipped,
        });
      }
    });
    return {
      offenderCount: offenders.length,
      offenders: offenders.slice(0, cap),
      offendersTruncated: Math.max(0, offenders.length - cap),
    };
  }, OFFENDER_CAP);
}

// ---------------------------------------------------------------------------
// 4. axe-core probe: full-page scan via the real @axe-core/playwright
// integration (never hand-rolled a11y checks). `include` optionally scopes
// the scan (e.g. to an open modal/drawer), same convention as tests/a11y.
// ---------------------------------------------------------------------------
export async function runAxeProbe(page, { include } = {}) {
  let builder = new AxeBuilder({ page })
    .withTags(['wcag2a', 'wcag2aa', 'best-practice'])
    .disableRules(['html-has-lang']);
  if (include) builder = builder.include(include);
  const results = await builder.analyze();
  return {
    violationCount: results.violations.length,
    violations: results.violations.map(v => ({
      id: v.id,
      impact: v.impact,
      description: v.description,
      nodeCount: v.nodes.length,
      targets: v.nodes.slice(0, 5).map(n => n.target),
    })),
  };
}

export { TAP_TARGET_MIN_PX };
