// settings-sortable-single-load.spec.js - /settings loads Sortable.min.js once
// and the provider-priority lists still initialise (#2189).
//
// History: the settings page emitted its own Sortable.min.js ahead of
// sortable-init.js, and LayoutGlobalChrome emitted a second one after the page
// content. The page copy was removed; sortable-init.js now defers its initial
// call to DOMContentLoaded so it no longer depends on a Sortable tag preceding
// it in document order.
//
// FIXTURE: none needs seeding. The provider-priority rows
// ([data-sortable-field]) are server-rendered from the default priorities the
// migrations seed, so they exist on the harness's empty database. The test
// asserts that precondition (count > 0) before trusting the init check, so a
// page that rendered no rows fails loudly instead of passing vacuously.
//
// WITHOUT THE FIX: "requests Sortable.min.js exactly once" fails (2 requests,
// 2 script elements). If the page copy is removed but the deferred init is
// reverted to a parse-time call, the instance check still PASSES (an
// htmx:afterSwap at load re-initialises the lists), so the run fails only on the
// final console-error assertion ("sortable-init: SortableJS not loaded; ...").

import { test, expect } from 'playwright/test';

const SORTABLE_RE = /sortable\.min.*\.js/i;

test('settings requests Sortable.min.js exactly once and initialises the priority lists', async ({ page }) => {
  const consoleErrors = [];
  page.on('console', (msg) => {
    if (msg.type() === 'error') consoleErrors.push(msg.text());
  });
  page.on('pageerror', (err) => consoleErrors.push(`pageerror: ${err.message}`));

  const sortableRequests = [];
  page.on('request', (req) => {
    if (SORTABLE_RE.test(new URL(req.url()).pathname)) sortableRequests.push(req.url());
  });

  await page.goto('/settings', { waitUntil: 'load' });

  // Precondition: the lists this check is about actually rendered.
  const containers = page.locator('[data-sortable-field]');
  expect(await containers.count(), 'settings rendered no [data-sortable-field] lists').toBeGreaterThan(0);

  // One script element and one network request for the library.
  const scriptSrcs = await page.evaluate(() =>
    Array.from(document.querySelectorAll('script[src]'), (s) => s.getAttribute('src')));
  const sortableScripts = scriptSrcs.filter((src) => SORTABLE_RE.test(src));
  expect(sortableScripts, `Sortable script tags: ${sortableScripts.join(', ')}`).toHaveLength(1);
  expect(sortableRequests, `Sortable requests: ${sortableRequests.join(', ')}`).toHaveLength(1);

  // Observable behaviour: SortableJS is defined and every list got an instance.
  expect(await page.evaluate(() => typeof window.Sortable)).toBe('function');
  const uninitialised = await page.evaluate(() =>
    Array.from(document.querySelectorAll('[data-sortable-field]'))
      .filter((el) => !el._sortable)
      .map((el) => el.dataset.sortableField));
  expect(uninitialised, `lists without a Sortable instance: ${uninitialised.join(', ')}`).toEqual([]);

  expect(consoleErrors, `console errors:\n${consoleErrors.join('\n')}`).toEqual([]);
});
