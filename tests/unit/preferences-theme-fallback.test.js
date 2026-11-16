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
function lightPage(fetchStub) {
  const dom = new JSDOM(HTML, { runScripts: 'dangerously', url: 'http://localhost:1973/' });
  const win = dom.window;
  win.localStorage.setItem('theme', 'light');
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
