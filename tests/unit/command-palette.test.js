import { describe, it, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { createDom } from './helpers/dom-harness.js';

const __dirname = dirname(fileURLToPath(import.meta.url));
const KEYBOARD_PATH = resolve(__dirname, '../../web/static/js/keyboard.js');

const HTML = '<!doctype html><html><head>' +
  '<meta name="htmx-base-path" content="">' +
  '</head><body>' +
  '<div id="sw-cmdk" class="hidden"><input id="sw-cmdk-input">' +
  '<div data-cmdk-list></div><div id="sw-cmdk-empty" class="hidden"></div></div>' +
  '</body></html>';

function withPalette() {
  const dom = createDom({ html: HTML, modules: ['command-palette'] });
  // Stub the registry the index reads from.
  dom.window.swKeyboardShortcuts = {
    list() {
      return [
        { key: 'g d', label: 'Go to Dashboard', scope: 'global', kind: 'manual' },
        { key: 'g a', label: 'Go to Artists', scope: 'global', kind: 'manual' },
        { key: 'j', label: 'next', scope: 'artists', kind: 'roving' },
      ];
    },
    register() {},
  };
  return dom;
}

describe('command palette index', () => {
  it('buildIndex maps g-leader entries to screen commands', () => {
    const dom = withPalette();
    const list = dom.window.swCommandPalette.buildIndex(dom.window.swKeyboardShortcuts.list());
    const screens = list.filter((i) => i.kind === 'screen');
    assert.equal(screens.length, 2);
    const dash = screens.find((s) => s.label === 'Go to Dashboard');
    // dash.shortcut is an Array from the jsdom realm (module eval'd via
    // win.eval); spread it into this realm's Array before deepEqual, since
    // assert/strict's deepEqual is deepStrictEqual and cross-realm arrays
    // are never reference-equal even with identical contents.
    assert.deepEqual([...dash.shortcut], ['g', 'd']);
    assert.equal(dash.href, '/next/');
  });

  it('buildIndex includes the curated actions', () => {
    const dom = withPalette();
    const list = dom.window.swCommandPalette.buildIndex(dom.window.swKeyboardShortcuts.list());
    const actions = list.filter((i) => i.kind === 'action').map((a) => a.id);
    assert.ok(actions.includes('act-theme'));
    assert.ok(actions.includes('act-run-rules'));
    assert.ok(actions.includes('act-fix-all'));
  });

  it('match is case-insensitive substring over label + keywords', () => {
    const dom = withPalette();
    const m = dom.window.swCommandPalette.match;
    assert.equal(m({ label: 'Go to Artists', keywords: [] }, 'art'), true);
    assert.equal(m({ label: 'Run all rules', keywords: ['validate'] }, 'valid'), true);
    assert.equal(m({ label: 'Go to Artists', keywords: [] }, 'xyz'), false);
    assert.equal(m({ label: 'Anything', keywords: [] }, ''), true);
  });

  it('buildIndex settings entries deep-link to a #section- anchor the rail can jump to (#1775 hostile-review finding 1)', () => {
    const dom = withPalette();
    const list = dom.window.swCommandPalette.buildIndex(dom.window.swKeyboardShortcuts.list());
    const settings = list.filter((i) => i.kind === 'setting');
    assert.ok(settings.length > 0);
    settings.forEach((s) => {
      assert.match(s.href, /^\/settings#section-/, `setting "${s.label}" href "${s.href}" must target a #section- anchor`);
    });
  });
});

describe('command palette open/hide/render', () => {
  it('open shows the root and renders rows; hide re-hides + clears query', () => {
    const dom = withPalette();
    const p = dom.window.swCommandPalette;
    const root = dom.window.document.getElementById('sw-cmdk');
    p.open();
    assert.equal(root.classList.contains('hidden'), false);
    const rows = dom.window.document.querySelectorAll('[data-cmdk-list] .sw-cmdk-row');
    assert.ok(rows.length >= 5); // 2 screens + 5 settings/actions minimum
    p.hide();
    assert.equal(root.classList.contains('hidden'), true);
    assert.equal(dom.window.document.getElementById('sw-cmdk-input').value, '');
  });

  it('groups rows under SCREENS/SETTINGS/ACTIONS section labels without disturbing row indices', () => {
    const dom = withPalette();
    const p = dom.window.swCommandPalette;
    p.open();
    const labels = dom.window.document.querySelectorAll('[data-cmdk-list] .sw-cmdk-section-label');
    assert.equal(labels.length, 3);
    assert.deepEqual([...labels].map((l) => l.textContent), ['Screens', 'Settings', 'Actions']);
    const rows = dom.window.document.querySelectorAll('[data-cmdk-list] .sw-cmdk-row');
    [...rows].forEach((row, i) => {
      assert.equal(row.getAttribute('data-idx'), String(i));
    });
  });

  it('screen rows render a two-key shortcut chip pair; non-screen rows render a kind tag instead', () => {
    const dom = withPalette();
    const p = dom.window.swCommandPalette;
    p.open();
    const rows = [...dom.window.document.querySelectorAll('[data-cmdk-list] .sw-cmdk-row')];
    const screenRows = rows.filter((r) => r.querySelector('.sw-cmdk-row-shortcut'));
    const kindRows = rows.filter((r) => r.querySelector('.sw-cmdk-row-kind'));
    // Two 'g'-leader entries from the stubbed registry -> two shortcut chip rows.
    assert.equal(screenRows.length, 2);
    screenRows.forEach((row) => {
      const chips = row.querySelectorAll('.sw-cmdk-row-shortcut kbd.sw-kbd');
      assert.equal(chips.length, 2, 'a screen row with a valid shortcut renders exactly a two-key chip pair');
      assert.equal(row.querySelector('.sw-cmdk-row-kind'), null, 'a shortcut-chip row must not also render a kind tag');
    });
    // Every non-screen row (settings + actions) falls back to a plain kind tag.
    assert.ok(kindRows.length > 0);
    kindRows.forEach((row) => {
      assert.equal(row.querySelector('.sw-cmdk-row-shortcut'), null, 'a kind-tag row must not also render shortcut chips');
    });
  });

  it('typing filters the rows and shows empty state on no match', () => {
    const dom = withPalette();
    const p = dom.window.swCommandPalette;
    const input = dom.window.document.getElementById('sw-cmdk-input');
    p.open();
    input.value = 'artists';
    input.dispatchEvent(new dom.window.Event('input'));
    const rows = dom.window.document.querySelectorAll('[data-cmdk-list] .sw-cmdk-row');
    assert.equal(rows.length, 1);
    input.value = 'zzznomatch';
    input.dispatchEvent(new dom.window.Event('input'));
    assert.equal(dom.window.document.getElementById('sw-cmdk-empty').classList.contains('hidden'), false);
  });
});

describe('command palette dispatch', () => {
  function domWithStubs() {
    const dom = withPalette();
    const w = dom.window;
    w.swNavigate = (url) => { w.__nav = url; };
    w.swSidebar = { cycleTheme() { w.__theme = (w.__theme || 0) + 1; } };
    w.swPrefsDrawer = { open() { w.__prefs = (w.__prefs || 0) + 1; } };
    w.swCsrfToken = () => 'csrf123';
    w.showSuccessToast = (m) => { w.__ok = m; };
    w.showWarningToast = (m) => { w.__warn = m; };
    w.__posts = [];
    w.fetch = (url, opts) => { w.__posts.push({ url, opts }); return Promise.resolve({ ok: true }); };
    return dom;
  }

  it('screen item navigates with base path', () => {
    const dom = domWithStubs();
    dom.window.swCommandPalette.activate({ kind: 'screen', href: '/next/artists' });
    assert.equal(dom.window.__nav, '/next/artists');
  });

  it('theme + prefs actions call their client fns', () => {
    const dom = domWithStubs();
    dom.window.swCommandPalette.activate({ kind: 'action', run: 'theme' });
    dom.window.swCommandPalette.activate({ kind: 'action', run: 'prefs' });
    assert.equal(dom.window.__theme, 1);
    assert.equal(dom.window.__prefs, 1);
  });

  it('post action POSTs with CSRF header', async () => {
    const dom = domWithStubs();
    await dom.window.swCommandPalette.activate({ kind: 'action', run: 'post', href: '/api/v1/scanner/run' });
    const call = dom.window.__posts[0];
    assert.equal(call.opts.method, 'POST');
    assert.equal(call.opts.headers['X-CSRF-Token'], 'csrf123');
  });

  it('confirm-post requires a second activate before POSTing', async () => {
    const dom = domWithStubs();
    const item = { kind: 'action', run: 'confirm-post', href: '/api/v1/notifications/fix-all', id: 'act-fix-all' };
    await dom.window.swCommandPalette.activate(item); // arms confirm, no POST
    assert.equal(dom.window.__posts.length, 0);
    await dom.window.swCommandPalette.activate(item); // confirmed
    assert.equal(dom.window.__posts.length, 1);
  });

  it('missing client target logs error + warns, no throw', () => {
    const dom = domWithStubs();
    delete dom.window.swSidebar;
    dom.window.swCommandPalette.activate({ kind: 'action', run: 'theme' });
    assert.ok(dom.window.__warn);
  });
});

describe('command palette keyboard nav', () => {
  it('ArrowDown then Enter activates the second row', () => {
    const dom = withPalette();
    const w = dom.window;
    const p = w.swCommandPalette;
    p.open();
    const rows = w.document.querySelectorAll('[data-cmdk-list] .sw-cmdk-row');
    assert.ok(rows.length >= 2);
    const secondId = rows[1].getAttribute('data-id');
    let activated = null;
    const origActivate = p.activate;
    p.activate = (item) => { activated = item; return origActivate.call(p, item); };
    w.document.dispatchEvent(new w.KeyboardEvent('keydown', { key: 'ArrowDown' }));
    w.document.dispatchEvent(new w.KeyboardEvent('keydown', { key: 'Enter' }));
    assert.equal(activated && activated.id, secondId);
  });
});

describe('Cmd-K keyboard integration (#1775)', () => {
  it('Cmd-K toggles the palette on next/ pages and ⌘K is registered', () => {
    const dom = createDom({
      html: '<!doctype html><html><head><meta name="htmx-base-path" content=""></head><body></body></html>',
      modules: [],
    });
    // The isNextPage() seam must be set BEFORE keyboard.js is eval'd, since the
    // global-shortcut registration (⌘K, g-leader, etc.) runs once at module
    // load time (see keyboard-gleader.test.js for the same pattern).
    dom.window.SW_IS_NEXT_PAGE = true;
    dom.window.eval(readFileSync(KEYBOARD_PATH, 'utf-8'));
    dom.window.swKeyboardShortcuts.rebuild();

    let toggled = 0;
    dom.window.swCommandPalette = { toggle() { toggled++; }, hide() {} };
    dom.window.document.dispatchEvent(new dom.window.KeyboardEvent('keydown', { key: 'k', metaKey: true }));
    assert.equal(toggled, 1);

    const keys = dom.window.swKeyboardShortcuts.list().map((e) => e.key);
    assert.ok(keys.includes('⌘K'));
  });

  // #2768: the palette DOM and window.swCommandPalette are mounted
  // unconditionally by Layout, so Cmd-K must also fire on stable-channel
  // (non-next/) pages -- the isNextPage() conjunct that used to gate it was
  // stale. Assert the observable effect (the #sw-cmdk hidden class), not an
  // internal call count.
  //
  // The event MUST be constructed with cancelable:true. A KeyboardEvent built
  // without it reports defaultPrevented === false no matter what the handler
  // does, so the preventDefault() assertion below would pass vacuously against
  // a build that never called it at all (verified by mutation: deleting the
  // preventDefault() left the whole suite green before this flag was added).
  it('Cmd-K toggles the palette on stable-channel (non-next/) pages and claims the key', () => {
    const dom = createDom({
      html: '<!doctype html><html><head><meta name="htmx-base-path" content=""></head>' +
        '<body><div id="sw-cmdk" class="hidden"></div></body></html>',
      modules: [],
    });
    dom.window.SW_IS_NEXT_PAGE = false;
    dom.window.eval(readFileSync(KEYBOARD_PATH, 'utf-8'));

    const cmdk = dom.window.document.getElementById('sw-cmdk');
    // Precondition: the palette starts hidden, so the assertion below measures
    // a real transition rather than an already-satisfied state.
    assert.equal(cmdk.classList.contains('hidden'), true,
      'precondition: palette must start hidden');
    dom.window.swCommandPalette = {
      toggle() { cmdk.classList.toggle('hidden'); },
    };
    const ev = new dom.window.KeyboardEvent('keydown', { key: 'k', metaKey: true, cancelable: true });
    dom.window.document.dispatchEvent(ev);
    assert.equal(cmdk.classList.contains('hidden'), false);
    assert.equal(ev.defaultPrevented, true,
      'the browser native Cmd-K must be suppressed once we act on the key');
  });

  // #2768: when the palette cannot open, the keystroke must fail loudly
  // (console.error), never silently swallow the key. There are TWO doors into
  // that state and both are covered here, because keyboard.js calls
  // preventDefault() before either is reached -- so a silent failure on either
  // path costs the user the browser's native Cmd-K with nothing in its place:
  //   (a) the controller never loaded  -> keyboard.js's own guard  ([swKbd])
  //   (b) the controller loaded but the #sw-cmdk root is absent from the DOM,
  //       so keyboard.js's capability check PASSES and open() bails ([swCmdk])
  it('fails loudly when the palette cannot open, on both the missing-controller and missing-root paths', () => {
    // (a) controller absent entirely.
    const noCtrl = createDom({
      html: '<!doctype html><html><head><meta name="htmx-base-path" content=""></head><body></body></html>',
      modules: [],
    });
    noCtrl.window.SW_IS_NEXT_PAGE = false;
    noCtrl.window.eval(readFileSync(KEYBOARD_PATH, 'utf-8'));

    const ctrlErrors = [];
    noCtrl.window.console.error = (...args) => ctrlErrors.push(args.join(' '));
    // window.swCommandPalette deliberately left undefined.
    const evA = new noCtrl.window.KeyboardEvent('keydown', { key: 'k', metaKey: true, cancelable: true });
    noCtrl.window.document.dispatchEvent(evA);

    assert.ok(ctrlErrors.some((e) => e.includes('swCommandPalette')),
      'missing command-palette controller is a loud console.error, not a silent no-op');
    assert.equal(evA.defaultPrevented, true,
      'the key is claimed even on the failure path, which is exactly why it must be loud');

    // (b) controller loaded, but the palette root is missing from the DOM.
    // keyboard.js's typeof-toggle check passes here, so this path is invisible
    // to guard (a) and needs its own diagnostic inside open().
    const noRoot = createDom({
      html: '<!doctype html><html><head><meta name="htmx-base-path" content=""></head><body></body></html>',
      modules: ['command-palette'],
    });
    noRoot.window.SW_IS_NEXT_PAGE = false;
    noRoot.window.eval(readFileSync(KEYBOARD_PATH, 'utf-8'));

    assert.equal(typeof noRoot.window.swCommandPalette.toggle, 'function',
      'precondition: the controller IS loaded, so keyboard.js\'s capability guard passes');
    assert.equal(noRoot.window.document.getElementById('sw-cmdk'), null,
      'precondition: the palette root is genuinely absent');

    const rootErrors = [];
    noRoot.window.console.error = (...args) => rootErrors.push(args.join(' '));
    const evB = new noRoot.window.KeyboardEvent('keydown', { key: 'k', metaKey: true, cancelable: true });
    noRoot.window.document.dispatchEvent(evB);

    assert.ok(rootErrors.some((e) => e.includes('sw-cmdk')),
      'a missing palette root must also fail loudly: the native Cmd-K was already suppressed');
    assert.equal(noRoot.window.swCommandPalette.isOpen(), false,
      'precondition check: nothing actually opened, so the keystroke really was lost');
  });
});
