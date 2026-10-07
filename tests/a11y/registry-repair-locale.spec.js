// registry-repair-locale.spec.js - the image-registry repair banner flow in fr
// and ja (#2678 slice 3). Runs on both projects (firefox-a11y, chromium-a11y).
//
// HOW THE LOCALE REACHES THE SERVER. internal/i18n/middleware.go picks the
// language from the Accept-Language request header only (the saved-preference
// branch is reserved, not built). Playwright's `locale` option sets that header,
// so test.use({ locale }) is what a French or Japanese browser sends. Each
// context below is created with the locale fixture explicitly, because
// browser.newContext() does not inherit test.use options by itself.
//
// EXPECTED TEXT IS READ FROM THE LOCALE JSON, never hardcoded: the spec proves
// the page shows what the file says, so a reworded translation does not break it.
// A key reverted to English IN the JSON would still match the page, so KEYS lists
// every key the spec reads and one test asserts each differs from en.
//
// FIXTURE: helpers/seed-registry-repair.js. The fixture can be repaired exactly
// once, so each locale boots its own server (describe-level beforeAll); the
// non-mutating tests run first and the one REAL repair run (real job, real
// status, real toasts) runs last, serially. No test depends on another spec file.

import { test, expect } from 'playwright/test';
import fs from 'node:fs';

import { disableTransitions } from './helpers/settle.js';
import { buildAxeBuilder, formatViolations, applyTheme } from './helpers/axe.js';
import { startRegistryRepairFixture } from './helpers/seed-registry-repair.js';

const BANNER = '#sw-registry-repair-banner';
const LOCALES_DIR = new URL('../../internal/i18n/locales/', import.meta.url);

const loadLocale = (code) => JSON.parse(fs.readFileSync(new URL(`${code}.json`, LOCALES_DIR), 'utf8'));
const escapeRe = (s) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
// fill replaces {name} placeholders with literal values.
const fill = (tpl, vars) => Object.entries(vars).reduce((s, [k, v]) => s.replaceAll(`{${k}}`, String(v)), tpl);
// templateRe turns a message with {placeholders} into a regex matching any value for each.
// {time} is free text; every other placeholder here is a number, so a literal
// "{rebuilt}" left unreplaced does not match.
const templateRe = (tpl) => new RegExp(`^${escapeRe(tpl).replace(/\\\{([a-z]+)\\\}/g, (_, n) => (n === 'time' ? '.+?' : '\\d+'))}$`);

// Every locale key the spec reads. One list, so the differs-from-English guard
// cannot drift from what is actually asserted.
const CHECKED_AT = '2026-03-04T15:06:07Z';

const KEYS = [
  'banner.registry_repair.title',
  'banner.registry_repair.run',
  'banner.registry_repair.running',
  'banner.registry_repair.confirm',
  'banner.registry_repair.body.one',
  'banner.registry_repair.body.other',
  'banner.registry_repair.checked',
  'banner.registry_repair.aria_label',
  'banner.registry_repair.dismiss_aria',
  'banner.registry_repair.toast.started',
  'banner.registry_repair.toast.done',
  'banner.registry_repair.toast.failed',
  'common.learn_more',
  'common.cancel',
  'common.confirm',
  'notifications.dismiss_aria',
];

// Records every toast that enters the container (text + lang): the started toast
// is auto-dismissed and a tiny fixture can finish before a poll would see it.
const recordToasts = (page) => page.addInitScript(() => {
  window.__toasts = [];
  const seen = new WeakSet();
  const scan = () => {
    const box = document.getElementById('error-toast-container');
    if (!box) return;
    box.querySelectorAll('[lang]').forEach((n) => {
      if (n.textContent.trim() && !seen.has(n)) {
        seen.add(n);
        window.__toasts.push({ text: n.textContent.trim(), lang: n.getAttribute('lang') });
      }
    });
  };
  new MutationObserver(scan).observe(document, { subtree: true, childList: true, characterData: true });
});

for (const [code, region] of [['fr', 'fr-FR'], ['ja', 'ja-JP']]) {
  test.describe(`repair banner flow in ${code}`, () => {
    test.describe.configure({ mode: 'serial' });
    test.use({ locale: region });

    const L = loadLocale(code);
    const en = loadLocale('en');
    let server;
    let stopServer;
    let fixtureBody;

    test.beforeAll(async () => {
      test.setTimeout(120_000);
      const f = await startRegistryRepairFixture(`/sw-repair-locale-${code}`, `Registry Repair ${code} Fixture`);
      ({ server, stop: stopServer, body: fixtureBody } = f);
    });
    test.afterAll(() => { if (stopServer) stopServer(); });

    async function openBanner(browser, locale, theme = 'dark', timezoneId = undefined) {
      const context = await browser.newContext({ colorScheme: theme, locale, timezoneId });
      await context.addCookies([{ name: 'session', value: server.sessionCookie, url: server.rootURL }]);
      const page = await context.newPage();
      await disableTransitions(page);
      await recordToasts(page);
      await page.goto(`${server.baseURL}/reports`);
      await expect(page.locator(BANNER)).toBeVisible();
      return { context, page };
    }

    test('every key the spec reads is translated, not English (the proof cannot pass on a fall-back)', () => {
      for (const key of KEYS) {
        expect(L[key], `${code}.json lacks ${key}`).toBeTruthy();
        expect(L[key], `${code}.json ${key} equals English`).not.toBe(en[key]);
      }
    });

    test('banner copy, count, checked line and lang come from the locale file', async ({ browser, locale }) => {
      const { context, page } = await openBanner(browser, locale);
      const banner = page.locator(BANNER);
      await expect(banner).toHaveAttribute('lang', code);
      await expect(page.locator('#sw-registry-repair-live')).toHaveAttribute('lang', code);
      await expect(banner.locator('#sw-registry-repair-title')).toContainText(L['banner.registry_repair.title']);
      const body = L[fixtureBody.count === 1 ? 'banner.registry_repair.body.one' : 'banner.registry_repair.body.other'];
      await expect(banner.locator('#sw-registry-repair-count')).toContainText(fill(body, { count: fixtureBody.count }));
      await expect(banner.locator('#sw-registry-repair-checked')).toHaveText(templateRe(L['banner.registry_repair.checked']));
      await expect(banner.getByRole('button', { name: L['banner.registry_repair.run'] })).toHaveCount(1);
      await expect(banner.getByRole('button', { name: L['banner.registry_repair.dismiss_aria'] })).toHaveCount(1);
      await expect(banner).not.toContainText(en['banner.registry_repair.title']);
      // Live region name and the link, from their own keys.
      await expect(page.locator('#sw-registry-repair-live')).toHaveAttribute('aria-label', L['banner.registry_repair.aria_label']);
      await expect(banner.getByRole('link', { name: L['common.learn_more'] })).toHaveCount(1);
      await context.close();
    });

    // Stubbed count 2: the real fixture has one row, so only a stub renders the
    // plural (.other) string, the one carrying {count}. The time must use the
    // page language (template comment). The instant, the zone (UTC) and the
    // expected text are all fixed OUTSIDE the page: the expectation is computed in
    // Node, so a page that formatted with the wrong locale or zone cannot agree
    // with itself. Whitespace is normalized on both sides (ICU in Node and in the
    // browser can differ by narrow no-break spaces).
    test('plural count and the checked time render in the page locale', async ({ browser, locale }) => {
      const { context, page } = await openBanner(browser, locale, 'dark', 'UTC');
      await page.route('**/registry-repair/banner', (route) => route.fulfill({
        json: { ok: true, needs_repair: true, count: 2, checked_at: CHECKED_AT },
      }));
      await page.goto(`${server.baseURL}/reports`);
      await expect(page.locator(BANNER)).toBeVisible();
      await expect(page.locator('#sw-registry-repair-count')).toContainText(fill(L['banner.registry_repair.body.other'], { count: 2 }));
      const norm = (t) => t.replace(/\s+/g, ' ').trim();
      const fixed = new Date(CHECKED_AT);
      const when = fixed.toLocaleString(locale, { timeZone: 'UTC' });
      const enUS = fixed.toLocaleString('en-US', { timeZone: 'UTC' });
      expect(norm(when), 'the fixed instant must render differently in en-US').not.toBe(norm(enUS));
      await expect.poll(async () => norm(await page.locator('#sw-registry-repair-checked').innerText()))
        .toBe(norm(fill(L['banner.registry_repair.checked'], { time: when })));
      await context.close();
    });

    // Stubbed run: the running label while the job reports running, then the
    // failed toast (fixed message) when it reports failed. Does not touch the fixture.
    test('running label and failed toast are localized', async ({ browser, locale }) => {
      const { context, page } = await openBanner(browser, locale);
      let status = { running: true, status: 'running' };
      await page.route('**/registry-repair/remediate', (route) => route.fulfill({ status: 202, json: { running: true, status: 'running' } }));
      await page.route('**/registry-repair/status', (route) => route.fulfill({ json: status }));
      await page.locator('#sw-registry-repair-run').click();
      await page.locator('#confirm-modal-accept').click();
      await expect(page.locator('#sw-registry-repair-run')).toHaveText(L['banner.registry_repair.running']);
      status = { running: false, status: 'failed', error_code: 'timeout' };
      await expect.poll(() => page.evaluate(() => window.__toasts.map((t) => t.text)), { timeout: 30_000 })
        .toContain(L['banner.registry_repair.toast.failed']);
      const failed = (await page.evaluate(() => window.__toasts)).find((t) => t.text === L['banner.registry_repair.toast.failed']);
      expect(failed, 'no failed toast was recorded').toBeTruthy();
      expect(failed.lang, 'failed toast lang').toBe(code);
      await context.close();
    });

    for (const theme of ['dark', 'light']) {
      test(`localized banner passes a full-page a11y scan (${theme} theme)`, async ({ browser, locale }) => {
        const { context, page } = await openBanner(browser, locale, theme);
        await applyTheme(expect, page, theme);
        const results = await buildAxeBuilder(page).analyze();
        expect(results.violations, `${code} ${theme} violations:\n${formatViolations(results.violations)}`).toHaveLength(0);
        await context.close();
      });
    }

    test('confirm dialog title, buttons and message are localized and tagged', async ({ browser, locale }) => {
      const { context, page } = await openBanner(browser, locale);
      await page.locator('#sw-registry-repair-run').click();
      await expect(page.locator('#confirm-modal')).toBeVisible();
      await expect(page.locator('#confirm-modal-title')).toHaveText(L['common.confirm']);
      await expect(page.locator('#confirm-modal-accept')).toHaveText(L['common.confirm']);
      await expect(page.locator('#confirm-modal-cancel')).toHaveText(L['common.cancel']);
      await expect(page.locator('#confirm-modal-message')).toHaveText(L['banner.registry_repair.confirm']);
      for (const id of ['title', 'cancel', 'accept', 'message']) {
        await expect(page.locator(`#confirm-modal-${id}`)).toHaveAttribute('lang', code);
      }
      await page.locator('#confirm-modal-cancel').click();
      await expect(page.locator('#confirm-modal')).toBeHidden();
      await context.close();
    });

    // LAST: the one real run. Real POST, job, status poll and cache update.
    test('real run: started and done toasts are localized and tagged', async ({ browser, locale }) => {
      const { context, page } = await openBanner(browser, locale);
      await page.locator('#sw-registry-repair-run').click();
      await expect(page.locator('#confirm-modal-message')).toHaveText(L['banner.registry_repair.confirm']);
      await page.locator('#confirm-modal-accept').click();
      const doneRe = templateRe(L['banner.registry_repair.toast.done']);
      await expect.poll(() => page.evaluate(() => window.__toasts.map((t) => t.text)), { timeout: 60_000 })
        .toEqual(expect.arrayContaining([L['banner.registry_repair.toast.started'], expect.stringMatching(doneRe)]));
      const toasts = await page.evaluate(() => window.__toasts);
      for (const key of ['started', 'done']) {
        const t = toasts.find((x) => templateRe(L[`banner.registry_repair.toast.${key}`]).test(x.text));
        expect(t, `no ${key} toast was recorded`).toBeTruthy();
        expect(t.lang, `${key} toast lang`).toBe(code);
      }
      // The done toast is sticky, so its dismiss button is still there: its
      // accessible name and language are the page locale's, not English.
      const dismiss = page.locator('#error-toast-container').getByRole('button', { name: L['notifications.dismiss_aria'] });
      await expect(dismiss).toHaveCount(1);
      await expect(dismiss).toHaveAttribute('lang', code);
      for (const t of toasts) {
        expect(t.text, 'no English toast text on a localized page').not.toMatch(/Image registry/);
      }
      await expect(page.locator(BANNER)).toBeHidden();
      await context.close();
    });
  });
}
