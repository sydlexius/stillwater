// registry-repair-run.spec.js - the RUN flow behind the image-registry repair
// banner (#2678 slice 2). Runs on both projects (firefox-a11y, chromium-a11y).
//
// FIXTURE. Same shape as registry-repair-banner.spec.js: the spec boots its OWN
// throwaway server (helpers/base-path-server.js) with the detector test seam
// SW_REGISTRY_REPAIR_CHECK_EVERY=2s, makes a real artist by a real scan, then
// writes an image file AFTER the scan so the folder holds a file with NO
// registry row. Before any page is trusted the fixture's defining property is
// asserted on the banner endpoint itself (needs_repair true, count > 0).
//
// ORDER MATTERS: the fixture can be repaired exactly once. Every test that must
// not change it (stubbed failure / 409 / running / axe) runs first against
// page.route stubs; the one REAL end-to-end run (real job, real status, real
// toast, banner hides because the real cache went clean) runs last, serially.
// A stub never replaces the real run: it only shapes responses the real server
// cannot produce on demand (a failed job, an in-flight 409).

import { test, expect } from 'playwright/test';
import fs from 'node:fs';
import path from 'node:path';

import { disableTransitions } from './helpers/settle.js';
import { buildAxeBuilder, formatViolations, applyTheme } from './helpers/axe.js';
import { startBasePathServer } from './helpers/base-path-server.js';

test.describe.configure({ mode: 'serial' });

const BASE_PATH = '/sw-repair-run-test';
const BANNER = '#sw-registry-repair-banner';
const RUN = '#sw-registry-repair-run';
const TOASTS = '#error-toast-container';
const BANNER_API = '/api/v1/reports/registry-repair/banner';
const REMEDIATE_API = '/api/v1/reports/registry-repair/remediate';
const STATUS_API = '/api/v1/reports/registry-repair/status';
const SHOTS = process.env.SW_SHOTS_DIR || '';
// A valid 1x1 PNG: the repair decodes every candidate, so it must be real.
const PNG_1X1 = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGP4z8DwHwAFAAH/iZk9HQAAAABJRU5ErkJggg==',
  'base64',
);
const GENERIC_FAILURE = 'Image registry repair failed. See the server log for details.';

let server;
let tmpDir;

async function fx(method, urlPath, body, headers = {}) {
  return fetch(`${server.baseURL}${urlPath}`, {
    method,
    headers: {
      'Content-Type': 'application/json',
      'X-CSRF-Token': server.csrfToken,
      Cookie: `session=${server.sessionCookie}; csrf_token=${server.csrfToken}`,
      ...headers,
    },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
}

async function seedFixture() {
  const lib = path.join(tmpDir, 'empty-library');
  const artistDir = path.join(lib, 'Registry Repair Run Fixture');
  fs.mkdirSync(artistDir, { recursive: true });
  expect((await fx('POST', '/api/v1/libraries', { name: 'repair run fixture', path: lib, type: 'regular' })).ok).toBe(true);
  expect((await fx('POST', '/api/v1/scanner/run')).ok).toBe(true);
  await expect.poll(async () => (await (await fx('GET', '/api/v1/scanner/status')).json()).status,
    { timeout: 30_000 }).toMatch(/completed|idle/);
  fs.writeFileSync(path.join(artistDir, 'folder.png'), PNG_1X1);
  await expect.poll(async () => (await (await fx('GET', BANNER_API)).json()).needs_repair,
    { message: 'detector never reported needs_repair=true for the fixture', timeout: 30_000 }).toBe(true);
  const body = await (await fx('GET', BANNER_API)).json();
  expect(body.ok).toBe(true);
  expect(body.count).toBeGreaterThan(0);
}

test.beforeAll(async () => {
  test.setTimeout(120_000);
  server = await startBasePathServer(BASE_PATH, {
    env: { SW_REGISTRY_REPAIR_CHECK_EVERY: '2s' },
    seed: (dir) => { tmpDir = dir; },
  });
  await seedFixture();
});

test.afterAll(() => {
  if (server) server.stop();
});

async function openBanner(browser, theme = 'dark') {
  const context = await browser.newContext({ colorScheme: theme });
  await context.addCookies([{ name: 'session', value: server.sessionCookie, url: server.rootURL }]);
  const page = await context.newPage();
  await disableTransitions(page);
  const errors = [];
  page.on('console', (m) => { if (m.type() === 'error' && m.text().includes('registry repair banner')) errors.push(m.text()); });
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
  // The dialog is open and nothing was accepted: no POST may have been sent.
  await page.waitForTimeout(300);
  expect(posts, 'no POST may be sent while the confirm dialog is open').toBe(0);
  await page.locator('#confirm-modal-cancel').click();
  await page.waitForTimeout(300);
  expect(posts, 'cancelling the confirmation must not start a repair').toBe(0);
  await expect(page.locator(RUN)).toBeEnabled();
  expect(errors).toEqual([]);
  await context.close();
});

test('running: button disabled and aria-busy until the job completes, then success toast', async ({ browser }) => {
  const { context, page, errors } = await openBanner(browser);
  let release;
  const gate = new Promise((r) => { release = r; });
  const body = {};
  await page.route(`**${REMEDIATE_API}`, (route) => {
    Object.assign(body, route.request().postDataJSON());
    return route.fulfill({ status: 202, json: { running: true, status: 'running' } });
  });
  await page.route(`**${STATUS_API}`, async (route) => {
    await gate;
    await route.fulfill({ json: DONE_REPORT });
  });
  await page.route(`**${BANNER_API}`, (route) => route.fulfill({ json: { ok: true, needs_repair: false, count: 0 } }));
  await confirmRun(page);
  expect(body, 'the POST body must be an explicit commit').toEqual({ commit: true });
  await expect(page.locator(RUN)).toBeDisabled();
  await expect(page.locator(RUN)).toHaveAttribute('aria-busy', 'true');
  await expect(page.locator(RUN)).toHaveText('Repairing...');
  await expect(page.locator(TOASTS)).toContainText('Image registry repair started');
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
  await expect(page.locator(BANNER)).toBeVisible();
  await expect(page.locator(RUN)).toBeEnabled();
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

test('already running (409): says so and follows the running job to its result', async ({ browser }) => {
  const { context, page } = await openBanner(browser);
  await page.route(`**${REMEDIATE_API}`, (route) => route.fulfill({ status: 409, json: { status: 'running', message: 'an image registry repair is already in progress' } }));
  await page.route(`**${STATUS_API}`, (route) => route.fulfill({ json: DONE_REPORT }));
  await confirmRun(page);
  await expect(page.locator(TOASTS)).toContainText('An image registry repair is already running.');
  await shot(page, 'already-running', 'dark');
  await expect(page.locator(TOASTS)).toContainText('Image registry repaired: 3 rebuilt, 2 restored.');
  await expect(page.locator(RUN)).toBeEnabled();
  await context.close();
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
