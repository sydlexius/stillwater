// Regression tests: preferences.js must never replace the theme already on the
// page with DEFAULTS.theme ('dark') when it has no session cache, nor persist
// 'dark' when the preferences GET fails. A light user must stay light.
//
// themeInitScript (not under test here) has already applied the user's theme
// before preferences.js runs, so each test starts from <html class="" data-theme="light">
// with localStorage theme=light and an empty sessionStorage.
import { describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { JSDOM } from 'jsdom';
import { MODULE_PATHS, makeFetchMock, flush } from './helpers/dom-harness.js';

const HTML = '<!doctype html><html class="" data-theme="light"><body></body></html>';

// Build a light-theme page and eval preferences.js with the given fetch stub.
// Returns the window; the module's own startup load() has already been invoked
// (immediately or on DOMContentLoaded, per jsdom version).
function lightPage(fetchStub, { storedTheme = 'light', html = HTML, osDark } = {}) {
  const dom = new JSDOM(html, { runScripts: 'dangerously', url: 'http://localhost:1973/' });
  const win = dom.window;
  if (osDark !== undefined) {
    // jsdom has no matchMedia; stub the OS colour-scheme query.
    win.matchMedia = (q) => ({ matches: /dark/.test(q) ? osDark : !osDark, media: q,
      addEventListener() {}, removeEventListener() {} });
  }
  if (storedTheme !== null) win.localStorage.setItem('theme', storedTheme);
  assert.equal(win.sessionStorage.length, 0, 'precondition: no session cache');
  win.fetch = fetchStub;
  win.eval(readFileSync(MODULE_PATHS.preferences, 'utf-8'));
  return win;
}

function assertLight(win, msg) {
  const root = win.document.documentElement;
  assert.equal(root.classList.contains('dark'), false, `${msg}: dark class must stay off`);
  assert.equal(root.getAttribute('data-theme'), 'light', `${msg}: data-theme must stay light`);
}

// Wait until the document has finished parsing so any DOMContentLoaded-deferred
// load() has run (a no-op on jsdom versions that return already parsed).
function domReady(win) {
  if (win.document.readyState !== 'loading') return Promise.resolve();
  return new Promise((res) => win.document.addEventListener('DOMContentLoaded', res));
}

describe('preferences load with no session cache keeps the on-page theme', () => {
  it('stays light while the API call is still pending', async () => {
    const win = lightPage(() => new Promise(() => {})); // never settles
    await domReady(win);
    // Also drive the public load path explicitly.
    win.swPreferences.load();
    await flush();
    assertLight(win, 'before the fetch settles');
  });
});

describe('preferences GET failure persists nothing and keeps the theme', () => {
  it('HTTP 500: page stays light, caches untouched', async () => {
    const stub = makeFetchMock({ ok: false, status: 500 });
    const win = lightPage(stub);
    await domReady(win);
    await win.swPreferences.load();
    await flush();
    assertLight(win, 'after 500');
    assert.equal(win.localStorage.getItem('theme'), 'light', 'localStorage theme not overwritten');
    assert.equal(win.sessionStorage.getItem('sw-preferences'), null, 'session cache not written');
  });

  it('network rejection: page stays light, caches untouched', async () => {
    const win = lightPage(() => Promise.reject(new Error('offline')));
    await domReady(win);
    await win.swPreferences.load();
    await flush();
    assertLight(win, 'after rejection');
    assert.equal(win.localStorage.getItem('theme'), 'light');
    assert.equal(win.sessionStorage.getItem('sw-preferences'), null);
  });
});

describe('no stored theme preference (first visit) with a failed GET', () => {
  // themeInitScript resolved the OS (light) before first paint: no class, data-theme light.
  it('HTTP 500, OS light: stays light, no theme key written, no dark flash', async () => {
    const classLog = [];
    const stub = makeFetchMock({ ok: false, status: 500 });
    const win = lightPage(stub, { storedTheme: null, osDark: false });
    const root = win.document.documentElement;
    new win.MutationObserver(() => classLog.push(root.className)).observe(root, { attributes: true });
    await domReady(win);
    await win.swPreferences.load();
    await flush();
    assert.equal(win.localStorage.getItem('theme'), null, 'no-preference user still has no theme key');
    assert.equal(root.classList.contains('dark'), false, 'OS light: no dark class');
    assert.equal(root.getAttribute('data-theme'), 'system', 'follows the system setting');
    assert.equal(classLog.some((c) => /dark/.test(c)), false, 'never flashed dark');
    assert.equal(win.sessionStorage.getItem('sw-preferences'), null);
  });

  it('HTTP 500, OS dark: follows the OS (dark), still no theme key', async () => {
    const win = lightPage(makeFetchMock({ ok: false, status: 500 }), {
      storedTheme: null, osDark: true,
      html: '<!doctype html><html class="dark" data-theme="dark"><body></body></html>' });
    await domReady(win);
    await win.swPreferences.load();
    await flush();
    assert.equal(win.localStorage.getItem('theme'), null);
    assert.equal(win.document.documentElement.classList.contains('dark'), true);
  });
});
