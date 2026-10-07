// load() reuses the request already in flight, so a caller that waits on it
// waits for the SAME response whose applyAll() the page will run (#3078).
import { describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { createDom, flush } from './helpers/dom-harness.js';

// A fetch whose responses are released by hand, recording every call.
function deferredFetch() {
  const calls = [];
  const pending = [];
  function fetchFn(url) {
    calls.push(url);
    return new Promise((resolve) => pending.push(() => resolve({
      ok: true, status: 200, json: () => Promise.resolve({ theme: 'light' }),
    })));
  }
  fetchFn.calls = calls;
  fetchFn.release = () => pending.splice(0).forEach((r) => r());
  return fetchFn;
}

async function setup() {
  const dom = createDom({ modules: ['preferences'] });
  await flush(); // the init load against the default stub has settled
  const fetchFn = deferredFetch();
  dom.window.fetch = fetchFn;
  return { win: dom.window, fetchFn };
}

describe('swPreferences.load in-flight reuse', () => {
  it('two calls before the response make ONE request and share the promise', async () => {
    const { win, fetchFn } = await setup();
    const a = win.swPreferences.load();
    const b = win.swPreferences.load();
    assert.equal(a, b, 'the second call returns the in-flight promise');
    assert.equal(fetchFn.calls.length, 1, 'one request');
    fetchFn.release();
    await a;
  });

  it('a call after the load settles makes a NEW request', async () => {
    const { win, fetchFn } = await setup();
    const first = win.swPreferences.load();
    fetchFn.release();
    await first;
    const second = win.swPreferences.load();
    assert.equal(fetchFn.calls.length, 2, 'fresh request after settle');
    fetchFn.release();
    await second;
  });

  it('a failed load also clears the reference', async () => {
    const { win } = await setup();
    let n = 0;
    win.fetch = () => { n++; return Promise.reject(new Error('offline')); };
    await win.swPreferences.load();
    await win.swPreferences.load();
    assert.equal(n, 2, 'a failure does not wedge later loads');
  });
});
