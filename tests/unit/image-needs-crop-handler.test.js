// Unit tests for the shared needs_crop response handler (#2415).
//
// /images/fetch and /images/upload answer an aspect-ratio mismatch with a 200
// whose body is {needs_crop: true, image_data: ...} and NOTHING SAVED. Before
// #2415, three of the four client surfaces that POST to those endpoints simply
// discarded that response: the user clicked Save, got no error, and got no
// image. These tests pin the reaction, not the status code -- the status code
// is the lie, so asserting on it would guard the bug.
//
// The handler is inline in web/templates/image_search.templ's crop <script>
// block rather than a standalone .js module, so this extracts that block at
// load time and evals it into a fresh jsdom context via dom-harness, the same
// way image-search-fetch-slot.test.js extracts the drag-drop IIFE.
import { describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, writeFileSync, mkdtempSync } from 'node:fs';
import { join, dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { tmpdir } from 'node:os';
import { createDom } from './helpers/dom-harness.js';

const __dirname = dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = resolve(__dirname, '../..');
const TEMPL_PATH = join(REPO_ROOT, 'web/templates/image_search.templ');

// extractCropScript pulls the script block that defines the needs_crop handlers
// out of image_search.templ. Identified by content, not position, so a future
// reordering of the file's script blocks cannot silently extract the wrong one.
//
// This block is the one ArtworkManageEditor renders on BOTH image layouts. The
// handlers deliberately live here and not in the drag-drop IIFE, which the
// generic layout never renders -- see the generic-layout case below.
//
// The open/close tags are anchored to their own line. A bare /<script>/ also
// matches the literal tag name inside templ's `//` comments (image_search.templ
// has one), which silently yields a block starting mid-comment -- a syntax
// error at eval, or worse, a subtly wrong block that still parses.
function extractCropScript() {
  const src = readFileSync(TEMPL_PATH, 'utf-8');
  const blocks = [...src.matchAll(/^\s*<script>$([\s\S]*?)^\s*<\/script>$/gm)];
  const match = blocks.find(b => b[1].includes('function swHandleFetchNeedsCrop'));
  if (!match) {
    throw new Error('could not find the swHandleFetchNeedsCrop script block in image_search.templ -- did it move or get renamed?');
  }
  return match[1];
}

function writeScriptToTempFile() {
  const dir = mkdtempSync(join(tmpdir(), 'sw-image-needs-crop-'));
  const path = join(dir, 'extracted.js');
  writeFileSync(path, extractCropScript(), 'utf-8');
  return path;
}

// CONTEXTUALIZED_HTML mirrors the single-type image editor: the crop modal
// (which carries the translated copy) plus the #upload-status line that only
// this layout renders.
const CONTEXTUALIZED_HTML = `<!doctype html><html><body>
<div id="upload-status"></div>
<div id="crop-modal" class="hidden"
     data-msg-needs-crop="Image needs cropping to match the required aspect ratio."
     data-msg-crop-unavailable="Crop editor unavailable; not saved."
     data-msg-save-unreadable="The server response could not be read.">
  <img id="crop-image"/>
  <select id="crop-type"><option value="thumb">Thumb</option><option value="fanart">Fanart</option></select>
  <input id="crop-lock-ratio" type="checkbox"/>
</div>
</body></html>`;

// GENERIC_HTML mirrors the multi-type ("all images") layout -- the one that
// renders components.ImageCard and components.ImageUpload. It has NO
// #upload-status and none of the contextualized editor's container, because
// imageSearchGeneric renders neither. The crop modal is still present, since
// ArtworkManageEditor renders it on both branches.
const GENERIC_HTML = `<!doctype html><html><body>
<div id="crop-modal" class="hidden"
     data-msg-needs-crop="Image needs cropping to match the required aspect ratio."
     data-msg-crop-unavailable="Crop editor unavailable; not saved."
     data-msg-save-unreadable="The server response could not be read.">
  <img id="crop-image"/>
  <select id="crop-type"><option value="thumb">Thumb</option><option value="fanart">Fanart</option></select>
  <input id="crop-lock-ratio" type="checkbox"/>
</div>
</body></html>`;

// loadDom evals the extracted block, then installs spies. openCropModal is the
// real dependency the handler reaches for; the extracted block does not define
// it (it is declared earlier in the same block, but Cropper.js is absent under
// jsdom), so each case stubs it and asserts on the call.
function loadDom(html = CONTEXTUALIZED_HTML) {
  const scriptPath = writeScriptToTempFile();
  const dom = createDom({ html, modules: [scriptPath] });
  const calls = { crop: [], toasts: [], errors: [] };
  dom.window.openCropModal = (...args) => calls.crop.push(args);
  dom.window.showToast = (msg) => calls.toasts.push(msg);
  dom.window.console.error = (msg) => calls.errors.push(msg);
  return { dom, calls };
}

// afterRequest fires the handler exactly as htmx's hx-on::after-request does,
// with a 2xx (successful) response carrying the given raw body.
function afterRequest(dom, responseText, successful = true) {
  dom.window.swHandleFetchNeedsCrop({
    detail: { successful, xhr: { responseText } },
  });
}

const NEEDS_CROP_BODY = JSON.stringify({
  status: 'needs_crop',
  needs_crop: true,
  type: 'thumb',
  image_data: 'data:image/jpeg;base64,AA==',
  required_ratio: 1,
  actual_ratio: 10,
  width: 1000,
  height: 100,
});

describe('swHandleFetchNeedsCrop: a needs_crop 200 saved nothing, so the user must get a crop prompt (#2415)', () => {
  it('opens the crop modal with the returned image, ratio and type', () => {
    const { dom, calls } = loadDom();
    afterRequest(dom, NEEDS_CROP_BODY);

    assert.equal(calls.crop.length, 1, 'a needs_crop response must open the crop modal exactly once');
    const [imgSrc, imageType, ratio] = calls.crop[0];
    assert.equal(imgSrc, 'data:image/jpeg;base64,AA==', 'the crop modal must receive the image the server returned');
    assert.equal(imageType, 'thumb');
    assert.equal(ratio, 1, 'the required aspect ratio must be locked into the cropper');
    assert.equal(dom.window.document.getElementById('crop-type').value, 'thumb',
      'the crop type select must be set so the follow-up crop save targets the right slot');
    assert.equal(calls.errors.length, 0, 'the happy path must not log an error');
  });

  it('threads append and slot through so the follow-up crop save lands where the fetch would have', () => {
    const { dom, calls } = loadDom();
    afterRequest(dom, JSON.stringify({
      needs_crop: true, type: 'fanart', image_data: 'data:image/png;base64,BB==',
      required_ratio: 1.78, append: true, slot: 2,
    }));

    const [, , , append, slot] = calls.crop[0];
    assert.equal(append, true, 'append must be forwarded so the crop save appends rather than replacing the primary');
    assert.equal(slot, 2, 'an explicit fanart slot must be forwarded so the crop save persists to that slot');
  });

  // The regression that motivated moving the handler out of the drag-drop IIFE.
  // components.ImageCard and components.ImageUpload render on the GENERIC
  // layout, which never renders that IIFE -- so a handler that depended on it
  // (or on the contextualized container / #upload-status) would no-op on
  // exactly the surfaces #2415 is about.
  it('still opens the crop modal on the generic layout, which has no #upload-status and no drag-drop IIFE', () => {
    const { dom, calls } = loadDom(GENERIC_HTML);
    assert.equal(dom.window.document.getElementById('upload-status'), null,
      'precondition: the generic layout renders no #upload-status');

    afterRequest(dom, NEEDS_CROP_BODY);

    assert.equal(calls.crop.length, 1,
      'the crop modal must open on the generic layout too -- that is where ImageCard and ImageUpload live');
    assert.equal(calls.errors.length, 0, 'a missing #upload-status is expected here, not an error');
  });

  it('sets the amber status line when the contextualized layout provides one', () => {
    const { dom } = loadDom();
    afterRequest(dom, NEEDS_CROP_BODY);

    const status = dom.window.document.getElementById('upload-status');
    assert.match(status.textContent, /needs cropping/i,
      'the contextualized layout must also tell the user why nothing was saved');
  });

  it('is a quiet no-op for a normal save', () => {
    const { dom, calls } = loadDom();
    afterRequest(dom, JSON.stringify({ status: 'ok', needs_crop: false }));

    assert.equal(calls.crop.length, 0, 'a real save must not open the crop modal');
    assert.equal(calls.errors.length, 0, 'a real save must not log anything');
    assert.equal(calls.toasts.length, 0, 'a real save must not toast');
  });

  it('ignores a failed request, leaving error handling to the surface', () => {
    const { dom, calls } = loadDom();
    afterRequest(dom, '{"error":"boom"}', false);

    assert.equal(calls.crop.length, 0);
    assert.equal(calls.errors.length, 0);
  });
});

describe('swHandleFetchNeedsCrop: no-silent-failure (#2415)', () => {
  it('fails loudly, not silently, when the crop modal cannot be opened', () => {
    const { dom, calls } = loadDom();
    delete dom.window.openCropModal;

    assert.doesNotThrow(() => afterRequest(dom, NEEDS_CROP_BODY),
      'a missing crop modal must not throw out of the htmx handler');
    assert.equal(calls.errors.length, 1, 'the missing-dependency branch must log exactly one error');
    assert.match(calls.errors[0], /openCropModal unavailable/);
    assert.equal(calls.toasts.length, 1, 'the user must be told their image was not saved, not just the console');
    assert.match(calls.toasts[0], /not saved/i);
  });

  it('fails loudly on an unparsable response rather than swallowing it', () => {
    const { dom, calls } = loadDom();

    assert.doesNotThrow(() => afterRequest(dom, 'not json at all'));
    assert.equal(calls.crop.length, 0);
    assert.equal(calls.errors.length, 1, 'an unreadable response must log exactly one error');
    assert.match(calls.errors[0], /could not parse/i);
    assert.equal(calls.toasts.length, 1, 'an unreadable response must reach the user');
  });

  it('still logs when even showToast is unavailable', () => {
    const { dom, calls } = loadDom();
    delete dom.window.openCropModal;
    delete dom.window.showToast;

    assert.doesNotThrow(() => afterRequest(dom, NEEDS_CROP_BODY));
    assert.equal(calls.errors.length, 2,
      'losing showToast must add a second error, never downgrade the failure to silence');
    assert.match(calls.errors[1], /showToast unavailable/);
  });
});

describe('swSuppressNeedsCropSwap: the base64 body must not be dumped into the page (#2415)', () => {
  // Both components.ImageUpload forms target #upload-result with
  // hx-swap="innerHTML". Without this, a needs_crop response lands as a
  // screenful of raw JSON and base64 image data -- with no hint that nothing
  // was saved -- while the crop modal opens over the top of it.
  function beforeSwap(dom, responseText) {
    const detail = { xhr: { responseText }, shouldSwap: true };
    dom.window.swSuppressNeedsCropSwap({ detail });
    return detail.shouldSwap;
  }

  it('suppresses the swap of a needs_crop body', () => {
    const { dom } = loadDom();
    assert.equal(beforeSwap(dom, NEEDS_CROP_BODY), false,
      'the needs_crop body must never be swapped into #upload-result');
  });

  it('leaves a normal save to swap as before', () => {
    const { dom } = loadDom();
    assert.equal(beforeSwap(dom, JSON.stringify({ status: 'ok' })), true,
      'a real save must still render its result');
  });

  it('leaves an unparsable body to swap, so the surface can show whatever the server said', () => {
    const { dom } = loadDom();
    assert.equal(beforeSwap(dom, 'not json'), true);
  });
});
