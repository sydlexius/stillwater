// ai-blocklist-settings.spec.js - the "AI Image List" card on Settings (#3277).
//
// Two states, each on a server the spec can assert about:
//   OFF    - the shared harness server (SW_AI_BLOCKLIST_URL empty, download off).
//   LOADED - a second server this spec starts with SW_AI_BLOCKLIST_URL set and a
//            pre-seeded cache file of FIXTURE_RULES rules.
//
// WHY A SEEDED CACHE AND NOT A LOCAL LIST SERVER: Stillwater downloads the list
// through its SSRF-guarded client, which refuses loopback and private addresses
// by design, so a static server on 127.0.0.1 can never be fetched. The store
// adopts <db dir>/cache/ai_blocklist.txt at startup before any download, so the
// seeded file loads as a real list; the URL points at a loopback port the guard
// refuses, which also gives the card a deterministic "could not be downloaded"
// error and, because that failed boot attempt counts as a refresh, a 429 on the
// first Refresh now click.

import fs from 'node:fs';
import path from 'node:path';
import { test, expect } from 'playwright/test';

import { startBasePathServer } from './helpers/base-path-server.js';
import { disableTransitions } from './helpers/settle.js';
import { buildAxeBuilder, formatViolations, applyTheme } from './helpers/axe.js';

const FIXTURE_RULES = 5;
const CARD = '#ai-image-list-card';

// Read settled colors, not a mid-transition blend (helpers/settle.js).
test.beforeEach(async ({ page }) => {
  await disableTransitions(page);
});

async function scanCard(page, theme) {
  await applyTheme(expect, page, theme);
  const results = await buildAxeBuilder(page).include(CARD).analyze();
  expect(results.violations, `${theme} violations in ${CARD}:\n${formatViolations(results.violations)}`).toHaveLength(0);
}

test.describe('AI image list card, download off (harness server)', () => {
  test('says the download is off, has no button, and passes axe in both themes', async ({ page }) => {
    await page.goto('/settings');
    const card = page.locator(CARD);
    await expect(card).toBeVisible();
    await expect(card.locator('[data-ai-list-state="off"]')).toContainText('SW_AI_BLOCKLIST_URL');
    await expect(card.locator('#ai-image-list-refresh')).toHaveCount(0);
    await scanCard(page, 'dark');
    await scanCard(page, 'light');
  });
});

test.describe('AI image list card, loaded list (own server)', () => {
  let server;

  test.beforeAll(async () => {
    server = await startBasePathServer('/sw-ailist-test', {
      env: { SW_AI_BLOCKLIST_URL: 'http://127.0.0.1:1/list.txt' },
      seed: (tmpDir) => {
        const dir = path.join(tmpDir, 'cache');
        fs.mkdirSync(dir, { recursive: true });
        const rules = Array.from({ length: FIXTURE_RULES }, (_, i) => `*://*.fixture${i}.example/*`);
        fs.writeFileSync(path.join(dir, 'ai_blocklist.txt'), `${rules.join('\n')}\n`);
      },
    });
  });

  test.afterAll(async () => {
    if (server) server.stop();
  });

  test('shows the fixture list, answers Refresh now with a readable 429, and passes axe', async ({ browser }) => {
    const context = await browser.newContext();
    await context.addCookies([
      { name: 'csrf_token', value: server.csrfToken, url: server.rootURL },
      { name: 'session', value: server.sessionCookie, url: server.rootURL },
    ]);
    const page = await context.newPage();
    await disableTransitions(page);

    // Precondition from the API, outside the page under test: the fixture
    // really loaded with FIXTURE_RULES rules, or the checks below prove nothing.
    const status = await context.request.get(`${server.baseURL}/api/v1/images/ai-blocklist/status`);
    expect(status.ok()).toBe(true);
    const body = await status.json();
    expect(body.loaded, 'fixture list did not load').toBe(true);
    expect(body.rules).toBe(FIXTURE_RULES);

    await page.goto(`${server.baseURL}/settings`);
    const card = page.locator(CARD);
    await expect(card.locator('[data-ai-list="rules"]')).toHaveText(String(FIXTURE_RULES));
    await expect(card.locator('[data-ai-list="source"]')).toHaveText('127.0.0.1:1/list.txt');
    await expect(card.locator('[data-ai-list="last-error"]')).toContainText('could not be downloaded');

    await scanCard(page, 'dark');
    await scanCard(page, 'light');

    // Keyboard activation: focus must come back to the (re-rendered) button.
    await card.locator('#ai-image-list-refresh').focus();
    await page.keyboard.press('Enter');
    await expect(card.getByRole('alert')).toContainText('Try again in');
    await expect(card.locator('#ai-image-list-refresh')).toBeFocused();
    await expect(card.locator('[data-ai-list="rules"]')).toHaveText(String(FIXTURE_RULES));
    await scanCard(page, 'dark');
    await context.close();
  });
});

test('a test server refuses a non-loopback AI blocklist URL', async () => {
  await expect(startBasePathServer('/sw-guard-test', {
    env: { SW_AI_BLOCKLIST_URL: 'https://lists.example.invalid/ai.txt' },
  })).rejects.toThrow(/must be loopback/);
});
