// extrafanart-migration-run.spec.js - running the extrafanart migration from its
// page (#3179): the Run button, the shared confirm modal, the receipt that
// replaces the preview, where focus lands, and what the operator is told when no
// answer comes back. Runs on both projects (firefox-a11y, chromium-a11y).
//
// OWN SERVER. A run MOVES files, so this spec never touches the shared a11y
// server: it boots a throwaway one (helpers/base-path-server.js) per project and
// stops it in afterAll. That server lives under a base path, so every test here
// also proves the button's root-relative hx-post is prefixed at request time.
//
// FIXTURE. helpers/seed-extrafanart-migration.js builds two artists inside the
// harness (the empty a11y database has none) and makes one folder read-only,
// then asserts the plan and the lock before returning. A run consumes the
// fixture, so EVERY test re-seeds first and none depends on another (no serial
// mode: one failure must not hide the rest).
//
// WHAT IS REAL AND WHAT IS NOT. The 200/207 runs are real. The 409 and 500
// browser cases are driven through page.route, so they prove the CLIENT (a 4xx/5xx
// answer is swapped in, takes focus, shows its amber colour, passes axe, raises no
// error toast) and not the server's status code. The 409 answer is the running
// notice in fixtures/extrafanart-running-notice.html, which a Go test
// (TestExtraFanartRunFragment_RunningNoticeMatchesTheSpecFixture) pins byte for
// byte to what the real handler renders, so it cannot drift. (A real 409 CAN be
// provoked by concurrent POSTs; it is not used here because a click cannot time it.)
// The 500 answer is a REAL run's fragment with only its status rewritten; the Go
// tests (handlers_extrafanart_run_page_test.go) pin both server statuses.

import { test, expect } from 'playwright/test';
import fs from 'node:fs';
import path from 'node:path';

import { disableTransitions } from './helpers/settle.js';
import { buildAxeBuilder, formatViolations, applyTheme, renderedContrast } from './helpers/axe.js';
import { startBasePathServer } from './helpers/base-path-server.js';
import {
  seedExtraFanartRunFixture, cleanupExtraFanartRunFixture, extraFanartFiles, RUN_FIXTURE, FILES_PER_ARTIST,
  serverFetch, inventory, missingContent, unlockRunFixture,
} from './helpers/seed-extrafanart-migration.js';

const BASE_PATH = '/sw-extrafanart-run-test';
const API = '/api/v1/reports/extrafanart-migration';
const RUN = '#extrafanart-migration-run-button';
const RECEIPT = '#extrafanart-migration-receipt';
const TITLE = '#extrafanart-migration-receipt-title';
const BODY = '#extrafanart-migration-body';
const TOASTS = '#error-toast-container';
const LOST = 'the migration may still be running';
const ALL_FILES = ['extra0.jpg', 'extra1.jpg'];
const SHOTS = process.env.SW_SHOTS_DIR || '';
const LIVE = '[role="alert"], [role="status"], [aria-live]';

// The body the server answers a refused run with (409); pinned by a Go test.
const RUNNING_NOTICE = fs.readFileSync(new URL('./fixtures/extrafanart-running-notice.html', import.meta.url), 'utf8').trim();

let server;
let libDir;

test.beforeAll(async () => {
  test.setTimeout(120_000);
  server = await startBasePathServer(BASE_PATH, { seed: (dir) => { libDir = path.join(dir, 'run-library'); } });
});

test.afterAll(async () => {
  if (!server) return;
  try { await cleanupExtraFanartRunFixture(server, libDir); } finally { server.stop(); }
});

// Opens the page on a FRESH fixture and proves the plan is there: one planned row
// per seeded file and exactly one enabled Run button.
async function openPlan(browser, theme = 'dark') {
  await seedExtraFanartRunFixture(server, libDir);
  expect(ALL_FILES.length).toBe(FILES_PER_ARTIST);
  const context = await browser.newContext({ colorScheme: theme });
  await context.addCookies([{ name: 'session', value: server.sessionCookie, url: server.rootURL }]);
  const page = await context.newPage();
  await disableTransitions(page);
  const errors = [];
  page.on('console', (m) => { if (m.type() === 'error') errors.push(m.text()); });
  await page.goto(`${server.baseURL}/reports/extrafanart-migration`);
  await expect(page.locator('#extrafanart-migration-table tbody tr').filter({ hasText: 'Will Move' })).toHaveCount(2 * FILES_PER_ARTIST);
  await expect(page.locator(RUN)).toHaveCount(1);
  await expect(page.locator(RUN)).toBeEnabled();
  await expect(page.locator(RUN)).toHaveText('Run migration');
  return { context, page, errors };
}

async function confirmRun(page) {
  await page.locator(RUN).click();
  await expect(page.locator('#confirm-modal-message')).toContainText('This is a one-way operation');
  await expect(page.locator('#confirm-modal-message')).toContainText('Nothing is deleted');
  await page.locator('#confirm-modal-accept').click();
}

function expectNothingMoved() {
  for (const artist of [RUN_FIXTURE.okArtist, RUN_FIXTURE.lockedArtist]) {
    expect(extraFanartFiles(libDir, artist), `${artist}/extrafanart must be untouched`).toEqual(ALL_FILES);
  }
}

// A moved file keeps its bytes under a new name, so a run that deleted anything
// leaves a before-file whose content is in no after-file. Names what went missing.
function expectNothingDeleted(before) {
  expect(missingContent(before, inventory(libDir)), 'files whose content vanished from the library').toEqual([]);
}

// The focused heading must not sit inside a live region (it would be announced
// twice: once for the focus move, once as inserted content), and the answer must
// still be announced by a live region that is NOT an ancestor of the focus.
async function expectAnnouncedOnce(page) {
  const wrapper = await page.evaluate((sel) => {
    const el = document.activeElement && document.activeElement.closest(sel);
    return el ? (el.id || el.tagName) : null;
  }, LIVE);
  expect(wrapper, 'the focused heading is inside a live region').toBeNull();
  await expect(page.locator(`${BODY} :is([role="alert"], [role="status"])`)).not.toHaveCount(0);
}

// The Run button is the only thing that may send a run: count POSTs to the API.
function countPosts(page) {
  const seen = { n: 0 };
  page.on('request', (r) => { if (r.method() === 'POST' && r.url().endsWith(API)) seen.n += 1; });
  return seen;
}

// The amber accent is the COLOUR of the card's left edge, resolved against the
// --color-amber-500 token the way platform-backdrop-perceptual.spec.js does. The
// edge's WIDTH is not asserted: the unlayered .sw-card border (1px) beats the
// layered border-l-4 utility, so it renders 1px.
async function expectAmberCard(locator) {
  await expect(locator).toHaveClass(/sw-card-accent-amber/);
  const { edge, token } = await locator.evaluate((el) => {
    const raw = getComputedStyle(document.documentElement).getPropertyValue('--color-amber-500').trim();
    const probe = document.createElement('span');
    probe.style.color = raw;
    document.body.appendChild(probe);
    const token = raw ? getComputedStyle(probe).color : '';
    probe.remove();
    return { edge: getComputedStyle(el).borderLeftColor, token };
  });
  expect(token, '--color-amber-500 did not resolve, so this check would be vacuous').not.toBe('');
  expect(edge, 'amber accent edge colour').toBe(token);
}

test('cancelling the shared confirm modal sends nothing and moves nothing', async ({ browser }) => {
  const { context, page } = await openPlan(browser);
  const before = inventory(libDir);
  const posts = countPosts(page);
  await page.locator(RUN).click();
  await expect(page.locator('#confirm-modal')).toBeVisible();
  await page.locator('#confirm-modal-cancel').click();
  await expect(page.locator('#confirm-modal')).toBeHidden();
  await expect(page.locator(RUN)).toBeFocused();
  await page.waitForTimeout(300);
  expect(posts.n, 'cancelling must not start a run').toBe(0);
  expectNothingMoved();
  expectNothingDeleted(before);
  expect(inventory(libDir)).toEqual(before);
  await context.close();
});

test('one confirmed click sends exactly one POST; the button is disabled and relabelled while it is out', async ({ browser }) => {
  const { context, page } = await openPlan(browser);
  const posts = countPosts(page);
  let release;
  const gate = new Promise((r) => { release = r; });
  await page.route(`**${API}`, async (route) => { await gate; await route.continue(); });
  await confirmRun(page);
  await expect(page.locator(RUN)).toBeDisabled();
  await expect(page.locator(RUN)).toHaveAttribute('aria-busy', 'true');
  await expect(page.locator(RUN)).toHaveText('Running...');
  // A second press while the first is out must open no dialog and send nothing.
  await page.locator(RUN).click({ force: true });
  await page.waitForTimeout(300);
  await expect(page.locator('#confirm-modal')).toBeHidden();
  release();
  await expect(page.locator(TITLE)).toBeFocused();
  expect(posts.n, 'exactly one POST for one confirmed click').toBe(1);
  await context.close();
});

test('lost connection: warning toast says the run may still be going; no second run is offered', async ({ browser }) => {
  const { context, page } = await openPlan(browser);
  const before = inventory(libDir);
  const posts = countPosts(page);
  let release;
  const gate = new Promise((r) => { release = r; });
  await page.route(`**${API}`, async (route) => { await gate; await route.abort('connectionreset'); });
  await confirmRun(page);
  await expect(page.locator(RUN)).toBeDisabled();
  release();
  const toast = page.locator(`${TOASTS} > div`, { hasText: LOST });
  await expect(toast).toHaveCount(1);
  await expect(toast).toContainText('Reload in a moment to preview what is left');
  await expect(page.locator(RUN)).not.toHaveAttribute('aria-busy', /.*/);
  await expect(page.locator(RUN)).toHaveText('Run migration');
  await expect(page.locator(RUN), 'a lost answer must not re-offer a blind second run').toBeDisabled();
  await expect(page.locator('#sw-main'), 'focus must land on #sw-main, not fall to body').toBeFocused();
  await expect(page.locator(RECEIPT)).toHaveCount(0);
  expect(posts.n).toBe(1);
  expectNothingMoved();
  expect(inventory(libDir)).toEqual(before);
  await context.close();
});

test('lost connection does not steal focus the user moved to a link while the request was in flight', async ({ browser }) => {
  const { context, page } = await openPlan(browser);
  let release;
  const gate = new Promise((r) => { release = r; });
  await page.route(`**${API}`, async (route) => { await gate; await route.abort('connectionreset'); });
  await confirmRun(page);
  await expect(page.locator(RUN)).toBeDisabled();
  const link = page.locator('#extrafanart-migration-table tbody tr a').first();
  await link.focus();
  await expect(link, 'precondition: the user moved focus to a link inside the page').toBeFocused();
  release();
  await expect(page.locator(`${TOASTS} > div`, { hasText: LOST })).toHaveCount(1);
  await expect(page.locator(RUN)).toBeDisabled();
  await expect(link, 'a lost answer must not pull focus away from a deliberate choice').toBeFocused();
  await context.close();
});

test('missingContent reports one of two byte-identical files that was deleted', () => {
  const before = [{ rel: 'a/fanart.jpg', sha: 'x' }, { rel: 'b/fanart.jpg', sha: 'x' }, { rel: 'a/n.nfo', sha: 'y' }];
  expect(missingContent(before, before)).toEqual([]);
  expect(missingContent(before, before.filter((f) => f.rel !== 'b/fanart.jpg'))).toEqual(['b/fanart.jpg']);
  expect(missingContent(before, [before[0], before[1]])).toEqual(['a/n.nfo']);
});

test('a proxy error page (504 HTML) is never swapped in: same warning toast, the plan stays', async ({ browser }) => {
  const { context, page } = await openPlan(browser);
  await page.route(`**${API}`, (route) => route.fulfill({
    status: 504, contentType: 'text/html', body: '<html><body><h1>504 Gateway Time-out</h1><hr>nginx</body></html>',
  }));
  await confirmRun(page);
  await expect(page.locator(`${TOASTS} > div`, { hasText: LOST })).toHaveCount(1);
  await expect(page.locator(TOASTS)).not.toContainText('Request failed');
  expect(await page.locator('body').innerText()).not.toContain('Gateway Time-out');
  await expect(page.locator('#extrafanart-migration-table tbody tr')).toHaveCount(2 * FILES_PER_ARTIST);
  await expect(page.locator(RUN)).toBeDisabled();
  await expect(page.locator('#sw-main'), 'focus must land on #sw-main, not fall to body').toBeFocused();
  await context.close();
});

test('a 429 JSON answer is not a lost answer: no lost toast, the button is enabled and focused', async ({ browser }) => {
  const { context, page } = await openPlan(browser);
  await page.route(`**${API}`, (route) => route.fulfill({
    status: 429, contentType: 'application/json', body: '{"error":"slow down"}',
  }));
  await confirmRun(page);
  await expect(page.locator(RUN)).toBeEnabled();
  await expect(page.locator(RUN)).toHaveText('Run migration');
  await expect(page.locator(RUN)).toBeFocused();
  await expect(page.locator(`${TOASTS} > div`, { hasText: LOST })).toHaveCount(0);
  await expect(page.locator(RECEIPT)).toHaveCount(0);
  await context.close();
});

test('a missing toast function fails loudly on the console', async ({ browser }) => {
  const { context, page, errors } = await openPlan(browser);
  await page.route(`**${API}`, (route) => route.abort('connectionreset'));
  await page.evaluate(() => { delete window.showWarningToast; });
  await confirmRun(page);
  await expect.poll(() => errors.some((e) => e.includes('extrafanart migration: showWarningToast unavailable'))).toBe(true);
  await context.close();
});

for (const theme of ['dark', 'light']) {
  test(`real run: 207 receipt names the unmovable artist and file, takes focus, passes axe and contrast (${theme})`, async ({ browser }) => {
    const { context, page, errors } = await openPlan(browser, theme);
    const before = inventory(libDir);
    const answered = page.waitForResponse((r) => r.request().method() === 'POST' && r.url().includes(API));
    await confirmRun(page);
    const resp = await answered;
    // The button's hx-post is root-relative; the page's configRequest hook must
    // have put the base path in front of it, or this server would not route it.
    expect(new URL(resp.url()).pathname).toBe(`${BASE_PATH}${API}`);
    expect(resp.request().headers()['hx-request']).toBe('true');
    expect(resp.request().postData()).toBe('dry_run=false');
    expect(resp.status(), 'one unmovable folder makes the finished run partial').toBe(207);
    expect(resp.headers()['content-type']).toContain('text/html');

    const receipt = page.locator(RECEIPT);
    await expect(receipt).toHaveCount(1);
    await expectAmberCard(receipt);
    await expect(page.locator('#extrafanart-migration-receipt-body')).toHaveAttribute('role', 'alert');
    await expect(page.locator(TITLE)).toHaveText('Partially Migrated');
    // htmx honors [autofocus] on swapped-in content in both browsers: asserted, not assumed.
    await expect(page.locator(TITLE)).toBeFocused();
    await expectAnnouncedOnce(page);
    await expect(page.locator(RUN)).toHaveCount(0);
    await expect(page.locator('#extrafanart-migration-warning')).toHaveCount(0);
    await expect(page.locator('#extrafanart-migration-moved')).toHaveText(String(FILES_PER_ARTIST));
    await expect(page.locator('#extrafanart-migration-failed')).toHaveText(String(FILES_PER_ARTIST));
    const rows = page.locator('#extrafanart-migration-table tbody tr');
    for (const file of ALL_FILES) {
      const failed = rows.filter({ hasText: RUN_FIXTURE.lockedArtist }).filter({ hasText: file });
      await expect(failed).toHaveCount(1);
      await expect(failed).toContainText('Failed - the move failed');
    }
    await expect(rows.filter({ hasText: RUN_FIXTURE.okArtist }).filter({ hasText: 'Moved' })).toHaveCount(FILES_PER_ARTIST);
    // The disk agrees with the receipt, read from outside the page.
    expect(extraFanartFiles(libDir, RUN_FIXTURE.okArtist), 'the movable artist was migrated').toEqual([]);
    expect(extraFanartFiles(libDir, RUN_FIXTURE.lockedArtist), 'nothing left the unmovable folder').toEqual(ALL_FILES);
    expectNothingDeleted(before);
    // The swap leaves exactly one body, still carrying its hook for the next run.
    await expect(page.locator(BODY)).toHaveCount(1);
    await expect(page.locator(BODY)).toHaveAttribute('hx-on::before-swap', /beforeSwap/);

    await applyTheme(expect, page, theme);
    if (SHOTS) await page.screenshot({ path: path.join(SHOTS, `extrafanart-receipt-${theme}.png`) });
    const measured = {
      receiptTitle: page.locator(TITLE),
      receiptBody: page.locator('#extrafanart-migration-receipt-body'),
      tileLabel: page.locator('#extrafanart-migration-body dl dt').nth(1),
      tileValue: page.locator('#extrafanart-migration-failed'),
      failedRow: rows.filter({ hasText: RUN_FIXTURE.lockedArtist }).first().locator('td').nth(3),
    };
    for (const [name, loc] of Object.entries(measured)) {
      expect(await loc.count(), `${name} selector matched nothing`).toBe(1);
      expect.soft(await renderedContrast(page, loc), `${name} contrast (${theme})`).toBeGreaterThanOrEqual(4.5);
    }
    const results = await buildAxeBuilder(page).analyze();
    expect(results.violations, formatViolations(results.violations)).toEqual([]);
    expect(errors, 'no console errors during a run').toEqual([]);

    // The receipt's link leads back to a preview of what is left: the locked files only.
    await receipt.getByRole('link', { name: 'Preview again' }).click();
    await expect(page).toHaveURL(`${server.baseURL}/reports/extrafanart-migration`);
    await expect(page.locator('#extrafanart-migration-table tbody tr').filter({ hasText: 'Will Move' })).toHaveCount(FILES_PER_ARTIST);
    await context.close();
  });
}

test('clean run, then run again: the second finds nothing to do and nothing is deleted', async ({ browser }) => {
  const { context, page, errors } = await openPlan(browser);
  // The fixture's defining lock is asserted by the seed; this test needs every
  // file movable, so it lifts the lock AFTER that proof.
  unlockRunFixture(libDir);
  const before = inventory(libDir);
  const answered = page.waitForResponse((r) => r.request().method() === 'POST' && r.url().includes(API));
  await confirmRun(page);
  expect((await answered).status(), 'every file movable: a clean 200').toBe(200);
  await expect(page.locator(TITLE)).toHaveText('Migrated');
  await expect(page.locator(TITLE)).toBeFocused();
  await expectAnnouncedOnce(page);
  await expect(page.locator(RECEIPT)).not.toHaveClass(/sw-card-accent-amber/);
  await expect(page.locator('#extrafanart-migration-moved')).toHaveText(String(2 * FILES_PER_ARTIST));
  await expect(page.locator('#extrafanart-migration-failed')).toHaveText('0');
  await expect(page.locator(RUN)).toHaveCount(0);
  const afterFirst = inventory(libDir);
  expectNothingDeleted(before);
  for (const artist of [RUN_FIXTURE.okArtist, RUN_FIXTURE.lockedArtist]) {
    expect(extraFanartFiles(libDir, artist), `${artist}/extrafanart is emptied`).toEqual([]);
  }

  // Second run, as the page would send it: nothing left to move, nothing changes.
  const again = await serverFetch(server, 'POST', API, { dry_run: false }, { 'HX-Request': 'true' });
  expect(again.status).toBe(200);
  expect(await again.text()).toContain('Nothing To Do');
  expect(inventory(libDir), 'a second run changes nothing').toEqual(afterFirst);
  expectNothingDeleted(before);

  // And the page agrees: a fresh load shows no plan to run and offers no button.
  await page.goto(`${server.baseURL}/reports/extrafanart-migration`);
  await expect(page.locator('#extrafanart-migration-empty')).toBeVisible();
  await expect(page.locator(RUN)).toHaveCount(0);
  expect(errors, 'no console errors').toEqual([]);
  await context.close();
});

// htmx refuses to swap a 4xx/5xx answer unless the swap TARGET allows it, and a
// refused (409) or stopped (500) run answers with exactly such a fragment. See the
// header for what these two interceptions do and do not prove.
const ERROR_ANSWERS = [
  {
    status: 409,
    title: 'A Migration Is Already Running',
    card: '#extrafanart-migration-running',
    focused: '#extrafanart-migration-running > p:first-child',
    fulfill: async (route) => route.fulfill({ status: 409, contentType: 'text/html; charset=utf-8', body: RUNNING_NOTICE }),
  },
  {
    status: 500,
    title: 'Partially Migrated', // the real run's own receipt; only its status code is rewritten
    card: RECEIPT,
    focused: TITLE,
    fulfill: async (route) => {
      const real = await route.fetch();
      expect(real.status(), 'precondition: the real run answered with its own fragment').toBe(207);
      await route.fulfill({ response: real, status: 500 });
    },
  },
];
for (const answer of ERROR_ANSWERS) {
  for (const theme of ['dark', 'light']) {
    test(`a ${answer.status} fragment is swapped in with its amber accent, takes focus, and passes axe (${theme})`, async ({ browser }) => {
      const { context, page, errors } = await openPlan(browser, theme);
      await page.route(`**${API}`, answer.fulfill);
      const posts = countPosts(page);
      const answered = page.waitForResponse((r) => r.request().method() === 'POST' && r.url().includes(API));
      await confirmRun(page);
      expect((await answered).status()).toBe(answer.status);
      await expect(page.locator(answer.card)).toHaveCount(1);
      await expectAmberCard(page.locator(answer.card));
      await expect(page.locator(answer.focused)).toHaveText(answer.title);
      await expect(page.locator(answer.focused)).toBeFocused();
      await expectAnnouncedOnce(page);
      await expect(page.locator(RUN)).toHaveCount(0);
      await expect(page.locator(BODY)).toHaveCount(1);
      await page.waitForTimeout(300);
      await expect(page.locator(`${TOASTS} > div`), 'an answered run raises no error toast').toHaveCount(0);
      expect(errors.filter((e) => !e.includes(`status of ${answer.status}`)), 'no script errors').toEqual([]);
      expect(posts.n).toBe(1);

      await applyTheme(expect, page, theme);
      const results = await buildAxeBuilder(page).analyze();
      expect(results.violations, `${theme}:\n${formatViolations(results.violations)}`).toEqual([]);
      await context.close();
    });
  }
}

test('the Run button passes axe and contrast before a run (dark and light)', async ({ browser }) => {
  for (const theme of ['dark', 'light']) {
    const { context, page } = await openPlan(browser, theme);
    await applyTheme(expect, page, theme);
    expect.soft(await renderedContrast(page, page.locator(RUN)), `run button contrast (${theme})`).toBeGreaterThanOrEqual(4.5);
    const box = await page.locator(RUN).boundingBox();
    expect(box.height, `run button height ${box.height}`).toBeGreaterThanOrEqual(24);
    const results = await buildAxeBuilder(page).analyze();
    expect(results.violations, `${theme}:\n${formatViolations(results.violations)}`).toEqual([]);
    await context.close();
  }
});
