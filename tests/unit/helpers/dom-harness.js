// dom-harness.js - shared utilities for loading ES5 IIFE modules into a fresh
// jsdom context. Each test gets its own JSDOM instance so there is no shared
// state between cases.
import { JSDOM } from 'jsdom';
import { readFileSync } from 'node:fs';
import { resolve, dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = resolve(__dirname, '../../..');

export const MODULE_PATHS = {
  lightbox:     join(REPO_ROOT, 'web/static/js/artist-detail/lightbox.js'),
  fanartManage: join(REPO_ROOT, 'web/static/js/artist-detail/fanart-manage.js'),
  artworkModal: join(REPO_ROOT, 'web/static/js/artist-detail/artwork-modal.js'),
};

/**
 * createDom creates a fresh jsdom window and optionally loads module scripts.
 *
 * @param {object} opts
 * @param {string}   [opts.html]       - initial HTML (defaults to blank page)
 * @param {string[]} [opts.modules]    - keys from MODULE_PATHS to eval in order
 * @param {string|null} [opts.csrfToken] - if set, stubs window.swCsrfToken
 * @returns {import('jsdom').JSDOM}
 */
export function createDom({ html, modules = [], csrfToken = null } = {}) {
  const dom = new JSDOM(
    html || '<!doctype html><html><body></body></html>',
    { runScripts: 'dangerously', url: 'http://localhost:1973/' },
  );
  const win = dom.window;

  win.confirm = () => true;
  win.alert   = () => {};
  win.htmx    = { ajax: () => Promise.resolve() };

  // Default fetch stub; tests override dom.window.fetch per-case.
  win.fetch = makeFetchMock();

  if (csrfToken !== null) {
    win.swCsrfToken = () => csrfToken;
  }

  for (const key of modules) {
    const path = MODULE_PATHS[key] ?? key;
    win.eval(readFileSync(path, 'utf-8'));
  }

  return dom;
}

/**
 * makeFetchMock returns { mock, calls }.
 * mock is a fetch-compatible function that records every call.
 * responseSpec can be an object or a function(url, options) => object.
 * Response shape: { ok, status, json, text }.
 */
export function makeFetchMock(responseSpec = { ok: true, status: 200 }) {
  const calls = [];
  function mock(url, options) {
    calls.push({ url, options });
    const spec = typeof responseSpec === 'function'
      ? responseSpec(url, options)
      : responseSpec;
    const jsonValue = spec.json ?? {};
    const textValue = spec.text ?? '';
    const response = {
      ok:     spec.ok !== undefined ? spec.ok : true,
      status: spec.status ?? 200,
      json:  () => Promise.resolve(jsonValue),
      text:  () => Promise.resolve(textValue),
      clone: () => ({
        json: () => Promise.resolve(jsonValue),
        text: () => Promise.resolve(textValue),
      }),
    };
    return Promise.resolve(response);
  }
  mock.calls = calls;
  return mock;
}

/** flush drains all pending microtasks + one macrotask turn. */
export function flush() {
  return new Promise(r => setTimeout(r, 0));
}
