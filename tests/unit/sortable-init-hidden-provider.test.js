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

describe('sortable-init onEnd', () => {
  it('PUTs enabled, then disabled, then hidden providers', async () => {
    const dom = createDom({ html: HTML, csrfToken: 'tok' });
    const win = dom.window;
    let opts;
    win.Sortable = { create: (_el, o) => { opts = o; return {}; } };
    win.fetch = makeFetchMock();
    win.eval(readFileSync(MODULE_PATHS.sortableInit, 'utf-8'));
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
