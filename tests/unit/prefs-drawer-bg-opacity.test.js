// Regression tests for the next/ preferences drawer background-opacity slider
// (M55 #1773).
//
// The slider (#pref-d-bg-opacity) previously called window.swUpdateBgOpacity /
// window.swSaveBgOpacity behind a `typeof === 'function'` guard. Those globals
// are defined ONLY by the LEGACY standalone preferences page's inline script
// (web/templates/preferences.templ) and are UNDEFINED on every next/ page, so
// the guarded calls silently no-opped: the slider moved its % label but never
// changed the surface opacity and never persisted.
//
// The fix routes the slider through window.swPreferences (preferences.js),
// which is present on every next/ page:
//   - `input`  (drag):    applySingle('bg_opacity', v)  -> live --sw-glass-bg
//   - `change` (release): set('bg_opacity', v)          -> PUT persist
// These tests prove the wiring works WITH the legacy globals absent, so the
// silent-no-op regression cannot return.
import { describe, it, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import { createDom, makeFetchMock, flush } from './helpers/dom-harness.js';

// Minimal drawer markup: the bg-opacity range slider + its live % label,
// inside the #sw-prefs-drawer container that prefs-drawer.js wires on load.
function drawerHTML(themeClass) {
  return `<!doctype html><html class="${themeClass}"><body>
<div id="sw-prefs-drawer">
  <input type="range" id="pref-d-bg-opacity" min="20" max="100" value="85">
  <span id="pref-d-bg-opacity-value">85%</span>
</div>
</body></html>`;
}

// Build a wired drawer in the given theme. preferences.js loads first so
// window.swPreferences (with applySingle) exists before prefs-drawer.js wires.
function setup(themeClass) {
  const dom = createDom({
    html: drawerHTML(themeClass),
    modules: ['preferences', 'prefsDrawer'],
    csrfToken: 'tok',
  });
  const win = dom.window;
  // Sanity: the legacy globals must NOT exist on a next/-style page. The whole
  // point of the fix is that the drawer no longer depends on them.
  assert.equal(typeof win.swUpdateBgOpacity, 'undefined');
  assert.equal(typeof win.swSaveBgOpacity, 'undefined');
  // Wire the drawer explicitly. In production wire() runs on DOMContentLoaded
  // (or after the HTMX lazy-mount swap); under jsdom the module evals while
  // document.readyState is still 'loading', so it defers to a DOMContentLoaded
  // listener that never fires synchronously. Calling the public entry point is
  // the same path production takes.
  win.swPrefsDrawer.wire();
  return dom;
}

function fireInput(win, slider, value) {
  slider.value = String(value);
  slider.dispatchEvent(new win.Event('input', { bubbles: true }));
}

function fireChange(win, slider, value) {
  slider.value = String(value);
  slider.dispatchEvent(new win.Event('change', { bubbles: true }));
}

describe('prefs-drawer bg-opacity: live preview on input', () => {
  it('dragging an in-range value applies --sw-glass-bg without the legacy globals (dark theme)', () => {
    const dom = setup('dark');
    const win = dom.window;
    const slider = win.document.getElementById('pref-d-bg-opacity');

    // 90 is within [85, 100]; alpha must track the slider exactly.
    fireInput(win, slider, 90);

    // Dark surface base is rgb(30,41,59); alpha tracks the slider (90% -> 0.9).
    assert.equal(
      win.document.documentElement.style.getPropertyValue('--sw-glass-bg'),
      'rgba(30, 41, 59, 0.9)',
    );
    // The % label updates too.
    assert.equal(win.document.getElementById('pref-d-bg-opacity-value').textContent, '90%');
  });

  it('light theme uses the white surface base', () => {
    const dom = setup('');
    const win = dom.window;
    const slider = win.document.getElementById('pref-d-bg-opacity');

    // 90 is within [85, 100]; alpha must track the slider exactly.
    fireInput(win, slider, 90);

    assert.equal(
      win.document.documentElement.style.getPropertyValue('--sw-glass-bg'),
      'rgba(255, 255, 255, 0.9)',
    );
  });

  it('sub-85 value is clamped to the 85% opacity floor (dark theme)', () => {
    const dom = setup('dark');
    const win = dom.window;
    const slider = win.document.getElementById('pref-d-bg-opacity');

    // 50 is below the global 85% floor; the CSS property must render at 0.85.
    fireInput(win, slider, 50);

    assert.equal(
      win.document.documentElement.style.getPropertyValue('--sw-glass-bg'),
      'rgba(30, 41, 59, 0.85)',
    );
  });

  it('sub-85 value is clamped to the 85% opacity floor (light theme)', () => {
    const dom = setup('');
    const win = dom.window;
    const slider = win.document.getElementById('pref-d-bg-opacity');

    // 30 is well below the floor; the CSS property must render at 0.85.
    fireInput(win, slider, 30);

    assert.equal(
      win.document.documentElement.style.getPropertyValue('--sw-glass-bg'),
      'rgba(255, 255, 255, 0.85)',
    );
  });

  it('at 100% the alpha is EXACTLY 1 (fully opaque, zero see-through)', () => {
    const dom = setup('dark');
    const win = dom.window;
    const slider = win.document.getElementById('pref-d-bg-opacity');

    fireInput(win, slider, 100);

    assert.equal(
      win.document.documentElement.style.getPropertyValue('--sw-glass-bg'),
      'rgba(30, 41, 59, 1)',
    );
  });
});

describe('prefs-drawer bg-opacity: persist on change', () => {
  it('release persists via swPreferences.set -> PUT /api/v1/preferences/bg_opacity', async () => {
    const dom = setup('dark');
    const win = dom.window;
    // Replace fetch AFTER setup so we only capture the change-handler call,
    // not preferences.js's own load() GET fired during module eval.
    const fetchMock = makeFetchMock({ ok: true, json: { value: '40' } });
    win.fetch = fetchMock;

    const slider = win.document.getElementById('pref-d-bg-opacity');
    fireChange(win, slider, 40);
    await flush();

    const puts = fetchMock.calls.filter(c => (c.options && c.options.method) === 'PUT');
    assert.equal(puts.length, 1);
    assert.match(puts[0].url, /\/api\/v1\/preferences\/bg_opacity$/);
    assert.equal(JSON.parse(puts[0].options.body).value, '40');
  });

  it('server rejection shows toast and reverts slider + label to previous value', async () => {
    const dom = setup('dark');
    const win = dom.window;

    const toastCalls = [];
    win.showToast = (msg) => toastCalls.push(msg);

    // Simulate a server reject (ok: false). swPreferences.set() will resolve
    // with the previousValue ('85', the DEFAULTS.bg_opacity fallback) rather
    // than rejecting, so the .catch() in the old handler was dead code.
    const fetchMock = makeFetchMock({ ok: false, status: 500 });
    win.fetch = fetchMock;

    const slider = win.document.getElementById('pref-d-bg-opacity');
    const label = win.document.getElementById('pref-d-bg-opacity-value');

    fireChange(win, slider, 40);
    await flush();

    // Toast must fire so the failure is visible.
    assert.equal(toastCalls.length, 1, 'showToast must be called on server reject');

    // Slider and label must revert to the previous value (DEFAULTS.bg_opacity = '85').
    assert.equal(slider.value, '85', 'slider must revert to previous value');
    assert.equal(label.textContent, '85%', 'label must revert to previous value');
  });
});
