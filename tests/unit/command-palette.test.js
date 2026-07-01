import { describe, it, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import { createDom } from './helpers/dom-harness.js';

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
});
