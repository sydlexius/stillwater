// rule-platform-prune-option.spec.js - the "Also delete near-duplicate backdrops
// on media servers" switch in the "No duplicate images" rule's Configure panel
// on Settings (#3138 S4a).
//
// Runs on both projects in playwright.config.js (firefox-a11y, chromium-a11y).
//
// The switch turns on deletion of images on a remote media server, so this
// proves in a real browser what the jsdom tier only models: turning it on asks
// first, Cancel stores nothing, accepting stores the option WITHOUT rewriting
// the rule's tolerance, and a refused tolerance leaves the switch locked off.
//
// FIXTURE: the harness database is empty, but migrations seed the rules. Each
// test PUTs the rule to a known config through the API first, and afterAll
// restores the config the server started with (the harness shares one server
// across spec files). No library or media server is needed: nothing runs a fix.

import { test, expect } from 'playwright/test';

import { apiFetch, BASE_URL } from './helpers/api.js';
import { disableTransitions } from './helpers/settle.js';
import { buildAxeBuilder, formatViolations, applyTheme } from './helpers/axe.js';

const RULE_ID = 'image_duplicate';
const RULE_URL = `/api/v1/rules/${RULE_ID}`;
const PANEL = `#rule-cfg-${RULE_ID}`;
const DIALOG = '#confirm-modal';
// NOT the 0.90 default: only a non-default value can show a dropped tolerance.
const TOLERANCE = 0.95;

let seededConfig;

async function storedRule(request) {
  const resp = await request.fetch(`${BASE_URL}/api/v1/rules`);
  expect(resp.ok(), `listing rules failed: ${resp.status()}`).toBe(true);
  const found = (await resp.json()).rules.find((r) => r.id === RULE_ID);
  // Fail loudly, never skip: without the row there is nothing to test.
  expect(found, `rule ${RULE_ID} is missing from GET /api/v1/rules; migrations should seed it`).toBeTruthy();
  return found;
}

async function putConfig(request, config) {
  const resp = await apiFetch(request, 'PUT', RULE_URL, { config });
  expect(resp.ok(), `PUT ${RULE_URL} failed: ${resp.status()} ${await resp.text()}`).toBe(true);
}

async function openPanel(page) {
  await page.goto('/settings');
  const configure = page.locator(`[aria-controls="rule-cfg-${RULE_ID}"]`);
  await expect(configure, 'the rule has no Configure button').toHaveCount(1);
  await configure.scrollIntoViewIfNeeded();
  await configure.click();
  const sw = page.locator(`${PANEL} [data-prune-platform-copies]`);
  await expect(sw, 'the Configure panel has no prune switch').toHaveCount(1);
  await expect(sw).toBeVisible();
  return sw;
}

async function scan(page, selector, label) {
  for (const theme of ['dark', 'light']) {
    await applyTheme(expect, page, theme);
    const results = await buildAxeBuilder(page).include(selector).analyze();
    expect(results.violations, `${theme} violations, ${label}:\n${formatViolations(results.violations)}`).toHaveLength(0);
  }
  await applyTheme(expect, page, 'dark');
}

// The settings skin paints the on and off track the SAME neutral color; what
// tells the states apart is the knob's position and the halo on the track.
const rendered = (sw) => sw.evaluate((el) => ({
  knobX: Math.round(el.querySelector('span').getBoundingClientRect().left - el.getBoundingClientRect().left),
  halo: getComputedStyle(el).boxShadow,
}));

test.beforeAll(async ({ request }) => {
  seededConfig = (await storedRule(request)).config;
});

test.afterAll(async ({ request }) => {
  if (!seededConfig) return;
  await putConfig(request, seededConfig);
  expect((await storedRule(request)).config, 'the seeded rule config was not restored').toEqual(seededConfig);
});

test.beforeEach(async ({ page }) => {
  await disableTransitions(page);
});

test('turning the option on asks first; cancel stores nothing, accept stores it and keeps the tolerance', async ({ page, request }) => {
  await putConfig(request, { severity: 'warning', tolerance: TOLERANCE });
  const before = await storedRule(request);
  expect(before.config.prune_platform_copies, 'precondition: the option is stored off').toBeFalsy();
  expect(before.config.tolerance, 'precondition: the tolerance is stored').toBe(TOLERANCE);

  const puts = [];
  page.on('request', (r) => { if (r.method() === 'PUT' && r.url().endsWith(RULE_URL)) puts.push(r); });

  const sw = await openPanel(page);
  await expect(sw).toHaveAttribute('aria-checked', 'false');
  await expect(sw).toHaveAccessibleName('Also delete near-duplicate backdrops on media servers');
  await expect(page.locator(`${PANEL} [data-prune-threshold]`)).toHaveText('Similarity threshold in use: 95%');
  const off = await rendered(sw);
  await scan(page, PANEL, 'panel open, switch off');

  await sw.click();
  await expect(sw).toHaveAttribute('aria-checked', 'true');
  const on = await rendered(sw);
  expect(on.knobX, 'the knob must move when the switch turns on').toBeGreaterThan(off.knobX + 10);
  expect(off.halo, 'the off track carries no halo').toBe('none');
  expect(on.halo, 'the on track carries the halo').not.toBe('none');
  expect(puts, 'a click on the switch must not save').toHaveLength(0);

  const save = page.locator(`${PANEL} button[type="submit"]`);
  await save.click();
  const dialog = page.locator(DIALOG);
  await expect(dialog).toBeVisible();
  await expect(dialog.locator('#confirm-modal-title')).toHaveText('Allow deletions on your media servers?');
  await expect(dialog.locator('#confirm-modal-message')).toContainText('These deletions cannot be undone from Stillwater.');
  await expect(dialog.locator('#confirm-modal-accept')).toHaveText('Turn on');
  await expect(dialog.locator('#confirm-modal-remember-wrapper')).toBeHidden();
  await scan(page, DIALOG, 'confirm dialog open');

  // Cancel: nothing stored, panel still open, switch as the user left it.
  await dialog.locator('#confirm-modal-cancel').click();
  await expect(dialog).toBeHidden();
  expect(puts, 'Cancel must send no request').toHaveLength(0);
  expect((await storedRule(request)).config.prune_platform_copies, 'Cancel must leave the option off').toBeFalsy();
  await expect(page.locator(PANEL)).toBeVisible();
  await expect(sw).toHaveAttribute('aria-checked', 'true');

  // Save again and accept: stored on, tolerance untouched.
  await save.click();
  await expect(dialog).toBeVisible();
  const saved = page.waitForResponse((r) => r.request().method() === 'PUT' && r.url().endsWith(RULE_URL));
  await dialog.locator('#confirm-modal-accept').click();
  expect((await saved).status()).toBe(200);
  await expect(page.locator(PANEL)).toBeHidden();
  const after = await storedRule(request);
  expect(after.config.prune_platform_copies, 'accepting must store the option on').toBe(true);
  expect(after.config.tolerance, 'the save must not rewrite the tolerance').toBe(TOLERANCE);

  const reloaded = await openPanel(page);
  await expect(reloaded).toHaveAttribute('aria-checked', 'true');
  await expect(reloaded).toHaveAttribute('data-initial', 'true');
  await scan(page, PANEL, 'panel open, switch on');
});

test('a tolerance the server cleanup refuses locks the switch off and says why', async ({ page, request }) => {
  await putConfig(request, { severity: 'warning', tolerance: 0.8 });
  expect((await storedRule(request)).config.tolerance, 'precondition: the refused tolerance is stored').toBe(0.8);

  const sw = await openPanel(page);
  await expect(page.locator(`${PANEL} [data-prune-refused]`)).toContainText('similarity threshold is 80%');
  await expect(sw).toHaveAttribute('aria-disabled', 'true');
  await expect(sw).toHaveCSS('cursor', 'not-allowed');
  await expect(sw).toHaveAccessibleDescription(/The server cleanup will not run/);
  // force: the click must reach the handler and be refused BY the handler.
  await sw.click({ force: true });
  await expect(sw).toHaveAttribute('aria-checked', 'false');
  await scan(page, PANEL, 'panel open, tolerance refused');
});
