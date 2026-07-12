// probe-helpers.js - browser-side utilities shared by every probe in
// probes.js. Installed once per page via installProbeHelpers(page) (an
// addInitScript, so these survive navigations within the same page/context).
//
// THE ONE WIDTH RULE (#2386 fix-round-3): every probe measures against
// `document.documentElement.clientWidth` -- NEVER `window.innerWidth`.
//
// Under Chromium's `isMobile: true` device emulation the two DIVERGE, badly.
// Measured live on /next/settings at a 360px viewport:
//
//     window.innerWidth                    = 874
//     document.documentElement.clientWidth = 360
//
// innerWidth there is the *visual* viewport after the mobile shrink-to-fit
// zoom-out -- it has already widened itself to swallow the very overflow we
// are trying to detect. Measuring offenders against it asks "does this element
// stick out of the widened box we grew in order to contain the bug?", to which
// the answer is almost always no. Firefox has no device emulation, so its
// innerWidth stays 360; the same page therefore produced 1 offender on
// chromium and 1232 on firefox. That count was an artifact of the engine, not
// a property of the page.
//
// clientWidth is the CSS/layout viewport in BOTH engines, and it is also the
// denominator the document-level overflow verdict (scrollWidth - clientWidth)
// already used -- so it is the only source that makes the verdict and the
// offender list describe the same page.

export async function installProbeHelpers(page) {
  await page.addInitScript(() => {
    // Subpixel tolerance shared by every geometry comparison below.
    const TOL = 1;

    // The single source of truth for "the box the user can actually see".
    // See THE ONE WIDTH RULE above.
    window.__swMetrics = function swMetrics() {
      const d = document.documentElement;
      return {
        viewportWidth: d.clientWidth,
        viewportHeight: d.clientHeight,
        scrollWidth: d.scrollWidth,
        scrollHeight: d.scrollHeight,
      };
    };

    // Element.checkVisibility() is the platform's own answer to "would the
    // user see this?" -- it folds display:none, visibility:hidden, opacity:0
    // and content-visibility into one call, including the ancestor-inherited
    // cases that the old per-element getComputedStyle checks missed. Both
    // engines the harness targets support it. If it ever goes missing we FAIL
    // LOUDLY rather than silently substitute a weaker check that would quietly
    // re-inflate every count (no-silent-failure rule).
    window.__swVisible = function swVisible(el) {
      if (typeof el.checkVisibility !== 'function') {
        throw new Error(
          'Element.checkVisibility() is unavailable in this engine -- the '
          + 'responsive harness relies on it for visibility filtering and will '
          + 'not silently substitute a weaker check.'
        );
      }
      return el.checkVisibility({ checkOpacity: true, checkVisibilityCSS: true });
    };

    // Visually-hidden-but-focusable affordances (skip links, sr-only text).
    // These are DELIBERATELY parked outside the viewport and revealed on
    // focus; they are neither layout bugs nor unreachable controls. The
    // canonical example is `a.sw-skip-link` (198x41 at top:-70), which the
    // pre-fix harness counted as an off-screen offender on every one of its
    // 132 records -- a permanent noise floor.
    window.__swVisuallyHidden = function swVisuallyHidden(el) {
      if (el.closest('.sw-skip-link, .sr-only, [data-sw-visually-hidden]')) return true;
      const s = getComputedStyle(el);
      // The two standard sr-only clipping idioms.
      if (s.clipPath === 'inset(50%)') return true;
      if (s.clip === 'rect(0px, 0px, 0px, 0px)') return true;
      return false;
    };

    // An element is UNREACHABLE-HORIZONTALLY if it sits outside the layout
    // viewport's left/right edges. Below-the-fold is NOT unreachable -- you
    // scroll to it. Conflating the two was the entire content of the old
    // off-screen probe: 45 of its 46 findings on /next/settings were
    // `clipped.bottom` on a page that scrolls.
    window.__swOutsideViewportX = function swOutsideViewportX(rect) {
      const { viewportWidth } = window.__swMetrics();
      return rect.right <= TOL || rect.left >= viewportWidth - TOL;
    };

    // Root-cause reduction. An element whose overflowing ancestor already
    // extends at least as far right is not an independent finding -- it is
    // being carried out of the viewport by that ancestor. Reporting 1252
    // offenders when a handful of containers explain all of them is not
    // actionable evidence, it is a haystack. `entries` is [{ el, right, ... }].
    window.__swRootCauses = function swRootCauses(entries) {
      const byEl = new Map(entries.map(e => [e.el, e]));
      return entries.filter(entry => {
        for (let p = entry.el.parentElement; p; p = p.parentElement) {
          const ancestor = byEl.get(p);
          if (ancestor && ancestor.right >= entry.right - TOL) return false;
        }
        return true;
      });
    };

    // Builds a short, human-readable selector for an offending element:
    // prefers #id, otherwise walks up to 4 ancestors joining tag + first 2
    // classes + :nth-of-type when siblings share a tag. Not guaranteed
    // unique -- it is diagnostic output for a report, not a re-query key.
    window.__swDescribeSelector = function swDescribeSelector(el) {
      if (el.id) return `#${el.id}`;
      const path = [];
      let node = el;
      let depth = 0;
      while (node && node.nodeType === 1 && depth < 4) {
        let seg = node.tagName.toLowerCase();
        if (node.classList && node.classList.length) {
          seg += '.' + Array.from(node.classList).slice(0, 2).join('.');
        }
        const parent = node.parentElement;
        if (parent) {
          const siblings = Array.from(parent.children).filter(c => c.tagName === node.tagName);
          if (siblings.length > 1) {
            seg += `:nth-of-type(${siblings.indexOf(node) + 1})`;
          }
        }
        path.unshift(seg);
        if (node.id) {
          path[0] = `#${node.id}`;
          break;
        }
        node = parent;
        depth++;
      }
      return path.join(' > ');
    };
  });
}
