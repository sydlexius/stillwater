// blast-radius.spec.js - Playwright a11y coverage for the blast-radius report
// pane (#2750), route /reports/blast-radius.
//
// Runs on BOTH projects declared in playwright.config.js:
//   - firefox-a11y  (authoritative: Firefox is the target browser)
//   - chromium-a11y (compatibility)
// Every assertion here is engine-observable on purpose. Focus rings, dialog
// focus behaviour and computed contrast differ between Gecko and Blink, which
// is exactly the defect class a single-engine tier cannot see.
//
// Surfaces covered:
//   1. Full-page axe-core scan, dark AND light theme.
//   2. The destructive restore confirm dialog (window.showConfirmDialog) --
//      the highest-risk keyboard surface on this pane.
//   3. Keyboard reachability + VISIBLE focus indicator for the pager.
//
// SCOPED TO WHAT THIS INCREMENT SHIPS. The pane's filter controls arrive with
// the filters increment, so their coverage lives on that side. Nothing here is
// a trimmed-down placeholder: each test below covers a surface this branch
// actually renders.
//
// The ContextHelp "?" affordance on the pane title IS rendered on this branch
// (added after the maintainer's UAT flagged that a destructive-recovery pane
// had no in-app route to its documentation). The full-page axe scans below
// therefore cover it, since they are not scoped to a subtree. Its structural
// contract -- present, accessible name, real <button>, handlers defined on the
// page, correct docs anchor -- is asserted server-side by
// TestBlastRadiusPane_CarriesContextHelpToTheDocs in internal/api. What is
// NOT yet covered anywhere is a browser test that the popover genuinely
// TOGGLES on click and dismisses on Escape; that belongs here and is
// outstanding.
//
// Auth: the single login from global-setup.js, loaded into every context via
// `use.storageState` in playwright.config.js. No credential ever appears in
// this file, on a command line, or in a URL.
//
// DESTRUCTIVE-FLOW POLICY: this spec never CONFIRMS a restore. The dialog is
// always dismissed with Escape or Cancel. The only network call the restore
// flow makes before confirmation is the commit:false preview, which the
// handler treats as a dry run and which writes nothing. Nothing here mutates
// the UAT database.

import { test, expect } from 'playwright/test';

import { disableTransitions } from './helpers/settle.js';
import { buildAxeBuilder, formatViolations, applyTheme, restorePersistedTheme } from './helpers/axe.js';

// The pane route. The paged variant asks for the SMALLEST page the server will
// honor, so the pager renders on the smallest possible database.
//
// page_size is clamped server-side to [PageSizeMin, PageSizeMax] = [10, 500]
// (getUserPageSize), so a smaller number here is silently raised to 10 rather
// than honored -- asking for 1 and asking for 10 produce the identical request.
// The value is written as 10 so it states what actually happens; an earlier
// page_size=1 read as though it forced single-row pages, which it never did.
//
// This means the pager renders only with MORE THAN 10 damaged rows in the
// database. Below that there is no second page and no pager. The test does not
// pass vacuously in that case -- it asserts every target EXISTS before walking
// it, so an absent pager fails loudly as a missing control rather than quietly
// as "nothing to check".
const PANE_URL = '/reports/blast-radius';
const PANE_URL_PAGED = '/reports/blast-radius?page_size=10';

// Disable CSS transitions so a synchronous getComputedStyle (axe's
// color-contrast rule, and our own focus-ring reads) never samples a
// mid-transition blended value and reports a false result.
test.beforeEach(async ({ page }) => {
  await disableTransitions(page);
});

// Restore the SERVER-SIDE theme after every test in this file. Tests that
// exercise the real toggle path persist their change, and spec files run in
// alphabetical order, so an unrestored light theme becomes the starting state
// for every later file and breaks scans that never touched the theme.
test.afterEach(async ({ page }) => {
  await restorePersistedTheme(page);
});

// gotoPane navigates and waits for the pane's own markup. 'networkidle' is
// never used: the SSE event stream keeps the connection open forever, so it
// would always time out.
//
// It waits on the RESULTS TABLE, which is the pane's core and is present
// wherever the pane is. It deliberately does NOT wait on the filter form: that
// is a later increment's markup, and gating the shared helper on it made every
// test in this file fail on a branch that ships the pane without the filters --
// one missing selector reported as ten unrelated failures. A shared helper must
// depend only on what every caller can rely on.
async function gotoPane(page, url = PANE_URL) {
  await page.goto(url);
  await page.waitForLoadState('load');
  await page.waitForSelector('#blast-radius-tbl', { timeout: 10_000 });
}


// ---------------------------------------------------------------------------
// 1. axe-core, full page, both themes.
//
// Full page, NOT scoped to the pane: scoping axe to a subtree hides violations
// in the surrounding chrome that the operator still has to navigate through
// (the same rule the cheat-sheet spec follows).
// ---------------------------------------------------------------------------

test('blast-radius pane passes full-page a11y scan (dark theme)', async ({ page }) => {
  // Dark is the app default. emulateMedia satisfies the 'system' preference
  // branch in preferences.js.
  //
  // The theme is then applied through the app's OWN path rather than by adding
  // the .dark class, for the reason the light-theme test below already
  // documents: forcing the class skips the --sw-glass-bg recompute and can
  // scan a half-applied theme. That is not hypothetical -- the identical
  // pattern in contrast.spec.js produced false contrast violations at ratios
  // as bad as 1.07:1 against a surface no user ever sees (#2872).
  //
  // This test passed with the forcing form only because dark is the default,
  // so the token already held the dark value. It was one default-flip away
  // from the same false failure.
  await page.emulateMedia({ colorScheme: 'dark' });
  await gotoPane(page);
  await applyTheme(expect, page, 'dark');

  const results = await buildAxeBuilder(page).analyze();
  expect(
    results.violations,
    `Blast-radius dark-theme a11y violations:\n${formatViolations(results.violations)}`,
  ).toHaveLength(0);
});

test('blast-radius pane passes full-page a11y scan (light theme)', async ({ page }) => {
  await gotoPane(page);


  // Switch to light through the REAL sidebar toggle so the whole preference
  // path runs (swPreferences.set -> applySingle -> classList + token
  // recompute). Forcing classList alone would skip the token recompute and
  // could scan a half-applied theme.
  await page.waitForFunction(
    () => !!(window.swPreferences && window.swSidebar
      && typeof window.swSidebar.cycleTheme === 'function'),
    { timeout: 10_000 },
  );
  // Seed to 'dark' first so a single cycleTheme() call deterministically lands
  // on 'light' (dark -> light is step 1 of the cycle).
  //
  // Then hold it there. preferences.js hydrates from a server fetch during page
  // init, and that response can land AFTER this switch and flip the page back
  // to dark -- measured happening on Firefox, where it turned the light scan
  // into a silent dark measurement. Re-applying until the state STICKS across a
  // short quiet period outlasts the late hydration without depending on a fixed
  // sleep. The assertions after the scan are the backstop if it still loses.
  // Re-apply light until it holds, rather than asserting once.
  // applySingle, NOT set: `set` PERSISTS the preference to the server, which
  // leaks light mode into every test that runs after this file. Playwright
  // orders spec files alphabetically, so blast-radius runs FIRST and the leak
  // reached cheat-sheet.spec.js and contrast.spec.js -- both of which scan in
  // dark mode and started failing on light-mode contrast (an amber "warning"
  // badge at 4.01:1). That reproduced only in CI, where the database contains
  // nothing but this fixture; a developer machine with a real library renders
  // different dashboard content and hides it.
  //
  // applySingle applies to the DOM without writing to the server, so the theme
  // change dies with this page.
  const holdLight = (message) => expect.poll(
    async () => page.evaluate(() => {
      if (document.documentElement.classList.contains('dark')) {
        window.swPreferences.applySingle('theme', 'light');
        return false;
      }
      return true;
    }),
    { message, timeout: 15_000, intervals: [200] },
  ).toBe(true);

  await holdLight('page never settled on the light theme');
  // Quiet period, then confirm it STAYED light: a late hydration response has
  // had its chance to undo the switch by now.
  await page.waitForTimeout(750);
  await holdLight('theme kept reverting to dark');

  // Reaching light once is not enough to TRUST the scan, so the state is
  // re-asserted either side of it. If the theme lost the race, this test must
  // fail rather than report a dark measurement under a light label -- a
  // vacuous pass would hide exactly the light-mode contrast defects it exists
  // to find.
  //
  // The surface is checked by its RENDERED luminance, not by diffing against a
  // pre-switch reading: the theme preference is stored server-side and shared
  // by every test in the run, so the page may already be light on arrival and a
  // before/after diff would then compare light against light and prove nothing.
  // Absolute luminance is unambiguous either way.
  const themeState = () => page.evaluate(() => {
    const probe = document.querySelector('tbody') || document.body;
    const bg = getComputedStyle(probe).backgroundColor;
    // Rasterize through a canvas so oklch()/color() values reduce to sRGB.
    const cv = document.createElement('canvas');
    cv.width = cv.height = 1;
    const c = cv.getContext('2d');
    c.fillStyle = '#ffffff';
    c.fillRect(0, 0, 1, 1);
    c.fillStyle = bg;
    c.fillRect(0, 0, 1, 1);
    const [r, g, b] = Array.from(c.getImageData(0, 0, 1, 1).data);
    const f = v => { v /= 255; return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4); };
    return {
      dark: document.documentElement.classList.contains('dark'),
      dataTheme: document.documentElement.getAttribute('data-theme'),
      bg,
      luminance: 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b),
    };
  });

  const before = await themeState();
  expect(before.dark, `theme reverted to dark before the light-theme scan: ${JSON.stringify(before)}`).toBe(false);

  const results = await buildAxeBuilder(page).analyze();

  // Re-check AFTER the scan: the preference fetch can land mid-scan and flip
  // the page back, which would make this a dark measurement wearing a light
  // label.
  const after = await themeState();
  expect(after.dark, `theme reverted to dark DURING the light-theme scan, so the result is not a light-mode measurement: ${JSON.stringify(after)}`).toBe(false);
  expect(
    after.luminance,
    `light-theme scan measured a DARK surface (bg=${after.bg}); the theme switch did not reach the rendered page, so a green result here would be meaningless`,
  ).toBeGreaterThan(0.5);

  expect(
    results.violations,
    `Blast-radius light-theme a11y violations:\n${formatViolations(results.violations)}`,
  ).toHaveLength(0);
});

// ---------------------------------------------------------------------------
// 2. The destructive restore confirm dialog.
//
// Why this test is the important one on this pane: restore WRITES a metadata
// value back over the current one. A keyboard user who cannot see the dialog
// (focus never moved into it), cannot escape it, or is stranded with no focus
// after it closes, is one blind Enter press away from an unintended write.
// Each assertion below therefore states the DANGEROUS inverse it rules out.
// ---------------------------------------------------------------------------

test('restore confirm dialog is keyboard-safe (focus in, trapped, Escape out, focus restored)', async ({ page }) => {
  await gotoPane(page);

  const rowIds = await page.evaluate(() => Array.from(
    document.querySelectorAll('tr[id^="blast-row-"]'),
    tr => tr.id.replace(/^blast-row-/, ''),
  ));
  if (rowIds.length === 0) {
    // An empty result set is a DATA condition, not a pass. Fail loudly rather
    // than skipping quietly: a green run with the dialog never opened would
    // misreport this surface as verified.
    throw new Error(
      'no blast-radius rows on this server, so the restore confirm dialog could not be reached. '
      + 'This surface is UNVERIFIED -- seed at least one tracked automated field change before trusting a green run.',
    );
  }

  // Pick a row the server considers ELIGIBLE. Not every row is: a change the
  // handler marks "not_revertible" produces eligible=0, and blastRestoreRow
  // then shows a toast and never opens the dialog at all -- correct product
  // behaviour, but it would make this test fail for a reason that has nothing
  // to do with accessibility. Choosing blind (first row / last row) makes the
  // test's meaning depend on row ordering.
  //
  // The probe is the SAME commit:false preview the pane itself issues before
  // showing the dialog. dry_run writes nothing, so this does not mutate the
  // database.
  const eligibleId = await page.evaluate(async (ids) => {
    const csrf = (document.cookie.match(/(?:^|;\s*)csrf_token=([^;]*)/) || [])[1] || '';
    const baseEl = document.querySelector('meta[name="htmx-base-path"]');
    const base = baseEl ? baseEl.content : '';
    const resp = await fetch(base + '/api/v1/reports/blast-radius/restore', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf },
      body: JSON.stringify({ change_ids: ids, commit: false }),
    });
    if (!resp.ok) return null;
    const plan = await resp.json();
    const ok = (plan.items || []).find(i => i.restore_status === 'planned');
    return ok ? ok.change_id : null;
  }, rowIds);

  if (!eligibleId) {
    throw new Error(
      `none of the ${rowIds.length} blast-radius rows on this server are restore-eligible, so the `
      + 'confirm dialog cannot be opened. This surface is UNVERIFIED.',
    );
  }

  const restoreBtn = page.locator(`#blast-row-${eligibleId} td:last-child button`);
  await expect(restoreBtn).toHaveCount(1);

  // Open via the KEYBOARD, not a mouse click: the dialog records
  // document.activeElement as the element to restore focus to, so a
  // mouse-driven open would not exercise the path a keyboard user takes.
  await restoreBtn.focus();
  await expect(restoreBtn).toBeFocused();
  await page.keyboard.press('Enter');

  const modal = page.locator('#confirm-modal');
  // The dialog only appears after the commit:false preview round-trip
  // resolves, so allow for a server hop.
  await page.waitForSelector('#confirm-modal:not(.hidden)', { timeout: 15_000 });

  // (a) ROLE. Without a dialog role, assistive tech announces the content as
  // ordinary page text and the destructive framing is lost.
  await expect(modal).toHaveAttribute('role', /^(dialog|alertdialog)$/);

  // (b) MODALITY / background inertness. aria-modal="true" is what tells a
  // screen reader that everything outside this subtree is unavailable. The
  // functional half of inertness (keyboard focus cannot leave) is asserted at
  // (d) below; both halves are required, neither substitutes for the other.
  await expect(modal).toHaveAttribute('aria-modal', 'true');

  // (c) ACCESSIBLE NAME. aria-labelledby must point at a real element with
  // real text -- a dangling id or an empty heading leaves the dialog unnamed.
  const accessibleName = await page.evaluate(() => {
    const el = document.getElementById('confirm-modal');
    const id = el.getAttribute('aria-labelledby');
    const target = id ? document.getElementById(id) : null;
    return target ? target.textContent.trim() : '';
  });
  expect(accessibleName, 'confirm dialog has no accessible name').not.toBe('');

  // (d) FOCUS MOVED IN. A dialog that opens while focus stays behind it is
  // invisible to a keyboard user: they keep tabbing the page underneath and
  // may never learn a confirmation is pending.
  const focusStartsInside = await page.evaluate(
    () => !!document.activeElement.closest('#confirm-modal'),
  );
  expect(focusStartsInside, 'focus did not move into the dialog on open').toBe(true);

  // (e) FOCUS TRAPPED. Enough Tab presses to wrap the dialog's own controls
  // several times over. If even one lands outside, focus has escaped behind a
  // modal that is still blocking the view -- the dangerous case.
  const escapedTo = [];
  for (let i = 0; i < 12; i++) {
    await page.keyboard.press('Tab');
    const where = await page.evaluate(() => {
      const a = document.activeElement;
      if (!a || a.closest('#confirm-modal')) return null;
      return `${a.tagName.toLowerCase()}${a.id ? '#' + a.id : ''}${a.className ? '.' + String(a.className).split(/\s+/)[0] : ''}`;
    });
    if (where) escapedTo.push(`step ${i + 1}: ${where}`);
  }
  expect(
    escapedTo,
    `focus escaped the modal while it was open:\n${escapedTo.join('\n')}`,
  ).toEqual([]);

  // Also check Shift+Tab, which wraps the other way. A trap implemented only
  // for forward Tab leaks on the first backward press from the first control.
  const escapedBack = [];
  for (let i = 0; i < 6; i++) {
    await page.keyboard.press('Shift+Tab');
    const where = await page.evaluate(() => {
      const a = document.activeElement;
      if (!a || a.closest('#confirm-modal')) return null;
      return `${a.tagName.toLowerCase()}${a.id ? '#' + a.id : ''}`;
    });
    if (where) escapedBack.push(`step ${i + 1}: ${where}`);
  }
  expect(
    escapedBack,
    `focus escaped the modal on Shift+Tab:\n${escapedBack.join('\n')}`,
  ).toEqual([]);

  // (f) ESCAPE CANCELS. Escape is the only dismissal a keyboard user can reach
  // without hunting for a control, and on a destructive dialog it must CANCEL,
  // never confirm. This is also how this spec avoids performing a restore.
  await page.keyboard.press('Escape');
  // toBeHidden, not waitForSelector('#confirm-modal.hidden'): waitForSelector
  // defaults to waiting for the VISIBLE state, and the dismissed dialog is
  // display:none, so that form can never be satisfied and times out on a
  // dialog that closed correctly.
  await expect(modal).toBeHidden({ timeout: 5_000 });
  await expect(modal).toHaveClass(/\bhidden\b/);

  // (g) FOCUS RETURNED. Focus stranded on <body> after close drops the
  // keyboard user back at the top of the document, which on this page means
  // re-tabbing the whole sidebar to get back to the row they were on.
  await expect(restoreBtn).toBeFocused();
});

// ---------------------------------------------------------------------------
// 3. Keyboard reachability + VISIBLE focus indicator.
//
// Reachability and visibility are separate failures and are asserted
// separately. A control that is reachable but shows no ring is a WCAG 2.4.7
// failure: the user is somewhere, with no way to tell where.
//
// The ring is read from the COMPUTED style of the element while it holds focus
// from a REAL Tab press. A CSS rule existing in a stylesheet is not evidence
// that its selector matched or that it won the cascade, and :focus-visible in
// particular does not match on a programmatic .focus() call in every engine --
// so the walk below drives the keyboard rather than calling focus().
// ---------------------------------------------------------------------------

// Selectors that must be reachable. Keyed by a human label used in failures.
//
// SCOPE: this is the pager only, because the pager is what this increment of
// the pane ships. The filter controls and the ContextHelp "?" arrive with the
// filters/copy increment and are covered by the fuller reachability test on
// that side; this is deliberately narrower, not an incomplete copy of it.
const REACHABLE_TARGETS = {
  'pager: next link': 'nav a[href*="/reports/blast-radius?"]',
};

// The properties sampled at each state. Kept as one list so the unfocused
// baseline and the focused reading are always directly comparable.
const FOCUS_PROPS = ['outline', 'outlineStyle', 'outlineWidth', 'outlineColor',
  'outlineOffset', 'boxShadow', 'borderColor', 'backgroundColor', 'color'];

// focusIndicatorFor reports HOW a control signals focus, by diffing its
// computed style against its own unfocused baseline.
//
// Two mechanisms count, and the codebase uses both deliberately:
//   - a RING: a drawn outline, or a box-shadow (Tailwind's focus:ring compiles
//     to box-shadow). This is what the pager links use.
//   - a STYLE SWAP: some controls set `outline: none` on :focus-visible ON
//     PURPOSE and signal focus by swapping colour, border colour and
//     background instead (.sw-context-help-btn does exactly this, on the
//     filters/copy side). Rejecting that as "no indicator" would be wrong --
//     WCAG 2.4.7 asks for a visible focus indicator, not for an outline
//     specifically. Both mechanisms are accepted here so this helper stays
//     correct for either side.
//
// What must NOT pass is the genuinely dangerous case: a control whose rendered
// appearance is byte-identical focused and unfocused, leaving a keyboard user
// with no way to tell where they are. That is exactly what an empty diff means.
function focusIndicatorFor(base, focused) {
  const ring = (focused.outlineStyle !== 'none' && parseFloat(focused.outlineWidth || '0') > 0)
    || (focused.boxShadow && focused.boxShadow !== 'none' && focused.boxShadow !== base.boxShadow);
  const changed = FOCUS_PROPS.filter(p => base[p] !== focused[p]);
  return { ring: !!ring, changed, visible: !!ring || changed.length > 0 };
}

test('pager is Tab-reachable with a visible focus ring', async ({ page }) => {
  // The smallest server-honored page size, so the pager renders with as few
  // damaged rows as possible (>10, per the clamp noted at PANE_URL_PAGED).
  await gotoPane(page, PANE_URL_PAGED);

  // Confirm each target EXISTS before walking. A missing element would
  // otherwise read as "not reachable", conflating a template gap with a
  // keyboard defect.
  const missing = [];
  for (const [label, sel] of Object.entries(REACHABLE_TARGETS)) {
    if (await page.locator(sel).count() === 0) missing.push(`${label} (${sel})`);
  }
  expect(missing, `expected pane controls are absent from the DOM:\n${missing.join('\n')}`).toEqual([]);

  // Capture the UNFOCUSED baseline for every target first. The focused reading
  // is only meaningful as a diff against this.
  const baseline = await page.evaluate(([targets, props]) => {
    const out = {};
    for (const [label, sel] of Object.entries(targets)) {
      const cs = getComputedStyle(document.querySelector(sel));
      out[label] = Object.fromEntries(props.map(p => [p, cs[p]]));
    }
    return out;
  }, [REACHABLE_TARGETS, FOCUS_PROPS]);

  // Start the walk from the very top of the document so the tab order is the
  // real one a keyboard user gets on page load.
  await page.evaluate(() => {
    document.activeElement && document.activeElement.blur();
    document.body.focus();
  });

  // Collect focus hits with a SINGLE in-page listener rather than one
  // page.evaluate per Tab press. The earlier form paid a round-trip for every
  // press, so a missing control -- the failure case that matters -- cost 250
  // presses AND 250 round trips before reporting. The listener records every
  // focus change as it happens; the walk then only presses Tab and asks for the
  // results once.
  //
  // MAX_TABS stays generous because the page chrome (sidebar, header, rail)
  // sits ahead of the pane and its depth varies with the data rendered. It is a
  // termination bound, not a measurement: the assertion below reports which
  // targets were never reached, so an under-tight bound would read as a
  // keyboard defect rather than as a truncated walk.
  const MAX_TABS = 250;

  await page.evaluate(([targets, props]) => {
    window.__swFocusHits = {};
    window.__swFocusListener = () => {
      const a = document.activeElement;
      if (!a || a === document.body) return;
      for (const [label, sel] of Object.entries(targets)) {
        if (a.matches(sel) && !window.__swFocusHits[label]) {
          const cs = getComputedStyle(a);
          window.__swFocusHits[label] = {
            label,
            focusVisible: a.matches(':focus-visible'),
            ...Object.fromEntries(props.map(p => [p, cs[p]])),
          };
        }
      }
    };
    document.addEventListener('focusin', window.__swFocusListener, true);
  }, [REACHABLE_TARGETS, FOCUS_PROPS]);

  const targetCount = Object.keys(REACHABLE_TARGETS).length;
  for (let i = 0; i < MAX_TABS; i++) {
    await page.keyboard.press('Tab');
    // Poll for completion occasionally rather than every press: this is the
    // only remaining round trip in the loop, and the walk terminates as soon as
    // every target has been seen.
    if (i % 10 === 9) {
      const n = await page.evaluate(() => Object.keys(window.__swFocusHits).length);
      if (n === targetCount) break;
    }
  }

  const found = await page.evaluate(() => {
    document.removeEventListener('focusin', window.__swFocusListener, true);
    return window.__swFocusHits;
  });

  // Reachability. Asserted separately from visibility: "cannot get there" and
  // "cannot tell you are there" are different defects with different fixes.
  const unreachable = Object.keys(REACHABLE_TARGETS).filter(l => !found[l]);
  expect(
    unreachable,
    `controls never received focus within ${MAX_TABS} Tab presses (not keyboard reachable):\n  ${unreachable.join('\n  ')}`,
  ).toEqual([]);

  // Visible indicator, from the computed diff rather than from any assumption
  // about which CSS property carries it.
  const indicators = Object.fromEntries(
    Object.entries(found).map(([label, s]) => [label, focusIndicatorFor(baseline[label], s)]),
  );
  const invisible = Object.entries(indicators)
    .filter(([, ind]) => !ind.visible)
    .map(([label]) => `${label}: computed style is identical focused and unfocused `
      + `(outline="${found[label].outline}" box-shadow="${found[label].boxShadow}" `
      + `:focus-visible=${found[label].focusVisible})`);
  expect(
    invisible,
    `focused controls render no visible focus indicator (WCAG 2.4.7):\n  ${invisible.join('\n  ')}`,
  ).toEqual([]);

  // Attach the measured values so the numbers are on the record rather than
  // only implied by a green check.
  test.info().annotations.push({
    type: 'focus-indicators',
    description: JSON.stringify({ baseline, focused: found, verdict: indicators }, null, 2),
  });
});

// ---------------------------------------------------------------------------
// 4. Focus survives the bulk bar being hidden.
//
// The bulk action bar is hidden with the .hidden utility, which is
// display:none. A browser blurs a focused element that becomes display:none,
// and focus falls all the way back to <body> -- a keyboard user is silently
// dumped at the top of the document.
//
// Two paths hide the bar with focus inside it: the bar's own Cancel button
// (exercised here) and blastClearSelection() after a committed restore. The
// commit path is NOT exercised, because this spec never confirms a restore
// (see the destructive-flow policy at the top of this file); it runs the same
// blastUpdateBulkBar n === 0 branch, so the guard under test is identical.
//
// THIS TEST CANNOT BE WRITTEN AT ANY LOWER TIER. Measured during the review
// that found the defect:
//   - jsdom reported focus STAYING on the hidden button. jsdom does not blur
//     on hide, so the defect is invisible there and a green jsdom test would
//     be an active lie.
//   - A real browser WITHOUT the built stylesheet also reported the wrong
//     answer, because .hidden carries no display rule until styles.css loads,
//     so the element never actually became display:none.
// Only a real engine plus the real served stylesheet reproduces it, which is
// exactly what this tier provides. The precondition below asserts the
// stylesheet is genuinely in force rather than assuming it.
// ---------------------------------------------------------------------------

test('hiding the bulk action bar does not drop focus to the document body', async ({ page }) => {
  await gotoPane(page);

  const checkboxes = page.locator('.blast-select');
  const n = await checkboxes.count();
  if (n === 0) {
    // A DATA condition, not a pass. Same reasoning as the dialog test above.
    throw new Error(
      'no blast-radius rows on this server, so the bulk selection bar could not be revealed. '
      + 'This surface is UNVERIFIED -- seed at least one tracked automated field change before trusting a green run.',
    );
  }

  const bar = page.locator('#blast-bulk-bar');
  await expect(bar).toHaveCount(1);

  // PRECONDITION 1: the real stylesheet is in force, i.e. .hidden actually
  // computes to display:none. Without this the whole test is vacuous -- the
  // element would stay visible and focusable, and the guard would never be
  // needed. This is the exact check that a stylesheet-less browser run failed.
  const hiddenIsDisplayNone = await page.evaluate(() => {
    const el = document.getElementById('blast-bulk-bar');
    return el ? getComputedStyle(el).display : null;
  });
  expect(
    hiddenIsDisplayNone,
    'the bulk bar starts hidden but .hidden does not compute to display:none, so the built stylesheet is not in force '
    + 'and this test would pass vacuously (a browser run without styles.css gives exactly this wrong answer)',
  ).toBe('none');

  // Select a row via the KEYBOARD so the whole flow is the one a keyboard user
  // takes. Reveal the bar.
  const firstBox = checkboxes.first();
  await firstBox.focus();
  await page.keyboard.press('Space');
  await page.waitForSelector('#blast-bulk-bar:not(.hidden)', { timeout: 10_000 });

  // PRECONDITION 2: the bar is genuinely visible now, so hiding it is a real
  // state change.
  await expect(bar).toBeVisible();

  // Move focus INTO the bar, onto the Cancel button, and confirm it is there.
  // This reproduces the state the shared modal's hideModal leaves behind on the
  // commit path, where focus is restored to the opener -- also inside the bar.
  // Located by its HANDLER, not its label: the label is translated, so a
  // text match would make this a11y test fail under a non-English locale for
  // a reason that has nothing to do with focus.
  const cancelBtn = bar.locator('button[onclick="blastClearSelection()"]');
  await expect(cancelBtn).toHaveCount(1);
  await cancelBtn.focus();
  await expect(cancelBtn).toBeFocused();

  // PRECONDITION 3: focus really is inside the bar, which is what makes the
  // hide destructive to focus. Asserting it rules out a green result produced
  // by focus having been somewhere harmless all along.
  const focusWasInBar = await page.evaluate(() => {
    const el = document.getElementById('blast-bulk-bar');
    return !!(el && el.contains(document.activeElement));
  });
  expect(focusWasInBar, 'focus was not inside the bulk bar before hiding it, so this test would not exercise the defect').toBe(true);

  // The action under test: Cancel clears the selection, which hides the bar.
  await page.keyboard.press('Enter');
  // state: 'hidden', not the default 'visible'. waitForSelector waits for the
  // element to be VISIBLE unless told otherwise, so '#blast-bulk-bar.hidden'
  // under the default state waits for an element to be simultaneously hidden
  // and visible -- it can only ever time out, even when the bar hides exactly
  // as intended.
  await page.waitForSelector('#blast-bulk-bar.hidden', { state: 'hidden', timeout: 10_000 });

  const after = await page.evaluate(() => {
    const el = document.getElementById('blast-bulk-bar');
    return {
      activeTag: document.activeElement ? document.activeElement.tagName : null,
      activeId: document.activeElement ? document.activeElement.id : null,
      activeIsBody: document.activeElement === document.body,
      barDisplay: el ? getComputedStyle(el).display : null,
    };
  });

  // The bar really did hide. If it did not, the focus reading below says
  // nothing about the defect.
  expect(after.barDisplay, 'the bulk bar did not become display:none, so focus was never at risk in this run').toBe('none');

  // THE ASSERTION. Before the fix this measured
  // {activeIsBody: true, activeTag: "BODY"}.
  expect(
    after.activeIsBody,
    `focus fell to <body> when the bulk action bar was hidden (measured: ${JSON.stringify(after)}). `
    + 'A keyboard user is stranded at the top of the document immediately after a destructive operation. '
    + 'blastUpdateBulkBar must move focus out of the bar BEFORE adding .hidden.',
  ).toBe(false);

  // And it landed somewhere USEFUL, not merely somewhere. #blast-select-all is
  // the control that owns the selection the bar was reporting on.
  expect(
    after.activeId,
    `focus survived the hide but did not land on the select-all control (measured: ${JSON.stringify(after)})`,
  ).toBe('blast-select-all');

  test.info().annotations.push({
    type: 'focus-after-bulk-bar-hide',
    description: JSON.stringify(after, null, 2),
  });
});
