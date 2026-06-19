// Regression tests for the next/ preferences-drawer toggle persistence bug
// (#1798 / #2037).
//
// Two linked defects in the SHARED prefs-drawer code caused a user-facing
// persistence regression: toggle a global setting (e.g. Keyboard Hints) off on
// one next/ screen, navigate to another, and the setting reverted.
//
//   (A) LISTENER LEAK (triple-PUT): wire() bound every control's click handler
//       with a bare addEventListener and no dedup guard. The drawer is mounted
//       lazily via an htmx outerHTML swap whose fragment has several root nodes
//       (scrim + drawer + the ContextHelp <script>); htmx fires htmx:afterSwap
//       per swapped node, each matching the #sw-prefs-mount target guard, so
//       wire() ran multiple times and a single toggle click fired N identical
//       PUTs. wireTriggers() already guards against this with a per-node
//       dataset flag; the control bindings did not.
//
//   (B) WRITE LOST ON NAVIGATION: the persist request (swPreferences.set's PUT)
//       was a plain fetch, so navigating immediately after toggling aborted the
//       in-flight write on page unload -> the value never persisted. The fix
//       sets keepalive:true so the write survives navigation.
//
// These tests prove the binding is idempotent (one click = one PUT regardless
// of how many times wire() runs) and that the persist PUT is keepalive.
import { describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { createDom, makeFetchMock, flush } from './helpers/dom-harness.js';

// Minimal drawer markup with two toggles (kbd_hints uses the show/hide
// vocabulary; auto_fetch_images uses true/false) inside the #sw-prefs-drawer
// container that prefs-drawer.js wires.
function drawerHTML() {
  return `<!doctype html><html class="dark"><body>
<div id="sw-prefs-drawer">
  <button id="pref-d-kbd-hints" class="sw-prefs-toggle" role="switch"
          aria-checked="true" data-pref-key="kbd_hints"
          data-pref-on="show" data-pref-off="hide">
    <span class="sw-prefs-toggle-knob"></span>
  </button>
  <button id="pref-d-auto-fetch" class="sw-prefs-toggle" role="switch"
          aria-checked="false" data-pref-key="auto_fetch_images"
          data-pref-on="true" data-pref-off="false">
    <span class="sw-prefs-toggle-knob"></span>
  </button>
</div>
</body></html>`;
}

// Build a drawer wired wireCount times (>=1). Returns { dom, win, fetchMock }
// with a fresh fetch mock installed AFTER wiring so only toggle-click PUTs are
// captured (not preferences.js's own load() GET fired during module eval).
function setup(wireCount = 1) {
  const dom = createDom({
    html: drawerHTML(),
    modules: ['preferences', 'prefsDrawer'],
    csrfToken: 'tok',
  });
  const win = dom.window;
  for (let i = 0; i < wireCount; i++) {
    win.swPrefsDrawer.wire();
  }
  const fetchMock = makeFetchMock({ ok: true, json: { value: 'hide' } });
  win.fetch = fetchMock;
  return { dom, win, fetchMock };
}

function clickToggle(win, id) {
  win.document.getElementById(id).dispatchEvent(new win.Event('click', { bubbles: true }));
}

function puts(fetchMock) {
  return fetchMock.calls.filter(c => (c.options && c.options.method) === 'PUT');
}

describe('prefs-drawer toggle: one click = exactly one PUT', () => {
  it('a single wire() then one kbd_hints click issues exactly ONE PUT', async () => {
    const { win, fetchMock } = setup(1);
    clickToggle(win, 'pref-d-kbd-hints');
    await flush();

    const p = puts(fetchMock);
    assert.equal(p.length, 1, 'exactly one PUT per click');
    assert.match(p[0].url, /\/api\/v1\/preferences\/kbd_hints$/);
    assert.equal(JSON.parse(p[0].options.body).value, 'hide', 'emits the off-value vocabulary');
  });

  it('re-wiring the drawer 3x (repeated htmx:afterSwap) STILL issues ONE PUT', async () => {
    // This is the regression: before the idempotency guard, wire() x3 stacked
    // three listeners so one click fired three identical PUTs.
    const { win, fetchMock } = setup(3);
    clickToggle(win, 'pref-d-kbd-hints');
    await flush();

    assert.equal(puts(fetchMock).length, 1, 'idempotent binding: still one PUT after 3 wire() calls');
  });

  it('the leak fix is not toggle-specific: auto_fetch_images also issues ONE PUT after 3 wire() calls', async () => {
    const { win, fetchMock } = setup(3);
    clickToggle(win, 'pref-d-auto-fetch');
    await flush();

    const p = puts(fetchMock);
    assert.equal(p.length, 1, 'shared fix covers every toggle');
    assert.match(p[0].url, /\/api\/v1\/preferences\/auto_fetch_images$/);
    assert.equal(JSON.parse(p[0].options.body).value, 'true');
  });
});

describe('prefs-drawer toggle: write survives navigation (keepalive)', () => {
  it('the persist PUT sets keepalive:true so an immediate navigation cannot abort it', async () => {
    const { win, fetchMock } = setup(1);
    clickToggle(win, 'pref-d-kbd-hints');
    await flush();

    const p = puts(fetchMock);
    assert.equal(p.length, 1);
    assert.equal(p[0].options.keepalive, true, 'PUT must be keepalive to survive page unload');
  });
});
