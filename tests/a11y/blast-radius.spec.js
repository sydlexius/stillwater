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
//   4. Focus survival when the bulk action bar hides.
//   5. The per-row REFUSAL REASON: that the reason the server returned reaches
//      the operator on the row it refused, and that every token renders prose
//      rather than a blank or a raw key.
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
// handler treats as a dry run and which writes nothing. No value is ever put
// BACK by this file.
//
// ONE EXCEPTION, stated rather than buried: the refusal-reason test writes a
// NEW value into one field of one fixture artist, through the ordinary edit
// endpoint, because "something changed this field after the report loaded" is
// the condition it needs and there is no way to produce it without a write.
// That is a forward edit, not a restore, and it targets only the seeded
// fixture artists. The policy above is about never COMMITTING a restore, and
// it is intact.

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

// ---------------------------------------------------------------------------
// 5. The refusal REASON reaches the operator, on the row that was refused.
//
// Why this test exists. The endpoint distinguishes seven refusal reasons, and
// before this every one of them arrived at the operator as the same sentence:
// a generic toast whose parenthetical asserted "already changed since this
// report loaded, or no longer eligible". For a name collision that sentence is
// simply FALSE, and it sends the operator looking for a change nobody made
// instead of at the artist holding the name. A classifier nobody can read is
// not a classifier.
//
// Two halves, because they prove different things:
//   (a) the LIVE chain, end to end -- a row is made stale through the API, the
//       operator presses its own Restore button, and the reason the SERVER
//       returned is what appears under that button. Nothing here is simulated:
//       the request, the classification and the render are the real ones.
//   (b) the RENDER of the tokens a preview cannot produce. A name collision is
//       decidable only inside the writing transaction, so no dry run can
//       return one and reaching it in a browser would mean committing a
//       destructive write -- which this spec never does (see the
//       DESTRUCTIVE-FLOW POLICY at the top). Its classification is covered in
//       Go (handlers_blast_restore_name_collision_test.go); what is covered
//       HERE is that the token renders as prose the operator can act on, in
//       the real cascade, rather than blank or as a raw key.
// ---------------------------------------------------------------------------

test('a refused row shows WHY it was refused, from the live restore chain', async ({ page }) => {
  await gotoPane(page);

  // Find a row the server currently considers restorable, using the pane's own
  // commit:false preview. An already-refused row would make the assertion
  // below pass for the wrong reason.
  const rowIds = await page.evaluate(() => Array.from(
    document.querySelectorAll('tr[id^="blast-row-"]'),
    tr => tr.id.replace(/^blast-row-/, ''),
  ));
  if (rowIds.length === 0) {
    throw new Error(
      'no blast-radius rows on this server, so no row could be refused. This surface is UNVERIFIED.',
    );
  }

  const target = await page.evaluate(async (ids) => {
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
    // The staleness write below is a plain PATCH of a free-form string, so the
    // row must be a field that accepts one. Two kinds are excluded rather than
    // one, and an ALLOW-list would be the wrong shape here: trackableFields is
    // free to grow, and a new entry must not silently become eligible for a
    // write this fixture cannot make.
    //
    //   - genres, styles and moods take an ARRAY, so a string body is the
    //     wrong shape entirely.
    //   - name carries validation rules (ValidateFieldUpdate refuses an empty,
    //     whitespace-only or identity-less value) AND routes through the
    //     collision guard, so a free-form write can be refused for reasons
    //     that have nothing to do with the staleness this fixture is creating.
    //
    // Picking one of those makes the PATCH fail and the test fail LATER, on an
    // absent element, naming the wrong cause.
    const unpatchable = new Set(['genres', 'styles', 'moods', 'name']);
    const ok = (plan.items || []).find(
      i => i.restore_status === 'planned' && !unpatchable.has(i.field),
    );
    return ok ? { id: ok.change_id, artistId: ok.artist_id, field: ok.field } : null;
  }, rowIds);

  if (!target) {
    throw new Error(
      `none of the ${rowIds.length} blast-radius rows are restore-eligible, so none can be made `
      + 'stale and refused. This surface is UNVERIFIED.',
    );
  }

  // PRECONDITION MADE, NOT ASSUMED. Write a NEW value into the field through
  // the ordinary edit path. That is exactly the "something changed this field
  // after the report loaded" condition, produced the way a real operator or a
  // rule pass produces it -- never by hand-inserting a row.
  //
  // This mutates one field of one fixture artist in the ephemeral a11y
  // database. It is not a restore: nothing is written BACK, and the
  // destructive-flow policy (never confirm a restore) is untouched.
  //
  // The value must be UNIQUE PER RUN, not a fixed literal. Service.UpdateField
  // skips the write entirely when the normalized new value equals the current
  // one, so a fixed string is a no-op the SECOND time this test runs against
  // the same server -- and this file runs twice, once per browser project.
  // Measured: with a literal, firefox-a11y passed and chromium-a11y then wrote
  // the value firefox had already written, changed nothing, and got a
  // "planned" row back; the refusal never happened and the test failed on a
  // hidden element rather than on the thing it tests.
  const stamp = `${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
  const patched = await page.evaluate(async (t) => {
    const csrf = (document.cookie.match(/(?:^|;\s*)csrf_token=([^;]*)/) || [])[1] || '';
    const baseEl = document.querySelector('meta[name="htmx-base-path"]');
    const base = baseEl ? baseEl.content : '';
    const resp = await fetch(`${base}/api/v1/artists/${t.artistId}/fields/${t.field}`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf },
      body: JSON.stringify({ value: `Edited after the report loaded, by the a11y fixture ${t.stamp}.` }),
    });
    return { ok: resp.ok, status: resp.status, body: resp.ok ? '' : await resp.text() };
  }, { ...target, stamp });
  expect(
    patched.ok,
    `could not make the row stale (PATCH ${target.field} -> ${patched.status} ${patched.body}); `
    + 'without a stale row the refusal path is never entered and this test proves nothing',
  ).toBe(true);

  // PRECONDITION ASSERTED, NOT ASSUMED. A 200 from the PATCH says the request
  // was accepted, NOT that the row is now stale -- a no-op write returns 200
  // and changes nothing. Ask the server directly, so a fixture that failed to
  // bite fails HERE, naming the reason, instead of surfacing 15 seconds later
  // as an element that never appeared.
  const precondition = await page.evaluate(async (id) => {
    const csrf = (document.cookie.match(/(?:^|;\s*)csrf_token=([^;]*)/) || [])[1] || '';
    const baseEl = document.querySelector('meta[name="htmx-base-path"]');
    const base = baseEl ? baseEl.content : '';
    const resp = await fetch(base + '/api/v1/reports/blast-radius/restore', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf },
      body: JSON.stringify({ change_ids: [id], commit: false }),
    });
    if (!resp.ok) return { status: resp.status };
    const plan = await resp.json();
    const item = (plan.items || [])[0] || {};
    return { status: resp.status, restoreStatus: item.restore_status, reason: item.refuse_reason };
  }, target.id);
  expect(
    precondition,
    'the row is not refused after the staleness write, so the refusal path is never entered',
  ).toMatchObject({ restoreStatus: 'refused', reason: 'no_longer_current' });

  // The operator's own affordance. The page still shows the row as it was, so
  // this is precisely the stale-tab case the refusal exists for.
  const reason = page.locator(`#blast-reason-${target.id}`);
  await expect(reason).toBeHidden();
  await page.locator(`#blast-row-${target.id} td:last-child button`).click();

  // THE ASSERTION. The reason appears, on the refused row, and says what to do
  // about THIS refusal.
  await expect(reason).toBeVisible({ timeout: 15_000 });
  const text = (await reason.textContent()).trim();
  expect(text, 'the refusal reason rendered empty').not.toBe('');
  expect(
    text,
    `the refused row rendered "${text}", which does not name the no_longer_current remedy`,
  ).toMatch(/changed this field after this report loaded/i);
  // Never a raw key or a raw token: both are the failure modes of a missing
  // translation, and both would still satisfy a bare non-empty check.
  expect(text).not.toMatch(/reports\.blast_radius/);
  expect(text).not.toMatch(/\bno_longer_current\b/);

  // No dialog opened: an ineligible plan must not offer a confirmation whose
  // only honest answer is Cancel.
  await expect(page.locator('#confirm-modal')).toBeHidden();

  // Rendered, not merely present: a reason nobody can read is the defect in
  // another costume. Contrast is left to the axe scan below; this checks the
  // element genuinely paints.
  const box = await reason.boundingBox();
  expect(box && box.height > 0, 'the reason element has zero height, so it is not actually shown').toBe(true);

  // And the surface is still clean with a refusal on it. The text is injected
  // after load, which is exactly the kind of late DOM the page-load scan never
  // sees -- including its rendered contrast against the row background.
  const results = await buildAxeBuilder(page).analyze();
  expect(
    results.violations,
    `a11y violations with a refusal reason rendered:\n${formatViolations(results.violations)}`,
  ).toHaveLength(0);

  test.info().annotations.push({
    type: 'refusal-reason-rendered',
    description: JSON.stringify({ changeID: target.id, field: target.field, text }, null, 2),
  });
});

test('every refusal token renders operator-actionable prose, and an unknown one degrades loudly', async ({ page }) => {
  await gotoPane(page);

  const rowId = await page.evaluate(() => {
    const tr = document.querySelector('tr[id^="blast-row-"]');
    return tr ? tr.id.replace(/^blast-row-/, '') : null;
  });
  if (!rowId) {
    throw new Error('no blast-radius rows on this server, so no row could carry a reason. UNVERIFIED.');
  }

  // The seven tokens are the blastRefuse* constants in
  // internal/api/handlers_blast_radius_restore.go, plus one the page has never
  // heard of. Rendering is driven through the page's OWN function against a
  // response shaped exactly like the endpoint's, so the strings, the element
  // lookup and the cascade are all the real ones.
  const tokens = [
    'change_not_found', 'not_revertible', 'revert_of_revert', 'no_longer_current',
    'restore_failed', 'old_value_invalid', 'name_collision',
    'a_token_from_a_newer_server',
  ];

  for (const token of tokens) {
    const rendered = await page.evaluate(({ id, reason }) => {
      window.blastApplyRefusalReasons({
        items: [{ change_id: id, restore_status: 'refused', refuse_reason: reason }],
      });
      const el = document.getElementById('blast-reason-' + id);
      if (!el) return null;
      return {
        text: el.textContent.trim(),
        hidden: el.classList.contains('hidden'),
        display: getComputedStyle(el).display,
      };
    }, { id: rowId, reason: token });

    expect(rendered, `no reason element for row ${rowId}`).not.toBeNull();
    expect(rendered.hidden, `the reason stayed hidden for ${token}`).toBe(false);
    expect(rendered.display, `the reason is display:none for ${token}`).not.toBe('none');
    // Not blank, not a raw key, and for a KNOWN token not the bare token
    // either -- the three ways an operator ends up with no usable information.
    expect(rendered.text, `${token} rendered empty`).not.toBe('');
    expect(rendered.text, `${token} rendered a raw translation key`).not.toMatch(/reports\.blast_radius/);
    if (token !== 'a_token_from_a_newer_server') {
      expect(rendered.text, `${token} rendered as the bare token`).not.toMatch(
        new RegExp(`\\b${token}\\b`),
      );
    } else {
      // The unknown branch does the opposite on purpose: it QUOTES the token,
      // so an operator has something to search for and a support conversation
      // has something to quote. Blank would hide the refusal entirely, which
      // is worse than the wrong sentence this whole change removes.
      expect(rendered.text, 'an unknown token must quote itself').toMatch(new RegExp(token));
    }

    test.info().annotations.push({ type: `refusal-${token}`, description: rendered.text });
  }

  // THE CLEAR ARM. An operator who fixes the cause and previews again must not
  // be left reading the previous attempt's refusal beside a row that is now
  // eligible.
  const cleared = await page.evaluate((id) => {
    window.blastApplyRefusalReasons({
      items: [{ change_id: id, restore_status: 'planned' }],
    });
    const el = document.getElementById('blast-reason-' + id);
    return { text: el.textContent, hidden: el.classList.contains('hidden') };
  }, rowId);
  expect(cleared.text, 'a stale refusal survived a plan that no longer refuses the row').toBe('');
  expect(cleared.hidden, 'the emptied reason element stayed visible').toBe(true);
});

// ---------------------------------------------------------------------------
// 6. The filter controls (#3093).
//
// The pane's filters are the newest interactive surface on it, and the
// chip-based flyout is the control type a11y regressions land on hardest: one
// that traps focus, loses it, or announces no state.
//
// Covered here, in BOTH themes where the assertion is a rendered-appearance
// one:
//   1. Every control has an accessible NAME (axe's own rules cover the
//      generic case; this asserts it per control, so a failure names the
//      control rather than a rule id).
//   2. Every control is Tab-REACHABLE with a visible focus indicator.
//   3. The flyout opens from the keyboard, moves focus INTO the panel, and
//      returns focus to its trigger on Escape.
//   4. A full axe scan with the flyout OPEN, in both themes -- the panel is
//      inert while closed, so the scans at the top of this file never see it.
//
// Nothing here is scoped away when the fixture is thin: the filter controls
// render regardless of how many rows the report holds (they are generated from
// artist.TrackableFields() and a fixed vocabulary, not from the rows), so an
// absent control is a defect and is reported as one.
// ---------------------------------------------------------------------------

// The narrowing controls this pane exposes, keyed by what a failure should say.
// The ordering selects live beside these in the same toolbar and carry their own
// coverage in the slice that adds them: a spec here naming a control this branch
// does not render would fail as a missing control rather than as a real defect.
const FILTER_CONTROLS = {
  'filters trigger': '#blast-radius-filter-trigger',
};

// accessibleNameOf reads the name a screen reader would announce, in the same
// precedence order the accname spec uses for these control types. Deliberately
// NOT reading textContent alone: a <select> announces its label, not its
// options, so a textContent check would pass a select with no label at all.
async function accessibleNameOf(page, selector) {
  return page.evaluate((sel) => {
    const el = document.querySelector(sel);
    if (!el) return null;
    const labelledBy = el.getAttribute('aria-labelledby');
    if (labelledBy) {
      const parts = labelledBy.split(/\s+/)
        .map(id => document.getElementById(id))
        .filter(Boolean)
        .map(n => n.textContent.trim());
      if (parts.join(' ').trim()) return parts.join(' ').trim();
    }
    const ariaLabel = (el.getAttribute('aria-label') || '').trim();
    if (ariaLabel) return ariaLabel;
    if (el.id) {
      const label = document.querySelector(`label[for="${CSS.escape(el.id)}"]`);
      if (label && label.textContent.trim()) return label.textContent.trim();
    }
    const closest = el.closest('label');
    if (closest && closest.textContent.trim()) return closest.textContent.trim();
    return (el.textContent || '').trim();
  }, selector);
}

test('every filter control has an accessible name', async ({ page }) => {
  await gotoPane(page);

  // Precondition: the controls are on the page at all. Without this an absent
  // control would report as an empty name, reading as a labelling defect when
  // the real fault is that the toolbar never rendered.
  const missing = [];
  for (const [label, sel] of Object.entries(FILTER_CONTROLS)) {
    if (await page.locator(sel).count() === 0) missing.push(`${label} (${sel})`);
  }
  expect(missing, `filter controls absent from the pane:\n${missing.join('\n')}`).toEqual([]);

  for (const [label, sel] of Object.entries(FILTER_CONTROLS)) {
    const name = await accessibleNameOf(page, sel);
    expect(name, `${label} (${sel}) has no accessible name; a screen-reader user is offered an unlabelled control`)
      .toBeTruthy();
  }
});

test('the filters trigger is Tab-reachable with a visible focus indicator', async ({ page }) => {
  await gotoPane(page);

  const sel = FILTER_CONTROLS['filters trigger'];
  // Precondition: the control exists. Absent, the walk below would report it as
  // unreachable, which reads as a keyboard defect rather than a missing
  // control.
  expect(await page.locator(sel).count(), `the filters trigger (${sel}) is absent from the pane`).toBe(1);

  const baseline = await page.evaluate(([s, props]) => {
    const cs = getComputedStyle(document.querySelector(s));
    return Object.fromEntries(props.map(p => [p, cs[p]]));
  }, [sel, FOCUS_PROPS]);

  const focused = await page.evaluate(([s, props]) => {
    const el = document.querySelector(s);
    el.focus();
    const cs = getComputedStyle(el);
    return Object.fromEntries(props.map(p => [p, cs[p]]));
  }, [sel, FOCUS_PROPS]);

  // A control that renders identically focused and unfocused leaves a keyboard
  // user with no way to tell where they are. Both mechanisms the codebase uses
  // count: a drawn ring, or a deliberate style swap. See focusIndicatorFor.
  expect(
    focusIndicatorFor(baseline, focused).visible,
    `the filters trigger renders identically focused and unfocused, so a keyboard user cannot tell where they are`,
  ).toBe(true);

  // Reachability is measured separately from the indicator: a control can be
  // focusable by script and still be skipped by Tab.
  //
  // The walk starts at the first focusable element and is bounded by the
  // document's own focusable count. Measured on a live page: pressing Tab a
  // fixed number of times from <body> runs focus off the end of the document
  // and into BROWSER chrome, where presses no longer reach the page at all, so
  // a generous fixed bound wastes every press after the first wrap rather than
  // "keeping looking".
  const focusableCount = await page.evaluate(() => document.querySelectorAll(
    'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), '
    + 'textarea:not([disabled]), [tabindex]:not([tabindex="-1"])',
  ).length);
  expect(focusableCount, 'the page reports no focusable elements at all').toBeGreaterThan(0);

  // ORDER IS LOAD-BEARING: the seeding focus() runs BEFORE the listener is
  // registered. focus() dispatches focusin synchronously, so a listener armed
  // first would record a hit for the seed element itself. Today the rail toggle
  // precedes the trigger in DOM order, so seeding never lands on the trigger --
  // but the day the toolbar moves above the rail, an armed-first listener turns
  // this into a vacuous pass: the flag is set before a single Tab is pressed,
  // the loop breaks on iteration one, and the test asserts nothing about Tab
  // reachability while still reporting green.
  await page.evaluate((s) => {
    window.__swFilterHit = false;
    const first = document.querySelector('a[href], button:not([disabled])');
    if (first) first.focus();
    document.addEventListener('focusin', (e) => {
      if (e.target && e.target.matches(s)) window.__swFilterHit = true;
    });
  }, sel);

  for (let i = 0; i < focusableCount; i++) {
    await page.keyboard.press('Tab');
    if (await page.evaluate(() => window.__swFilterHit)) break;
  }

  expect(
    await page.evaluate(() => window.__swFilterHit),
    `the filters trigger was never reached by Tab within ${focusableCount} presses (one per focusable element `
    + 'on the page), so a keyboard-only operator cannot filter the damage report at all',
  ).toBe(true);
});

test('the filter flyout opens from the keyboard and returns focus on Escape', async ({ page }) => {
  await gotoPane(page);

  const trigger = page.locator('#blast-radius-filter-trigger');
  expect(await trigger.count(), 'the filters trigger is absent').toBe(1);

  await trigger.focus();
  expect(await page.evaluate(() => document.activeElement.id), 'focusing the trigger did not take')
    .toBe('blast-radius-filter-trigger');

  await page.keyboard.press('Enter');
  // The default 'visible' state is correct here: an OPEN panel is painted.
  await page.waitForSelector('#blast-radius-filter-flyout:not([inert])', { timeout: 5000 });

  // Focus must land INSIDE the panel. A panel that opens without moving focus
  // strands a keyboard user behind it: the controls are ahead in the tab order
  // only by accident of DOM position, and the scrim swallows the pointer.
  const focusInside = await page.evaluate(() => {
    const panel = document.getElementById('blast-radius-filter-flyout');
    return !!(panel && document.activeElement && panel.contains(document.activeElement)
      && document.activeElement !== document.body);
  });
  expect(focusInside, 'opening the filter flyout left focus outside the panel').toBe(true);

  expect(
    await trigger.getAttribute('aria-expanded'),
    'the trigger still reports aria-expanded=false while the panel is open',
  ).toBe('true');

  await page.keyboard.press('Escape');
  // state:'attached', NOT the default 'visible'. A CLOSED flyout is
  // visibility:hidden by design (the panel is removed from paint, find-in-page
  // and the tab order once the slide-out finishes), so the default wait can
  // never resolve on a correctly closed panel and times out on success. Measured
  // against a live page: after Escape the panel really does carry inert and
  // focus really does return to the trigger; only this wait was wrong.
  await page.waitForSelector('#blast-radius-filter-flyout[inert]', { state: 'attached', timeout: 5000 });

  // Focus returns to the trigger. Dropping it to <body> is the classic
  // dialog-dismiss defect: the next Tab restarts from the top of the document.
  const returned = await page.evaluate(() => document.activeElement && document.activeElement.id);
  expect(returned, 'closing the filter flyout did not return focus to its trigger; the next Tab restarts '
    + 'from the top of the document').toBe('blast-radius-filter-trigger');

  expect(
    await trigger.getAttribute('aria-expanded'),
    'the trigger still reports aria-expanded=true after the panel closed',
  ).toBe('false');
});

for (const theme of ['dark', 'light']) {
  test(`the open filter flyout passes a full-page a11y scan (${theme} theme)`, async ({ page }) => {
    // The panel is inert while closed, so the scans at the top of this file
    // never measure it. Its contrast and labelling are only observable open.
    if (theme === 'dark') {
      await page.emulateMedia({ colorScheme: 'dark' });
    }
    await gotoPane(page);
    await applyTheme(expect, page, theme);

    await page.locator('#blast-radius-filter-trigger').click();
    await page.waitForSelector('#blast-radius-filter-flyout:not([inert])', { timeout: 5000 });

    // Precondition: the panel is genuinely rendered and visible, or the scan
    // below measures a hidden subtree and reports a meaningless green.
    //
    // The vacuity guard counts FOCUSABLE CONTROLS, not filter chips. This slice
    // ships the flyout deliberately empty of axes, so a chip count is zero here
    // and a `chips > 0` precondition fails on this branch -- which it did:
    // 4 failed / 76 passed on both engines, while the scan itself reported zero
    // violations in all four combinations. The precondition was wrong, not the
    // page, and the pre-push gate missed it because the a11y tier defaults to
    // SKIP.
    //
    // Focusables is the honest property: the panel always ships its close,
    // Clear All and Apply controls (3 on this branch), and the number only grows
    // as axes land, so this keeps its teeth through the later slices without
    // needing a re-edit. Deleting the guard instead would leave a scan that
    // passes over a hidden or empty subtree, which is a green light wired to
    // nothing.
    const visible = await page.evaluate(() => {
      const panel = document.getElementById('blast-radius-filter-flyout');
      if (!panel) return { open: false, focusables: 0 };
      const cs = getComputedStyle(panel);
      return {
        open: cs.display !== 'none' && cs.visibility !== 'hidden',
        focusables: panel.querySelectorAll(
          'button:not([disabled]), [href], input:not([disabled]), select:not([disabled]), '
          + 'textarea:not([disabled]), [tabindex]:not([tabindex="-1"])',
        ).length,
      };
    });
    expect(visible.open, 'the filter flyout is not visibly open; the scan would measure a hidden subtree').toBe(true);
    expect(visible.focusables, 'the open flyout exposes no controls; there is nothing to scan').toBeGreaterThan(0);

    const results = await buildAxeBuilder(page).analyze();
    expect(
      results.violations,
      `Blast-radius filter flyout ${theme}-theme a11y violations:\n${formatViolations(results.violations)}`,
    ).toHaveLength(0);
  });
}

// ---------------------------------------------------------------------------
// The active-filter badge survives first paint on a deep link (D-F2).
//
// THE DEFECT THIS PINS. The server renders the trigger with `is-active` and a
// count when the URL narrows the report. swFilterFlyout.initFromURL's last act
// is refreshActiveCount, which counts controls INSIDE the panel and writes that
// number to the trigger badge -- so run over a panel holding no axis controls
// it counts zero and sets the badge to display:none, ERASING the correct
// server-rendered answer a beat after load.
//
// The operator-facing shape is specific and bad: a deep link narrows a
// multi-thousand-row damage report, the table shows a subset, and the trigger
// reads a bare "Filters" with no count. Nothing on the pane says rows are being
// hidden, so the short table reads as the whole report -- an understatement of
// how much of the library was destroyed, on the surface whose only job is
// stating that number. It is also intermittent: after any Apply the badge comes
// back, because refreshActiveCount runs before the swap replaces the trigger.
//
// WHY THIS IS A BROWSER TEST AND NOT A GO ONE. The server-rendered markup is
// already correct -- a Go assertion over the response body passes both before
// and after the defect. The erasure happens in the DOM, after DOMContentLoaded,
// and only a real browser running the page's own scripts can see it.
//
// The wait is deliberate. The badge is correct at parse time and wrong only
// once the hydration handler has run, so asserting immediately would pass
// against the very defect this covers.
test('a deep-linked filter keeps its trigger badge on first paint', async ({ page }) => {
  // class=blanked narrows on one axis, and blanked/replaced is a fixed
  // vocabulary rather than fixture data -- so the URL is genuinely narrowing on
  // the a11y harness's empty database, where any row-derived value would not be.
  await gotoPane(page, `${PANE_URL}?class=blanked`);

  // PRECONDITION: the server really did render a badge for this URL. Without
  // this the assertion below cannot tell "hydration erased the badge" from
  // "this URL was never narrowing", and a change to the neutral-value handling
  // would turn the whole test green while proving nothing.
  const served = await page.evaluate(() => {
    const trigger = document.getElementById('blast-radius-filter-trigger');
    if (!trigger) return null;
    const badge = trigger.querySelector('.sw-filter-trigger-badge');
    return {
      active: trigger.classList.contains('is-active'),
      badgeText: badge ? badge.textContent.trim() : null,
    };
  });
  expect(served, 'the filters trigger is absent').not.toBeNull();
  expect(
    served.active,
    'the server did not mark the trigger active for ?class=blanked, so this URL is not narrowing and the '
    + 'badge assertion below would be vacuous',
  ).toBe(true);
  expect(
    served.badgeText,
    'the server rendered no count in the trigger badge for ?class=blanked',
  ).toBe('1');

  // Give the DOMContentLoaded hydration handler a full turn to run. This is the
  // window in which the defect lands.
  await page.waitForFunction(() => document.readyState === 'complete');
  await page.waitForTimeout(1000);

  const afterHydration = await page.evaluate(() => {
    const trigger = document.getElementById('blast-radius-filter-trigger');
    const badge = trigger && trigger.querySelector('.sw-filter-trigger-badge');
    if (!badge) return { present: false };
    const cs = getComputedStyle(badge);
    return {
      present: true,
      text: badge.textContent.trim(),
      display: cs.display,
      visibility: cs.visibility,
      // getClientRects() is empty for a box that paints nothing, which catches
      // display:none set inline as well as by a rule.
      painted: badge.getClientRects().length > 0,
      triggerActive: trigger.classList.contains('is-active'),
    };
  });

  expect(
    afterHydration.present,
    'the trigger badge was removed from the DOM after hydration; a deep-linked narrowing report shows no '
    + 'sign that rows are hidden',
  ).toBe(true);
  expect(
    afterHydration.painted,
    `the trigger badge is not painted after hydration (display=${afterHydration.display}, `
    + `visibility=${afterHydration.visibility}); the operator sees a bare "Filters" over a narrowed table and `
    + 'reads the short row set as the whole report',
  ).toBe(true);
  expect(
    afterHydration.text,
    'the trigger badge no longer reports the number of active filters after hydration',
  ).toBe('1');
  expect(
    afterHydration.triggerActive,
    'the trigger lost its is-active styling after hydration, so the only remaining signal that the report '
    + 'is narrowed is gone',
  ).toBe(true);
});
