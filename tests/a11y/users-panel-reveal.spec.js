// users-panel-reveal.spec.js - Playwright functional smoke covering the
// Settings > Users panel's data-population contract.
//
// History: #2132 fixed the v1 tabbed Settings page, where the panel populated
// only if Users happened to be the tab active at initial render (hx-trigger=
// "load" never refired on a client-side tab switch, so switching to Users
// left it on a permanent loading skeleton). #1757 PR-5 then promoted Settings
// to a single scrollable pane and, per buildSettingsData's doc comment
// (internal/api/handlers_platform.go), changed loadUsers to always be true:
// "the promoted page passes true because the Users section is always present
// on its single-scroll page". That means the Users table and pending-invites
// list are now server-rendered with real content on every /settings GET --
// confirmed directly: the initial HTML response already contains a
// `tr[id^="user-row-"]` row, with no client-side interaction involved. The
// v1 tab-click reveal this spec used to assert against no longer exists (the
// data-tab-panel hidden-class model was deleted by the promotion commit), and
// there is no separate "first population" moment left to test for a reveal --
// the promoted architecture removed the lazy-load gap #2132 was guarding
// against by rendering everything eagerly instead.
//
// This spec asserts the CURRENT, honest equivalent of the original intent:
// the Users section carries real, populated user/invite content from first
// paint (never left showing the loading-placeholder copy), regardless of
// which section of the page happens to be scrolled into view.
//
// Auth: reuses the single login from global-setup.js (session + csrf_token
// cookies loaded via storageState in playwright.config.js).

import { test, expect, request } from 'playwright/test';

const BASE_URL = process.env.SW_TEST_URL
  || `http://127.0.0.1:${process.env.SW_PORT || '1973'}`;

import { STORAGE_STATE } from './global-setup.js';

// The Users panel only renders its table/invites content when multi-user mode
// is on (see settings_users.templ's `if data.MultiUserEnabled` gate); it
// defaults off. Flip it once for this spec via the same authenticated session
// captured in global-setup, mirroring the onboarding-completion PUT in
// helpers/bootstrap.js. This is idempotent and additive, so it does not
// affect the other a11y specs sharing the ephemeral server.
test.beforeAll(async () => {
  const ctx = await request.newContext({ baseURL: BASE_URL, storageState: STORAGE_STATE });
  try {
    const cookies = (await ctx.storageState()).cookies;
    const csrfToken = cookies.find(c => c.name === 'csrf_token')?.value;
    if (!csrfToken) {
      throw new Error('no csrf_token cookie in storageState -- global-setup.js must run first');
    }
    const resp = await ctx.put('/api/v1/settings', {
      headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrfToken },
      data: JSON.stringify({ 'multi_user.enabled': 'true' }),
    });
    if (!resp.ok()) {
      throw new Error(`failed to enable multi_user.enabled: ${resp.status()} ${await resp.text()}`);
    }
  } finally {
    await ctx.dispose();
  }
});

test('Users section renders real content on first paint, not a loading skeleton', async ({ page }) => {
  await page.goto('/settings');
  await page.waitForLoadState('load');

  // The Users section is present in the single-pane DOM from first paint
  // (per #1757 PR-5's promotion, never behind a hidden tab panel).
  const usersSection = page.locator('#section-users');
  await expect(usersSection).toBeAttached();

  const usersTableBody = page.locator('#users-table-body');

  // At least the admin account itself must render as a real row -- proving
  // buildSettingsData's loadUsers=true-always contract, not the loading
  // placeholder row settings_users.templ emits when data.Users is empty.
  const userRows = usersTableBody.locator('tr[id^="user-row-"]');
  expect(await userRows.count()).toBeGreaterThanOrEqual(1);

  const bodyText = await usersTableBody.innerText();
  expect(bodyText).not.toContain('Loading');

  // The pending-invites list is NOT asserted against its "Loading..." string:
  // settings_users.templ reuses that same copy as the zero-invites empty
  // state (`if len(data.Invites) == 0`), so it legitimately still reads
  // "Loading invites..." when there are simply no pending invites -- which is
  // the case for a fresh admin-only account. That is an existing UX rough
  // edge (the empty state and the loading state share one string), not a
  // population-timing bug this spec is scoped to catch.
});
