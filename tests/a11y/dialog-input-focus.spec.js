// dialog-input-focus.spec.js - focus management for the dialogs that collect
// input (#2511). Opening each must move keyboard focus INTO the dialog (the
// URL box, the provider search box, the crop "Save as" select), and closing
// it must return focus to something the operator can see. `autofocus` cannot
// do this: the fetch modal is revealed by a class toggle and the provider body
// arrives by HTMX swap, so the handlers focus explicitly.
//
// The restore cases go through the REAL Actions menu: the element focused at
// open time is then a menu ITEM that is hidden again by close time, so focus
// must fall back to the menu's trigger. Calling the open function with a
// pre-focused trigger (the first cut of this spec) skipped exactly that path.
//
// Fixture: reuses seed-google-images-link.js (one scanned artist with a real
// fanart file) because the images page needs an artist to render at all.
// The harness boots an empty database, so the fixture is built in beforeAll.

import { test, expect } from 'playwright/test';

import { disableTransitions } from './helpers/settle.js';
import { buildAxeBuilder, formatViolations, applyTheme, restorePersistedTheme } from './helpers/axe.js';
import { seedGoogleImagesLinkArtist } from './helpers/seed-google-images-link.js';

let artistId;

test.beforeAll(async ({ request }) => {
  artistId = await seedGoogleImagesLinkArtist(request);
});
test.beforeEach(async ({ page }) => { await disableTransitions(page); });
test.afterEach(async ({ page }) => { await restorePersistedTheme(page); });

async function gotoImages(page, type = 'logo') {
  await page.goto(`/artists/${artistId}/images?type=${type}`);
  await page.waitForLoadState('load');
  const trigger = page.locator('[aria-haspopup="true"]').first();
  await trigger.waitFor({ state: 'visible', timeout: 10_000 });
  return trigger;
}

/** Real menu path: Actions trigger -> "Fetch from URL" item. */
async function openFetchViaMenu(page, trigger) {
  await trigger.click();
  await page.getByRole('menuitem', { name: 'Fetch from URL' }).click();
  await expect(page.locator('#fetch-url-modal')).toBeVisible();
}

const CANCEL = '#fetch-url-modal button:has-text("Cancel")';

test('fetch dialog: focus enters the input, Cancel returns focus to the Actions trigger', async ({ page }) => {
  const trigger = await gotoImages(page);
  await openFetchViaMenu(page, trigger);
  // Fails without the focus call: focus stays on the hidden menu item / body.
  await expect(page.locator('#fetch-url-input')).toBeFocused();
  await page.locator(CANCEL).click();
  await expect(page.locator('#fetch-url-modal')).toBeHidden();
  // Fails without the trigger fallback: the captured opener is the (now
  // hidden) menu item, so focus ends on body or the hidden Cancel button.
  await expect(trigger).toBeFocused();
});

test('fetch dialog: Escape closes it and returns focus to the Actions trigger', async ({ page }) => {
  const trigger = await gotoImages(page);
  await openFetchViaMenu(page, trigger);
  await page.keyboard.press('Escape');
  // Fails without the Escape handler: the modal stays open.
  await expect(page.locator('#fetch-url-modal')).toBeHidden();
  await expect(trigger).toBeFocused();
});

test('fetch dialog: submitting returns focus to the Actions trigger', async ({ page }) => {
  const trigger = await gotoImages(page);
  await openFetchViaMenu(page, trigger);
  // Unreachable host: the request fails after the dialog has already closed,
  // which is all this case needs (the submit path's own restore call).
  await page.locator('#fetch-url-input').fill('http://127.0.0.1:1/x.jpg');
  await page.locator('#fetch-url-submit').click();
  await expect(page.locator('#fetch-url-modal')).toBeHidden();
  // Fails without the restore call in the submit handler.
  await expect(trigger).toBeFocused();
});

test('fetch dialog: dialog semantics are present', async ({ page }) => {
  await gotoImages(page);
  const modal = page.locator('#fetch-url-modal');
  await expect(modal).toHaveAttribute('role', 'dialog');
  await expect(modal).toHaveAttribute('aria-modal', 'true');
  await expect(modal).toHaveAttribute('aria-labelledby', 'fetch-url-modal-title');
  await expect(page.locator('#fetch-url-modal-title')).toHaveText('Fetch from URL');
});

test('backdrop slot: the per-slot fetch button focuses the input and Cancel restores to it', async ({ page }) => {
  await gotoImages(page, 'fanart');
  const slotBtn = page.locator('[onclick*="swOpenFetchUrlForSlot"]:visible').first();
  await expect(slotBtn, 'the fixture guarantees one backdrop slot with a fetch button').toHaveCount(1);
  await slotBtn.click();
  await expect(page.locator('#fetch-url-modal')).toBeVisible();
  // Fails without the focus call in swOpenFetchUrlForSlot.
  await expect(page.locator('#fetch-url-input')).toBeFocused();
  await page.locator(CANCEL).click();
  await expect(slotBtn).toBeFocused();
});

test('backdrop slot: the crop button focuses Save-as; Escape (via the discard guard) restores focus', async ({ page }) => {
  await gotoImages(page, 'fanart');
  const cropBtn = page.locator('[onclick*="swOpenCropForSlot"]:visible').first();
  await expect(cropBtn, 'the fixture guarantees one backdrop slot with a crop button').toHaveCount(1);
  await cropBtn.click();
  await expect(page.locator('#crop-modal')).toBeVisible();
  // Fails without the focus call in openCropModal.
  await expect(page.locator('#crop-type')).toBeFocused();
  // A loaded image marks the session dirty (in Cropper's ready callback), so
  // Escape raises the discard confirm exactly as Cancel does; accepting it
  // closes the dialog. Wait for the crop box so the session IS dirty first.
  await expect(page.locator('#crop-modal .cropper-crop-box')).toBeVisible();
  await page.keyboard.press('Escape');
  // Fails without the Escape handler: no confirm appears and the modal stays.
  await expect(page.locator('#confirm-modal')).toBeVisible();
  await page.locator('#confirm-modal-accept').click();
  await expect(page.locator('#crop-modal')).toBeHidden();
  // Fails without the restore call in closeCropModal.
  await expect(cropBtn).toBeFocused();
});

test('crop dialog: dialog semantics are present', async ({ page }) => {
  await gotoImages(page);
  const modal = page.locator('#crop-modal');
  await expect(modal).toHaveAttribute('role', 'dialog');
  await expect(modal).toHaveAttribute('aria-modal', 'true');
  await expect(modal).toHaveAttribute('aria-labelledby', 'crop-modal-title');
  await expect(page.locator('#crop-modal-title')).toHaveText('Crop Image');
  // The Save-as select is the crop dialog's focus target, so it must be named.
  await expect(page.locator('label[for="crop-type"]')).toHaveCount(1);
});

/**
 * Drives the shared provider-modal afterSwap listener with a realistic swapped
 * body (the identify form's query input), after emulating a menu-item click:
 * the item is focused, then its menu is hidden, as the real menu does before
 * the swap lands. No live provider is needed, which the empty harness lacks.
 */
async function openProviderFromMenu(page, trigger) {
  await trigger.click();
  await page.evaluate(() => {
    const item = document.querySelector('[data-context-menu] [role=menuitem]');
    item.focus();
    item.closest('[role=menu]').classList.add('hidden');
    const body = document.getElementById('field-provider-modal-body');
    body.innerHTML = '<form><input name="query" type="text" aria-label="Artist name"><button type="submit">Search</button></form>';
    document.body.dispatchEvent(new CustomEvent('htmx:afterSwap', { bubbles: true, detail: { target: body } }));
  });
  await expect(page.locator('#field-provider-modal')).toBeVisible();
}

test('provider modal: focus enters the swapped input; Escape restores focus to the menu trigger', async ({ page }) => {
  const trigger = await gotoImages(page);
  await openProviderFromMenu(page, trigger);
  // Fails without the focus call in showFieldProviderModal.
  await expect(page.locator('#field-provider-modal-body input[name="query"]')).toBeFocused();
  await page.keyboard.press('Escape');
  await expect(page.locator('#field-provider-modal')).toBeHidden();
  // Fails without the trigger fallback (opener is the hidden menu item).
  await expect(trigger).toBeFocused();
});

test('provider modal: the close button has an accessible name', async ({ page }) => {
  await gotoImages(page);
  await expect(page.locator('#field-provider-modal button[onclick="hideFieldProviderModal()"]')).toHaveAttribute('aria-label', 'Close');
});

// axe with each dialog OPEN, both themes: the roles added in #2511 change the
// accessibility tree, so a scan of the closed page proves nothing about them.
for (const theme of ['dark', 'light']) {
  test(`axe: fetch, crop and provider dialogs open are clean (${theme})`, async ({ page }) => {
    const trigger = await gotoImages(page);
    await applyTheme(expect, page, theme);

    await openFetchViaMenu(page, trigger);
    let results = await buildAxeBuilder(page).include('#fetch-url-modal').analyze();
    expect(results.violations, `fetch dialog (${theme}):\n${formatViolations(results.violations)}`).toEqual([]);
    await page.locator(CANCEL).click();

    await page.evaluate(() => openCropModal(
      'data:image/gif;base64,R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7', 'logo', undefined, false));
    await expect(page.locator('#crop-modal')).toBeVisible();
    results = await buildAxeBuilder(page).include('#crop-modal').analyze();
    expect(results.violations, `crop dialog (${theme}):\n${formatViolations(results.violations)}`).toEqual([]);
    await page.evaluate(() => closeCropModal());

    await openProviderFromMenu(page, trigger);
    results = await buildAxeBuilder(page).include('#field-provider-modal').analyze();
    expect(results.violations, `provider dialog (${theme}):\n${formatViolations(results.violations)}`).toEqual([]);
  });
}
