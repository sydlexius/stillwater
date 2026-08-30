// filter-flyout-open-scan.spec.js - open-flyout axe coverage for the four
// remaining filter panes (#3095).
//
// WHY THIS FILE EXISTS
//
// #3093 (filter-flyout-contrast.spec.js) proved that a CLOSED filter flyout is
// visibility:hidden by design, so axe never measures its contents, and that a
// full-page scan of every OTHER pane in this tier consequently ran green over
// a real, shared AA contrast defect in .sw-filter-item for as long as the
// component has existed. That spec fixed the gap on exactly one surface
// (compliance). blast-radius.spec.js independently covers a second (its own
// "the open filter flyout passes a full-page a11y scan" tests).
//
// .sw-filter-item / FilterFlyout is shared by SIX panes: artists, dashboard,
// activity, logs, compliance, blast-radius. Only the last two have an
// open-flyout scan. A regression in the shared component -- or a pane-specific
// override that reintroduces the same class of defect -- is caught on exactly
// two of six surfaces without this file. This closes the remaining four:
// artists, dashboard, activity, logs.
//
// Runs on BOTH engines (firefox-a11y authoritative, chromium-a11y for
// compatibility) and BOTH themes, same as filter-flyout-contrast.spec.js:
// contrast is a rendered-color property, so each theme is a separate
// measurement rather than an inference from the other.
//
// RUNTIME COST (measured, single worker, this machine): 16 tests (4 panes x 2
// themes x 2 engines) in ~30s total, ~1.9s/test average -- each test pays one
// navigation, one theme apply, one flyout open and one scoped axe pass. That
// is roughly the same per-test cost as filter-flyout-contrast.spec.js's
// existing compliance scan, so adding four more panes does not change the
// tier's order of magnitude and does not need splitting.
//
// NO SEEDED FIXTURE NEEDED for any of the four panes. Every chip these flyouts
// render is a STATIC filter axis (severity, category, field, level,
// component, ...) whose label and neutral/selected state are computed from the
// request's own query string and, for logs, the fixed logLevels ladder plus
// whatever the (possibly empty) log ring buffer's distinct components are --
// none of them require an artist row, a library, or a metadata change to
// exist. Verified against CURRENT code:
//   - artists:   ArtistFilterFlyout (web/templates/artists.templ) renders one
//                FilterItem per fixed metadata/image/platform/status/type
//                facet, unconditionally. The library section is the only
//                conditional part (len(data.Libraries) > 1) and this spec does
//                not depend on it.
//   - dashboard: DashboardFilterFlyout (web/templates/index.templ) renders
//                severity + category + fixable unconditionally; only the
//                library and rule sections are conditional on data present,
//                and this spec does not depend on them either.
//   - activity:  ActivityBody (web/templates/activity.templ) renders one
//                FilterItem per fixed field token, unconditionally, always in
//                the "neutral" state (the activity flyout has no server-side
//                selected state to restore).
//   - logs:      LogsPage (web/templates/logs.templ) renders one
//                FilterItemSingle per entry in the fixed logLevels ladder
//                (trace..error), unconditionally; the Component section is the
//                only conditional part (len(logComponents) > 0) and this spec
//                does not depend on it.
// So this is the "the pattern already exists and is proven, so the work is
// applying it, not designing it" case the issue describes: no fixture, just
// the same open-and-scan idiom against four more panes.
//
// Theme switching goes through the app's own preference path (applyTheme, see
// helpers/axe.js), never a manual .dark class toggle -- see that helper's
// comment for why a class-only toggle produces half-themed false readings.
//
// SCOPED TO THE OPEN PANEL, unlike filter-flyout-contrast.spec.js's full-page
// scan. The issue's own AC says the spec "runs axe against the opened panel",
// and two of the four panes make the difference load-bearing, measured
// against a real running server:
//   - dashboard: the action-queue cards it renders carry a PRE-EXISTING,
//     already-tracked color-contrast defect in the severity "warning" badge
//     (severityBadgeClass, web/templates/dashboard.templ; #2964, 4.01:1 in
//     light mode) that has nothing to do with the filter flyout.
//   - activity: #activity-showing-counter (web/templates/activity.templ)
//     renders at 3.66:1 in dark mode, another pre-existing defect outside the
//     flyout, untracked before this file found it. Hostile review (fix round
//     for #3095) separately found a THIRD pre-existing, unrelated defect on
//     this same pane with an unscoped full-page scan: the .text-green-600
//     summary text on #activity-change-* <summary> elements measures 3.19:1.
// None of the three is part of what #3095 asks this file to cover, and fixing
// any of them (or adding a KNOWN_VIOLATIONS allowance for a defect this spec
// does not exist to police) is out of scope for a spec whose job is proving
// the SHARED .sw-filter-item component, not auditing the rest of each page.
// Scoping with `.include()` still evaluates the panel's real computed styles
// (contrast is not affected by which elements axe is told to visit), so the
// ink under test -- the same shared chip class filter-flyout-contrast.spec.js
// exists for -- is scanned exactly as thoroughly as the full-page form would
// scan it; only the three pre-existing, unrelated defects above are out of
// view.

import { test, expect } from 'playwright/test';

import { disableTransitions } from './helpers/settle.js';
import { buildAxeBuilder, formatViolations, applyTheme, restorePersistedTheme } from './helpers/axe.js';

// One entry per remaining pane. triggerID/flyoutID/panelSel follow the same
// naming FilterFlyout itself requires (aria-controls on the trigger names the
// flyout id, which is also the flyout's DOM id).
const PANES = [
  { name: 'artists', url: '/artists', triggerID: 'artist-filter-trigger', flyoutID: 'artist-filters-flyout' },
  { name: 'dashboard', url: '/', triggerID: 'dashboard-filters-trigger', flyoutID: 'dashboard-filter-flyout' },
  { name: 'activity', url: '/activity', triggerID: 'activity-filter-trigger', flyoutID: 'activity-filters-flyout' },
  { name: 'logs', url: '/logs', triggerID: 'sw-logs-filter-trigger', flyoutID: 'sw-logs-filter-flyout' },
];

test.beforeEach(async ({ page }) => {
  await disableTransitions(page);
});

// Spec files run in alphabetical order; an unrestored light theme would
// become the starting state for every later file (same reasoning as
// filter-flyout-contrast.spec.js).
test.afterEach(async ({ page }) => {
  await restorePersistedTheme(page);
});

for (const pane of PANES) {
  for (const theme of ['dark', 'light']) {
    test(`the open ${pane.name} filter flyout passes an a11y scan (${theme} theme)`, async ({ page }) => {
      if (theme === 'dark') {
        // Satisfies the 'system' preference branch in preferences.js.
        await page.emulateMedia({ colorScheme: 'dark' });
      }
      await page.goto(pane.url);
      await page.waitForLoadState('load');
      await page.waitForSelector(`#${pane.triggerID}`, { timeout: 10_000 });

      // The theme is applied through the app's OWN path, never by forcing a
      // .dark class -- see helpers/axe.js's applyTheme comment for the false
      // positives (#2872) a class-only toggle produces.
      await applyTheme(expect, page, theme);

      await page.locator(`#${pane.triggerID}`).click();
      await page.waitForSelector(`#${pane.flyoutID}:not([inert])`, { timeout: 5000 });

      // Precondition: the panel is genuinely open AND holds at least one
      // ACTUALLY-RENDERED .sw-filter-item chip, or the scan below measures a
      // hidden or empty subtree and reports a meaningless green (the exact
      // failure mode filter-flyout-contrast.spec.js exists to rule out).
      //
      // "Rendered" is checked per chip, not just counted via querySelectorAll:
      // a chip can be present IN THE DOM while being invisible on screen (a
      // rule collapsing .sw-filter-item to display:none inside an otherwise
      // open panel), and axe does not report violations on elements it
      // considers non-rendered -- so a DOM-presence-only count passes over
      // exactly the case this guard exists to catch, one level deeper than it
      // used to look. Demonstrated by hostile review: adding
      // `.sw-filter-flyout--open .sw-filter-item { display: none !important; }`
      // left the OLD chips-count check (DOM presence) green while axe scanned
      // zero visible nodes. Each chip is now checked for a non-zero rendered
      // rect AND a non-'hidden' computed visibility (visibility is inherited,
      // so this also catches an ancestor between the panel and the chip
      // setting visibility:hidden, which a rect-only check would miss: an
      // invisible ancestor still reserves layout space, so a hidden
      // descendant's own getBoundingClientRect can still be non-zero).
      const panel = await page.evaluate((flyoutID) => {
        const el = document.getElementById(flyoutID);
        if (!el) return { open: false, chips: 0 };
        const cs = getComputedStyle(el);
        const isRendered = (node) => {
          const style = getComputedStyle(node);
          if (style.visibility === 'hidden') return false;
          const r = node.getBoundingClientRect();
          return r.width > 0 && r.height > 0;
        };
        return {
          open: cs.display !== 'none' && cs.visibility !== 'hidden',
          chips: Array.from(el.querySelectorAll('.sw-filter-item')).filter(isRendered).length,
        };
      }, pane.flyoutID);
      expect(panel.open, `the ${pane.name} filter flyout is not visibly open; the scan would measure a `
        + 'hidden subtree and report a green that proves nothing').toBe(true);
      expect(panel.chips, `the open ${pane.name} flyout holds no VISIBLY RENDERED .sw-filter-item chips `
        + '(present-in-DOM does not count -- see the comment above), so the ink under test is not on '
        + 'screen and this scan is vacuous').toBeGreaterThan(0);

      const results = await buildAxeBuilder(page)
        .include(`#${pane.flyoutID}`)
        .analyze();
      expect(
        results.violations,
        `${pane.name} filter flyout ${theme}-theme a11y violations:\n${formatViolations(results.violations)}`,
      ).toHaveLength(0);
    });
  }
}

// -----------------------------------------------------------------------
// Negative control (CodeRabbit finding on #3154; PR review, verified valid).
//
// Every test above proves these flyouts currently have NO violations.
// Nothing above proves the scan CAN report one. That gap matters more here
// than for a typical axe spec because the scan is `.include()`-scoped to
// `#${pane.flyoutID}`: if that selector were wrong, or resolved to an empty
// or non-matching subtree, axe would silently find nothing and every test
// above would pass forever while verifying nothing -- the same class of
// vacuity the hostile review already caught in the chip-rendered
// precondition (see the block above), just one layer further out. A
// negative control makes the SCOPING itself falsifiable, not just the
// chips inside it.
//
// Targets the artists pane only (PANES[0]). One control is enough: it
// verifies the axe-scan WIRING (the .include() selector actually reaches a
// real element and axe actually flags what is inside it), not the panes --
// a copy per pane or per theme would be four-plus tests proving the same
// wiring fact repeatedly, which is dead maintenance weight for the reason
// the file's RUNTIME COST note above exists.
//
// THE INJECTION IS RUNTIME-ONLY. It sets inline styles on a live DOM node
// via page.evaluate() -- it does not touch any committed stylesheet, and it
// is gone the moment this test's page context closes (each Playwright test
// in this file gets its own fresh `page`, so nothing here can bleed into
// the tests before or after it in the same run).
//
// ASSERTS SPECIFICALLY, not just "some violation happened": the flagged
// rule id must be 'color-contrast' AND the violation's node HTML must
// contain the marker attribute this test itself set, so a scan that found
// a DIFFERENT, unrelated defect (there are three known pre-existing ones on
// other panes, see the file header) cannot be mistaken for evidence the
// injected one was caught.
test('the flyout scan actually detects a violation when one is injected (negative control)', async ({ page }) => {
  const pane = PANES[0];
  const marker = 'sw-a11y-negative-control-3095';

  await page.goto(pane.url);
  await page.waitForLoadState('load');
  await page.waitForSelector(`#${pane.triggerID}`, { timeout: 10_000 });

  await page.locator(`#${pane.triggerID}`).click();
  await page.waitForSelector(`#${pane.flyoutID}:not([inert])`, { timeout: 5000 });

  // Poison one RENDERED chip label with a deliberately failing foreground/
  // background pair (~1.6:1, well under the 4.5:1 AA floor), tagged with a
  // marker attribute so the assertion below can name the exact node it
  // poisoned rather than accepting any color-contrast finding. Reuses the
  // same rendered-geometry + own-visibility test as the precondition above
  // (#3095 fix round) so this negative control cannot pick a node that was
  // never actually on screen.
  const injected = await page.evaluate(({ flyoutID, marker: dataMarker }) => {
    const isRendered = (node) => {
      const style = getComputedStyle(node);
      if (style.visibility === 'hidden') return false;
      const r = node.getBoundingClientRect();
      return r.width > 0 && r.height > 0;
    };
    const panel = document.getElementById(flyoutID);
    if (!panel) return { found: false };
    const label = Array.from(panel.querySelectorAll('.sw-filter-item-label')).find(isRendered);
    if (!label) return { found: false };
    label.setAttribute('data-sw-a11y-negative-control', dataMarker);
    label.style.setProperty('color', '#eeeeee', 'important');
    label.style.setProperty('background-color', '#ffffff', 'important');
    return { found: true };
  }, { flyoutID: pane.flyoutID, marker });

  // Precondition: a real, rendered chip label existed to poison. Without
  // this the test could pass vacuously against a panel that never had one.
  expect(injected.found, `no rendered .sw-filter-item-label chip label was found in #${pane.flyoutID} to `
    + 'poison; the negative control cannot prove the scan detects anything without a real element to '
    + 'inject into').toBe(true);

  const results = await buildAxeBuilder(page)
    .include(`#${pane.flyoutID}`)
    .analyze();

  const matched = results.violations.filter((v) => v.id === 'color-contrast'
    && v.nodes.some((n) => typeof n.html === 'string' && n.html.includes(marker)));

  expect(matched.length, `expected a color-contrast violation naming the injected [data-sw-a11y-`
    + `negative-control="${marker}"] node; the scan reported:\n${formatViolations(results.violations)}`)
    .toBeGreaterThan(0);
});
