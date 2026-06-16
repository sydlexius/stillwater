// Tests for the register()/unregister() programmatic-registration API added
// to web/static/js/keyboard.js (#1939 keyboard-shortcut parity). These APIs
// allow pages to expose inline-script-handled shortcuts to the shared registry
// so the help overlay (list()) sees them alongside DOM-discovered shortcuts.
import { describe, it, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import { createDom } from './helpers/dom-harness.js';

// Create a fresh DOM with keyboard.js loaded and a minimal action-queue page
// that mirrors the dashboard's roving-list structure. The data-sw-shortcut
// attribute on the search input makes the registry non-empty after rebuild(),
// exercising the combined list() path.
function setup(extraHtml = '') {
  const dom = createDom({
    html: `<!doctype html><html><body>
      <input id="search" type="search" data-sw-shortcut="/" data-sw-shortcut-label="Search">
      ${extraHtml}
    </body></html>`,
    modules: ['keyboard'],
  });
  // Force a rebuild so the DOM-discovered entries are in the registry. In
  // jsdom the initial DOMContentLoaded listener may not fire after win.eval(),
  // so we call rebuild() explicitly to mirror the real-browser behavior.
  dom.window.swKeyboardShortcuts.rebuild();
  return dom.window;
}

describe('keyboard.js: register() / unregister() API', () => {
  it('window.swKeyboardShortcuts.register is a function', () => {
    const win = setup();
    assert.equal(typeof win.swKeyboardShortcuts.register, 'function',
      'register must be exported on window.swKeyboardShortcuts');
  });

  it('window.swKeyboardShortcuts.unregister is a function', () => {
    const win = setup();
    assert.equal(typeof win.swKeyboardShortcuts.unregister, 'function',
      'unregister must be exported on window.swKeyboardShortcuts');
  });

  it('register() appends entries to the combined list()', () => {
    const win = setup();
    const kbs = win.swKeyboardShortcuts;

    kbs.register('artist-detail', [
      { key: 'j', label: 'Next section' },
      { key: 'k', label: 'Previous section' },
    ]);

    const entries = kbs.list();
    const keys = entries.map(e => e.key);
    assert.ok(keys.includes('j'), 'list() must include registered key "j"');
    assert.ok(keys.includes('k'), 'list() must include registered key "k"');
  });

  it('registered entries have kind:"manual" and the given scope', () => {
    const win = setup();
    const kbs = win.swKeyboardShortcuts;

    kbs.register('my-page', [{ key: 'e', label: 'Edit' }]);

    const entry = kbs.list().find(e => e.key === 'e');
    assert.ok(entry, 'registered entry must appear in list()');
    assert.equal(entry.kind,  'manual',  'kind must be "manual"');
    assert.equal(entry.scope, 'my-page', 'scope must match the argument passed to register()');
    assert.equal(entry.label, 'Edit',    'label must be preserved verbatim');
  });

  it('list() includes both DOM-discovered and manually-registered entries', () => {
    const win = setup(); // DOM has data-sw-shortcut="/" so "/" is DOM-discovered
    const kbs = win.swKeyboardShortcuts;

    kbs.register('artist-detail', [{ key: 'e', label: 'Edit' }]);

    const entries = kbs.list();
    const slashEntry = entries.find(e => e.key === '/');
    const eEntry     = entries.find(e => e.key === 'e');

    assert.ok(slashEntry, '/ (DOM-discovered) must appear in list()');
    assert.ok(eEntry,     'e (manual) must appear in list()');
    assert.equal(slashEntry.kind, 'action', 'DOM-discovered entry must have kind "action"');
    assert.equal(eEntry.kind,     'manual', 'manual entry must have kind "manual"');
  });

  it('rebuild() does not clear manually-registered entries', () => {
    const win = setup();
    const kbs = win.swKeyboardShortcuts;

    kbs.register('artist-detail', [{ key: 'j', label: 'Next section' }]);
    kbs.rebuild(); // simulates an HTMX swap

    const entry = kbs.list().find(e => e.key === 'j' && e.kind === 'manual');
    assert.ok(entry, 'manual entry must survive a rebuild() call');
  });

  it('unregister() removes all entries for the given scope', () => {
    const win = setup();
    const kbs = win.swKeyboardShortcuts;

    kbs.register('artist-detail', [
      { key: 'j', label: 'Next section' },
      { key: 'k', label: 'Previous section' },
    ]);
    kbs.register('other-scope', [{ key: 'x', label: 'Other' }]);

    kbs.unregister('artist-detail');

    const list = kbs.list();
    const jEntry = list.find(e => e.key === 'j' && e.scope === 'artist-detail');
    const kEntry = list.find(e => e.key === 'k' && e.scope === 'artist-detail');
    const xEntry = list.find(e => e.key === 'x' && e.scope === 'other-scope');

    assert.equal(jEntry, undefined, '"j" entry for artist-detail must be removed');
    assert.equal(kEntry, undefined, '"k" entry for artist-detail must be removed');
    assert.ok(xEntry, '"x" entry for other-scope must NOT be removed');
  });

  it('unregister() of an unknown scope is a no-op and does not throw', () => {
    const win = setup();
    const kbs = win.swKeyboardShortcuts;

    kbs.register('artist-detail', [{ key: 'j', label: 'Next section' }]);

    assert.doesNotThrow(
      () => kbs.unregister('nonexistent-scope'),
      'unregister on unknown scope must not throw',
    );

    // The existing registration must be unaffected.
    const entry = kbs.list().find(e => e.key === 'j' && e.scope === 'artist-detail');
    assert.ok(entry, 'existing manual entry must survive unregister of a different scope');
  });

  it('register() with a non-array entries arg does not throw and emits console.error', () => {
    const win = setup();
    const kbs = win.swKeyboardShortcuts;

    const errors = [];
    win.console.error = (...args) => errors.push(args.join(' '));

    assert.doesNotThrow(
      () => kbs.register('bad-caller', 'not-an-array'),
      'register() must not throw on invalid entries arg',
    );
    assert.ok(errors.length > 0, 'register() must emit console.error on invalid entries arg');
  });

  it('list() returns a snapshot (mutating it does not affect the registry)', () => {
    const win = setup();
    const kbs = win.swKeyboardShortcuts;

    kbs.register('artist-detail', [{ key: 'j', label: 'Next section' }]);
    const snap1 = kbs.list();
    snap1.push({ key: 'INJECTED', label: '', scope: '', kind: 'manual' });

    const snap2 = kbs.list();
    assert.equal(
      snap2.find(e => e.key === 'INJECTED'),
      undefined,
      'Mutating the list() snapshot must not affect the internal registry',
    );
  });
});
