// Regression tests for #2108 (drawer save drops unrendered sections) and
// #2110 (drawer layout controls don't live-apply) in
// web/static/js/prefs-drawer.js.
//
// The drawer's layout list (#sw-prefs-layout-list) only ever renders the
// fixed default section set (orderedPrefsLayoutSections, prefs_drawer.templ):
// metadata, artwork, findings, providers, discography, identifiers. A section
// the ON-PAGE controls manage but the drawer does not -- "debug", which only
// exists in the DOM when show_platform_debug is on (section-layout.js) -- is
// never a row in this list. Before the fix, saveSectionOrder() built its PATCH
// body from these rows only, silently dropping "debug"'s stored order/collapsed
// state on every drawer-triggered save (#2108).
//
// #2110: the drawer's per-section move/collapse actions and the reset buttons
// only updated the drawer's own list + persisted to the server -- they never
// touched the already-open artist-detail page's live section DOM. The fix
// calls window.swArtistSectionLayout.applyLayout(order, collapsed) (the API
// #2110 adds to section-layout.js) after every layout-changing action.
import { describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { createDom, makeFetchMock, flush } from './helpers/dom-harness.js';

function drawerHTML() {
  return `<!doctype html><html><body>
<div id="sw-prefs-drawer">
  <ul id="sw-prefs-layout-list">
    <li data-section-id="metadata" data-hidden="false" data-collapsed="false">
      <button class="sw-prefs-layout-btn" data-action="move-up"></button>
      <button class="sw-prefs-layout-btn" data-action="move-down"></button>
      <button class="sw-prefs-layout-btn" data-action="toggle-collapsed" data-label-collapse="Collapse"></button>
    </li>
    <li data-section-id="artwork" data-hidden="false" data-collapsed="false">
      <button class="sw-prefs-layout-btn" data-action="move-up"></button>
      <button class="sw-prefs-layout-btn" data-action="move-down"></button>
      <button class="sw-prefs-layout-btn" data-action="toggle-collapsed" data-label-collapse="Collapse"></button>
    </li>
  </ul>
  <button data-action="reset-layout"></button>
</div>
</body></html>`;
}

function setup() {
  const dom = createDom({
    html: drawerHTML(),
    modules: ['preferences', 'prefsDrawer'],
    csrfToken: 'tok',
  });
  return dom;
}

describe('#2108: prefs-drawer.js saveSectionOrder() preserves unrendered sections', () => {
  it('merges a cached "debug" id (never a drawer row) into order and collapsed instead of dropping it', async () => {
    const dom = setup();
    const win = dom.window;
    await flush(); // let the async DOMContentLoaded-triggered wire()/initLayoutCard() run once

    win.sessionStorage.setItem('sw-preferences', JSON.stringify({
      artist_detail_section_order: ['metadata', 'artwork', 'debug'],
      artist_detail_hidden_sections: [],
      artist_detail_collapsed_sections: ['debug'],
    }));

    const fetchMock = makeFetchMock({ ok: true, json: {} });
    win.fetch = fetchMock;

    // Move "artwork" up one slot via the arrow-button keyboard fallback.
    const artworkRow = win.document.querySelector('[data-section-id="artwork"]');
    const moveUpBtn = artworkRow.querySelector('[data-action="move-up"]');
    moveUpBtn.dispatchEvent(new win.Event('click', { bubbles: true }));
    await flush();

    const patches = fetchMock.calls.filter(c => c.options && c.options.method === 'PATCH');
    assert.equal(patches.length, 1, 'exactly one PATCH issued');
    const body = JSON.parse(patches[0].options.body);

    assert.deepEqual(body.artist_detail_section_order, ['artwork', 'metadata', 'debug'],
      'debug is preserved at the end of the order even though the drawer never renders it as a row');
    assert.deepEqual(body.artist_detail_collapsed_sections, ['debug'],
      'debug stays collapsed in the stored preference');
  });
});

describe('#2110: prefs-drawer.js layout actions live-apply to an already-open artist-detail page', () => {
  it('saveSectionOrder() forwards the merged order/collapsed to window.swArtistSectionLayout.applyLayout', async () => {
    const dom = setup();
    const win = dom.window;
    await flush(); // let the async DOMContentLoaded-triggered wire()/initLayoutCard() run once

    const applyLayoutCalls = [];
    win.swArtistSectionLayout = {
      applyLayout: (order, collapsed) => applyLayoutCalls.push({ order: Array.from(order), collapsed: Array.from(collapsed) }),
    };
    win.fetch = makeFetchMock({ ok: true, json: {} });

    const metadataRow = win.document.querySelector('[data-section-id="metadata"]');
    metadataRow.querySelector('[data-action="toggle-collapsed"]').dispatchEvent(new win.Event('click', { bubbles: true }));
    await flush();

    assert.equal(applyLayoutCalls.length, 1, 'applyLayout called once for the toggle-collapsed action');
    assert.deepEqual(applyLayoutCalls[0].order, ['metadata', 'artwork']);
    assert.deepEqual(applyLayoutCalls[0].collapsed, ['metadata']);
  });

  it('reset-layout live-applies the default order with everything expanded', async () => {
    const dom = setup();
    const win = dom.window;
    await flush(); // let the async DOMContentLoaded-triggered wire()/initLayoutCard() run once

    const applyLayoutCalls = [];
    win.swArtistSectionLayout = {
      applyLayout: (order, collapsed) => applyLayoutCalls.push({ order: Array.from(order), collapsed: Array.from(collapsed) }),
    };
    win.fetch = makeFetchMock({ ok: true, json: {} });

    win.document.querySelector('[data-action="reset-layout"]').dispatchEvent(new win.Event('click', { bubbles: true }));
    await flush();

    assert.equal(applyLayoutCalls.length, 1);
    assert.deepEqual(applyLayoutCalls[0].order,
      ['metadata', 'artwork', 'findings', 'providers', 'discography', 'identifiers']);
    assert.deepEqual(applyLayoutCalls[0].collapsed, []);
  });

  it('logs loudly (never silently no-ops) when window.swArtistSectionLayout is unavailable', async () => {
    const dom = setup();
    const win = dom.window;
    await flush(); // let the async DOMContentLoaded-triggered wire()/initLayoutCard() run once

    const errors = [];
    win.console.error = (...args) => errors.push(args.join(' '));
    win.fetch = makeFetchMock({ ok: true, json: {} });
    // window.swArtistSectionLayout deliberately left undefined (script failed to load).

    const metadataRow = win.document.querySelector('[data-section-id="metadata"]');
    metadataRow.querySelector('[data-action="toggle-collapsed"]').dispatchEvent(new win.Event('click', { bubbles: true }));
    await flush();

    assert.ok(errors.some(e => e.includes('swArtistSectionLayout.applyLayout unavailable')),
      'missing live-apply capability is a loud console.error, not a silent no-op');
  });
});
