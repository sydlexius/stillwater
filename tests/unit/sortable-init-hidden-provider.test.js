// Tests for sortable-init.js hidden-provider preservation (#3190 review F1).
// A drag-reorder PUT must keep stored providers that render as no chip
// ([data-hidden-provider]) so the save never deletes them.
import { describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { createDom, makeFetchMock, flush, MODULE_PATHS } from './helpers/dom-harness.js';
import { readFileSync } from 'node:fs';

const HTML = `<!doctype html><html><body>
<div id="priority-row-moods">
  <div data-sortable-field="moods">
    <div data-provider="audiodb"><span class="drag-handle"></span></div>
    <div data-provider="lastfm"><span class="drag-handle"></span></div>
  </div>
  <span hidden data-hidden-provider="discogs"></span>
  <div data-disabled-provider="genius"></div>
</div></body></html>`;

// jsdom parses asynchronously: createDom returns with readyState 'loading' and
// fires DOMContentLoaded afterwards, exactly like a browser parsing the page.
function domReady(win) {
  if (win.document.readyState !== 'loading') return Promise.resolve();
  return new Promise((res) => win.document.addEventListener('DOMContentLoaded', res));
}

function loadInit(win) {
  const calls = [];
  win.Sortable = { create: (_el, o) => { calls.push(o); return {}; } };
  win.fetch = makeFetchMock();
  win.eval(readFileSync(MODULE_PATHS.sortableInit, 'utf-8'));
  return calls;
}

describe('sortable-init initial init timing (#2189)', () => {
  it('defers while the document is loading, then inits on DOMContentLoaded', async () => {
    const win = createDom({ html: HTML, csrfToken: 'tok' }).window;
    assert.equal(win.document.readyState, 'loading', 'precondition: still parsing');
    const calls = loadInit(win);
    assert.equal(calls.length, 0, 'must not init at parse time');
    await domReady(win);
    assert.equal(calls.length, 1, 'inits once DOMContentLoaded fires');
  });

  it('inits immediately when the document is already past loading', async () => {
    const win = createDom({ html: HTML, csrfToken: 'tok' }).window;
    await domReady(win);
    assert.notEqual(win.document.readyState, 'loading', 'precondition: parsed');
    const calls = loadInit(win);
    assert.equal(calls.length, 1, 'late-mounted script inits at eval time');
  });
});

describe('sortable-init onEnd', () => {
  it('PUTs enabled, then disabled, then hidden providers', async () => {
    const win = createDom({ html: HTML, csrfToken: 'tok' }).window;
    await domReady(win);
    const calls = loadInit(win);
    const opts = calls[0];
    assert.ok(opts, 'Sortable.create was not called');
    opts.onEnd();
    await flush();
    assert.equal(win.fetch.calls.length, 1);
    const body = JSON.parse(win.fetch.calls[0].options.body);
    assert.deepEqual(body.priorities, [
      { field: 'moods', providers: ['audiodb', 'lastfm', 'genius', 'discogs'] },
    ]);
  });
});
