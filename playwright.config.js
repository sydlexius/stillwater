// playwright.config.js - Playwright configuration for the a11y smoke tier.
//
// This tier runs axe-core against a real Chromium browser attached to an
// ephemeral Stillwater server (the bruno-ci ephemeral-server pattern). It
// catches computed-style contrast failures that jsdom cannot detect.
//
// The server URL is supplied via the SW_TEST_URL env var (set by the
// make test-a11y target). If absent, we fall back to SW_PORT. The default
// port 1973 is the local dev server.

import { defineConfig, devices } from 'playwright/test';

const port = process.env.SW_PORT || '1973';
const baseURL = process.env.SW_TEST_URL || `http://127.0.0.1:${port}`;

export default defineConfig({
  testDir: './tests/a11y',
  // Deterministic: no retries so a failure is a failure.
  retries: 0,
  // Single worker: tests authenticate sequentially against the ephemeral server.
  workers: 1,
  // Reasonable wall-clock budget for a smoke set.
  timeout: 30_000,
  // Concise reporter for CI; full verbose logs on failure.
  reporter: [['list'], ['html', { open: 'never', outputFolder: 'tests/a11y/report' }]],

  use: {
    baseURL,
    // Headless Chromium for CI. The a11y scan needs a real rendering engine for
    // CSS cascade + contrast calculations.
    headless: true,
    // Capture screenshots + traces on failure for debugging.
    screenshot: 'only-on-failure',
    trace: 'on-first-retry',
    // Force dark mode so we exercise the dark-theme color tokens.
    // (Admin default theme is "system"; CI runners report prefers-color-scheme:dark
    // via this setting.)
    colorScheme: 'dark',
  },

  projects: [
    {
      name: 'chromium-a11y',
      use: { ...devices['Desktop Chrome'] },
    },
  ],
});
