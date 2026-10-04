// modal-escape.spec.js - Escape dismisses a global dialog WITHOUT also
// exiting artist-detail edit mode (#2727). The field-provider modal (Identify
// / provider results) and the hx-confirm modal register persistent document
// keydown listeners AFTER the page's own listener, so the page handler runs
// first; it must yield while either dialog is open.
//
// Fixture: seed-modal-escape.js scans one artist. Each test enters page edit
// mode first, because the defect only shows when there is edit state to lose.

import { test, expect } from 'playwright/test';

import { disableTransitions } from './helpers/settle.js';
import { seedModalEscapeArtist } from './helpers/seed-modal-escape.js';

let artistId;

test.beforeAll(async ({ request }) => {
  artistId = await seedModalEscapeArtist(request);
});
test.beforeEach(async ({ page }) => { await disableTransitions(page); });

const ROOT = '.sw-next-artist-detail';

/** Loads the detail page and enters edit mode with the 'e' shortcut. */
async function enterEditMode(page) {
  await page.goto(`/artists/${artistId}`);
  await page.waitForLoadState('load');
  await expect(page.locator(ROOT), 'fixture: detail page root must render').toHaveCount(1);
  await expect(page.locator(ROOT)).not.toHaveClass(/is-editing/);
  await page.locator('body').press('e');
  await expect(page.locator(ROOT), 'precondition: page must be in edit mode').toHaveClass(/is-editing/);
  // is-editing is set synchronously, but the page ignores setEditAll(false)
  // (editBusy) until the batch edit-all request has swapped the field editors
  // in. An Escape sent before that is silently dropped, so wait for the editors.
  await expect(page.locator(`${ROOT} form[hx-patch]`).first(), 'precondition: field editors opened').toBeVisible();
}

// Absence assertions need a window: without the fix the page handler's
// setEditAll(false) drops is-editing within a few ms (saveAllEditors with
// nothing dirty resolves immediately), so 600ms is ample.
async function settle(page) { await page.waitForTimeout(600); }

test('control: Escape with no dialog open exits edit mode', async ({ page }) => {
  await enterEditMode(page);
  await page.keyboard.press('Escape');
  await expect(page.locator(ROOT)).not.toHaveClass(/is-editing/);
});

test('Escape closing the field-provider modal keeps edit mode', async ({ page }) => {
  await enterEditMode(page);
  await page.evaluate(() => {
    document.getElementById('field-provider-modal-body').innerHTML = '<p>fixture result</p>';
    window.showFieldProviderModal();
  });
  const modal = page.locator('#field-provider-modal');
  await expect(modal, 'precondition: modal must be open').toBeVisible();

  await page.keyboard.press('Escape');
  await expect(modal).toBeHidden();
  await settle(page);
  // Fails without the fix: the page handler also ran and removed is-editing.
  await expect(page.locator(ROOT)).toHaveClass(/is-editing/);

  // The modal is closed now, so the next Escape is the page's again.
  await page.keyboard.press('Escape');
  await expect(page.locator(ROOT)).not.toHaveClass(/is-editing/);
});

test('Escape cancelling the confirm modal keeps edit mode', async ({ page }) => {
  await enterEditMode(page);
  await page.evaluate(() => {
    window.showConfirmDialog('Fixture confirm?', null, () => {});
  });
  const modal = page.locator('#confirm-modal');
  await expect(modal, 'precondition: confirm modal must be open').toBeVisible();

  await page.keyboard.press('Escape');
  await expect(modal).toBeHidden();
  await settle(page);
  await expect(page.locator(ROOT)).toHaveClass(/is-editing/);
});

test('Escape closing the cheat sheet (real ? key) keeps edit mode', async ({ page }) => {
  await enterEditMode(page);
  await page.keyboard.press('Shift+?');
  const sheet = page.locator('#cheat-sheet-modal');
  await expect(sheet, 'precondition: cheat sheet opened by the real ? key').toBeVisible();

  await page.keyboard.press('Escape');
  await expect(sheet).toBeHidden();
  await settle(page);
  await expect(page.locator(ROOT)).toHaveClass(/is-editing/);
});

test('Escape closing the hero Actions menu keeps edit mode', async ({ page }) => {
  await enterEditMode(page);
  // Skip the sticky-bar twin (tabindex=-1, not operable); the hero trigger is the one a user clicks.
  const trigger = page.locator(`${ROOT} [data-context-menu] > button[aria-haspopup]:not([tabindex="-1"]):visible`).first();
  await trigger.click();
  await expect(trigger, 'precondition: Actions menu open').toHaveAttribute('aria-expanded', 'true');

  await page.keyboard.press('Escape');
  await expect(trigger).toHaveAttribute('aria-expanded', 'false');
  await settle(page);
  await expect(page.locator(ROOT)).toHaveClass(/is-editing/);
});

// The mobile "More" nav sheet (#bs-more-nav) is the one .ctx-bottom-sheet
// reachable from this page (the hero Actions menu has no sheet form). The page
// handler's open-overlay check has a branch for it; without that branch Escape
// would close the sheet and also leave edit mode.
test('Escape closing the mobile More nav sheet keeps edit mode', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await enterEditMode(page);
  await page.locator('button[aria-controls="bs-more-nav"]').first().click();
  const sheet = page.locator('#bs-more-nav');
  await expect(sheet, 'precondition: More sheet open').toHaveClass(/ctx-sheet-open/);

  await page.keyboard.press('Escape');
  await expect(sheet).not.toHaveClass(/ctx-sheet-open/);
  await settle(page);
  await expect(page.locator(ROOT)).toHaveClass(/is-editing/);
});
