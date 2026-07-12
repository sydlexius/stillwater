// probe-helpers.js - browser-side utilities shared by every probe in
// probes.js. Installed once per page via installProbeHelpers(page) (an
// addInitScript, so these survive navigations within the same page/context).
//
// THE ONE WIDTH RULE (#2386 fix-round-3): every probe measures against
// `document.documentElement.clientWidth` -- NEVER `window.innerWidth`.
//
// Under Chromium's `isMobile: true` device emulation the two DIVERGE, badly.
// Measured live at a 360px viewport (on /next/settings, before the harness moved
// to the canonical paths -- same page, same markup, see run.js PAGES):
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
//
// THE SCROLL-ORIGIN INVARIANT (#2386 fix-round-4). Every probe compares a
// VIEWPORT-relative rect (getBoundingClientRect) against a DOCUMENT-absolute
// extent (documentElement.scrollHeight). Those two share an origin only while
// the page is scrolled to (0, 0). That was previously an ASSUMPTION, recorded
// in a comment ("the harness never scrolls before probing") -- and it was FALSE:
// openAffordance() clicks a trigger, and Playwright AUTO-SCROLLS a click target
// into view. At scrollY > 0 the off-screen probe's `above` test
// (rect.bottom < -1) fires for every element scrolled past, fabricating
// "unreachable" offenders that are simply above the current scroll position.
//
// So the invariant is now ENFORCED, in two places:
//   * run.js calls resetScrollToOrigin() (lib/page-guards.js) after any open-
//     state interaction and before the first probe -- normalising the scroll
//     position, which also makes the numbers reproducible run-to-run (Playwright
//     scrolls by however much that particular trigger needed).
//   * every geometry probe calls window.__swAssertUnscrolled() and THROWS if the
//     page is not at the origin, so a future code path that scrolls without
//     normalising fails loudly instead of quietly fabricating offenders.
//
// Normalising (rather than offsetting every rect by scrollY) is the genuinely
// correct choice here: probing from a nondeterministic scroll offset would also
// make sticky/lazy geometry depend on how far Playwright happened to scroll, and
// the harness's whole value is exact numbers that a later run can be diffed
// against.
//
// SINGLE SOURCE OF TRUTH. The pure geometry predicates below are exported as
// real JS functions AND installed into the page by SOURCE (Function.toString()
// via addInitScript({ content })). There is exactly one definition of each: the
// unit tests (tests/unit/responsive-harness.test.js) exercise the same code the
// browser runs, rather than a copy that can drift out of sync with it.

// Subpixel tolerance shared by every geometry comparison.
export const TOL = 1;

// swAssertUnscrolled: the scroll-origin invariant, as an assertion.
//
// Throws when the page is not scrolled to the origin. Every geometry probe
// calls this before measuring, because a viewport-relative rect and a
// document-absolute extent only share an origin at (0, 0). See THE SCROLL-ORIGIN
// INVARIANT above. Failing loudly is the point: the alternative is a report full
// of fabricated off-screen offenders that look exactly like real ones.
export function swAssertUnscrolled(m) {
  if (Math.abs(m.scrollX) > 0.5 || Math.abs(m.scrollY) > 0.5) {
    throw new Error(
      `responsive harness: page is scrolled to (${m.scrollX}, ${m.scrollY}), not the `
      + '(0, 0) origin every probe measures from. A viewport-relative rect and a '
      + 'document-absolute extent share an origin ONLY at (0, 0); probing here would '
      + 'report elements scrolled PAST as "unreachable". Call resetScrollToOrigin() '
      + '(lib/page-guards.js) after any interaction that may scroll the page.'
    );
  }
}

// swClassifyOffscreen: is this rect genuinely UNREACHABLE?
//
// Pure, and deliberately so -- it is the single hard call the off-screen probe
// makes, and the one that got it wrong before. Below-the-fold is NOT unreachable;
// you scroll to it. Only these are:
//
//   left           it extends left of the origin -- nothing scrolls you there
//   right          it sticks out past the layout viewport's right edge --
//                  reachable only via the horizontal overflow the layout probe
//                  already reports as a bug
//   above          it ends above the origin
//   belowDocument  it starts past the end of the scrollable document
//
// PRECONDITION: the page is at the scroll origin (swAssertUnscrolled). `rect` is
// viewport-relative; `m.scrollHeight` is document-absolute; the two are only
// comparable at scrollY = 0. This is the predicate the invariant exists to
// protect: at scrollY > 0, `above` fires for everything scrolled past.
export function swClassifyOffscreen(rect, m, tol) {
  return {
    left: rect.left < -tol,
    right: rect.right > m.viewportWidth + tol,
    above: rect.bottom < -tol,
    belowDocument: rect.top > m.scrollHeight + tol,
  };
}

// swIsOffscreen: does any unreachable condition hold?
export function swIsOffscreen(offscreen) {
  return offscreen.left || offscreen.right || offscreen.above || offscreen.belowDocument;
}

// The page-side bundle. Installed by SOURCE so the functions above are the ONLY
// definitions (see SINGLE SOURCE OF TRUTH). Anything referenced in here must be
// self-contained: this runs in the browser, where module scope does not exist.
function pageScript() {
  // NOTE: no local tolerance constant. The subpixel threshold has exactly ONE
  // source -- TOL, exported above and injected as window.__swTol by
  // installProbeHelpers -- and the helpers below read it at CALL time (both init
  // scripts have long since run by then). A `const TOL = 1` here would be a
  // second source that silently diverges the moment the exported one is retuned.

  // The single source of truth for "the box the user can actually see", plus
  // the scroll offset the origin invariant is asserted against.
  window.__swMetrics = function swMetrics() {
    const d = document.documentElement;
    return {
      viewportWidth: d.clientWidth,
      viewportHeight: d.clientHeight,
      scrollWidth: d.scrollWidth,
      scrollHeight: d.scrollHeight,
      scrollX: window.scrollX,
      scrollY: window.scrollY,
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
  // off-screen probe: 45 of its 46 findings on settings were `clipped.bottom`
  // on a page that scrolls.
  window.__swOutsideViewportX = function swOutsideViewportX(rect) {
    const TOL = window.__swTol;
    const { viewportWidth } = window.__swMetrics();
    return rect.right <= TOL || rect.left >= viewportWidth - TOL;
  };

  // Root-cause reduction. An element whose overflowing ancestor already
  // extends at least as far right is not an independent finding -- it is
  // being carried out of the viewport by that ancestor. Reporting 1252
  // offenders when a handful of containers explain all of them is not
  // actionable evidence, it is a haystack. `entries` is [{ el, right, ... }].
  window.__swRootCauses = function swRootCauses(entries) {
    const TOL = window.__swTol;
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
}

export async function installProbeHelpers(page) {
  await page.addInitScript(pageScript);
  // The pure predicates, installed from their ONE definition (see SINGLE SOURCE
  // OF TRUTH above) rather than re-typed into the page script.
  await page.addInitScript({
    content: [
      `window.__swTol = ${TOL};`,
      `window.__swAssertUnscrolled = function () { return (${swAssertUnscrolled.toString()})(window.__swMetrics()); };`,
      `window.__swClassifyOffscreen = ${swClassifyOffscreen.toString()};`,
      `window.__swIsOffscreen = ${swIsOffscreen.toString()};`,
    ].join('\n'),
  });
}
