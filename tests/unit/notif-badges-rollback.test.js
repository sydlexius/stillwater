// Regression tests for the notif-badges.js optimistic-rollback pattern (#1829).
//
// Verifies that updateSetting(key, el) rolls back el.checked and calls
// showToast() on both server-error and network-error paths, and that the
// data-inflight re-entrancy guard is set during the fetch and cleared on
// completion (success and failure).
import { describe, it, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import { createDom, makeFetchMock, flush } from './helpers/dom-harness.js';

// Minimal page: a single checkbox; notif-badges.js loads updateSetting onto window.
const PAGE_HTML = `<!doctype html><html><body>
<input type="checkbox" id="cb" checked />
</body></html>`;

function setup(fetchSpec) {
  const dom = createDom({
    html: PAGE_HTML,
    modules: ['notifBadges'],
    csrfToken: 'tok',
  });
  const win = dom.window;
  if (fetchSpec !== undefined) {
    win.fetch = makeFetchMock(fetchSpec);
  }
  const cb = win.document.getElementById('cb');
  return { dom, win, cb };
}

describe('notif-badges updateSetting: success path', () => {
  it('fires PUT /api/v1/settings with the correct body and clears inflight', async () => {
    const { win, cb } = setup({ ok: true, status: 200 });
    const fetchMock = makeFetchMock({ ok: true, status: 200 });
    win.fetch = fetchMock;

    // Simulate browser toggling checked to false before onclick fires.
    cb.checked = false;
    win.updateSetting('notif_badge_enabled', cb);
    await flush();

    const puts = fetchMock.calls.filter(c => c.options && c.options.method === 'PUT');
    assert.equal(puts.length, 1, 'should fire exactly one PUT');
    assert.match(puts[0].url, /\/api\/v1\/settings$/);
    const body = JSON.parse(puts[0].options.body);
    assert.equal(body.notif_badge_enabled, 'false', 'body must encode new value as string');

    // Inflight guard must be cleared after completion.
    assert.equal(cb.dataset.inflight, undefined, 'inflight guard must be cleared on success');
    // Checkbox must stay at the new value (server accepted).
    assert.equal(cb.checked, false, 'checkbox must stay at new value on success');
  });
});

describe('notif-badges updateSetting: server error rolls back', () => {
  it('reverts el.checked and calls showToast on non-ok response', async () => {
    const { win, cb } = setup();

    const toastCalls = [];
    win.showToast = (msg) => toastCalls.push(msg);
    win.fetch = makeFetchMock({ ok: false, status: 500 });

    // Simulate browser toggling checked to false before onclick fires.
    cb.checked = false;
    win.updateSetting('notif_badge_enabled', cb);
    await flush();

    // Checkbox must be rolled back to the prior value (true).
    assert.equal(cb.checked, true, 'checkbox must roll back to prior value on server error');
    // Toast must fire.
    assert.equal(toastCalls.length, 1, 'showToast must be called on server error');
    // Inflight guard must be cleared.
    assert.equal(cb.dataset.inflight, undefined, 'inflight guard must be cleared after server error');
  });
});

describe('notif-badges updateSetting: network error rolls back', () => {
  it('reverts el.checked and calls showToast on fetch rejection', async () => {
    const { win, cb } = setup();

    const toastCalls = [];
    win.showToast = (msg) => toastCalls.push(msg);
    win.fetch = () => Promise.reject(new Error('network'));

    cb.checked = false;
    win.updateSetting('notif_badge_enabled', cb);
    await flush();

    assert.equal(cb.checked, true, 'checkbox must roll back to prior value on network error');
    assert.equal(toastCalls.length, 1, 'showToast must be called on network error');
    assert.equal(cb.dataset.inflight, undefined, 'inflight guard must be cleared after network error');
  });
});

describe('notif-badges updateSetting: inflight re-entrancy guard', () => {
  it('does not fire a second PUT while the first is in flight', async () => {
    const { win, cb } = setup();

    const fetchCalls = [];
    let resolveFetch;
    win.fetch = (url, opts) => {
      fetchCalls.push({ url, opts });
      return new Promise(resolve => {
        resolveFetch = resolve;
      });
    };

    // First click: browser toggles cb from true to false; guard not yet set.
    cb.checked = false;
    win.updateSetting('notif_badge_enabled', cb);

    // Inflight guard is now active. Simulate a second click during the PUT.
    // The browser pre-toggles cb.checked before the handler fires; the guard
    // must revert that pre-toggle synchronously so the UI stays in sync.
    cb.checked = true;
    win.updateSetting('notif_badge_enabled', cb);

    // The guard must have reverted the browser's pre-toggle immediately.
    assert.equal(cb.checked, false, 'guard must revert the browser pre-toggle on re-entrant click');

    // Resolve the first fetch.
    resolveFetch({ ok: true, status: 200, json: () => Promise.resolve({}), text: () => Promise.resolve('') });
    await flush();

    // Only one PUT must have been fired - the re-entrant call was dropped.
    assert.equal(fetchCalls.length, 1, 'only one PUT must fire; re-entrant call must be ignored');
    // Inflight guard must be cleared once the first completes.
    assert.equal(cb.dataset.inflight, undefined, 'inflight guard must be cleared after completion');
  });
});
