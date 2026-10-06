// registry-repair-banner.spec.js - the conditional image-registry repair banner
// (#2678 slice 1). Runs on both projects (firefox-a11y, chromium-a11y).
//
// FIXTURE. The banner reads a CACHED detector (GET .../registry-repair/banner)
// that a background job fills 2 minutes after boot and every 12h after, so the
// shared harness server (empty database, empty library) can never be made to
// report needs_repair=true after the fact. This spec boots its OWN throwaway
// server (helpers/base-path-server.js) with SW_REGISTRY_REPAIR_CHECK_EVERY=2s,
// the detector-cadence test seam in cmd/stillwater/main.go, then builds the
// defect the detector looks for: an artist (made by a real scan) whose folder
// holds an image file that has NO registry row. The image is written AFTER the
// scan because the scanner would otherwise register it. Before any page is
// trusted, the fixture's defining property is asserted on the endpoint itself:
// needs_repair true and count > 0.
//
// SW_REGISTRY_REPAIR_CHECK_EVERY is a TEST HOOK, not an operator setting: it is
// REFUSED on release builds (server will not start) and ignored outside [1s, 24h].
//
// The negative case ("hidden when the registry is clean") uses the shared
// harness server, whose empty library is clean by construction; that is also
// asserted on the endpoint, never assumed.

import { test, expect } from 'playwright/test';
import fs from 'node:fs';
import path from 'node:path';

import { disableTransitions } from './helpers/settle.js';
import { buildAxeBuilder, formatViolations, applyTheme } from './helpers/axe.js';
import { startBasePathServer } from './helpers/base-path-server.js';

const BASE_PATH = '/sw-repair-test';
const BANNER = '#sw-registry-repair-banner';
const API = '/api/v1/reports/registry-repair/banner';
// A valid 1x1 PNG: the repair decodes every candidate, so it must be real.
const PNG_1X1 = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGP4z8DwHwAFAAH/iZk9HQAAAABJRU5ErkJggg==',
  'base64',
);

let server;
let tmpDir;
let operatorSession;

// fx issues an authenticated request against the fixture server.
async function fx(method, urlPath, body, session = server.sessionCookie) {
  const resp = await fetch(`${server.baseURL}${urlPath}`, {
    method,
    headers: {
      'Content-Type': 'application/json',
      'X-CSRF-Token': server.csrfToken,
      Cookie: `session=${session}; csrf_token=${server.csrfToken}`,
    },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  return resp;
}

async function seedFixture() {
  const lib = path.join(tmpDir, 'empty-library');
  const artistDir = path.join(lib, 'Registry Repair Fixture');
  fs.mkdirSync(artistDir, { recursive: true });
  expect((await fx('POST', '/api/v1/libraries', { name: 'repair fixture', path: lib, type: 'regular' })).ok).toBe(true);
  expect((await fx('POST', '/api/v1/scanner/run')).ok).toBe(true);
  await expect.poll(async () => (await (await fx('GET', '/api/v1/scanner/status')).json()).status,
    { timeout: 30_000 }).toMatch(/completed|idle/);
  // Written after the scan: a file on disk with no registry row.
  fs.writeFileSync(path.join(artistDir, 'folder.png'), PNG_1X1);

  // The fixture's defining property, asserted on the endpoint itself.
  await expect.poll(async () => (await (await fx('GET', API)).json()).needs_repair,
    { message: 'detector never reported needs_repair=true for the fixture', timeout: 30_000 }).toBe(true);
  const body = await (await fx('GET', API)).json();
  expect(body.ok).toBe(true);
  expect(body.count).toBeGreaterThan(0);
  return body;
}

// A real non-admin: multi-user mode, an operator invite, register, login.
async function makeOperator() {
  expect((await fx('PUT', '/api/v1/settings', { 'multi_user.enabled': 'true' })).ok).toBe(true);
  const invite = await (await fx('POST', '/api/v1/users/invites', { role: 'operator', expires_in: '1h' })).json();
  const reg = await fetch(`${server.baseURL}/api/v1/users/register`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': server.csrfToken, Cookie: `csrf_token=${server.csrfToken}` },
    body: JSON.stringify({ code: invite.code, username: 'repair-operator', password: 'Operator-pw-12345!' }),
  });
  expect(reg.ok, `register: ${reg.status}`).toBe(true);
  const login = await fetch(`${server.baseURL}/api/v1/auth/login`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': server.csrfToken, Cookie: `csrf_token=${server.csrfToken}` },
    body: JSON.stringify({ username: 'repair-operator', password: 'Operator-pw-12345!' }),
  });
  expect(login.ok, `operator login: ${login.status}`).toBe(true);
  return (login.headers.get('set-cookie') || '').match(/session=([^;]+)/)[1];
}

let fixtureBody;

test.beforeAll(async () => {
  test.setTimeout(120_000);
  server = await startBasePathServer(BASE_PATH, {
    env: { SW_REGISTRY_REPAIR_CHECK_EVERY: '2s' },
    seed: (dir) => { tmpDir = dir; },
  });
  fixtureBody = await seedFixture();
  operatorSession = await makeOperator();
});

test.afterAll(() => {
  if (server) server.stop();
});

// Fresh browser context per test = a fresh browser session (empty sessionStorage).
async function openAs(browser, session) {
  const context = await browser.newContext({ colorScheme: 'dark' });
  await context.addCookies([{ name: 'session', value: session, url: server.rootURL }]);
  const page = await context.newPage();
  await disableTransitions(page);
  return { context, page };
}

test('banner shows for an admin when the detector reports rows needing repair', async ({ browser }) => {
  const { context, page } = await openAs(browser, server.sessionCookie);
  await page.goto(`${server.baseURL}/`);
  const banner = page.locator(BANNER);
  await expect(banner).toBeVisible();
  await expect(banner).toHaveCount(1);
  // The count comes from the endpoint, rendered with the right plural form.
  await expect(banner).toContainText(fixtureBody.count === 1 ? '1 registry row may' : `${fixtureBody.count} registry rows may`);
  await expect(banner.locator('#sw-registry-repair-checked')).toContainText('Last checked');
  await expect(banner.getByRole('link', { name: 'Learn more' })).toHaveAttribute('href', /how-to\/repair-image-registry\/#background-check/);
  // Slice 1 is read-only: the dismiss control is the only button.
  await expect(banner.getByRole('button')).toHaveCount(1);
  await context.close();
});

test('dismissal holds across navigation in the session and resets in a new session', async ({ browser }) => {
  const { context, page } = await openAs(browser, server.sessionCookie);
  await page.goto(`${server.baseURL}/`);
  await expect(page.locator(BANNER)).toBeVisible();
  await page.locator('#sw-registry-repair-dismiss').click();
  await expect(page.locator(BANNER)).toHaveCount(0);

  // Registered BEFORE goto so it can actually observe the banner fetch.
  const bannerCalls = [];
  page.on('request', (r) => { if (r.url().includes(API)) bannerCalls.push(r.url()); });
  await page.goto(`${server.baseURL}/artists`);
  await page.waitForLoadState('networkidle');
  expect(bannerCalls, 'a dismissed session must not even ask the endpoint again').toEqual([]);
  await expect(page.locator(BANNER)).toHaveCount(0);
  await context.close();

  const fresh = await openAs(browser, server.sessionCookie);
  await fresh.page.goto(`${server.baseURL}/`);
  await expect(fresh.page.locator(BANNER)).toBeVisible();
  await fresh.context.close();
});

test('banner never reaches a non-admin: no mount on the page, endpoint refuses', async ({ browser }) => {
  const { context, page } = await openAs(browser, operatorSession);
  const resp = await page.goto(`${server.baseURL}/`);
  expect(resp.ok()).toBe(true);
  await expect(page.locator('#sw-main')).toBeVisible();
  await expect(page.locator(BANNER)).toHaveCount(0);
  const api = await page.request.get(`${server.baseURL}${API}`);
  expect(api.status()).toBe(403);
  await context.close();
});

test('banner stays hidden when the registry is clean (empty harness library)', async ({ page }) => {
  const body = await (await page.request.get(API)).json();
  expect(body.needs_repair, 'the empty harness library must report a clean registry').toBe(false);
  const answered = page.waitForResponse((r) => r.url().includes(API));
  await page.goto('/');
  expect((await answered).ok()).toBe(true);
  await expect(page.locator('#sw-main')).toBeVisible();
  await expect(page.locator(BANNER)).toBeHidden();
});

// Deterministic response shaping: page.route fulfills the banner endpoint, so the
// ok:true/needs_repair:false and ok:false branches do not depend on the shared
// server's detector having run.
const HIDDEN_CASES = [
  ['clean (ok true, needs_repair false)', { ok: true, needs_repair: false, count: 0 }],
  ['never checked (ok false)', { ok: false, needs_repair: false, count: 0 }],
  ['needs_repair true with count 0', { ok: true, needs_repair: true, count: 0 }],
  // ok must gate on its own: needs_repair and count alone would show it.
  ['ok false despite needs_repair and count', { ok: false, needs_repair: true, count: 5 }],
];
for (const [name, payload] of HIDDEN_CASES) {
  test(`banner hidden for a stubbed response: ${name}`, async ({ browser }) => {
    const { context, page } = await openAs(browser, server.sessionCookie);
    const errors = [];
    page.on('console', (m) => { if (m.type() === 'error' && m.text().includes('registry repair banner')) errors.push(m.text()); });
    await page.route(`**${API}`, (route) => route.fulfill({ json: payload }));
    const answered = page.waitForResponse((r) => r.url().includes(API));
    await page.goto(`${server.baseURL}/`);
    await answered;
    await expect(page.locator('#sw-main')).toBeVisible();
    await page.waitForTimeout(300);
    await expect(page.locator(BANNER)).toBeHidden();
    expect(errors, 'a normal not-needed/never-checked answer must not log console.error').toEqual([]);
    await context.close();
  });
}

test('a response arriving after dismissal is ignored without a console error', async ({ browser }) => {
  const { context, page } = await openAs(browser, server.sessionCookie);
  const errors = [];
  page.on('console', (m) => { if (m.type() === 'error' && m.text().includes('registry repair banner')) errors.push(m.text()); });
  page.on('pageerror', (e) => errors.push(String(e)));
  let release;
  const gate = new Promise((r) => { release = r; });
  await page.route(`**${API}`, async (route) => {
    await gate;
    await route.fulfill({ json: { ok: true, needs_repair: true, count: 3 } });
  });
  await page.goto(`${server.baseURL}/`);
  // The banner is still hidden while the response is pending; reveal it so the
  // dismiss control is reachable, then dismiss before the response lands.
  await page.locator(BANNER).evaluate((el) => el.classList.remove('hidden'));
  await page.locator('#sw-registry-repair-dismiss').click();
  await expect(page.locator(BANNER)).toHaveCount(0);
  const answered = page.waitForResponse((r) => r.url().includes(API));
  release();
  await answered;
  await page.waitForTimeout(300);
  expect(errors).toEqual([]);
  await context.close();
});

test('dismiss target is at least 24x24 and focus moves to #sw-main', async ({ browser }) => {
  const { context, page } = await openAs(browser, server.sessionCookie);
  await page.goto(`${server.baseURL}/`);
  await expect(page.locator(BANNER)).toBeVisible();
  const box = await page.locator('#sw-registry-repair-dismiss').boundingBox();
  expect(box.width, `dismiss width ${box.width}`).toBeGreaterThanOrEqual(24);
  expect(box.height, `dismiss height ${box.height}`).toBeGreaterThanOrEqual(24);
  await page.locator('#sw-registry-repair-dismiss').focus();
  await page.keyboard.press('Enter');
  await expect(page.locator(BANNER)).toHaveCount(0);
  expect(await page.evaluate(() => document.activeElement && document.activeElement.id)).toBe('sw-main');
  await context.close();
});

test('announcement uses a separate role=status live region; the banner is a non-live region', async ({ browser }) => {
  const { context, page } = await openAs(browser, server.sessionCookie);
  await page.goto(`${server.baseURL}/`);
  await expect(page.locator(BANNER)).toBeVisible();
  await expect(page.locator(BANNER)).toHaveAttribute('role', 'region');
  const live = page.locator('#sw-registry-repair-live');
  await expect(live).toHaveAttribute('role', 'status');
  await expect(live).toContainText('Image registry may need repair');
  expect(await live.evaluate((e) => getComputedStyle(e).display), 'live region must not be display:none').not.toBe('none');
  await context.close();
});

test('an unusable first answer is retried and the banner appears without a reload', async ({ browser }) => {
  const { context, page } = await openAs(browser, server.sessionCookie);
  let calls = 0;
  await page.clock.install();
  await page.route(`**${API}`, (route) => {
    calls += 1;
    return route.fulfill({ json: calls === 1
      ? { ok: false, needs_repair: false, count: 0 }
      : { ok: true, needs_repair: true, count: 2, checked_at: new Date().toISOString() } });
  });
  await page.goto(`${server.baseURL}/`);
  await expect.poll(() => calls).toBe(1);
  await expect(page.locator(BANNER)).toBeHidden();
  await page.clock.runFor(6_000);
  await expect(page.locator(BANNER)).toBeVisible();
  await expect(page.locator('#sw-registry-repair-live')).toContainText('2 registry rows');
  await context.close();
});

for (const theme of ['dark', 'light']) {
  test(`banner passes a full-page a11y scan (${theme} theme)`, async ({ browser }) => {
    const { context, page } = await openAs(browser, server.sessionCookie);
    // Not the dashboard: the fixture's artist puts an action card there whose
    // pre-existing "warning" badge is 4.01:1 in light mode (see blast-radius
    // spec), which would fail this scan for a defect that is not the banner.
    await page.goto(`${server.baseURL}/reports`);
    await expect(page.locator(BANNER)).toBeVisible();
    await applyTheme(expect, page, theme);
    const results = await buildAxeBuilder(page).analyze();
    expect(results.violations, `${theme} violations:\n${formatViolations(results.violations)}`).toHaveLength(0);
    await context.close();
  });
}
