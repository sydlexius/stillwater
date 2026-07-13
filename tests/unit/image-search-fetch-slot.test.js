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
