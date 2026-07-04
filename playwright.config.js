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

import { STORAGE_STATE } from './tests/a11y/global-setup.js';

const port = process.env.SW_PORT || '1973';
const baseURL = process.env.SW_TEST_URL || `http://127.0.0.1:${port}`;

export default defineConfig({
  testDir: './tests/a11y',
  // Authenticate ONCE for the whole run (avoids tripping the login rate
  // limiter); every test context loads the resulting session via storageState.
  globalSetup: './tests/a11y/global-setup.js',
  // A genuine violation still fails on every attempt; a load-induced
  // transient (the CPU-starved theme-toggle timeout root-caused in #2223)
  // self-heals on retry.
  retries: 2,
  // Single worker: tests authenticate sequentially against the ephemeral server.
  workers: 1,
  // Wall-clock budget per test. These tests drive a real booted server +
  // Chromium and the /next/settings light-mode test does several sequential
  // steps (navigate, wait for sidebar JS, theme toggle, settle); 30s was tight
  // enough to false-timeout under CI/parallel load, so give headroom.
  timeout: 60_000,
  // Concise reporter for CI; full verbose logs on failure.
  reporter: [['list'], ['html', { open: 'never', outputFolder: 'tests/a11y/report' }]],

  use: {
    baseURL,
    // Pre-authenticated session captured once in global-setup; replaces the
    // former per-spec-file login + per-test addCookies plumbing.
    storageState: STORAGE_STATE,
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
