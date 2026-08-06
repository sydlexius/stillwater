// mobile-nav.spec.js -- the mobile navigation contract (#2382).
//
// At mobile widths the desktop sidebar is display:none. The bottom tab bar's
// 5th "More" tab plus its BottomSheet is the ONLY route to Activity, Logs,
// Preferences, the admin report items, the theme toggle and Log Out. Both
// defects these tests pin were invisible to the Go templ tests, which assert
// rendered MARKUP: the sheet was present and correct in the DOM in both cases
// and still could not be opened by a user.
//
//   1. A document-level outside-click listener closed the sheet on the SAME
//      click that opened it (a standalone BottomSheet trigger sits outside
//      both [data-context-menu] and .ctx-bottom-sheet).
//   2. The bottom tabs showed at `max-width: 768px` while .ctx-bottom-sheet
//      was suppressed at `min-width: 768px`, so at EXACTLY 768px the More tab
//      was visible but its sheet could never paint.
//
// Both are behavioural/cascade failures, so they need a real browser.

import { test, expect } from 'playwright/test';

import { disableTransitions } from './helpers/settle.js';
import { buildAxeBuilder, formatViolations } from './helpers/axe.js';

const TRIGGER = 'button[aria-controls="bs-more-nav"]';
const SHEET = '#bs-more-nav';

test.beforeEach(async ({ page }) => {
  await disableTransitions(page);
});

// The widths that matter: a phone, and both sides of the 768 boundary the
// bottom tabs and the sheet used to disagree about.
for (const width of [390, 767, 768]) {
  test(`More sheet opens and is usable at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 844 });
    await page.goto('/');

    // Precondition: this really is the mobile layout. Without it the test
    // would pass vacuously on a desktop render where no tab bar exists.
    await expect(page.locator('.sw-bottom-tabs')).toBeVisible();
    const trigger = page.locator(TRIGGER).first();
    await expect(trigger).toBeVisible();

    // UX0: 44px minimum tap target.
    const box = await trigger.boundingBox();
    expect(box.height, `More tab is ${box.height}px tall; the 44px rule is a floor`).toBeGreaterThanOrEqual(44);

    await trigger.click();

    // The regression: the sheet must still be open after the click settles.
    // Asserting computed display AND the open attributes, because the two
    // bugs failed differently -- one reverted the attributes, the other left
    // them correct while CSS kept display:none.
    const sheet = page.locator(SHEET);
    await expect(sheet).toHaveClass(/ctx-sheet-open/);
    await expect(sheet).toHaveAttribute('aria-hidden', 'false');
    const display = await sheet.evaluate((el) => getComputedStyle(el).display);
    expect(display, `sheet is display:${display} at ${width}px; the More tab is a dead button`).not.toBe('none');
    await expect(sheet.locator('a,button').first()).toBeVisible();

    // Log Out is the item the issue names as having no other mobile route.
    await expect(sheet.getByText('Log Out')).toBeVisible();

    // Focus moves into the sheet, and Escape closes it and gives focus back.
    await expect.poll(async () =>
      sheet.evaluate((el) => el.contains(document.activeElement))).toBe(true);
    await page.keyboard.press('Escape');
    await expect(sheet).not.toHaveClass(/ctx-sheet-open/);
    await expect.poll(async () =>
      trigger.evaluate((el) => el === document.activeElement)).toBe(true);
  });
}

// Every width must offer SOME navigation. 769px is the first desktop width:
// the tabs go away, so the sidebar has to come back.
test('769px hands off to the sidebar rather than leaving no nav', async ({ page }) => {
  await page.setViewportSize({ width: 769, height: 844 });
  await page.goto('/');
  await expect(page.locator('.sw-bottom-tabs')).toBeHidden();
  await expect(page.locator('#sw-sidebar')).toBeVisible();
});

// The sheet's non-link actions. These are the ones a markup test cannot reach
// and the ones the first version of this spec missed entirely: it clicked the
// trigger and nothing else, so it passed green while Preferences and Help both
// threw ReferenceError on their first line and did nothing at all.
//
// Root cause worth remembering: a templ `script` block compiles to a
// hash-suffixed global, and a block never used as an OnClick is never emitted
// into the page, so calling one script from another by its SOURCE name throws.
test('More sheet actions actually run (no ReferenceError)', async ({ page }) => {
  const errors = [];
  page.on('pageerror', (e) => errors.push(String(e.message)));

  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto('/');
  const sheet = page.locator(SHEET);

  // Preferences: closes the sheet and opens the drawer.
  await page.locator(TRIGGER).first().click();
  await expect(sheet).toHaveClass(/ctx-sheet-open/);
  await sheet.getByText('Preferences', { exact: false }).first().click();
  await expect(sheet).not.toHaveClass(/ctx-sheet-open/);
  await expect(page.locator('#sw-prefs-drawer')).toHaveAttribute('aria-hidden', 'false');
  expect(errors, `page errors after Preferences: ${errors.join('; ')}`).toEqual([]);

  // Help: same shape, different global.
  await page.reload();
  errors.length = 0;
  await page.locator(TRIGGER).first().click();
  await expect(sheet).toHaveClass(/ctx-sheet-open/);
  await sheet.getByText('Help shortcuts', { exact: false }).first().click();
  await expect(sheet).not.toHaveClass(/ctx-sheet-open/);
  expect(errors, `page errors after Help: ${errors.join('; ')}`).toEqual([]);

  // Theme cycle: deliberately leaves the sheet OPEN (the theme change is the
  // feedback), so this also pins that documented difference.
  //
  // Asserts the STORED PREFERENCE advanced, not the html class. The cycle is
  // dark -> light -> system, and "system" resolves back to whatever the OS
  // says -- which in a dark-defaulted context re-applies the same class the
  // run started with. Comparing class strings made this test flaky (it failed
  // once and passed on retry); the preference is the thing the click actually
  // changes.
  await page.reload();
  errors.length = 0;
  await page.locator(TRIGGER).first().click();
  const before = await page.evaluate(() =>
    localStorage.getItem('sw-theme') || document.documentElement.getAttribute('data-theme'));
  await sheet.getByText('Cycle theme', { exact: false }).first().click();
  await expect.poll(async () => page.evaluate(() =>
    localStorage.getItem('sw-theme') || document.documentElement.getAttribute('data-theme')),
    { message: 'theme preference did not advance' }).not.toBe(before);
  // The sheet stays open on purpose here -- that is the documented behaviour.
  await expect(sheet).toHaveClass(/ctx-sheet-open/);
  expect(errors, `page errors after theme cycle: ${errors.join('; ')}`).toEqual([]);

  // RESTORE the theme. The click runs the production action, which persists
  // through swPreferences to the SHARED test server -- this tier runs one
  // ephemeral server for every spec file, so leaving the theme advanced makes
  // a later a11y scan run under a theme it did not choose and report contrast
  // violations that are artefacts of this test. Restoring is the fix rather
  // than avoiding the write, because the persisted write is the behaviour
  // under test.
  if (before) {
    await page.evaluate(
      (theme) => window.swPreferences && window.swPreferences.set('theme', theme),
      before,
    );
  }
});

// A short phone must still reach the sheet's destructive action. At 375x568
// (iPhone SE / 8) the admin sheet's content is taller than the panel, and the
// panel used to be the scroll container -- so Cancel and Log Out rendered below
// the fold with nothing indicating the list scrolled.
//
// The fix is structural, not a smaller max-height: shrinking the cap was
// measured and makes it WORSE, because the content (586px) exceeds the panel
// (483px) either way. The footer now sits outside the scroll region.
test('the sheet footer stays on screen on a short phone', async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 568 });
  await page.goto('/');
  await page.locator(TRIGGER).first().click();

  const sheet = page.locator(SHEET);
  await expect(sheet).toHaveClass(/ctx-sheet-open/);

  // Precondition: this viewport really does overflow. If the sheet ever gets
  // short enough to fit, this test is no longer exercising the case it names
  // and should be re-pointed rather than left passing for the wrong reason.
  const overflows = await sheet.locator('.ctx-sheet-items')
    .evaluate((el) => el.scrollHeight > el.clientHeight);
  expect(overflows, 'the item list no longer overflows at 375x568; re-point this test').toBe(true);

  // Cancel must be IN the viewport, not merely in the DOM.
  const cancel = sheet.locator('.ctx-sheet-cancel');
  const box = await cancel.boundingBox();
  expect(box.y + box.height,
    `Cancel bottom is ${box.y + box.height}px in a 568px viewport -- it is below the fold`)
    .toBeLessThanOrEqual(568);

  // And Log Out must be reachable by scrolling the list.
  const logout = sheet.getByText('Log Out', { exact: false }).first();
  await logout.scrollIntoViewIfNeeded();
  await expect(logout).toBeInViewport();
});

// A real axe scan with the sheet OPEN. The sheet previously declared
// role="menu" + aria-modal="true" -- aria-modal is only valid on
// dialog/alertdialog -- and put Cancel and the drag-handle divs under that
// role="menu" without menuitem roles. Two critical violations that no markup
// test could see, because the markup was exactly as intended; only a real
// accessibility engine reading the open sheet reports them.
test('the open sheet is free of axe violations', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto('/');
  await page.locator(TRIGGER).first().click();

  const sheet = page.locator(SHEET);
  await expect(sheet).toHaveClass(/ctx-sheet-open/);

  // Precondition: scanning a CLOSED sheet passes trivially and would make this
  // test a decoration. Assert the element under test is really in the tree.
  await expect(sheet).toBeVisible();

  // The container is a modal overlay -- scrim, scroll lock, focus trap -- so it
  // must say dialog, and its rows must not claim menu semantics outside a menu.
  await expect(sheet).toHaveAttribute('role', 'dialog');
  await expect(sheet).toHaveAttribute('aria-modal', 'true');
  expect(await sheet.locator('[role="menuitem"]').count(),
    'menuitem outside a menu context is an aria-required-context-role violation').toBe(0);

  const results = await buildAxeBuilder(page).analyze();
  expect(results.violations, formatViolations(results.violations)).toEqual([]);
});

// A disabled row must be skipped by BOTH focus paths. The More sheet ships no
// disabled items today, so the row is injected -- the defect is in the
// component's focus queries, not in this page's data, and it would otherwise
// only surface for whichever caller adds the first disabled item.
//
// Why it broke: an <a> takes no `disabled` attribute, so a disabled link is
// marked with aria-disabled + tabindex="-1" -- and `a[href]` matches it anyway.
// Both queries select on `a[href]`, so initial focus landed on the dead row and
// the Tab cycle included it. Mutation-proved: reverting either filter puts
// "Dead Row" back in both.
test('a disabled sheet row is skipped by initial focus and the trap', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto('/');

  await page.evaluate(() => {
    const list = document.querySelector('#bs-more-nav .ctx-sheet-items');
    const a = document.createElement('a');
    a.href = '/dead';
    a.textContent = 'Dead Row';
    a.className = 'context-menu-item context-menu-item-disabled';
    a.setAttribute('aria-disabled', 'true');
    a.setAttribute('tabindex', '-1');
    list.insertBefore(a, list.firstChild);
  });

  await page.locator(TRIGGER).first().click();
  await expect(page.locator(SHEET)).toHaveClass(/ctx-sheet-open/);

  // Precondition: the injected row is really the first candidate, so a broken
  // query would demonstrably land on it rather than passing by luck.
  const firstRow = await page.locator(`${SHEET} .ctx-sheet-items > *`).first().innerText();
  expect(firstRow.trim(), 'the disabled row is not first; this test would not prove anything').toBe('Dead Row');

  await expect.poll(async () =>
    page.evaluate(() => (document.activeElement.innerText || '').trim()),
    { message: 'initial focus landed on the disabled row' }).not.toBe('Dead Row');

  const visited = [];
  for (let i = 0; i < 16; i += 1) {
    await page.keyboard.press('Tab');
    visited.push(await page.evaluate(() => (document.activeElement.innerText || '').trim()));
  }
  expect(visited, 'the disabled row is reachable by Tab').not.toContain('Dead Row');

  const back = [];
  for (let i = 0; i < 16; i += 1) {
    await page.keyboard.press('Shift+Tab');
    back.push(await page.evaluate(() => (document.activeElement.innerText || '').trim()));
  }
  expect(back, 'the disabled row is reachable by Shift+Tab').not.toContain('Dead Row');
});
