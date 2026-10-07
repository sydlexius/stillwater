// registry-repair-run.spec.js - the RUN flow behind the image-registry repair
// banner (#2678 slice 2). Runs on both projects (firefox-a11y, chromium-a11y).
//
// FIXTURE: helpers/seed-registry-repair.js (own server, real scan, image written
// after it, needs_repair/count asserted on the endpoint before any page is trusted).
//
// ORDER MATTERS: the fixture can be repaired exactly once. Every test that must
// not change it (stubbed failure / 409 / running / axe) runs first against
// page.route stubs; the one REAL end-to-end run (real job, real status, real
// toast, banner hides because the real cache went clean) runs last, serially.
// A stub never replaces the real run: it only shapes responses the real server
// cannot produce on demand (a failed job, an in-flight 409).

import { test, expect } from 'playwright/test';
import path from 'node:path';
import fs from 'node:fs';

import { disableTransitions } from './helpers/settle.js';
import { buildAxeBuilder, formatViolations, applyTheme } from './helpers/axe.js';
import { startRegistryRepairFixture, planDialogPattern, BANNER_API } from './helpers/seed-registry-repair.js';

test.describe.configure({ mode: 'serial' });

const BASE_PATH = '/sw-repair-run-test';
const BANNER = '#sw-registry-repair-banner';
const RUN = '#sw-registry-repair-run';
const TOASTS = '#error-toast-container';
const REMEDIATE_API = '/api/v1/reports/registry-repair/remediate';
const STATUS_API = '/api/v1/reports/registry-repair/status';
const SHOTS = process.env.SW_SHOTS_DIR || '';
const GENERIC_FAILURE = 'Image registry repair failed. See the server log for details.';

let server;
let fx;
let stopServer;

test.beforeAll(async () => {
  test.setTimeout(120_000);
  ({ server, fx, stop: stopServer } = await startRegistryRepairFixture(BASE_PATH, 'Registry Repair Run Fixture'));
});

test.afterAll(() => {
  if (stopServer) stopServer();
});

// openBanner opens /reports as the admin. `stub` (optional) is a banner answer
// served in place of the real endpoint, registered BEFORE the page loads;
// `locale` (optional) is the Accept-Language the browser sends.
async function openBanner(browser, theme = 'dark', stub = undefined, locale = undefined) {
  const context = await browser.newContext({ colorScheme: theme, locale });
  await context.addCookies([{ name: 'session', value: server.sessionCookie, url: server.rootURL }]);
  const page = await context.newPage();
  await disableTransitions(page);
  const errors = [];
  page.on('console', (m) => { if (m.type() === 'error' && m.text().includes('registry repair banner')) errors.push(m.text()); });
  if (stub) await page.route(`**${BANNER_API}`, (route) => route.fulfill({ json: stub }));
  await page.goto(`${server.baseURL}/reports`);
  await expect(page.locator(BANNER)).toBeVisible();
  return { context, page, errors };
}

async function shot(page, name, theme) {
  if (!SHOTS) return;
  await page.screenshot({ path: path.join(SHOTS, `${name}-${theme}.png`) });
}

async function confirmRun(page) {
  await page.locator(RUN).click();
  await expect(page.locator('#confirm-modal-message')).toContainText('Repair the image registry now?');
  await page.locator('#confirm-modal-accept').click();
}

const DONE_REPORT = { running: false, status: 'completed', report: { rebuilt: 3, restored: 2, write_failures: 0 } };

test('idle: one run button, labelled, 24px target, confirm dialog can be cancelled', async ({ browser }) => {
  const { context, page, errors } = await openBanner(browser);
  await expect(page.locator(RUN)).toHaveCount(1);
  await expect(page.locator(RUN)).toHaveText('Repair now');
  const box = await page.locator(RUN).boundingBox();
  expect(box.height, `run height ${box.height}`).toBeGreaterThanOrEqual(24);
  await shot(page, 'idle', 'dark');
  let posts = 0;
  await page.route(`**${REMEDIATE_API}`, (route) => { posts += 1; return route.abort(); });
  await page.locator(RUN).click();
  await expect(page.locator('#confirm-modal-message')).toContainText('no image files are changed');
  // Language tags (#2678): the root is never tagged (its message is caller text);
  // the template-translated chrome carries the render locale; the banner passes its own lang.
  await expect(page.locator('#confirm-modal')).toBeVisible();
  await expect(page.locator('#confirm-modal')).not.toHaveAttribute('lang', /.*/);
  for (const id of ['title', 'cancel', 'accept', 'message']) {
    await expect(page.locator(`#confirm-modal-${id}`)).toHaveAttribute('lang', 'en');
  }
  // The dialog is open and nothing was accepted: no POST may have been sent.
  await page.waitForTimeout(300);
  expect(posts, 'no POST may be sent while the confirm dialog is open').toBe(0);
  await page.locator('#confirm-modal-cancel').click();
  await page.waitForTimeout(300);
  // hideModal ran to the end: chrome lang restored and focus back on the opener.
  await expect(page.locator('#confirm-modal')).toBeHidden();
  await expect(page.locator('#confirm-modal-title')).toHaveAttribute('lang', 'en');
  await expect(page.locator('#confirm-modal-accept')).toHaveAttribute('lang', 'en');
  await expect(page.locator(RUN)).toBeFocused();
  expect(posts, 'cancelling the confirmation must not start a repair').toBe(0);
  await expect(page.locator(RUN)).toBeEnabled();
  expect(errors).toEqual([]);
  await context.close();
});

test('running: button disabled and aria-busy until the job completes, then success toast', async ({ browser }) => {
  const { context, page, errors } = await openBanner(browser);
  let release;
  const gate = new Promise((r) => { release = r; });
  await page.route(`**${REMEDIATE_API}`, (route) => route.fulfill({ status: 202, json: { running: true, status: 'running' } }));
  await page.route(`**${STATUS_API}`, async (route) => {
    await gate;
    await route.fulfill({ json: DONE_REPORT });
  });
  await page.route(`**${BANNER_API}`, (route) => route.fulfill({ json: { ok: true, needs_repair: false, count: 0 } }));
  // Arm the request wait BEFORE the click: the POST is sent asynchronously after accept,
  // so reading a handler-filled variable right after the click raced the request.
  const postSent = page.waitForRequest((r) => r.url().endsWith(REMEDIATE_API) && r.method() === 'POST');
  await confirmRun(page);
  expect((await postSent).postDataJSON(), 'the POST body must be an explicit commit').toEqual({ commit: true });
  await expect(page.locator(RUN)).toBeDisabled();
  await expect(page.locator(RUN)).toHaveAttribute('aria-busy', 'true');
  await expect(page.locator(RUN)).toHaveText('Repairing...');
  await expect(page.locator(TOASTS)).toContainText('Image registry repair started');
  // The banner's toast message and the shared dismiss button are language-tagged.
  const toastEl = page.locator(`${TOASTS} > div`, { hasText: 'Image registry repair started' });
  await expect(toastEl).toHaveCount(1);
  await expect(toastEl.locator('span').first()).toHaveAttribute('lang', 'en');
  await expect(toastEl.locator('button')).toHaveAttribute('lang', 'en');
  await shot(page, 'running', 'dark');
  release();
  await expect(page.locator(TOASTS)).toContainText('Image registry repaired: 3 rebuilt, 2 restored.');
  await shot(page, 'success', 'dark');
  // The banner endpoint now says clean: the banner hides itself.
  await expect(page.locator(BANNER)).toBeHidden();
  // Focus must not be stranded in the hidden banner (the modal returned it to the run button).
  expect(await page.evaluate(() => document.activeElement && document.activeElement.id), 'focus after the banner hides').toBe('sw-main');
  expect(errors).toEqual([]);
  await context.close();
});

test('a null status body ends the run with the generic failure and re-enables the button', async ({ browser }) => {
  const { context, page, errors } = await openBanner(browser);
  await page.route(`**${REMEDIATE_API}`, (route) => route.fulfill({ status: 202, json: { running: true, status: 'running' } }));
  await page.route(`**${STATUS_API}`, (route) => route.fulfill({ status: 200, contentType: 'application/json', body: 'null' }));
  await confirmRun(page);
  await expect(page.locator(TOASTS)).toContainText(GENERIC_FAILURE);
  await expect(page.locator(RUN)).toBeEnabled();
  await expect(page.locator(RUN)).toHaveText('Repair now');
  expect(errors.some((e) => e.includes('registry repair banner: status HTTP 200'))).toBe(true);
  await context.close();
});

test('incomplete: write failures give a warning toast and the banner stays', async ({ browser }) => {
  const { context, page } = await openBanner(browser);
  await page.route(`**${REMEDIATE_API}`, (route) => route.fulfill({ status: 202, json: { running: true, status: 'running' } }));
  await page.route(`**${STATUS_API}`, (route) => route.fulfill({ json: { status: 'completed', report: { rebuilt: 1, restored: 0, write_failures: 4 } } }));
  await confirmRun(page);
  await expect(page.locator(TOASTS)).toContainText('4 changes could not be saved');
  // The warning helper forwards the banner's language like the other toast helpers (#2678).
  const warnToast = page.locator(`${TOASTS} > div`, { hasText: '4 changes could not be saved' });
  await expect(warnToast).toHaveCount(1);
  await expect(warnToast.locator('span').first()).toHaveAttribute('lang', 'en');
  await expect(warnToast.locator('button')).toHaveAttribute('lang', 'en');
  await expect(page.locator(BANNER)).toBeVisible();
  await expect(page.locator(RUN)).toBeEnabled();
  await context.close();
});

test('incomplete: a single write failure uses the singular wording', async ({ browser }) => {
  const { context, page } = await openBanner(browser);
  await page.route(`**${REMEDIATE_API}`, (route) => route.fulfill({ status: 202, json: { running: true, status: 'running' } }));
  await page.route(`**${STATUS_API}`, (route) => route.fulfill({ json: { status: 'completed', report: { rebuilt: 1, restored: 0, write_failures: 1 } } }));
  await confirmRun(page);
  await expect(page.locator(TOASTS)).toContainText('1 change could not be saved');
  await expect(page.locator(TOASTS)).not.toContainText('1 changes');
  await context.close();
});

test('last-checked time keeps the regional format when navigator.languages is empty (navigator.language fallback)', async ({ browser }) => {
  const context = await browser.newContext({ timezoneId: 'UTC' });
  await context.addCookies([{ name: 'session', value: server.sessionCookie, url: server.rootURL }]);
  await context.addInitScript(() => {
    Object.defineProperty(navigator, 'languages', { get: () => [] });
    Object.defineProperty(navigator, 'language', { get: () => 'en-GB' });
  });
  const page = await context.newPage();
  await page.route(`**${BANNER_API}`, (route) => route.fulfill({
    json: { ok: true, needs_repair: true, count: 2, checked_at: '2026-03-25T15:04:05Z' },
  }));
  await page.goto(`${server.baseURL}/reports`);
  // en-GB is day-first and 24-hour; en-US would be 3/25/2026, 3:04:05 PM.
  await expect(page.locator('#sw-registry-repair-checked')).toContainText('25/03/2026, 15:04:05');
  await context.close();
});

for (const theme of ['dark', 'light']) {
  test(`failure: fixed generic message, no server detail leaks, button recovers (${theme})`, async ({ browser }) => {
    const { context, page, errors } = await openBanner(browser, theme);
    await page.route(`**${REMEDIATE_API}`, (route) => route.fulfill({ status: 202, json: { running: true, status: 'running' } }));
    await page.route(`**${STATUS_API}`, (route) => route.fulfill({
      json: { running: false, status: 'failed', error: 'SECRET-INTERNAL-DETAIL /srv/music', error_code: 'timeout' },
    }));
    await confirmRun(page);
    await expect(page.locator(TOASTS)).toContainText(GENERIC_FAILURE);
    expect(await page.locator('body').innerText()).not.toContain('SECRET-INTERNAL-DETAIL');
    await expect(page.locator(RUN)).toBeEnabled();
    await expect(page.locator(RUN)).toHaveText('Repair now');
    await shot(page, 'failure', theme);
    // A failed run is a loud console error, not a silent guard.
    expect(errors.some((e) => e.includes('registry repair banner: run ended with status failed'))).toBe(true);
    await context.close();
  });
}

test('start refused (HTTP 500): generic failure toast and a console.error', async ({ browser }) => {
  const { context, page, errors } = await openBanner(browser);
  await page.route(`**${REMEDIATE_API}`, (route) => route.fulfill({ status: 500, json: { error: 'SECRET-INTERNAL-DETAIL' } }));
  await confirmRun(page);
  await expect(page.locator(TOASTS)).toContainText(GENERIC_FAILURE);
  expect(await page.locator('body').innerText()).not.toContain('SECRET-INTERNAL-DETAIL');
  await expect(page.locator(RUN)).toBeEnabled();
  expect(errors.some((e) => e.includes('registry repair banner: start refused, HTTP 500'))).toBe(true);
  await context.close();
});

test('showConfirmDialog unavailable: console.error and no POST', async ({ browser }) => {
  const { context, page, errors } = await openBanner(browser);
  let posts = 0;
  await page.route(`**${REMEDIATE_API}`, (route) => { posts += 1; return route.abort(); });
  await page.evaluate(() => { delete window.showConfirmDialog; });
  await page.locator(RUN).click();
  await expect.poll(() => errors.some((e) => e.includes('registry repair banner: showConfirmDialog unavailable'))).toBe(true);
  await page.waitForTimeout(300);
  expect(posts, 'no POST without a confirmation').toBe(0);
  await context.close();
});

const CONFLICT = { status: 409, json: { status: 'running', message: 'an image registry repair is already in progress' } };

test('already running (409, committed repair): says so and follows it to its result', async ({ browser }) => {
  const { context, page } = await openBanner(browser);
  let polls = 0;
  await page.route(`**${REMEDIATE_API}`, (route) => route.fulfill(CONFLICT));
  // First read classifies the conflict (a committed run in flight); later reads are the poll.
  await page.route(`**${STATUS_API}`, (route) => route.fulfill({
    json: ++polls === 1 ? { running: true, status: 'running', commit: true, dry_run: false } : DONE_REPORT,
  }));
  await confirmRun(page);
  await expect(page.locator(TOASTS)).toContainText('An image registry repair is already running.');
  await shot(page, 'already-running', 'dark');
  await expect(page.locator(TOASTS)).toContainText('Image registry repaired: 3 rebuilt, 2 restored.');
  await expect(page.locator(RUN)).toBeEnabled();
  await context.close();
});

test('409 while a PREVIEW runs: says so, never reports success, button recovers', async ({ browser }) => {
  const { context, page } = await openBanner(browser);
  await page.route(`**${REMEDIATE_API}`, (route) => route.fulfill(CONFLICT));
  await page.route(`**${STATUS_API}`, (route) => route.fulfill({ json: { running: true, status: 'running', commit: false, dry_run: true } }));
  await confirmRun(page);
  await expect(page.locator(TOASTS)).toContainText('An image registry repair is already running.');
  await expect(page.locator(RUN)).toBeEnabled();
  await expect(page.locator(RUN)).toHaveText('Repair now');
  await page.waitForTimeout(500);
  await expect(page.locator(TOASTS)).not.toContainText('Image registry repaired');
  await context.close();
});

test('409 from detector contention: retries the POST, then runs the repair', async ({ browser }) => {
  const { context, page } = await openBanner(browser);
  let posts = 0;
  await page.route(`**${REMEDIATE_API}`, (route) => (++posts < 3
    ? route.fulfill(CONFLICT)
    : route.fulfill({ status: 202, json: { running: true, status: 'running' } })));
  // Nothing is running while the detector holds the claim: an idle status.
  let polls = 0;
  await page.route(`**${STATUS_API}`, (route) => route.fulfill({ json: posts < 3 ? { status: 'idle', dry_run: true } : (++polls, DONE_REPORT) }));
  await confirmRun(page);
  await expect(page.locator(TOASTS)).toContainText('Image registry repaired: 3 rebuilt, 2 restored.', { timeout: 15_000 });
  expect(posts, 'the POST must have been retried past the conflicts').toBe(3);
  await context.close();
});

test('409 that never clears (detector contention): bounded retries, then the generic failure and a console.error', async ({ browser }) => {
  const { context, page, errors } = await openBanner(browser);
  let posts = 0;
  await page.route(`**${REMEDIATE_API}`, (route) => { posts += 1; return route.fulfill(CONFLICT); });
  await page.route(`**${STATUS_API}`, (route) => route.fulfill({ json: { status: 'idle', dry_run: true } }));
  await confirmRun(page);
  await expect(page.locator(TOASTS)).toContainText(GENERIC_FAILURE, { timeout: 15_000 });
  expect(posts, 'retries are bounded (1 + 5)').toBe(6);
  await expect(page.locator(RUN)).toBeEnabled();
  expect(errors.some((e) => e.includes('still refused (409)'))).toBe(true);
  await context.close();
});

// ---- Cached plan in the confirm dialog (#2678 slice 4) ----
// The dialog shows what the detector already knows; nothing is fetched on click.
// Expected text: the template from the locale JSON, the numbers from the endpoint
// read independently of the page, the {age} alternatives from Node's own Intl.
const loadLocale = (code) => JSON.parse(fs.readFileSync(new URL(`../../internal/i18n/locales/${code}.json`, import.meta.url), 'utf8'));
const MSG = '#confirm-modal-message';
const REGISTRY_API_RE = /\/api\/v1\/reports\/registry-repair\//;
const FRESH_AGES = [0, 1, 2, 3].map((n) => [n, 'minute']);

test('dialog shows the endpoint\'s cached plan and sends no request on click', async ({ browser }) => {
  const live = await (await fx('GET', BANNER_API)).json();
  expect(live.plan, 'precondition: the endpoint reports a plan').toBeTruthy();
  expect(live.plan.rebuild).toBeGreaterThanOrEqual(1);
  expect(live.plan.rebuild + live.plan.restore).toBe(live.count);
  const { context, page, errors } = await openBanner(browser);
  const seen = [];
  page.on('request', (r) => { if (REGISTRY_API_RE.test(r.url())) seen.push(`${r.method()} ${r.url()}`); });
  await page.locator(RUN).click();
  await expect(page.locator(MSG)).toHaveText(
    planDialogPattern(loadLocale('en')['banner.registry_repair.confirm_plan'], live.plan, 'en-US', FRESH_AGES),
  );
  await expect(page.locator(MSG)).toHaveAttribute('lang', 'en');
  await page.waitForTimeout(500);
  expect(seen, 'opening the dialog must not scan, preview or re-read anything').toEqual([]);
  expect(errors).toEqual([]);
  await context.close();
});

test('accepting sends exactly one commit POST; nothing is sent before', async ({ browser }) => {
  const { context, page } = await openBanner(browser);
  const posts = [];
  page.on('request', (r) => { if (r.url().endsWith(REMEDIATE_API) && r.method() === 'POST') posts.push(r.postDataJSON()); });
  await page.route(`**${REMEDIATE_API}`, (route) => route.fulfill({ status: 202, json: { running: true, status: 'running' } }));
  await page.route(`**${STATUS_API}`, (route) => route.fulfill({ json: DONE_REPORT }));
  await page.route(`**${BANNER_API}`, (route) => route.fulfill({ json: { ok: true, needs_repair: false, count: 0 } }));
  await page.locator(RUN).click();
  await expect(page.locator('#confirm-modal')).toBeVisible();
  await page.waitForTimeout(300);
  expect(posts, 'no POST while the dialog is open').toEqual([]);
  await page.locator('#confirm-modal-accept').click();
  await expect.poll(() => posts.length).toBe(1);
  await page.waitForTimeout(500);
  expect(posts, 'exactly one POST, explicitly a commit').toEqual([{ commit: true }]);
  await context.close();
});

test('cancel and Escape return focus to the run button and send nothing', async ({ browser }) => {
  const { context, page } = await openBanner(browser);
  let posts = 0;
  await page.route(`**${REMEDIATE_API}`, (route) => { posts += 1; return route.abort(); });
  for (const close of ['cancel', 'escape']) {
    await page.locator(RUN).click();
    await expect(page.locator('#confirm-modal')).toBeVisible();
    if (close === 'cancel') await page.locator('#confirm-modal-cancel').click();
    else await page.keyboard.press('Escape');
    await expect(page.locator('#confirm-modal')).toBeHidden();
    await expect(page.locator(RUN), `focus after ${close}`).toBeFocused();
    await expect(page.locator(RUN)).toBeEnabled();
  }
  expect(posts).toBe(0);
  await context.close();
});

test('no plan in the banner answer: the dialog falls back to the generic text', async ({ browser }) => {
  const en = loadLocale('en');
  const { context, page, errors } = await openBanner(browser, 'dark', { ok: true, needs_repair: true, count: 2, checked_at: new Date().toISOString() });
  await page.locator(RUN).click();
  await expect(page.locator(MSG)).toHaveText(en['banner.registry_repair.confirm']);
  expect(await page.locator(MSG).innerText()).not.toMatch(/undefined|NaN|\{/);
  expect(errors).toEqual([]);
  await context.close();
});

// The age is computed at CLICK time from the answer's checked_at. The page clock
// is pinned (Date only, timers keep running) and moved between cases, so one
// loaded page proves every bucket; the strings come from Node's Intl per locale.
const AGE_BASE = Date.parse('2026-03-04T12:00:00Z');
const AGE_CASES = [
  ['10 minutes', 10 * 60_000, [10, 'minute']],
  ['90 minutes flips to hours', 90 * 60_000, [1, 'hour']],
  ['3 hours', 3 * 3600_000, [3, 'hour']],
  ['3 days', 3 * 86_400_000, [3, 'day']],
  ['clock skew (checked in the future) shows no negative age', -5 * 60_000, [0, 'minute']],
];
for (const [code, region] of [['en', 'en-US'], ['fr', 'fr-FR'], ['ja', 'ja-JP']]) {
  test(`dialog age is relative, in the page locale, and computed at click time (${code})`, async ({ browser }) => {
    const L = loadLocale(code);
    const plan = { rebuild: 3, restore: 2 };
    const { context, page, errors } = await openBanner(browser, 'dark', {
      ok: true, needs_repair: true, count: 5, checked_at: new Date(AGE_BASE).toISOString(), plan,
    }, region);
    for (const [name, deltaMs, age] of AGE_CASES) {
      await page.clock.setFixedTime(AGE_BASE + deltaMs);
      await page.locator(RUN).click();
      await expect(page.locator(MSG), name).toHaveText(
        planDialogPattern(L['banner.registry_repair.confirm_plan'], plan, region, [age]),
      );
      await page.locator('#confirm-modal-cancel').click();
      await expect(page.locator('#confirm-modal')).toBeHidden();
    }
    expect(errors).toEqual([]);
    await context.close();
  });
}

// One test, both themes, soft assertions: serial mode would skip the second theme after a red.
test('open confirm dialog with the cached plan passes axe (dark and light)', async ({ browser }) => {
  for (const theme of ['dark', 'light']) {
    const { context, page } = await openBanner(browser, theme, {
      ok: true, needs_repair: true, count: 5, checked_at: new Date().toISOString(), plan: { rebuild: 3, restore: 2 },
    });
    try {
      await applyTheme(expect, page, theme);
      await page.locator(RUN).click();
      await expect(page.locator(MSG)).toContainText('rows to rebuild: 3');
      const results = await buildAxeBuilder(page).analyze();
      expect.soft(results.violations, `${theme} dialog violations:\n${formatViolations(results.violations)}`).toHaveLength(0);
    } finally {
      await context.close();
    }
  }
});

// A plan field that is not a finite number must never render ("null", "undefined"):
// the dialog takes the generic text and logs loudly.
test('a malformed plan falls back to the generic text with a console.error', async ({ browser }) => {
  const en = loadLocale('en');
  // Both fields are checked: null and a numeric string each, on either side.
  for (const plan of [{ rebuild: null, restore: 2 }, { rebuild: 3, restore: null }, { rebuild: '3', restore: 2 }, { rebuild: 3, restore: 'x' }]) {
    const { context, page, errors } = await openBanner(browser, 'dark', {
      ok: true, needs_repair: true, count: 5, checked_at: new Date().toISOString(), plan,
    });
    await page.locator(RUN).click();
    await expect(page.locator(MSG), JSON.stringify(plan)).toHaveText(en['banner.registry_repair.confirm']);
    expect(errors.some((e) => e.includes('plan is not a pair of finite numbers')), JSON.stringify(plan)).toBe(true);
    await context.close();
  }
});

test('logged out: the remediate endpoint refuses an unauthenticated POST', async () => {
  const resp = await fetch(`${server.baseURL}${REMEDIATE_API}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ commit: true }),
  });
  expect([401, 403]).toContain(resp.status);
});

test('CSRF: a session POST without the CSRF token is refused', async () => {
  const resp = await fetch(`${server.baseURL}${REMEDIATE_API}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Cookie: `session=${server.sessionCookie}; csrf_token=${server.csrfToken}` },
    body: JSON.stringify({ commit: true }),
  });
  expect(resp.status).toBe(403);
});

for (const theme of ['dark', 'light']) {
  test(`banner with run button and a success toast pass axe (${theme})`, async ({ browser }) => {
    const { context, page } = await openBanner(browser, theme);
    await page.route(`**${REMEDIATE_API}`, (route) => route.fulfill({ status: 202, json: { running: true, status: 'running' } }));
    await page.route(`**${STATUS_API}`, (route) => route.fulfill({ json: DONE_REPORT }));
    await page.route(`**${BANNER_API}`, (route) => route.fulfill({ json: { ok: true, needs_repair: true, count: 5, checked_at: new Date().toISOString() } }));
    await applyTheme(expect, page, theme);
    await shot(page, 'idle-axe', theme);
    let results = await buildAxeBuilder(page).analyze();
    expect(results.violations, `${theme} idle violations:\n${formatViolations(results.violations)}`).toHaveLength(0);
    await confirmRun(page);
    await expect(page.locator(TOASTS)).toContainText('Image registry repaired');
    await expect(page.locator(RUN)).toBeEnabled();
    await shot(page, 'success-axe', theme);
    results = await buildAxeBuilder(page).analyze();
    expect(results.violations, `${theme} toast violations:\n${formatViolations(results.violations)}`).toHaveLength(0);
    await context.close();
  });
}

// LAST: the one real run. Real POST, real job, real status, real cache update.
test('real run: repair completes, success toast, banner hides, endpoint reports clean', async ({ browser }) => {
  const { context, page, errors } = await openBanner(browser);
  await confirmRun(page);
  await expect(page.locator(TOASTS)).toContainText(/Image registry repaired: [1-9]\d* rebuilt, \d+ restored\./, { timeout: 60_000 });
  await expect(page.locator(BANNER)).toBeHidden();
  const after = await (await fx('GET', BANNER_API)).json();
  expect(after.needs_repair, 'the real cache must report clean after a committed repair').toBe(false);
  expect(errors).toEqual([]);
  await context.close();
});
