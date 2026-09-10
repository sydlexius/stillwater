// Unit tests for the fetch-URL-modal IIFE in web/templates/image_search.templ
// (the plain <script> block containing swOpenFetchUrlForSlot / _fetchUrlSlot).
//
// The script is embedded inline in a .templ file rather than a standalone
// .js module, so this test extracts the relevant <script>...</script> block
// at load time (it is pure JS, no Go interpolation) and evals it into a fresh
// jsdom context via dom-harness, exactly like the extracted artist-detail/*.js
// modules.
//
// Covers the #2281 fix-round P1 data-loss finding: _fetchUrlSlot (armed by
// swOpenFetchUrlForSlot for a per-slot backdrop Fetch/Replace) must be
// cleared not just by the submit handler, but also by the fetch-url-modal's
// Cancel button and by the Actions-menu "Fetch from URL" entry -- otherwise a
// cancelled per-slot fetch leaves a stale target that silently replaces that
// backdrop slot on the next "current type" fetch.
import { describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, writeFileSync, mkdtempSync } from 'node:fs';
import { join, dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { tmpdir } from 'node:os';
import { createDom, makeFetchMock, flush } from './helpers/dom-harness.js';

const __dirname = dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = resolve(__dirname, '../..');
const TEMPL_PATH = join(REPO_ROOT, 'web/templates/image_search.templ');

// extractFetchUrlScript pulls the SECOND <script>...</script> block out of
// image_search.templ -- the drag-drop/upload/fetch-URL IIFE that defines
// swOpenFetchUrlForSlot, swOpenFetchUrlModal, swCloseFetchUrlModal, and
// swOpenCropForSlot. Identified by containing "swOpenFetchUrlForSlot" so a
// future reordering of the file's <script> blocks does not silently extract
// the wrong one.
// The open/close tags are anchored to their own line: a bare /<script>/ also
// matches the literal tag name inside templ's `//` comments (image_search.templ
// has one), which yields a block starting mid-comment.
function extractFetchUrlScript() {
  const src = readFileSync(TEMPL_PATH, 'utf-8');
  const blocks = [...src.matchAll(/^\s*<script>$([\s\S]*?)^\s*<\/script>$/gm)];
  const match = blocks.find(b => b[1].includes('swOpenFetchUrlForSlot'));
  if (!match) {
    throw new Error('could not find the swOpenFetchUrlForSlot <script> block in image_search.templ -- did it move or get renamed?');
  }
  return match[1];
}

// writeScriptToTempFile writes the extracted script to a temp .js file so
// dom-harness's createDom (which loads modules via readFileSync) can eval it.
function writeScriptToTempFile() {
  const dir = mkdtempSync(join(tmpdir(), 'sw-image-search-fetch-slot-'));
  const path = join(dir, 'extracted.js');
  writeFileSync(path, extractFetchUrlScript(), 'utf-8');
  return path;
}

// FIXTURE_HTML provides every element ID the IIFE touches (directly or via
// addEventListener) so it runs to completion without throwing on a null
// getElementById result. data-artist-id + data-image-type on the container
// satisfy the IIFE's own early-return guard.
// #3223 review round 3, C3: data-context-menu/[role=menu]/[aria-haspopup]
// mirror the real Actions-menu markup image_search.templ renders around the
// indexed backdrop branch's Fetch-from-URL and Crop menuitems, so this
// fixture can exercise swOpenFetchUrlForSlot / swOpenCropForSlot's own
// menu-closing behavior (added in C3), not just the request-body threading
// the other tests in this file already cover.
//
// #3223 review round 4, H3: #unrelated-field-sheet stands in for a
// completely UNRELATED ContextMenu's mobile bottom sheet elsewhere on the
// page -- e.g. an artist-detail field's actions sheet
// (artist_field.templ ~510), rendered alongside this editor inside the
// Manage-artwork modal (handlers_artist_detail.go ~110). It shares
// role="menu" and starts OPEN (.ctx-sheet-open, no .hidden class -- exactly
// how components.ContextMenu's real bottom sheet markup behaves), which is
// the shape that let swCloseAnyOpenContextMenu's old selector match it by
// mistake and add .hidden to it (a class its own open/close logic never
// checks or clears). It is nested inside its OWN, SEPARATE
// [data-context-menu] wrapper -- matching the real markup exactly
// (components.ContextMenu, context_menu.templ ~46-84, renders BOTH the
// desktop panel AND the bottom sheet as siblings inside one
// [data-context-menu] div) -- because the old selector's bug was never
// about wrapper scoping (querySelectorAll('[data-context-menu] [role=menu]')
// intentionally matches ANY [data-context-menu] on the page, not just this
// editor's own), it was about failing to exclude .ctx-bottom-sheet from the
// element type. Putting the fixture sheet outside any [data-context-menu]
// wrapper would not have reproduced the bug (confirmed: an earlier version
// of this fixture did exactly that, and the pre-fix code passed against it
// for the wrong reason -- the selector's [data-context-menu] ancestor
// requirement, not the .hidden exclusion, was what saved it).
const FIXTURE_HTML = `<!doctype html><html><body>
<div data-artist-id="artist123" data-image-type="fanart"
     data-msg-fetching="Fetching..." data-msg-fetch-unable="Unable to fetch"
     data-msg-fetch-failed="Fetch failed" data-msg-needs-crop="Needs crop"
     data-msg-uploading="Uploading..." data-msg-upload-failed="Upload failed">
  <div id="image-drop-zone"></div>
  <div id="drop-hint" class="hidden"></div>
  <input id="image-file-input" type="file"/>
  <div id="upload-status"></div>
</div>
<div data-context-menu="img-actions-artist123">
  <button aria-haspopup="true" aria-expanded="true">Actions</button>
  <div role="menu">
    <button role="menuitem" id="menu-fetch-btn">Fetch from URL</button>
    <button role="menuitem" id="menu-crop-btn">Crop</button>
  </div>
</div>
<div id="fetch-url-modal" class="hidden">
  <input id="fetch-url-input" type="url"/>
  <button id="fetch-url-submit">Fetch</button>
</div>
<div id="crop-modal" class="hidden">
  <img id="crop-image"/>
  <select id="crop-type"><option value="fanart">Fanart</option></select>
  <input id="crop-lock-ratio" type="checkbox"/>
  <div id="crop-error" class="hidden"></div>
  <button id="crop-save-btn">Save</button>
</div>
<div data-context-menu="field-name-actions">
  <button aria-haspopup="true" aria-expanded="false">Edit</button>
  <div id="unrelated-field-sheet" class="ctx-bottom-sheet ctx-sheet-open" role="menu" aria-hidden="false">
    <button role="menuitem">Edit field</button>
  </div>
</div>
</body></html>`;

function loadDom() {
  const scriptPath = writeScriptToTempFile();
  return createDom({ html: FIXTURE_HTML, modules: [scriptPath] });
}

// submitAndCaptureBody arms the fetch-url-input with a URL, clicks Submit,
// and returns the JSON body of the resulting POST to .../images/fetch.
async function submitAndCaptureBody(dom) {
  // Respond with needs_crop rather than a plain "ok": a plain "ok" success
  // triggers window.location.reload(), which jsdom does not implement. The
  // needs_crop path instead calls openAutoCrop, which since #2415 lives in the
  // file's OTHER <script> block (the crop block, rendered on both image
  // layouts) and so is not defined by this isolated eval. Stub it -- this test
  // is about the request body, and the handler's own behavior is covered by
  // image-needs-crop-handler.test.js.
  dom.window.openAutoCrop = () => {};
  const fetchMock = makeFetchMock({
    ok: true,
    json: { status: 'needs_crop', needs_crop: true, type: 'fanart', image_data: 'data:image/jpeg;base64,AA==', required_ratio: 1 },
  });
  dom.window.fetch = fetchMock;
  dom.window.document.getElementById('fetch-url-input').value = 'https://example.com/img.jpg';
  dom.window.document.getElementById('fetch-url-submit').click();
  await flush();
  const call = fetchMock.calls.find(c => String(c.url).includes('/images/fetch'));
  if (!call) throw new Error('expected a POST to .../images/fetch, got none');
  return JSON.parse(call.options.body);
}

describe('image_search.templ fetch-url-modal: _fetchUrlSlot staleness (#2281 fix-round P1)', () => {
  it('a per-slot fetch (swOpenFetchUrlForSlot) threads slot into the request body', async () => {
    const dom = loadDom();
    dom.window.swOpenFetchUrlForSlot(2);
    const body = await submitAndCaptureBody(dom);
    assert.equal(body.slot, 2, 'submitting after swOpenFetchUrlForSlot(2) must include slot: 2');
    assert.equal(body.type, 'fanart', 'a slotted fetch must force type=fanart');
  });

  it('Cancel (swCloseFetchUrlModal) clears a stale slot before the next submit', async () => {
    const dom = loadDom();
    dom.window.swOpenFetchUrlForSlot(2);
    // Simulate the user cancelling the per-slot fetch dialog.
    dom.window.swCloseFetchUrlModal();
    // ...then later opening + submitting a plain "current type" fetch. If
    // _fetchUrlSlot were not cleared by Cancel, this would silently replace
    // slot 2 instead of fetching the current type (the #2281 P1 finding).
    dom.window.swOpenFetchUrlModal();
    const body = await submitAndCaptureBody(dom);
    assert.equal(body.slot, undefined, 'slot must NOT be present after Cancel clears the stale per-slot target');
    assert.equal(body.type, 'fanart', 'falls back to the container\'s current image-type (fanart in this fixture)');
  });

  it('the Actions-menu open path (swOpenFetchUrlModal) clears a stale slot even without an explicit Cancel', async () => {
    const dom = loadDom();
    dom.window.swOpenFetchUrlForSlot(3);
    // No explicit Cancel this time -- the Actions-menu "Fetch from URL" entry
    // itself must also clear the stale slot (this is the other half of the
    // #2281 P1 finding: the Actions-menu path only ever unhid the modal).
    dom.window.swOpenFetchUrlModal();
    const body = await submitAndCaptureBody(dom);
    assert.equal(body.slot, undefined, 'slot must NOT be present after swOpenFetchUrlModal (Actions-menu path) clears the stale per-slot target');
  });

  it('swCloseFetchUrlModal hides the modal', () => {
    const dom = loadDom();
    dom.window.swOpenFetchUrlForSlot(1);
    const modal = dom.window.document.getElementById('fetch-url-modal');
    assert.ok(!modal.classList.contains('hidden'), 'precondition: modal is visible after opening');
    dom.window.swCloseFetchUrlModal();
    assert.ok(modal.classList.contains('hidden'), 'swCloseFetchUrlModal must hide the fetch-url-modal');
  });
});

// #3223 review round 3, C2/P2: the fetch-url-submit handler used to call
// r.json() unconditionally. A non-OK response whose body is not valid JSON
// (an empty body, or an HTML error page from a proxy) makes that Promise
// REJECT, which fell through to the .catch() and showed msgFetchFailed --
// contradicting the code's own comment, which promised a msgFetchUnable
// fallback for exactly this case. Fixed by reading the body as text and
// JSON.parse-ing it inside try/catch, treating a parse failure as data=null.
//
// These are the UNIT-level counterpart to the C2 Playwright cases in
// tests/a11y/google-images-link.spec.js, which cover the same three shapes
// against a REAL browser + a routed HTTP response; this file's dom-harness
// mock lets each case assert the exact #upload-status text synchronously,
// without a browser.
describe('image_search.templ fetch-url-modal: non-OK response body parsing (#3223 review round 3, C2)', () => {
  // errorFetchMock returns a non-OK response whose text() is the given raw
  // body. json() is also implemented -- as a REAL browser's Response.json()
  // would: parse the SAME body text() exposes, rejecting if it isn't valid
  // JSON, exactly the way JSON.parse would.
  //
  // #3223 review round 4, H4: the earlier version of this mock omitted
  // json() entirely, on the reasoning that "the fixed handler now calls
  // ONLY text(), never json(), so a mock missing json() doubles as a
  // regression guard". That reasoning was wrong in a way that was proven by
  // reverting the C2 fix and re-running: with r.json() restored in the
  // handler, the "422 JSON" case failed with 'Fetch failed', NOT the
  // meaningful assertion about the wrong text -- because
  // "fetchMock.json is not a function" threw, was caught by the handler's
  // OWN outer .catch (a real code path, not a test artifact), and showed
  // msgFetchFailed. The 422 case was passing before this fix, but for the
  // WRONG reason: it was guarding "the mock has no json() method", not
  // "data.error displays correctly". Giving the mock a real json() that
  // mirrors text() means all three cases now guard the actual C2 behavior:
  // (1) a genuinely-JSON 422 body's data.error must display, and (2)/(3) a
  // non-JSON 502 body must fall back to msgFetchUnable -- not "the handler
  // happens to call the one method this mock implements".
  function errorFetchMock(status, text) {
    const calls = [];
    function mock(url, options) {
      calls.push({ url, options });
      return Promise.resolve({
        ok: false,
        status,
        text: () => Promise.resolve(text),
        json: () => {
          try {
            return Promise.resolve(JSON.parse(text));
          } catch (e) {
            return Promise.reject(e);
          }
        },
      });
    }
    mock.calls = calls;
    return mock;
  }

  function statusText(dom) {
    return dom.window.document.getElementById('upload-status').textContent;
  }

  async function submitWithMock(dom, fetchMock) {
    dom.window.fetch = fetchMock;
    dom.window.document.getElementById('fetch-url-input').value = 'https://example.com/img.jpg';
    dom.window.document.getElementById('fetch-url-submit').click();
    await flush();
  }

  // Guards: r.text() is parsed as JSON and data.error is displayed verbatim
  // when the body genuinely parses -- the "happy path" of the C2 fix's
  // try/catch (the try succeeds, data is non-null, data.error exists).
  it('a 422 with a JSON {error: "X"} body shows X', async () => {
    const dom = loadDom();
    const svgMessage = 'That link points to an SVG image, which Stillwater cannot use. Pick a PNG or JPG result instead.';
    await submitWithMock(dom, errorFetchMock(422, JSON.stringify({ error: svgMessage })));

    assert.equal(statusText(dom), svgMessage,
      'a 422 with a JSON error body must show that exact server message');
  });

  // Guards: an empty string fails JSON.parse (caught by the C2 fix's
  // try/catch, data=null) and falls back to msgFetchUnable -- NOT into the
  // outer .catch's msgFetchFailed, which is what happened before the C2 fix
  // when r.json() itself rejected on an empty/non-JSON body.
  it('a 502 with an EMPTY body shows the msgFetchUnable text, not msgFetchFailed', async () => {
    const dom = loadDom();
    await submitWithMock(dom, errorFetchMock(502, ''));

    assert.equal(statusText(dom), 'Unable to fetch',
      'an empty error body must fall back to msgFetchUnable (the fixture\'s data-msg-fetch-unable), not reject into msgFetchFailed');
  });

  // Guards: the same fallback as the empty-body case, but for a body that IS
  // non-empty text yet still invalid JSON (an HTML error page) -- proves the
  // try/catch's fallback isn't merely "empty string special-cased", it's a
  // genuine JSON.parse failure catch.
  it('a 502 with an HTML body (a proxy error page) does the same as an empty body', async () => {
    const dom = loadDom();
    await submitWithMock(dom, errorFetchMock(502, '<html><body><h1>502 Bad Gateway</h1></body></html>'));

    assert.equal(statusText(dom), 'Unable to fetch',
      'an HTML (non-JSON) error body must fall back to msgFetchUnable, not reject into msgFetchFailed');
  });
});

// #3223 review round 3, C3: swOpenFetchUrlForSlot and swOpenCropForSlot did
// not close the Actions menu themselves. The comment beside the Google
// Images menu entry claimed its own menu-close "matches the Fetch from URL
// button above", which was false for the indexed backdrop branch (the one
// calling swOpenFetchUrlForSlot) at the time -- the same defect class F5
// fixed on the Google entry was still live on its siblings. Fixed by adding
// a shared swCloseAnyOpenContextMenu() call to both functions.
describe('image_search.templ Actions menu: swOpenFetchUrlForSlot / swOpenCropForSlot close the menu (#3223 review round 3, C3)', () => {
  function menuState(dom) {
    const menu = dom.window.document.querySelector('[role="menu"]');
    const trigger = dom.window.document.querySelector('[aria-haspopup="true"]');
    return {
      menuHidden: menu.classList.contains('hidden'),
      triggerExpanded: trigger.getAttribute('aria-expanded'),
    };
  }

  it('swOpenFetchUrlForSlot closes the open Actions menu before opening the fetch dialog', () => {
    const dom = loadDom();
    // Precondition: the fixture's menu starts open (matches the real DOM
    // state at the moment a menuitem inside it is clicked).
    const before = menuState(dom);
    assert.equal(before.menuHidden, false, 'precondition: the menu starts open');
    assert.equal(before.triggerExpanded, 'true', 'precondition: the trigger starts expanded');

    dom.window.swOpenFetchUrlForSlot(1);

    const after = menuState(dom);
    assert.equal(after.menuHidden, true, 'swOpenFetchUrlForSlot must close the open Actions menu');
    assert.equal(after.triggerExpanded, 'false', 'swOpenFetchUrlForSlot must mark the trigger collapsed (aria-expanded=false)');
    assert.equal(dom.window.document.getElementById('fetch-url-modal').classList.contains('hidden'), false,
      'the fetch-url-modal must still open as before');
  });

  it('swOpenCropForSlot closes the open Actions menu before opening the crop modal', () => {
    const dom = loadDom();
    const opened = [];
    dom.window.openCropModal = (...args) => opened.push(args);

    dom.window.swOpenCropForSlot(1);

    const after = menuState(dom);
    assert.equal(after.menuHidden, true, 'swOpenCropForSlot must close the open Actions menu');
    assert.equal(after.triggerExpanded, 'false', 'swOpenCropForSlot must mark the trigger collapsed (aria-expanded=false)');
    assert.equal(opened.length, 1, 'openCropModal must still be invoked as before');
  });

  it('swOpenCropForSlot is a harmless no-op on menu-closing when there is no open menu (the backdrop-gallery call site)', () => {
    const dom = loadDom();
    // Simulate the backdrop_management.templ gallery tile call site, which
    // has no [data-context-menu] ancestor at all -- close the fixture's menu
    // first so none is open, matching that call site's real DOM shape.
    dom.window.document.querySelector('[role="menu"]').classList.add('hidden');
    dom.window.document.querySelector('[aria-haspopup="true"]').setAttribute('aria-expanded', 'false');
    dom.window.openCropModal = () => {};

    assert.doesNotThrow(() => dom.window.swOpenCropForSlot(1),
      'swCloseAnyOpenContextMenu must not throw when no menu is open');
  });

  // #3223 review round 4, H3.
  function unrelatedSheetState(dom) {
    const sheet = dom.window.document.getElementById('unrelated-field-sheet');
    return {
      hidden: sheet.classList.contains('hidden'),
      sheetOpen: sheet.classList.contains('ctx-sheet-open'),
      ariaHidden: sheet.getAttribute('aria-hidden'),
    };
  }

  it('swOpenFetchUrlForSlot leaves an unrelated OPEN .ctx-bottom-sheet alone (does not add .hidden to it)', () => {
    const dom = loadDom();
    const before = unrelatedSheetState(dom);
    assert.equal(before.hidden, false, 'precondition: the unrelated sheet starts without .hidden');
    assert.equal(before.sheetOpen, true, 'precondition: the unrelated sheet starts open (.ctx-sheet-open)');

    dom.window.swOpenFetchUrlForSlot(1);

    const after = unrelatedSheetState(dom);
    assert.equal(after.hidden, false,
      'swOpenFetchUrlForSlot must NOT add .hidden to an unrelated bottom sheet -- that class is never checked by the sheet\'s own open/close logic, so adding it leaves the sheet permanently invisible (display:none via CSS keyed on .ctx-bottom-sheet.ctx-sheet-open) until a full page reload');
    assert.equal(after.sheetOpen, true, 'the unrelated sheet must remain open -- this function has no business touching a sheet outside its own [data-context-menu] scope');
  });

  it('swOpenCropForSlot leaves an unrelated OPEN .ctx-bottom-sheet alone (does not add .hidden to it)', () => {
    const dom = loadDom();
    dom.window.openCropModal = () => {};

    dom.window.swOpenCropForSlot(1);

    const after = unrelatedSheetState(dom);
    assert.equal(after.hidden, false, 'swOpenCropForSlot must NOT add .hidden to an unrelated bottom sheet');
    assert.equal(after.sheetOpen, true, 'the unrelated sheet must remain open');
  });
});
