// settings-import-dropped-rows.spec.js - Playwright a11y coverage for the
// Settings import summary when the import drops rows (#3012).
//
// Runs on both projects in playwright.config.js (firefox-a11y, chromium-a11y).
//
// A partial restore must not read as a complete one. The server-side handler
// tests prove the fragment's text; this spec proves an operator can SEE it:
// the warning is rendered, announced via the container live region, names the rejected key
// and counts, and the page stays axe-clean in both themes with it present.
//
// FIXTURE: the harness database is empty, so the spec builds its own encrypted
// backup (helpers/seed-settings-import.js) and imports it. Before trusting
// the page it imports the same file through the API and asserts the server
// really reports non-zero dropped counters, so a fixture that stopped dropping
// anything fails there, not as a confusing missing-warning failure.
//
// The import rejects or skips every row in the file, so it writes nothing and
// cannot affect other specs.

import { test, expect } from 'playwright/test';

import { disableTransitions } from './helpers/settle.js';
import { buildAxeBuilder, formatViolations, applyTheme, restorePersistedTheme } from './helpers/axe.js';
import {
  IMPORT_PASSPHRASE, REJECTED_KEY, EXPECTED_COUNTS,
  buildDroppedRowsEnvelope, importViaApi,
} from './helpers/seed-settings-import.js';

test.beforeEach(async ({ page }) => {
  await disableTransitions(page);
});

test.afterEach(async ({ page }) => {
  await restorePersistedTheme(page);
});

test('an import that drops rows renders a visible, accessible warning (#3012)', async ({ page, request }) => {
  const envelope = buildDroppedRowsEnvelope();

  // Fixture property: the server really reports every dropped counter.
  const result = await importViaApi(request, envelope);
  for (const [field, want] of Object.entries(EXPECTED_COUNTS)) {
    expect(result[field], `fixture must make ${field} non-zero`).toBe(want);
  }
  expect(result.settings_rejected_keys, 'fixture must name the rejected key').toEqual([REJECTED_KEY]);

  // Drive the real UI: the HTMX import form on the Settings page.
  await page.goto('/settings');
  const fileInput = page.locator('#import-file');
  await fileInput.scrollIntoViewIfNeeded();
  await expect(fileInput).toBeVisible();
  await fileInput.setInputFiles({
    name: 'a11y-backup.json',
    mimeType: 'application/json',
    buffer: Buffer.from(envelope),
  });
  await page.locator('#import-passphrase').fill(IMPORT_PASSPHRASE);
  const submitted = page.waitForResponse(
    (r) => r.url().includes('/api/v1/settings/import') && r.request().method() === 'POST',
  );
  await page.locator('#import-passphrase').locator('xpath=ancestor::form').locator('button[type="submit"]').click();
  expect((await submitted).status()).toBe(200);

  const summary = page.locator('#import-result');
  // The container is the (single) polite live region for the swapped-in result.
  await expect(summary).toHaveAttribute('role', 'status');
  await expect(summary).toHaveAttribute('aria-live', 'polite');
  await expect(summary).toContainText('Import complete:');

  const warning = summary.locator('[data-import-drops]');
  await expect(warning).toBeVisible();
  await expect(warning).toContainText('Import completed with dropped rows');
  await expect(warning).toContainText(`Settings rejected as invalid: 1 (${REJECTED_KEY})`);
  await expect(warning).toContainText('Settings discarded because the file also carried the current name: 1');
  await expect(warning).toContainText('Libraries skipped: 1');
  await expect(warning).toContainText('API tokens skipped: 1');

  // The warning is the thing under test, so keep it on screen for the scans.
  await warning.scrollIntoViewIfNeeded();
  for (const theme of ['dark', 'light']) {
    await applyTheme(expect, page, theme);
    await expect(warning).toBeVisible();
    const results = await buildAxeBuilder(page).analyze();
    expect(
      results.violations,
      `${theme} theme violations with the dropped-rows warning shown:\n${formatViolations(results.violations)}`,
    ).toHaveLength(0);
  }
});
