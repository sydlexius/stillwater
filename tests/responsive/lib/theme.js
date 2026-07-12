// theme.js - deterministic theme forcing for the responsive harness.
//
// The app resolves its theme from localStorage['theme'] ('dark' | 'light' |
// 'system'), falling back to prefers-color-scheme only when that key is
// absent (see web/static/js/preferences.js). Dark is the app's DEFAULT
// theme -- if a caller skips this and just navigates, the browser's default
// color scheme resolution takes over and the "evidence" is not deterministic
// (it can silently render light when the intent was dark). ALWAYS call
// setTheme() explicitly for both 'dark' and 'light' runs.
//
// Must be called before page.goto() -- it installs an addInitScript that
// seeds localStorage ahead of the app's first-paint theme script, plus sets
// the browser's prefers-color-scheme media feature as a second, independent
// signal (belt-and-suspenders: matters for pages we haven't audited for a
// stray matchMedia read).
export async function setTheme(page, theme) {
  if (theme !== 'dark' && theme !== 'light') {
    throw new Error(`setTheme: unsupported theme "${theme}" (expected "dark" or "light")`);
  }
  await page.addInitScript((t) => {
    try {
      localStorage.setItem('theme', t);
    } catch {
      // Private-browsing / quota -- non-fatal, matches the app's own guard.
    }
  }, theme);
  await page.emulateMedia({ colorScheme: theme });
}

// verifyTheme reads back the RENDERED state (the .dark class the app's theme
// script applies to <html>) rather than trusting the request we made --
// per the project's rendered-evidence rule, "we asked for dark" is not the
// same claim as "the page is dark". Call after page.goto() / waitForLoadState.
export async function verifyTheme(page, theme, { timeout = 5_000 } = {}) {
  await page.waitForFunction(
    (t) => document.documentElement.classList.contains('dark') === (t === 'dark'),
    theme,
    { timeout },
  );
}

// applyTheme forces the theme through the app's REAL preference-set path
// (window.swPreferences.set), not just localStorage seeding. setTheme()'s
// localStorage/emulateMedia seeding only controls the FIRST paint;
// preferences.js's post-load() call (fired on DOMContentLoaded) fetches the
// account's server-persisted preference and re-applies it, silently
// overwriting a localStorage-only theme within a second of navigation. Any
// authenticated account with a persisted theme opposite of what a run is
// requesting would otherwise flip back before the probes run and the "dark"
// evidence would silently be light (discovered while sample-running this
// harness: every dark-mode run timed out in verifyTheme against the
// harness's own ci/dev admin account, which has a persisted light
// preference). swPreferences.set() is the same call the sidebar theme
// toggle and the tests/a11y light-mode spec use -- it persists to the
// account's server-side preference (via PUT /api/v1/preferences/theme) AND
// applies the DOM class synchronously, so subsequent navigations in the same
// run stay on the requested theme too.
export async function applyTheme(page, theme) {
  // waitForFunction's signature is (pageFunction, arg, options) -- the
  // middle positional param is `arg` (passed into pageFunction), NOT
  // options. Passing { timeout } there silently becomes `arg` and the call
  // falls back to Playwright's 30s default timeout instead of failing fast.
  // `null` here is the (unused) arg; options is the third param.
  await page.waitForFunction(
    () => !!(window.swPreferences && typeof window.swPreferences.set === 'function'),
    null,
    { timeout: 10_000 },
  );
  await page.evaluate((t) => window.swPreferences.set('theme', t), theme);
  await verifyTheme(page, theme);
}
