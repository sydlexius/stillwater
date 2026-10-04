// setup-restore-dropped-rows.spec.js - first-run setup restore with dropped
// rows, plus the live-region contract of the settings import container (#3012).
//
// Runs on both projects in playwright.config.js (firefox-a11y, chromium-a11y).
//
// FIXTURE: the shared a11y server is bootstrapped (admin created) by
// global-setup.js before any spec runs, so the first-run setup page is not
// reachable on it - the restore endpoint is gated on "no users yet". This spec
// therefore boots its OWN fresh throwaway server (helpers/base-path-server.js
// with skipBootstrap), the same pattern the base-path spec uses, and imports a
// backup in which every row is one the importer drops (helpers/seed-settings-
// import.js). Before trusting the page, the spec asserts the fixture's defining
// property: the server is genuinely un-set-up (the restore form is served).

import { test, expect } from 'playwright/test';

import { disableTransitions } from './helpers/settle.js';
import { buildAxeBuilder, formatViolations, restorePersistedTheme, renderedContrast } from './helpers/axe.js';
import { startBasePathServer } from './helpers/base-path-server.js';
import { IMPORT_PASSPHRASE, REJECTED_KEY, buildDroppedRowsEnvelope } from './helpers/seed-settings-import.js';

let server;

test.beforeAll(async () => {
  server = await startBasePathServer('', { skipBootstrap: true });
});

test.afterAll(() => {
  if (server) server.stop();
});

// The fresh server shares nothing with the harness server: no session, and the
// harness storageState would present a cookie it never minted.
test.use({ storageState: { cookies: [], origins: [] } });

test.beforeEach(async ({ page }) => {
  await disableTransitions(page);
});

test.afterEach(async ({ page }) => {
  await restorePersistedTheme(page);
});

// P1: a failed restore used to leave the container empty (HTMX skips non-2xx).
// Runs BEFORE the restore test below on purpose: a failed restore changes no
// server state, a successful one flips onboarding.completed.
test('a failed first-run restore shows its error in the result container (#3012)', async ({ page }) => {
  await page.goto(`${server.baseURL}/`);
  await expect(page.locator('#setup-restore-form')).toHaveCount(1);
  await page.locator('#card-mode-restore').click();
  await page.locator('#restore-file').setInputFiles({
    name: 'a11y-backup.json',
    mimeType: 'application/json',
    buffer: Buffer.from(buildDroppedRowsEnvelope(IMPORT_PASSPHRASE)),
  });
  await page.locator('#restore-passphrase').fill('not-the-passphrase');
  await page.locator('#setup-restore-form button[type="submit"]').click();
  await expect(page.locator('#setup-restore-result')).toContainText('incorrect passphrase');
});

test('setup restore that drops rows shows the notice and a sign-in link instead of redirecting (#3012)', async ({ page }) => {
  // The restore handler may be driven once per fresh server: it flips
  // onboarding.completed. A second project run gets its own server (beforeAll
  // runs per worker/project), so no cross-talk.
  await page.goto(`${server.baseURL}/`);

  // Fixture property: this really is the un-set-up first-run page.
  await expect(page.locator('#setup-restore-form')).toHaveCount(1);
  const result = page.locator('#setup-restore-result');
  await expect(result).toHaveAttribute('role', 'status');
  await expect(result).toHaveAttribute('aria-live', 'polite');

  await page.locator('#card-mode-restore').click();
  await page.locator('#restore-file').setInputFiles({
    name: 'a11y-backup.json',
    mimeType: 'application/json',
    buffer: Buffer.from(buildDroppedRowsEnvelope(IMPORT_PASSPHRASE, { withUser: true })),
  });
  await page.locator('#restore-passphrase').fill(IMPORT_PASSPHRASE);
  const submitted = page.waitForResponse(
    (r) => r.url().includes('/api/v1/setup/restore') && r.request().method() === 'POST',
  );
  await page.locator('#setup-restore-form button[type="submit"]').click();
  const resp = await submitted;
  expect(resp.status()).toBe(200);
  expect(resp.headers()['hx-redirect'], 'a partial restore must not redirect').toBeUndefined();

  const notice = result.locator('[data-import-drops]');
  await expect(notice).toBeVisible();
  await expect(notice).toContainText('Import completed with dropped rows');
  await expect(notice).toContainText(`Settings rejected as invalid: 1 (${REJECTED_KEY})`);
  await expect(notice).toContainText('Libraries skipped: 1');
  await expect(notice).toContainText('API tokens skipped: 1');
  // Nested live regions would announce the notice twice.
  await expect(result.locator('[role="status"]')).toHaveCount(0);

  const link = result.getByRole('link', { name: 'Continue to sign in' });
  await expect(link).toBeVisible();

  // The whole first-run page has landmarks (content lives in <main>).
  const regions = await buildAxeBuilder(page).withRules(['region']).analyze();
  expect(regions.violations, formatViolations(regions.violations)).toHaveLength(0);

  for (const theme of ['dark', 'light']) {
    // The first-run page has no preferences.js (swPreferences is absent, so
    // applyTheme cannot run) and no inline --sw-glass-bg: its theme is only the
    // `dark` class set by themeInitScript at load, which is what is toggled.
    await page.evaluate((t) => document.documentElement.classList.toggle('dark', t === 'dark'), theme);
    await expect(page.locator('html')).toHaveClass(theme === 'dark' ? /\bdark\b/ : /^(?!.*\bdark\b).*$/);
    await expect(notice).toBeVisible();
    // Scoped to the result container. axe cannot judge contrast here (the card
    // is translucent over an image: color-contrast comes back "incomplete"), so
    // contrast is asserted from rendered pixels below instead.
    const results = await buildAxeBuilder(page).include('#setup-restore-result').analyze();
    expect(
      results.violations,
      `${theme} theme violations with the setup-restore notice shown:\n${formatViolations(results.violations)}`,
    ).toHaveLength(0);

    const button = page.locator('#setup-restore-form button[type="submit"]');
    const measured = {
      'Restore complete line': await renderedContrast(page, result.locator('> div').first()),
      'notice list item': await renderedContrast(page, notice.locator('li').first()),
      'sign-in link': await renderedContrast(page, link),
    };
    await page.mouse.move(0, 0);
    measured['Restore button (rest)'] = await renderedContrast(page, button);
    await button.hover();
    measured['Restore button (hover)'] = await renderedContrast(page, button);
    await page.mouse.move(0, 0);
    for (const [name, ratio] of Object.entries(measured)) {
      expect(ratio, `${theme} theme: ${name} rendered contrast ${ratio.toFixed(2)}:1`).toBeGreaterThanOrEqual(4.5);
    }
  }

  // The link works: onboarding is complete and a user exists now, so it lands on the sign-in page.
  await link.click();
  await expect(page).toHaveURL(`${server.baseURL}/`);
  await expect(page.locator('#setup-restore-form')).toHaveCount(0);
});
