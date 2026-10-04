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

import { apiFetch } from './helpers/api.js';

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

// A rendered drag on a fresh load: the first provider chip of a priority list is
// dragged below the second with real pointer events, the PUT carries the new
// order (enabled, then disabled, then hidden, as sortable-init.js builds it),
// the order survives a reload, and the original order is restored afterwards
// because the harness shares one server and DB across spec files.
test('dragging a provider chip saves and persists the new order', async ({ page, request }) => {
  const consoleErrors = [];
  page.on('console', (msg) => {
    if (msg.type() === 'error') consoleErrors.push(msg.text());
  });
  page.on('pageerror', (err) => consoleErrors.push(`pageerror: ${err.message}`));

  await page.goto('/settings', { waitUntil: 'load' });

  // Precondition: a list with two or more draggable provider chips.
  const target = await page.evaluate(() => {
    for (const list of document.querySelectorAll('[data-sortable-field]')) {
      const chips = Array.from(list.querySelectorAll('[data-provider]'))
        .filter((c) => c.querySelector('.drag-handle'));
      if (chips.length < 2) continue;
      const row = list.closest('[id^="priority-row-"]');
      const attr = (sel, key) => Array.from(row.querySelectorAll(sel), (e) => e.dataset[key]);
      return {
        field: list.dataset.sortableField,
        enabled: chips.map((c) => c.dataset.provider),
        tail: [...attr('[data-disabled-provider]', 'disabledProvider'),
               ...attr('[data-hidden-provider]', 'hiddenProvider')],
      };
    }
    return null;
  });
  expect(target, 'no priority list with two draggable provider chips').not.toBeNull();
  const { field, enabled, tail } = target;
  const original = [...enabled, ...tail];
  const swapped = [enabled[1], enabled[0], ...enabled.slice(2)];
  const list = page.locator(`[data-sortable-field="${field}"]`);
  const handles = list.locator('[data-provider] .drag-handle');

  try {
    // Pointer coordinates are viewport-relative: bring the list on screen first.
    await handles.nth(0).scrollIntoViewIfNeeded();
    const from = await handles.nth(0).boundingBox();
    const to = await handles.nth(1).boundingBox();
    const sx = from.x + from.width / 2, sy = from.y + from.height / 2;
    const ex = to.x + to.width + 6, ey = to.y + to.height / 2;

    const putSeen = page.waitForResponse((r) =>
      r.url().endsWith('/api/v1/providers/priorities') && r.request().method() === 'PUT',
      { timeout: 10_000 });
    await page.mouse.move(sx, sy);
    await page.mouse.down();
    for (let i = 1; i <= 12; i++) {
      await page.mouse.move(sx + ((ex - sx) * i) / 12, sy + ((ey - sy) * i) / 12);
    }
    await page.mouse.up();
    const put = await putSeen;

    expect(put.ok(), `PUT status ${put.status()}`).toBe(true);
    const body = JSON.parse(put.request().postData());
    expect(body.priorities).toEqual([{ field, providers: [...swapped, ...tail] }]);

    await page.reload({ waitUntil: 'load' });
    const order = await page.locator(`[data-sortable-field="${field}"] [data-provider]`)
      .evaluateAll((els) => els.map((e) => e.dataset.provider));
    expect(order).toEqual(swapped);
  } finally {
    const restore = await apiFetch(request, 'PUT', '/api/v1/providers/priorities', {
      priorities: [{ field, providers: original }],
    });
    expect(restore.ok(), `restore PUT status ${restore.status()}`).toBe(true);
  }

  expect(consoleErrors, `console errors:\n${consoleErrors.join('\n')}`).toEqual([]);
});
