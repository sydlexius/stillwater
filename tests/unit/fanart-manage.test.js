// Unit tests for web/static/js/artist-detail/fanart-manage.js
// Covers: reorder swap-index math, setFanartPrimary order construction, and
// the empty-CSRF guards that gate every mutating call.
import { describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { createDom, makeFetchMock, flush } from './helpers/dom-harness.js';

// Minimal gallery HTML: 3 fanart slots + move/primary buttons.
// The reorder logic counts .fanart-cb checkboxes to derive `total`.
const GALLERY_HTML = `<!doctype html><html><body>
<div id="gallery" data-sw-fanart-gallery data-artist-id="artist123">
  <input type="checkbox" class="fanart-cb" value="0">
  <input type="checkbox" class="fanart-cb" value="1">
  <input type="checkbox" class="fanart-cb" value="2">

  <button class="fanart-move-btn" id="btn-up-1"
    data-index="1" data-direction="up" data-artist-id="artist123"></button>
  <button class="fanart-move-btn" id="btn-down-0"
    data-index="0" data-direction="down" data-artist-id="artist123"></button>
  <button class="fanart-move-btn" id="btn-up-0"
    data-index="0" data-direction="up" data-artist-id="artist123"></button>
  <button class="fanart-move-btn" id="btn-down-last"
    data-index="2" data-direction="down" data-artist-id="artist123"></button>

  <button id="set-primary-2"
    data-set-primary-index="2"
    data-set-primary-count="3"
    data-set-primary-artist="artist123"></button>
  <button id="set-primary-0"
    data-set-primary-index="0"
    data-set-primary-count="3"
    data-set-primary-artist="artist123"></button>
</div>
</body></html>`;

// ---------------------------------------------------------------------------
// Reorder swap-index math
// ---------------------------------------------------------------------------
describe('fanart-manage: reorder swap-index math', () => {
  it('move-up on index 1 swaps with index 0 → order [1, 0, 2]', async () => {
    const fetchMock = makeFetchMock({ ok: true });
    const dom = createDom({ html: GALLERY_HTML, modules: ['fanartManage'], csrfToken: 'tok' });
    dom.window.fetch = fetchMock;

    dom.window.document.getElementById('btn-up-1').click();
    await flush();

    assert.equal(fetchMock.calls.length, 1);
    assert.deepEqual(JSON.parse(fetchMock.calls[0].options.body).order, [1, 0, 2]);
  });

  it('move-down on index 0 swaps with index 1 → order [1, 0, 2]', async () => {
    const fetchMock = makeFetchMock({ ok: true });
    const dom = createDom({ html: GALLERY_HTML, modules: ['fanartManage'], csrfToken: 'tok' });
    dom.window.fetch = fetchMock;

    dom.window.document.getElementById('btn-down-0').click();
    await flush();

    assert.equal(fetchMock.calls.length, 1);
    assert.deepEqual(JSON.parse(fetchMock.calls[0].options.body).order, [1, 0, 2]);
  });

  it('move-up on index 0 is a no-op (lower boundary guard)', async () => {
    const fetchMock = makeFetchMock({ ok: true });
    const dom = createDom({ html: GALLERY_HTML, modules: ['fanartManage'], csrfToken: 'tok' });
    dom.window.fetch = fetchMock;

    dom.window.document.getElementById('btn-up-0').click();
    await flush();

    assert.equal(fetchMock.calls.length, 0, 'move-up at index 0 must not call fetch');
  });

  it('move-down on last index is a no-op (upper boundary guard)', async () => {
    const fetchMock = makeFetchMock({ ok: true });
    const dom = createDom({ html: GALLERY_HTML, modules: ['fanartManage'], csrfToken: 'tok' });
    dom.window.fetch = fetchMock;

    dom.window.document.getElementById('btn-down-last').click();
    await flush();

    assert.equal(fetchMock.calls.length, 0, 'move-down on last index must not call fetch');
  });

  it('reorder call targets the fanart reorder endpoint', async () => {
    const fetchMock = makeFetchMock({ ok: true });
    const dom = createDom({ html: GALLERY_HTML, modules: ['fanartManage'], csrfToken: 'tok' });
    dom.window.fetch = fetchMock;

    dom.window.document.getElementById('btn-up-1').click();
    await flush();

    assert.ok(
      fetchMock.calls[0].url.includes('/images/fanart/reorder'),
      `expected reorder URL, got: ${fetchMock.calls[0].url}`,
    );
    assert.equal(fetchMock.calls[0].options.method, 'POST');
  });
});

// ---------------------------------------------------------------------------
// setFanartPrimary order construction
// ---------------------------------------------------------------------------
describe('fanart-manage: setFanartPrimary order construction', () => {
  it('set-primary at index 2 builds order [2, 0, 1] (chosen index first)', async () => {
    const fetchMock = makeFetchMock({ ok: true });
    const dom = createDom({ html: GALLERY_HTML, modules: ['fanartManage'], csrfToken: 'tok' });
    dom.window.fetch = fetchMock;
    dom.window.confirm = () => true;

    dom.window.document.getElementById('set-primary-2').click();
    await flush();

    assert.equal(fetchMock.calls.length, 1);
    assert.deepEqual(JSON.parse(fetchMock.calls[0].options.body).order, [2, 0, 1],
      'chosen index goes first, remaining indices follow in original order');
  });

  it('set-primary at index 0 builds order [0, 1, 2] (no effective reorder)', async () => {
    const fetchMock = makeFetchMock({ ok: true });
    const dom = createDom({ html: GALLERY_HTML, modules: ['fanartManage'], csrfToken: 'tok' });
    dom.window.fetch = fetchMock;
    dom.window.confirm = () => true;

    dom.window.document.getElementById('set-primary-0').click();
    await flush();

    assert.deepEqual(JSON.parse(fetchMock.calls[0].options.body).order, [0, 1, 2]);
  });

  it('cancelled confirm does not call fetch', async () => {
    const fetchMock = makeFetchMock({ ok: true });
    const dom = createDom({ html: GALLERY_HTML, modules: ['fanartManage'], csrfToken: 'tok' });
    dom.window.fetch = fetchMock;
    dom.window.confirm = () => false;

    dom.window.document.getElementById('set-primary-2').click();
    await flush();

    assert.equal(fetchMock.calls.length, 0);
  });
});

// ---------------------------------------------------------------------------
// CSRF guards
// ---------------------------------------------------------------------------
describe('fanart-manage: CSRF guards', () => {
  it('reorder alerts and skips fetch when CSRF token is empty', async () => {
    const fetchMock = makeFetchMock({ ok: true });
    const alerts = [];
    const dom = createDom({ html: GALLERY_HTML, modules: ['fanartManage'], csrfToken: '' });
    dom.window.fetch = fetchMock;
    dom.window.alert = msg => alerts.push(msg);

    dom.window.document.getElementById('btn-up-1').click();
    await flush();

    assert.equal(fetchMock.calls.length, 0, 'fetch must not be called with empty CSRF');
    assert.equal(alerts.length, 1, 'an alert must be shown when CSRF is missing');
  });

  it('setFanartPrimary alerts and skips fetch when CSRF token is empty', async () => {
    const fetchMock = makeFetchMock({ ok: true });
    const alerts = [];
    const dom = createDom({ html: GALLERY_HTML, modules: ['fanartManage'], csrfToken: '' });
    dom.window.fetch = fetchMock;
    dom.window.alert = msg => alerts.push(msg);
    dom.window.confirm = () => true;

    dom.window.document.getElementById('set-primary-2').click();
    await flush();

    assert.equal(fetchMock.calls.length, 0, 'fetch must not be called with empty CSRF');
    assert.equal(alerts.length, 1, 'an alert must be shown when CSRF is missing');
  });
});
