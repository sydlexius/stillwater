// Tests for the settings section-refresh helper (M55 #1339 items 1+6).
//
// swRefreshSettingsSection(name) replaces the old reload-on-save: it re-fetches
// the current page, parses it inertly (so the ambient backdrop <img> is NEVER
// requested -> no random re-roll), extracts the one [data-settings-fragment]
// node, and swaps it in -- no document reload, scroll position untouched.
//
// These cover the contract that matters for the bug fix:
//   - the swap happens (live node replaced by the fresh server copy),
//   - the page is fetched exactly once and the backdrop image is NOT requested
//     (the parse is inert),
//   - htmx re-binds the new subtree and an htmx:afterSwap fires (sortable re-init),
//   - missing live target / missing fragment fail loudly and do NOT swap.
import { describe, it, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import { createDom, makeFetchMock, flush } from './helpers/dom-harness.js';

// A live settings page fragment in its "stale" state.
const LIVE_HTML = `<!doctype html><html><body>
<img id="ambient-backdrop-img" src="/api/v1/images/random-backdrop" />
<main>
  <div data-settings-fragment="webhooks"><span id="marker">OLD</span></div>
</main>
</body></html>`;

// What the server returns when the helper re-fetches the page: the SAME backdrop
// img tag (must stay unrequested) plus the fragment in its "fresh" state.
const REFRESHED_PAGE = `<!doctype html><html><body>
<img id="ambient-backdrop-img" src="/api/v1/images/random-backdrop" />
<main>
  <div data-settings-fragment="webhooks"><span id="marker">FRESH</span></div>
</main>
</body></html>`;

function setup({ html = LIVE_HTML, pageResponse = REFRESHED_PAGE } = {}) {
  const dom = createDom({ html, modules: ['sectionRefresh'] });
  const win = dom.window;

  const processed = [];
  win.htmx = { process: (el) => processed.push(el) };

  const errors = [];
  win.console.error = (...args) => errors.push(args.join(' '));

  // Fetch returns the full refreshed page as text. .calls records every request.
  win.fetch = makeFetchMock({ ok: true, status: 200, text: pageResponse });

  return { dom, win, processed, errors };
}

describe('swRefreshSettingsSection: happy path swaps just the fragment', () => {
  it('replaces the live fragment with the server copy and never reloads', async () => {
    const { win, processed } = setup();

    let afterSwap = 0;
    win.document.addEventListener('htmx:afterSwap', () => { afterSwap++; });

    const ok = await win.swRefreshSettingsSection('webhooks');
    assert.equal(ok, true, 'refresh resolves true on success');

    // The live fragment now shows the fresh server content.
    assert.equal(
      win.document.querySelector('[data-settings-fragment="webhooks"] #marker').textContent,
      'FRESH',
      'the stale fragment must be replaced by the refreshed server copy',
    );
    // htmx re-processed the new subtree so its hx-* controls re-bind.
    assert.equal(processed.length, 1, 'htmx.process must run once on the swapped node');
    assert.equal(
      processed[0].getAttribute('data-settings-fragment'), 'webhooks',
      'htmx.process must receive the new fragment node',
    );
    // afterSwap fired so sortable-init (and other consumers) re-attach.
    assert.equal(afterSwap, 1, 'an htmx:afterSwap event must fire after the swap');
  });

  it('fetches the page exactly once and never requests the ambient backdrop', async () => {
    const { win } = setup();
    await win.swRefreshSettingsSection('webhooks');

    // Exactly one network call -- the page itself. The inert DOMParser parse of
    // the response must NOT trigger a request for the backdrop image, which is
    // the whole point of the no-reroll fix.
    assert.equal(win.fetch.calls.length, 1, 'must fetch exactly once');
    assert.equal(win.fetch.calls[0].url, win.location.href, 'must re-fetch the current page URL');
    const backdropHits = win.fetch.calls.filter(c => String(c.url).includes('random-backdrop'));
    assert.equal(backdropHits.length, 0, 'the ambient backdrop must never be requested');
  });
});

describe('swRefreshSettingsSection: loud failures, no swap', () => {
  it('errors and does not fetch when the live target is absent', async () => {
    const { win, errors } = setup({ html: '<!doctype html><html><body></body></html>' });

    const ok = await win.swRefreshSettingsSection('webhooks');
    assert.equal(ok, false, 'returns false when there is no live target');
    assert.equal(win.fetch.calls.length, 0, 'must not fetch when nothing to swap');
    assert.equal(errors.length, 1, 'must log exactly one error (no silent no-op)');
    assert.match(errors[0], /no live .*webhooks/);
  });

  it('errors and leaves the live node intact when the fragment is missing from the response', async () => {
    const { win, errors } = setup({ pageResponse: '<!doctype html><html><body><main>no fragment here</main></body></html>' });

    const ok = await win.swRefreshSettingsSection('webhooks');
    assert.equal(ok, false, 'returns false when the refreshed page lacks the fragment');
    // The stale live node must remain untouched rather than be wiped.
    assert.equal(
      win.document.querySelector('[data-settings-fragment="webhooks"] #marker').textContent,
      'OLD',
      'the live fragment must be left intact when the refresh cannot find a replacement',
    );
    assert.equal(errors.length, 1, 'must log exactly one error');
    assert.match(errors[0], /missing from the refreshed page/);
  });
});
