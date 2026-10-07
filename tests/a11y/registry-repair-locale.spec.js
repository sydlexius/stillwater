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
// the page shows what the file says, so a reworded translation does not break it
// while a missing or reverted key (a fall-back to English) does.
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
const templateRe = (tpl) => new RegExp(`^${escapeRe(tpl).replace(/\\\{[a-z]+\\\}/g, '.+?')}$`);

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

    async function openBanner(browser, locale, theme = 'dark') {
      const context = await browser.newContext({ colorScheme: theme, locale });
      await context.addCookies([{ name: 'session', value: server.sessionCookie, url: server.rootURL }]);
      const page = await context.newPage();
      await disableTransitions(page);
      await recordToasts(page);
      await page.goto(`${server.baseURL}/reports`);
      await expect(page.locator(BANNER)).toBeVisible();
      return { context, page };
    }

    test('the translations differ from English (the proof cannot pass on a fall-back)', () => {
      for (const key of ['banner.registry_repair.title', 'banner.registry_repair.run', 'banner.registry_repair.toast.done', 'common.cancel']) {
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
        expect(t.lang, `${key} toast lang`).toBe(code);
      }
      for (const t of toasts) {
        expect(t.text, 'no English toast text on a localized page').not.toMatch(/Image registry/);
      }
      await expect(page.locator(BANNER)).toBeHidden();
      await context.close();
    });
  });
}
