// Unit tests for web/static/js/artist-detail/artwork-modal.js
// Covers:
//   - KIND_TO_TYPE parity with Go's artworkKindToType (handlers_artist_detail.go)
//   - doRevert: HTTP status branching (200, 404, 409) and CSRF guard
import { describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { createDom, makeFetchMock, flush } from './helpers/dom-harness.js';

// ---------------------------------------------------------------------------
// Go source of truth for kind -> image-type (handlers_artist_detail.go,
// artworkKindToType). This constant is MANUALLY maintained: when Go's
// artworkKindToType gains or changes a case, update GO_KIND_TO_TYPE here.
// The tests then catch JS drift from this constant -- they do NOT
// auto-detect Go-side changes on their own.
// ---------------------------------------------------------------------------
const GO_KIND_TO_TYPE = {
  primary:   'thumb',
  logo:      'logo',
  banner:    'banner',
  backdrops: 'fanart',
};

// Minimal modal DOM. refreshRevert needs artwork-revert-row, artwork-modal
// (with data-artist-id), and the kind-tab buttons. doRevert additionally needs
// artwork-revert-btn and artwork-gate-banner / artwork-gate-reason.
const MODAL_HTML = `<!doctype html><html><body>
<button id="outside-btn">Outside</button>
<div id="artwork-modal" class="hidden" data-artist-id="artist456" tabindex="-1">
  <div id="artwork-modal-body"></div>
  <button id="artwork-revert-btn">Revert</button>
  <div id="artwork-revert-row"></div>
  <div id="artwork-gate-banner" class="hidden">
    <span id="artwork-gate-reason"></span>
  </div>
  <button data-sw-artwork-kind-tab data-artwork-kind="primary" aria-pressed="true">Primary</button>
  <button data-sw-artwork-kind-tab data-artwork-kind="logo"    aria-pressed="false">Logo</button>
  <button data-sw-artwork-kind-tab data-artwork-kind="banner"  aria-pressed="false">Banner</button>
  <button data-sw-artwork-kind-tab data-artwork-kind="backdrops" aria-pressed="false">Backdrops</button>
  <button data-sw-artwork-close>Close</button>
</div>
</body></html>`;

// ---------------------------------------------------------------------------
// KIND_TO_TYPE parity: JS map vs Go artworkKindToType
//
// Strategy: open the modal for a given kind and observe what image-type segment
// appears in the /info fetch URL that refreshRevert dispatches. This tests the
// live code path rather than parsing source text, so a renamed variable or a
// wrong value both fail the assertion.
// ---------------------------------------------------------------------------
describe('artwork-modal: KIND_TO_TYPE parity with Go artworkKindToType', () => {
  for (const [kind, expectedType] of Object.entries(GO_KIND_TO_TYPE)) {
    it(`kind "${kind}" resolves to image type "${expectedType}"`, async () => {
      const fetchMock = makeFetchMock({ ok: true, json: { backup_exists: false } });
      const dom = createDom({ html: MODAL_HTML, modules: ['artworkModal'], csrfToken: 'tok' });
      dom.window.fetch = fetchMock;

      dom.window.swArtworkModal.open(kind);
      await flush();

      if (expectedType === 'fanart') {
        // fanart is the multi-slot kind: refreshRevert hides the row immediately
        // and returns without fetching /info (no single-slot backup applies).
        const row = dom.window.document.getElementById('artwork-revert-row');
        assert.ok(
          row.classList.contains('hidden'),
          `kind "${kind}" (type fanart): revert row must be hidden (no single-slot backup)`,
        );
        const infoFetches = fetchMock.calls.filter(c => String(c.url).includes('/info'));
        assert.equal(infoFetches.length, 0,
          `kind "${kind}" (type fanart): must not fetch /info`);
      } else {
        const infoFetches = fetchMock.calls.filter(c => String(c.url).includes('/info'));
        assert.equal(infoFetches.length, 1,
          `kind "${kind}": expected exactly one /info fetch, got ${infoFetches.length}`);
        assert.ok(
          String(infoFetches[0].url).includes(`/images/${expectedType}/info`),
          `kind "${kind}": expected URL to contain /images/${expectedType}/info, got: ${infoFetches[0].url}`,
        );
      }
    });
  }
});

// ---------------------------------------------------------------------------
// doRevert: HTTP status branching
// ---------------------------------------------------------------------------
describe('artwork-modal: doRevert HTTP status branching', () => {
  it('200 ok: reloads the modal body (htmx.ajax called)', async () => {
    // doRevert POST returns 200: expects loadBody() to be called,
    // which calls window.htmx.ajax.
    const ajaxCalls = [];
    const dom = createDom({ html: MODAL_HTML, modules: ['artworkModal'], csrfToken: 'tok' });
    dom.window.htmx = { ajax: (...args) => { ajaxCalls.push(args); return Promise.resolve(); } };
    // First call (from open()) is loadBody; we'll count only POST-200 induced calls.
    dom.window.fetch = makeFetchMock({ ok: true, status: 200, json: { backup_exists: true } });

    dom.window.swArtworkModal.open('logo');
    await flush();
    const callsAfterOpen = ajaxCalls.length;

    // Now trigger revert (POST /revert returns 200)
    dom.window.fetch = makeFetchMock({ ok: true, status: 200 });
    dom.window.document.getElementById('artwork-revert-btn').click();
    await flush();

    assert.ok(ajaxCalls.length > callsAfterOpen,
      'a successful revert (200) must reload the modal body via htmx.ajax');
  });

  it('409: shows the gate banner with the reason from the response body', async () => {
    const dom = createDom({ html: MODAL_HTML, modules: ['artworkModal'], csrfToken: 'tok' });
    dom.window.fetch = makeFetchMock({ ok: false, status: 409, json: { reason: 'test conflict' } });

    dom.window.swArtworkModal.open('logo');
    await flush();

    // Re-assign fetch for the revert call so it returns 409
    dom.window.fetch = makeFetchMock({ ok: false, status: 409, json: { reason: 'test conflict' } });
    dom.window.document.getElementById('artwork-revert-btn').click();
    await flush();

    const banner = dom.window.document.getElementById('artwork-gate-banner');
    assert.ok(!banner.classList.contains('hidden'),
      '409 must show the gate banner');
    assert.equal(
      dom.window.document.getElementById('artwork-gate-reason').textContent,
      'test conflict',
      '409 must populate the banner with the reason from the response',
    );
  });

  it('404: hides the revert row (backup is gone)', async () => {
    const dom = createDom({ html: MODAL_HTML, modules: ['artworkModal'], csrfToken: 'tok' });
    dom.window.fetch = makeFetchMock({ ok: false, status: 404 });

    dom.window.swArtworkModal.open('logo');
    await flush();

    dom.window.fetch = makeFetchMock({ ok: false, status: 404 });
    dom.window.document.getElementById('artwork-revert-btn').click();
    await flush();

    const row = dom.window.document.getElementById('artwork-revert-row');
    assert.ok(row.classList.contains('hidden'),
      '404 must hide the revert row (backup no longer exists)');
  });

  it('CSRF guard: empty token skips the POST and logs a warning', async () => {
    const fetchMock = makeFetchMock({ ok: true });
    const warnings = [];
    const dom = createDom({ html: MODAL_HTML, modules: ['artworkModal'], csrfToken: '' });
    dom.window.fetch = fetchMock;
    dom.window.console = { warn: msg => warnings.push(msg) };

    dom.window.swArtworkModal.open('logo');
    await flush();

    const callsAfterOpen = fetchMock.calls.length;

    // Attempt revert with empty CSRF
    dom.window.document.getElementById('artwork-revert-btn').click();
    await flush();

    assert.equal(fetchMock.calls.length, callsAfterOpen,
      'doRevert must not call fetch when CSRF token is empty');
    assert.ok(warnings.some(w => /csrf/i.test(w)),
      'doRevert must log a CSRF warning when token is empty');
  });
});

// ---------------------------------------------------------------------------
// close / focus-restore
// ---------------------------------------------------------------------------
describe('artwork-modal: open and close', () => {
  it('open makes the modal visible', () => {
    const dom = createDom({ html: MODAL_HTML, modules: ['artworkModal'] });
    dom.window.swArtworkModal.open('primary');
    const m = dom.window.document.getElementById('artwork-modal');
    assert.ok(!m.classList.contains('hidden'), 'modal must not have hidden class when open');
    assert.ok(m.classList.contains('flex'), 'modal must have flex class when open');
  });

  it('close hides the modal and restores focus to opener', () => {
    const dom = createDom({ html: MODAL_HTML, modules: ['artworkModal'] });
    const outsideBtn = dom.window.document.getElementById('outside-btn');
    outsideBtn.focus();

    dom.window.swArtworkModal.open('primary');
    dom.window.swArtworkModal.close();

    const m = dom.window.document.getElementById('artwork-modal');
    assert.ok(m.classList.contains('hidden'), 'modal must be hidden after close');
    assert.equal(dom.window.document.activeElement, outsideBtn,
      'focus must return to the opener element after close');
  });

  it('Escape key closes the modal', () => {
    const dom = createDom({ html: MODAL_HTML, modules: ['artworkModal'] });
    dom.window.swArtworkModal.open('primary');
    const m = dom.window.document.getElementById('artwork-modal');

    const evt = new dom.window.KeyboardEvent('keydown', {
      key: 'Escape', bubbles: true, cancelable: true,
    });
    dom.window.document.dispatchEvent(evt);

    assert.ok(m.classList.contains('hidden'), 'Escape must close the artwork modal');
    assert.ok(evt.defaultPrevented, 'Escape must call preventDefault');
  });

  // #2305: the lightbox can be opened on top of this modal as an in-modal zoom
  // viewer. While it is visible it must own Escape/Tab so a single Escape only
  // closes the lightbox, leaving the modal open underneath.
  it('Escape no-ops on the modal while the in-modal lightbox is open', () => {
    const dom = createDom({ html: MODAL_HTML, modules: ['artworkModal'] });
    dom.window.swArtworkModal.open('primary');
    const m = dom.window.document.getElementById('artwork-modal');

    const lightbox = dom.window.document.createElement('div');
    lightbox.id = 'sw-lightbox';
    lightbox.classList.add('flex'); // visible: no "hidden" class
    dom.window.document.body.appendChild(lightbox);

    const evt = new dom.window.KeyboardEvent('keydown', {
      key: 'Escape', bubbles: true, cancelable: true,
    });
    dom.window.document.dispatchEvent(evt);

    assert.ok(!m.classList.contains('hidden'),
      'the artwork modal must stay open while the lightbox is visible on top of it');
    assert.ok(!evt.defaultPrevented,
      'the modal keydown handler must no-op (not preventDefault) while the lightbox is visible');
  });

  it('Escape closes the modal again once the in-modal lightbox is hidden', () => {
    const dom = createDom({ html: MODAL_HTML, modules: ['artworkModal'] });
    dom.window.swArtworkModal.open('primary');
    const m = dom.window.document.getElementById('artwork-modal');

    const lightbox = dom.window.document.createElement('div');
    lightbox.id = 'sw-lightbox';
    lightbox.classList.add('hidden'); // closed
    dom.window.document.body.appendChild(lightbox);

    const evt = new dom.window.KeyboardEvent('keydown', {
      key: 'Escape', bubbles: true, cancelable: true,
    });
    dom.window.document.dispatchEvent(evt);

    assert.ok(m.classList.contains('hidden'),
      'Escape must still close the artwork modal once the lightbox is hidden again');
  });
});

// ---------------------------------------------------------------------------
// #2323/#2281 QOL #48: the outer modal's three close paths (Escape, its own
// backdrop click, its own X button) must not silently discard an
// in-progress crop staged in the nested Cropper overlay (image_search.templ).
// That script sets window._cropDirty / window.guardedCloseCropModal as real
// globals (an intentional top-level, non-IIFE <script>); these tests stub
// them directly on dom.window rather than loading the whole crop editor.
// ---------------------------------------------------------------------------
describe('artwork-modal: crop discard-guard on the outer modal close paths (#2323)', () => {
  function openModalDirty(dom) {
    dom.window.swArtworkModal.open('backdrops');
    dom.window._cropDirty = true;
    dom.window.guardedCloseCropModal = () => { dom.window.guardedCloseCropModalCalls = (dom.window.guardedCloseCropModalCalls || 0) + 1; };
  }

  it('Escape routes through guardedCloseCropModal instead of closing when a crop is dirty', () => {
    const dom = createDom({ html: MODAL_HTML, modules: ['artworkModal'] });
    openModalDirty(dom);
    const m = dom.window.document.getElementById('artwork-modal');

    const evt = new dom.window.KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true });
    dom.window.document.dispatchEvent(evt);

    assert.ok(!m.classList.contains('hidden'), 'the outer modal must stay open while a crop is dirty');
    assert.equal(dom.window.guardedCloseCropModalCalls, 1, 'Escape must call guardedCloseCropModal exactly once');
  });

  it('outer backdrop click routes through guardedCloseCropModal instead of closing when a crop is dirty', () => {
    const dom = createDom({ html: MODAL_HTML, modules: ['artworkModal'] });
    openModalDirty(dom);
    const m = dom.window.document.getElementById('artwork-modal');

    m.dispatchEvent(new dom.window.MouseEvent('click', { bubbles: true, cancelable: true }));

    assert.ok(!m.classList.contains('hidden'), 'the outer modal must stay open while a crop is dirty');
    assert.equal(dom.window.guardedCloseCropModalCalls, 1, 'backdrop click must call guardedCloseCropModal exactly once');
  });

  it('outer X button routes through guardedCloseCropModal instead of closing when a crop is dirty', () => {
    const dom = createDom({ html: MODAL_HTML, modules: ['artworkModal'] });
    openModalDirty(dom);
    const m = dom.window.document.getElementById('artwork-modal');
    const closeBtn = dom.window.document.querySelector('[data-sw-artwork-close]');

    closeBtn.dispatchEvent(new dom.window.MouseEvent('click', { bubbles: true, cancelable: true }));

    assert.ok(!m.classList.contains('hidden'), 'the outer modal must stay open while a crop is dirty');
    assert.equal(dom.window.guardedCloseCropModalCalls, 1, 'the X button must call guardedCloseCropModal exactly once');
  });

  it('Escape still closes the modal normally once the crop is no longer dirty', () => {
    const dom = createDom({ html: MODAL_HTML, modules: ['artworkModal'] });
    openModalDirty(dom);
    dom.window._cropDirty = false; // e.g. the Cropper's own guarded Cancel just ran
    const m = dom.window.document.getElementById('artwork-modal');

    const evt = new dom.window.KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true });
    dom.window.document.dispatchEvent(evt);

    assert.ok(m.classList.contains('hidden'), 'Escape must close the outer modal once the crop is clean');
    assert.equal(dom.window.guardedCloseCropModalCalls, undefined, 'guardedCloseCropModal must not be called when the crop is clean');
  });

  // No-silent-failure: a missing guardedCloseCropModal (e.g. the crop editor
  // fragment was never loaded, so _cropDirty is somehow true without the
  // guard function existing) must fail loudly, not swallow the close.
  it('falls back to a normal close and logs an error if guardedCloseCropModal is missing while dirty', () => {
    const dom = createDom({ html: MODAL_HTML, modules: ['artworkModal'] });
    dom.window.swArtworkModal.open('backdrops');
    dom.window._cropDirty = true;
    // guardedCloseCropModal deliberately left undefined.
    const errors = [];
    dom.window.console.error = (msg) => errors.push(msg);
    const m = dom.window.document.getElementById('artwork-modal');

    const evt = new dom.window.KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true });
    dom.window.document.dispatchEvent(evt);

    assert.ok(m.classList.contains('hidden'), 'the outer modal must still close rather than get stuck open');
    assert.equal(errors.length, 1, 'the missing-guard fallback must log exactly one error');
    assert.match(errors[0], /guardedCloseCropModal unavailable/);
  });
});
