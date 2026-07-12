// probes.js - the four rendered-evidence measurements the harness runs per
// page x viewport x theme (issue #2386). Each probe reads live computed
// geometry/style from the page -- never static source -- per the project's
// RENDERED EVIDENCE ONLY rule.
//
// Call installProbeHelpers(page) (probe-helpers.js) once before using these.
// Every probe measures widths against document.documentElement.clientWidth via
// window.__swMetrics(); see THE ONE WIDTH RULE in probe-helpers.js for why
// window.innerWidth is banned here.

import AxeBuilder from '@axe-core/playwright';

import { TAP_TARGET_MIN_PX, INTERACTIVE_SELECTOR } from './constants.js';

const OFFENDER_CAP = 25;

// ---------------------------------------------------------------------------
// 1. Layout probe: horizontal overflow.
//
// Verdict: the document overflows when scrollWidth > clientWidth.
// Offenders: every element whose right edge exceeds clientWidth -- THE SAME
// width the verdict uses. The pre-fix probe took its verdict from clientWidth
// and its offenders from window.innerWidth, so on Chromium's mobile emulation
// (where those are 360 and 874) it reported a 514px overflow alongside a single
// offender "over by 43.7px" -- a magnitude measured against an 874px viewport
// that does not exist -- while the actual 514px culprits were missing from the
// list entirely.
//
// The raw offender list is then reduced to ROOT CAUSES: an element carried out
// of the viewport by an ancestor that already overflows at least as far is not
// an independent finding. Both numbers are reported -- `offenderCount` is the
// actionable one, `descendantCount` records what was folded into it so the
// reduction is auditable rather than invisible.
// ---------------------------------------------------------------------------
export async function runLayoutProbe(page) {
  return page.evaluate((cap) => {
    const TOLERANCE = 1; // px, avoid subpixel false positives
    const m = window.__swMetrics();
    window.__swAssertUnscrolled();
    const viewportWidth = m.viewportWidth;
    const docOverflowPx = m.scrollWidth - viewportWidth;

    const raw = [];
    let visuallyHiddenSkipped = 0;
    document.querySelectorAll('body *').forEach(el => {
      if (!window.__swVisible(el)) return;
      const rect = el.getBoundingClientRect();
      if (rect.width === 0 && rect.height === 0) return;
      if (rect.right - viewportWidth <= TOLERANCE) return;
      // Visually-hidden-until-focus affordances (a.sw-skip-link) are parked
      // off-canvas on purpose. They are not a layout defect and they appeared
      // on every single record, so they are excluded from the DIAGNOSIS (this
      // offender list) -- but counted, and never excluded from the VERDICT
      // above, which is measured from scrollWidth and is whatever it is.
      if (window.__swVisuallyHidden(el)) { visuallyHiddenSkipped++; return; }
      raw.push({ el, right: rect.right, rect });
    });

    const roots = window.__swRootCauses(raw);
    roots.sort((a, b) => b.right - a.right);

    const describe = e => ({
      selector: window.__swDescribeSelector(e.el),
      overflowPx: Math.round((e.right - viewportWidth) * 100) / 100,
      rect: {
        left: Math.round(e.rect.left),
        right: Math.round(e.rect.right),
        width: Math.round(e.rect.width),
      },
    });

    return {
      viewportWidth,
      scrollWidth: m.scrollWidth,
      hasHorizontalOverflow: docOverflowPx > TOLERANCE,
      overflowPx: Math.max(0, Math.round(docOverflowPx * 100) / 100),
      offenderCount: roots.length,
      // Descendants folded into a root cause above. Not offenders in their own
      // right; recorded so the reduction is auditable.
      descendantCount: raw.length - roots.length,
      visuallyHiddenSkipped,
      offenders: roots.slice(0, cap).map(describe),
      offendersTruncated: Math.max(0, roots.length - cap),
    };
  }, OFFENDER_CAP);
}

// ---------------------------------------------------------------------------
// 2. Tap-target probe: interactive elements under TAP_TARGET_MIN_PX square.
//
// This probe is KEPT alongside axe's WCAG 2.2 `target-size` rule (which the axe
// probe below now enables) rather than replaced by it, because the two measure
// different thresholds: axe's target-size is hardcoded to the WCAG 2.2 AA floor
// of 24x24 and is not configurable, while this milestone's design token
// (TAP_TARGET_MIN_PX, pending #2377) is 44. Deleting this probe in favour of
// axe would silently drop the harness's threshold from 44 to 24 -- i.e. stop
// measuring the thing the milestone is actually about. axe's rule is the
// maintained floor; this probe is the project's stricter ceiling.
//
// EXCLUSIONS. An element that cannot be tapped at all is not a "small tap
// target" -- counting it inflates both the denominator and the offender list
// with findings no one can act on. The pre-fix probe counted 377 elements on
// /next/settings of which ~170 were unreachable: 168 pushed outside the layout
// viewport by the C1 overflow bug (so the tap metric was silently a function of
// a DIFFERENT bug), 16 `disabled`, 5 covered by another element. Every
// exclusion is COUNTED and returned in `excluded` -- nothing is dropped
// silently.
// ---------------------------------------------------------------------------
export async function runTapTargetProbe(page, { minPx = TAP_TARGET_MIN_PX } = {}) {
  return page.evaluate(({ minPx, cap, selector }) => {
    const m = window.__swMetrics();
    window.__swAssertUnscrolled();
    const offenders = [];
    const excluded = {
      notVisible: 0,
      visuallyHidden: 0,
      disabled: 0,
      pointerEventsNone: 0,
      outsideViewportX: 0,
      covered: 0,
    };
    let total = 0;

    document.querySelectorAll(selector).forEach(el => {
      if (el.tagName === 'INPUT' && el.type === 'hidden') return;

      if (!window.__swVisible(el)) { excluded.notVisible++; return; }
      if (window.__swVisuallyHidden(el)) { excluded.visuallyHidden++; return; }
      if (el.disabled === true || el.getAttribute('aria-disabled') === 'true') {
        excluded.disabled++; return;
      }
      if (getComputedStyle(el).pointerEvents === 'none') {
        excluded.pointerEventsNone++; return;
      }

      const rect = el.getBoundingClientRect();
      if (rect.width === 0 || rect.height === 0) { excluded.notVisible++; return; }
      if (window.__swOutsideViewportX(rect)) { excluded.outsideViewportX++; return; }

      // Covered-by-another-element check. Only meaningful when the element's
      // centre is inside the currently-painted viewport box -- elementFromPoint
      // returns null for anything below the fold, and reading that null as
      // "covered" would exclude most of a long page. Below-the-fold controls
      // are scrolled to and tapped normally, so they stay counted.
      const cx = rect.left + rect.width / 2;
      const cy = rect.top + rect.height / 2;
      if (cx >= 0 && cy >= 0 && cx < m.viewportWidth && cy < m.viewportHeight) {
        const hit = document.elementFromPoint(cx, cy);
        if (hit && hit !== el && !el.contains(hit) && !hit.contains(el)) {
          excluded.covered++; return;
        }
      }

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
      excluded,
      excludedTotal: Object.values(excluded).reduce((a, b) => a + b, 0),
      offenders: offenders.slice(0, cap),
      offendersTruncated: Math.max(0, offenders.length - cap),
    };
  }, { minPx, cap: OFFENDER_CAP, selector: INTERACTIVE_SELECTOR });
}

// ---------------------------------------------------------------------------
// 3. Off-screen / unreachable AFFORDANCE probe.
//
// The question this probe answers is the one its name always claimed: "is a
// control the user needs sitting somewhere they cannot reach it?" It scans the
// INTERACTIVE set (same selector as the tap-target probe) at any `position`.
//
// Two things changed from the pre-fix version, both of which made it report
// almost exclusively noise while missing every real finding:
//
//  (a) It filtered to `position: fixed|absolute`, so it never even LOOKED at
//      the 1196 static/relative elements that were genuinely off-viewport
//      horizontally on /next/settings.
//  (b) It compared getBoundingClientRect() (viewport-relative, at scrollY=0)
//      against window.innerHeight, so every absolutely-positioned element below
//      the initial fold on a scrollable page came back "clipped.bottom". 45 of
//      its 46 findings on settings were exactly that -- including a 1x1 tooltip
//      anchor at top:3495. Below the fold is SCROLLABLE-TO, not clipped.
//
// The only genuinely unreachable conditions are the ones swClassifyOffscreen
// (lib/probe-helpers.js) tests -- that predicate is the single definition,
// exercised by unit tests and installed into the page from the same source.
//
// SCROLL ORIGIN. The predicate compares a VIEWPORT-relative rect against a
// DOCUMENT-absolute scrollHeight, which is only valid at scrollY = 0. That used
// to be an assumption written in a comment ("the harness never scrolls before
// probing") and it was false -- openAffordance() clicks a trigger and Playwright
// auto-scrolls it into view, after which `above` fires for every element
// scrolled past, fabricating unreachable offenders. run.js now normalises the
// scroll position before probing (resetScrollToOrigin) and __swAssertUnscrolled
// THROWS here if it somehow did not.
//
// Visually-hidden skip links are exempt: parked off-canvas on purpose, revealed
// on focus.
// ---------------------------------------------------------------------------
export async function runOffscreenProbe(page) {
  return page.evaluate(({ cap, selector }) => {
    const m = window.__swMetrics();
    window.__swAssertUnscrolled();
    const TOLERANCE = window.__swTol;
    const offenders = [];

    document.querySelectorAll(selector).forEach(el => {
      if (!window.__swVisible(el)) return;
      if (window.__swVisuallyHidden(el)) return;
      const rect = el.getBoundingClientRect();
      if (rect.width === 0 || rect.height === 0) return;

      const offscreen = window.__swClassifyOffscreen(rect, m, TOLERANCE);
      if (window.__swIsOffscreen(offscreen)) {
        offenders.push({
          selector: window.__swDescribeSelector(el),
          position: getComputedStyle(el).position,
          rect: {
            left: Math.round(rect.left), top: Math.round(rect.top),
            right: Math.round(rect.right), bottom: Math.round(rect.bottom),
          },
          offscreen,
        });
      }
    });

    return {
      viewportWidth: m.viewportWidth,
      scrollHeight: m.scrollHeight,
      offenderCount: offenders.length,
      offenders: offenders.slice(0, cap),
      offendersTruncated: Math.max(0, offenders.length - cap),
    };
  }, { cap: OFFENDER_CAP, selector: INTERACTIVE_SELECTOR });
}

// ---------------------------------------------------------------------------
// 4. axe-core probe: full-page scan via the real @axe-core/playwright
// integration (never hand-rolled a11y checks). `include` optionally scopes the
// scan (e.g. to an open modal/drawer), same convention as tests/a11y.
//
// TAGS. wcag21a/wcag21aa/wcag22aa are REQUIRED here, not optional extras. The
// pre-fix tag set (wcag2a + wcag2aa + best-practice) predates WCAG 2.1/2.2
// entirely, and axe files its `target-size` rule -- the single rule most
// relevant to a mobile milestone -- under wcag22aa. So the harness built to
// measure tap targets had axe's own tap-target rule switched off. On
// /next/settings @360 that cost 1 of 2 violations, the missing one being
// target-size at 4 nodes. Every `axe=N` in a pre-fix report is an undercount on
// the mobile axis.
//
// No disableRules(). The pre-fix code disabled `html-has-lang`, which was a
// stale copy-paste: <html lang="en"> is present and the rule passes. All the
// disable achieved was suppressing a real WCAG A rule, so that losing `lang`
// would have reported clean.
// ---------------------------------------------------------------------------
const AXE_TAGS = ['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa', 'best-practice'];

export async function runAxeProbe(page, { include } = {}) {
  let builder = new AxeBuilder({ page }).withTags(AXE_TAGS);
  if (include) builder = builder.include(include);
  const results = await builder.analyze();
  return {
    tags: AXE_TAGS,
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
