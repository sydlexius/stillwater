// Unit tests for the crop-modal script in web/templates/image_search.templ
// (the <script> block defining openCropModal / stageAndLoadCrop /
// closeCropModal / _cropSessionToken). Extracted the same way
// image-search-fetch-slot.test.js extracts its own script block.
//
// Covers #2331 CR-4: stageAndLoadCrop's async fetch callback used to write
// straight into the shared #crop-image / #upload-status elements with no
// check that the session it belongs to is still the active one. A stale
// callback (a slow stage resolving after a newer openCropModal call, or
// after the modal was closed) could clobber the current session or wrongly
// close a modal that has already moved on. The fix is a monotonic
// _cropSessionToken, captured by the caller at request time and re-checked
// by every callback before touching shared DOM.
import { describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, writeFileSync, mkdtempSync } from 'node:fs';
import { join, dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { tmpdir } from 'node:os';
import { createDom, flush } from './helpers/dom-harness.js';

const __dirname = dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = resolve(__dirname, '../..');
const TEMPL_PATH = join(REPO_ROOT, 'web/templates/image_search.templ');

// extractCropModalScript pulls the FIRST <script>...</script> block out of
// image_search.templ -- the one defining stageAndLoadCrop/openCropModal/
// closeCropModal/_cropSessionToken. Identified by containing
// "_cropSessionToken" so a future reordering of the file's <script> blocks
// does not silently extract the wrong one.
function extractCropModalScript() {
  const src = readFileSync(TEMPL_PATH, 'utf-8');
  // Anchor `<script>`/`</script>` to their own line (only leading whitespace
  // before them) so this does not match the literal substring "<script>"
  // appearing inside a prose comment elsewhere in the file (this file has
  // one: "...to avoid inline Go string interpolation inside <script>.").
  const blocks = [...src.matchAll(/^[ \t]*<script>\r?\n([\s\S]*?)^[ \t]*<\/script>/gm)];
  const match = blocks.find(b => b[1].includes('_cropSessionToken'));
  if (!match) {
    throw new Error('could not find the _cropSessionToken <script> block in image_search.templ -- did it move or get renamed?');
  }
  return match[1];
}

function writeScriptToTempFile() {
  const dir = mkdtempSync(join(tmpdir(), 'sw-image-search-crop-token-'));
  const path = join(dir, 'extracted.js');
  writeFileSync(path, extractCropModalScript(), 'utf-8');
  return path;
}

// FIXTURE_HTML provides every element ID stageAndLoadCrop/closeCropModal
// touch directly, so they run to completion without throwing on a null
// getElementById result. Cropper.js itself is NOT loaded -- these tests only
// exercise stageAndLoadCrop's own fetch-callback staleness guard, never
// letting a resolved stage actually reach cropImage.onload's `new Cropper(...)`
// call (jsdom does not fire img.onload on a bare `.src =` assignment anyway).
const FIXTURE_HTML = `<!doctype html><html><body>
<div id="crop-modal" class="hidden" data-artist-id="artist123"
     data-msg-staging="Loading image for cropping..."
     data-msg-crop-load-failed="Could not load this image for cropping. Please try again.">
  <img id="crop-image"/>
  <select id="crop-type"><option value="fanart">Fanart</option></select>
  <input id="crop-lock-ratio" type="checkbox"/>
  <div id="crop-error" class="hidden"></div>
  <button id="crop-save-btn">Save</button>
</div>
<div id="upload-status"></div>
</body></html>`;

function loadDom() {
  const scriptPath = writeScriptToTempFile();
  return createDom({ html: FIXTURE_HTML, modules: [scriptPath] });
}

// deferred returns a {promise, resolve} pair so a test can control exactly
// when a fetch call settles, independent of call order.
function deferred() {
  let resolve;
  const promise = new Promise(r => { resolve = r; });
  return { promise, resolve };
}

// jsonResponse builds a minimal fetch Response-shaped object around a JSON
// body, matching what stageAndLoadCrop's own .then(r => r.json()...) chain
// expects.
function jsonResponse(ok, body) {
  return { ok, json: () => Promise.resolve(body) };
}

describe('image_search.templ crop modal: stageAndLoadCrop session-token staleness (#2331 CR-4)', () => {
  it('a stale (slower) stage response must not overwrite a newer session\'s crop image', async () => {
    const dom = loadDom();
    const calls = [];
    const first = deferred();
    const second = deferred();
    dom.window.fetch = (url, opts) => {
      const d = calls.length === 0 ? first : second;
      calls.push({ url, opts });
      return d.promise;
    };

    // Session 1 starts staging a remote URL (openCropModal has already
    // incremented _cropSessionToken to 1 and passes that same value through).
    dom.window._cropSessionToken = 1;
    dom.window.stageAndLoadCrop('https://example.com/first.jpg', 'fanart', 1);
    // Session 2 starts before session 1's fetch resolves (e.g. the user
    // re-clicked Crop on a different candidate) -- openCropModal would have
    // incremented _cropSessionToken to 2 and passed that new token through.
    dom.window._cropSessionToken = 2;
    dom.window.stageAndLoadCrop('https://example.com/second.jpg', 'fanart', 2);

    // Resolve session 2 (the CURRENT session) first...
    second.resolve(jsonResponse(true, { image_data: 'data:image/jpeg;base64,SECOND' }));
    await flush();
    const cropImage = dom.window.document.getElementById('crop-image');
    assert.equal(cropImage.src, 'data:image/jpeg;base64,SECOND',
      'the current session\'s staged image must load into #crop-image');

    // ...then resolve session 1 (now STALE) after the fact.
    first.resolve(jsonResponse(true, { image_data: 'data:image/jpeg;base64,FIRST' }));
    await flush();
    assert.equal(cropImage.src, 'data:image/jpeg;base64,SECOND',
      'a stale session\'s late-resolving stage response must NOT clobber the current crop image');
  });

  it('a stale stage FAILURE must not close whatever session is currently active', async () => {
    const dom = loadDom();
    const stale = deferred();
    dom.window.fetch = () => stale.promise;

    dom.window._cropSessionToken = 1;
    dom.window.stageAndLoadCrop('https://example.com/stale.jpg', 'fanart', 1);
    // A newer session opens (token now 2) before the stale fetch settles.
    dom.window._cropSessionToken = 2;
    const modal = dom.window.document.getElementById('crop-modal');
    modal.classList.remove('hidden');
    modal.classList.add('flex'); // simulate the newer session's modal being open

    // The stale (session-1) fetch now fails.
    stale.resolve(jsonResponse(false, {}));
    await flush();

    assert.ok(!modal.classList.contains('hidden'),
      'a stale session\'s failure must not close the modal out from under the newer, still-open session');
  });

  it('closeCropModal invalidates an in-flight staging fetch so it cannot repaint a closed modal', async () => {
    const dom = loadDom();
    const pending = deferred();
    dom.window.fetch = () => pending.promise;

    dom.window._cropSessionToken = 1;
    dom.window.stageAndLoadCrop('https://example.com/x.jpg', 'fanart', 1);
    // The user gives up on the crop entirely before staging resolves.
    dom.window.closeCropModal();

    const modal = dom.window.document.getElementById('crop-modal');
    assert.ok(modal.classList.contains('hidden'), 'precondition: closeCropModal hides the modal');

    pending.resolve(jsonResponse(true, { image_data: 'data:image/jpeg;base64,LATE' }));
    await flush();

    const cropImage = dom.window.document.getElementById('crop-image');
    assert.notEqual(cropImage.src, 'data:image/jpeg;base64,LATE',
      'a staging fetch that resolves after the modal was closed must not still load its image');
    assert.ok(modal.classList.contains('hidden'), 'the modal must remain closed');
  });

  it('a fresh openCropModal call advances the session token, invalidating anything captured before it', () => {
    const dom = loadDom();
    dom.window._cropSessionToken = 5;
    // openCropModal needs Cropper's constructor only inside cropImage.onload,
    // which jsdom never fires for a bare `.src =` assignment -- safe to call
    // directly here just to observe the token increment.
    dom.window.openCropModal('/api/v1/artists/artist123/images/fanart/file', 'fanart', undefined, false);
    assert.equal(dom.window._cropSessionToken, 6, 'openCropModal must increment _cropSessionToken exactly once');
  });
});
