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
      assert.match(s.href, /^\/next\/settings#section-/, `setting "${s.label}" href "${s.href}" must target a #section- anchor`);
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
});
