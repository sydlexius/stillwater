// Tests for the Settings rail keyword filter (#2429): the filter is transient
// view state and must never be persisted to (or restored from) localStorage.
//
// These cover the contract that matters for the bug fix:
//   - typing into the filter input never writes to localStorage,
//   - a pre-seeded deprecated 'sw-settings-filter' value is removed on init
//     and does NOT pre-fill the input (a fresh page load always starts empty),
//   - applyFilter still hides/shows rail items correctly for in-memory queries.
import { describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { createDom, flush } from './helpers/dom-harness.js';

const __dirname = dirname(fileURLToPath(import.meta.url));
const NEXT_SETTINGS_PATH = resolve(__dirname, '../../web/static/js/settings/next-settings.js');

const HTML = `<!doctype html><html><body>
<div id="sw-next-settings-pane">
  <section data-rail-section id="section-general"></section>
  <section data-rail-section id="section-library"></section>
</div>
<div class="sw-next-settings-rail">
  <div class="sw-next-rail-group" data-rail-group>
    <a class="sw-next-rail-item" data-rail-link="general" data-label="General" data-keywords="theme|appearance" href="#section-general">
      General
      <span data-rail-hit hidden></span>
    </a>
    <a class="sw-next-rail-item" data-rail-link="library" data-label="Library" data-keywords="scan|paths" href="#section-library">
      Library
      <span data-rail-hit hidden></span>
    </a>
  </div>
</div>
<div data-rail-empty data-empty-template='No settings match "{query}".' hidden>
  <span data-rail-empty-text></span>
  <button data-rail-clear>Clear</button>
</div>
<input id="settings-search-input" />
</body></html>`;

describe('next-settings filter: transient view state only (#2429)', () => {
  it('does not write the query to localStorage while typing', async () => {
    const { window } = createDom({ html: HTML, modules: ['nextSettings'] });
    await flush(); // let the deferred DOMContentLoaded-driven init run
    const input = window.document.getElementById('settings-search-input');

    input.value = 'theme';
    input.dispatchEvent(new window.Event('input', { bubbles: true }));

    assert.equal(
      window.localStorage.getItem('sw-settings-filter'), null,
      'typing into the filter must never persist the query',
    );
  });

  it('removes a pre-seeded deprecated key on init and does not pre-fill the input', async () => {
    // Seed the DOM's own localStorage (mirrors a browser carrying the old
    // persisted behavior forward) BEFORE the module initializes, by
    // constructing the JSDOM window first and writing to it, then evaling
    // the module against that same window.
    const { window } = createDom({ html: HTML });
    window.localStorage.setItem('sw-settings-filter', 'stale-query');

    window.eval(readFileSync(NEXT_SETTINGS_PATH, 'utf-8'));
    await flush(); // let the deferred DOMContentLoaded-driven init run

    const input = window.document.getElementById('settings-search-input');
    assert.equal(input.value, '', 'the input must start empty, never restored from storage');
    assert.equal(
      window.localStorage.getItem('sw-settings-filter'), null,
      'the deprecated stored key must be removed on init',
    );
  });

  it('applyFilter still hides/shows rail items correctly for in-memory queries', async () => {
    const { window } = createDom({ html: HTML, modules: ['nextSettings'] });
    await flush(); // let the deferred DOMContentLoaded-driven init run
    const input = window.document.getElementById('settings-search-input');
    const general = window.document.querySelector('a[data-rail-link="general"]');
    const library = window.document.querySelector('a[data-rail-link="library"]');

    input.value = 'theme';
    input.dispatchEvent(new window.Event('input', { bubbles: true }));

    assert.equal(general.hidden, false, 'a matching item (by keyword) stays visible');
    assert.equal(library.hidden, true, 'a non-matching item is hidden');

    input.value = '';
    input.dispatchEvent(new window.Event('input', { bubbles: true }));

    assert.equal(general.hidden, false, 'clearing the query restores all items');
    assert.equal(library.hidden, false, 'clearing the query restores all items');
  });
});
