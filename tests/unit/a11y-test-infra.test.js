// a11y-test-infra.test.js - regression coverage for the a11y smoke-tier test
// infrastructure changes (the "smoke suite reliable" prereq for #2120):
//
//   - tests/a11y/global-setup.js: ONE login for the whole Playwright run,
//     persisted to a gitignored storageState file.
//   - playwright.config.js: wires that global setup + storageState, and
//     widened the per-test timeout.
//   - tests/a11y/{cheat-sheet,contrast}.spec.js: no longer perform their own
//     per-spec-file login / per-test cookie injection (that is what tripped
//     the production auth rate limiter), and now settle CSS
//     transitions/animations before each axe scan.
//
// These are config/wiring files consumed by the Playwright test runner, not
// plain functions, so the tests below check shape and regression guards
// (string content) rather than driving a real browser -- the actual scans
// still run end-to-end under `make test-a11y`.
import { describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { resolve, dirname, join, sep } from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = resolve(__dirname, '../..');

const GLOBAL_SETUP_PATH = join(REPO_ROOT, 'tests/a11y/global-setup.js');
const PLAYWRIGHT_CONFIG_PATH = join(REPO_ROOT, 'playwright.config.js');

describe('tests/a11y/global-setup.js', () => {
  it('exports a STORAGE_STATE path under the gitignored tests/a11y/.auth directory', async () => {
    const { STORAGE_STATE } = await import(GLOBAL_SETUP_PATH);
    assert.equal(typeof STORAGE_STATE, 'string');
    assert.ok(
      STORAGE_STATE.endsWith(['tests', 'a11y', '.auth', 'state.json'].join(sep)),
      `STORAGE_STATE = ${STORAGE_STATE}, want it under tests/a11y/.auth/state.json`,
    );
  });

  it('exports a default globalSetup function (the one-time auth hook)', async () => {
    const mod = await import(GLOBAL_SETUP_PATH);
    assert.equal(typeof mod.default, 'function');
  });

  it('tests/a11y/.auth/ (the STORAGE_STATE directory) is gitignored', () => {
    const gitignore = readFileSync(join(REPO_ROOT, '.gitignore'), 'utf-8');
    assert.match(
      gitignore,
      /^tests\/a11y\/\.auth\/$/m,
      'tests/a11y/.auth/ must be gitignored -- it holds a captured session cookie',
    );
  });
});

describe('playwright.config.js', () => {
  it('wires globalSetup + storageState from global-setup.js, single worker, no retries', async () => {
    const { default: config } = await import(PLAYWRIGHT_CONFIG_PATH);
    const { STORAGE_STATE } = await import(GLOBAL_SETUP_PATH);

    assert.equal(config.testDir, './tests/a11y');
    assert.match(config.globalSetup, /global-setup\.js$/);
    assert.equal(config.retries, 0, 'a11y smoke tier must stay deterministic: no retries');
    assert.equal(config.workers, 1, 'tests must authenticate/run sequentially against the ephemeral server');
    assert.equal(config.use.storageState, STORAGE_STATE);
  });

  it('keeps the widened (>= 60s) per-test timeout (regression: 30s false-timed-out the settings light-mode test)', async () => {
    const { default: config } = await import(PLAYWRIGHT_CONFIG_PATH);
    assert.ok(config.timeout >= 60_000, `config.timeout = ${config.timeout}, want >= 60000`);
  });

  it('forces headless Chromium with dark colorScheme for contrast/CSS-token checks', async () => {
    const { default: config } = await import(PLAYWRIGHT_CONFIG_PATH);
    assert.equal(config.use.headless, true);
    assert.equal(config.use.colorScheme, 'dark');
  });
});

describe('tests/a11y/*.spec.js no longer perform their own per-file login', () => {
  const SPEC_FILES = ['cheat-sheet.spec.js', 'contrast.spec.js'];

  for (const file of SPEC_FILES) {
    it(`${file} does not import or call setupAndLogin (auth now comes once from global-setup.js)`, () => {
      const src = readFileSync(join(REPO_ROOT, 'tests/a11y', file), 'utf-8');
      // Match actual usage (an import of the helper, or a call), not prose --
      // this intentionally tolerates stray comments that merely mention the
      // name without reintroducing the per-file login.
      assert.doesNotMatch(
        src,
        /from\s+['"]\.\/helpers\/bootstrap\.js['"]|setupAndLogin\(/,
        `${file} must not reintroduce its own beforeAll login -- that is what tripped the auth rate limiter`,
      );
    });

    it(`${file} does not manually inject a session cookie via page.context().addCookies`, () => {
      const src = readFileSync(join(REPO_ROOT, 'tests/a11y', file), 'utf-8');
      assert.doesNotMatch(
        src,
        /addCookies\(/,
        `${file} must rely on the pre-authenticated storageState instead of per-test addCookies`,
      );
    });

    it(`${file} disables CSS transitions/animations before each test so axe reads settled colors`, () => {
      const src = readFileSync(join(REPO_ROOT, 'tests/a11y', file), 'utf-8');
      assert.match(src, /test\.beforeEach/, `${file} should set up the transition guard in a beforeEach hook`);
      assert.match(
        src,
        /transition:\s*none\s*!important/,
        `${file} should force "transition: none !important" so a synchronous color-contrast read can't sample a mid-transition color`,
      );
    });
  }
});