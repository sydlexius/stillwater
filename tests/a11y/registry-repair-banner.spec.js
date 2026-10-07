// registry-repair-banner.spec.js - the conditional image-registry repair banner
// (#2678 slice 1). Runs on both projects (firefox-a11y, chromium-a11y).
//
// FIXTURE: helpers/seed-registry-repair.js (own server, detector seam, real scan,
// image written after it, needs_repair/count asserted on the endpoint before any
// page is trusted). The negative case ("hidden when the registry is clean") uses
// the shared harness server, whose empty library is clean by construction; that
// is also asserted on the endpoint, never assumed.

import { test, expect } from 'playwright/test';

import { disableTransitions } from './helpers/settle.js';
import { buildAxeBuilder, formatViolations, applyTheme } from './helpers/axe.js';
import { startRegistryRepairFixture } from './helpers/seed-registry-repair.js';

const BASE_PATH = '/sw-repair-test';
const BANNER = '#sw-registry-repair-banner';
const API = '/api/v1/reports/registry-repair/banner';

let server;
let fx;
let stopServer;
let operatorSession;

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
  const f = await startRegistryRepairFixture(BASE_PATH, 'Registry Repair Fixture', { SW_UX: 'dual' }); // dual: /next/ is served (stable-only 404s)
  ({ server, fx, body: fixtureBody, stop: stopServer } = f);
  operatorSession = await makeOperator();
});

test.afterAll(() => {
  if (stopServer) stopServer();
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
  // Two controls: the run button (slice 2) and the dismiss button, by name.
  await expect(banner.getByRole('button', { name: 'Repair now' })).toHaveCount(1);
  await expect(banner.getByRole('button', { name: 'Dismiss image registry notice' })).toHaveCount(1);
  await expect(banner.getByRole('button')).toHaveCount(2);
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

// ---- Layout mount and look parity (#2678 slice 4) ----

// LayoutNext delegates to the shared Layout, so the banner must mount on /next/
// too. Red if the mount moves out of Layout.
test('banner also mounts on the /next/ layout', async ({ browser }) => {
  const live = await (await fx('GET', API)).json();
  expect(live.needs_repair, 'precondition: the fixture needs repair').toBe(true);
  expect(live.count).toBeGreaterThan(0);
  const { context, page } = await openAs(browser, server.sessionCookie);
  const nav = await page.goto(`${server.baseURL}/next/`);
  expect(nav.status(), '/next/ must be served (a 404 would read as banner absent)').toBe(200);
  await expect(page.locator(BANNER)).toHaveCount(1);
  await expect(page.locator(BANNER)).toBeVisible();
  await context.close();
});

// The tokens the repair banner shares with the ConflictBanner warn bar. DUPLICATED
// from TestRegistryRepairBanner_SharesConflictBannerWarnTokens (web/templates/
// registry_repair_banner_test.go), which asserts this same list on BOTH components;
// change them together. The Go test is the real drift guard; this copy can go
// stale. No real warn ConflictBanner can be produced here (it needs a seeded
// platform conflict), so the reference is built at runtime from the list.
const WARN_TOKENS = [
  'bg-gradient-to-r', 'from-amber-900/60', 'to-yellow-700/50',
  'dark:from-amber-900/60', 'dark:to-yellow-700/50', 'backdrop-blur-md',
  'border-b', 'border-amber-300/30', 'px-6', 'py-3',
];
const LOOK_PROPS = [
  'background-image', 'border-bottom-color', 'border-bottom-width',
  'padding-top', 'padding-right', 'padding-bottom', 'padding-left',
  'backdrop-filter', '-webkit-backdrop-filter',
];

for (const theme of ['dark', 'light']) {
  test(`repair banner renders the same warn look as the conflict banner tokens (${theme})`, async ({ browser }) => {
    const { context, page } = await openAs(browser, server.sessionCookie);
    await page.goto(`${server.baseURL}/reports`);
    await expect(page.locator(BANNER)).toHaveCount(1);
    await expect(page.locator(BANNER)).toBeVisible();
    await applyTheme(expect, page, theme);
    const { ref, got } = await page.evaluate(({ tokens, props, sel }) => {
      const banner = document.querySelector(sel);
      // Sibling of the banner: same ancestors and theme, so only the classes differ.
      const el = document.createElement('div');
      el.className = tokens.join(' ');
      banner.parentElement.appendChild(el);
      const read = (node) => {
        const cs = getComputedStyle(node);
        return Object.fromEntries(props.map((p) => [p, cs.getPropertyValue(p)]));
      };
      const out = { ref: read(el), got: read(banner) };
      el.remove();
      return out;
    }, { tokens: WARN_TOKENS, props: LOOK_PROPS, sel: BANNER });
    // Preconditions: the Tailwind classes generated, so equality below is not two blanks.
    expect(ref['background-image'], 'reference gradient').toContain('gradient');
    // py-3 / px-6 are 0.75rem / 1.5rem: derive px from the page's root font size (14px in the harness).
    const rootPx = await page.evaluate(() => parseFloat(getComputedStyle(document.documentElement).fontSize));
    expect(`${ref['padding-top']} ${ref['padding-right']}`, 'reference padding').toBe(`${0.75 * rootPx}px ${1.5 * rootPx}px`);
    expect(ref['backdrop-filter'] || ref['-webkit-backdrop-filter'], 'reference backdrop blur').not.toMatch(/^(none)?$/);
    for (const p of LOOK_PROPS) {
      expect(got[p], `${theme} ${p}`).toBe(ref[p]);
    }
    await expect(page.locator(`${BANNER} ~ div[class*="bg-gradient-to-r"]`)).toHaveCount(0);
    await context.close();
  });
}
