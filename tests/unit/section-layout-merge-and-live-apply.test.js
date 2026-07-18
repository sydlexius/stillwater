// Regression tests for #2108 (drawer/on-page save drops unrendered sections)
// and #2110 (drawer layout controls don't live-apply) in
// web/static/js/artist-detail/section-layout.js.
//
// #2108: save() built the PATCH body from the live DOM only, so a section not
// currently rendered on this page load (e.g. "debug", which only renders when
// show_platform_debug is on) had its stored order/collapsed state silently
// dropped on the next save. The fix merges in any id from the sessionStorage
// preferences cache that is absent from the live DOM, preserving its place.
//
// #2110: window.swArtistSectionLayout now exposes applyLayout(order,
// collapsed), which live-applies a layout to the current page's rendered
// sections (reorder + collapse/expand), gated to a no-op when the
// artist-detail section container is not present.
import { describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { createDom, makeFetchMock, flush } from './helpers/dom-harness.js';

// Minimal artist-detail markup: two rendered sections (metadata, artwork).
// "debug" is deliberately absent, simulating a page load where
// show_platform_debug is off and the debug section never renders.
function pageHTML() {
  return `<!doctype html><html><body>
<div data-sw-sortable-section id="sections">
  <section data-sw-section="metadata">
    <button data-sw-section-toggle aria-expanded="true" aria-controls="metadata-body"></button>
    <div id="metadata-body"></div>
  </section>
  <section data-sw-section="artwork">
    <button data-sw-section-toggle aria-expanded="true" aria-controls="artwork-body"></button>
    <div id="artwork-body"></div>
  </section>
</div>
</body></html>`;
}

function setup() {
  const dom = createDom({
    html: pageHTML(),
    modules: ['preferences', 'sectionLayout'],
    csrfToken: 'tok',
  });
  return dom;
}

describe('#2108: section-layout.js save() preserves unrendered sections', () => {
  it('merges a cached-but-absent section id into order and collapsed instead of dropping it', async () => {
    const dom = setup();
    const win = dom.window;
    // Let the automatic preferences.js load() (GET) settle first so it does
    // not clobber the cache we are about to seed.
    await flush();

    // Seed the cache as if "debug" had previously been saved (e.g. from an
    // earlier session with show_platform_debug on) at the end of the order,
    // collapsed.
    win.sessionStorage.setItem('sw-preferences', JSON.stringify({
      artist_detail_section_order: ['metadata', 'artwork', 'debug'],
      artist_detail_collapsed_sections: ['debug'],
    }));

    const fetchMock = makeFetchMock({ ok: true, json: {} });
    win.fetch = fetchMock;

    // Trigger a save via the on-page collapse toggle for "metadata".
    const btn = win.document.querySelector('[data-sw-section="metadata"] [data-sw-section-toggle]');
    btn.dispatchEvent(new win.Event('click', { bubbles: true }));
    await flush();

    const patches = fetchMock.calls.filter(c => c.options && c.options.method === 'PATCH');
    assert.equal(patches.length, 1, 'exactly one PATCH issued');
    const body = JSON.parse(patches[0].options.body);

    assert.deepEqual(body.artist_detail_section_order, ['metadata', 'artwork', 'debug'],
      'debug is preserved at the end of the order even though it is not live-rendered');
    assert.deepEqual(body.artist_detail_collapsed_sections, ['metadata', 'debug'],
      'metadata (just collapsed by this click) is live, and debug is preserved from the '
      + 'cache even though it is not live-rendered');
  });

  it('when nothing is cached, save() falls back to the live DOM only (no regression)', async () => {
    const dom = setup();
    const win = dom.window;
    await flush();

    const fetchMock = makeFetchMock({ ok: true, json: {} });
    win.fetch = fetchMock;

    const btn = win.document.querySelector('[data-sw-section="metadata"] [data-sw-section-toggle]');
    btn.dispatchEvent(new win.Event('click', { bubbles: true }));
    await flush();

    const patches = fetchMock.calls.filter(c => c.options && c.options.method === 'PATCH');
    const body = JSON.parse(patches[0].options.body);
    assert.deepEqual(body.artist_detail_section_order, ['metadata', 'artwork']);
    assert.deepEqual(body.artist_detail_collapsed_sections, ['metadata']);
  });
});

describe('#2110: window.swArtistSectionLayout.applyLayout live-applies a layout', () => {
  it('reorders live sections and applies collapsed state without a network call', async () => {
    const dom = setup();
    const win = dom.window;
    await flush();

    win.swArtistSectionLayout.applyLayout(['artwork', 'metadata'], ['metadata']);

    const ids = Array.prototype.slice.call(
      win.document.querySelectorAll('[data-sw-sortable-section] > [data-sw-section]')
    ).map(s => s.getAttribute('data-sw-section'));
    assert.deepEqual(ids, ['artwork', 'metadata'], 'sections reordered to match the requested order');

    const metaBtn = win.document.querySelector('[data-sw-section="metadata"] [data-sw-section-toggle]');
    assert.equal(metaBtn.getAttribute('aria-expanded'), 'false', 'metadata collapsed');
    assert.equal(win.document.getElementById('metadata-body').hasAttribute('hidden'), true);

    const artBtn = win.document.querySelector('[data-sw-section="artwork"] [data-sw-section-toggle]');
    assert.equal(artBtn.getAttribute('aria-expanded'), 'true', 'artwork stays expanded');
    assert.equal(win.document.getElementById('artwork-body').hasAttribute('hidden'), false);
  });

  it('is a no-op when the artist-detail section container is not present on the page', async () => {
    const dom = createDom({
      html: '<!doctype html><html><body><div id="not-artist-detail"></div></body></html>',
      modules: ['preferences', 'sectionLayout'],
      csrfToken: 'tok',
    });
    const win = dom.window;
    await flush();

    assert.doesNotThrow(() => {
      win.swArtistSectionLayout.applyLayout(['metadata'], ['metadata']);
    });
  });
});
