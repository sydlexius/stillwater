// users-panel-reveal.spec.js - Playwright functional smoke for #2132: the
// stable Settings > Users panel must populate on a client-side tab switch,
// not only on a full page load.
//
// Context: the Users panel's #users-table-body and #invites-list fetch via
// hx-trigger="intersect once" (previously "load", which only ever fired for
// whichever tab happened to be active at initial page render -- switching
// tabs client-side never re-triggered a "load" event, so the panel stayed on
// its permanent loading skeleton). This spec proves the fix behaviorally: it
// loads /settings on a non-Users tab, clicks the Users tab control, and
// asserts (a) the GET requests actually fire on reveal and (b) the DOM swaps
// from the loading skeleton to real content.
//
// Runs against the STABLE UI. The shared a11y harness boots the server with
// SW_UX=next (see playwright.config.js / Makefile test-a11y), under which
// ResolveUX defaults bare paths like /settings to the next/ channel. A
// sw_ux=stable cookie forces the stable resolution for this spec only,
// per internal/api/middleware/ux.go's ResolveUX (mode="next", cookie="stable"
// -> UXStable), without touching the shared server config.
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

test('Users tab populates on client-side reveal, not just page load', async ({ page, context }) => {
  // Force the stable UI channel for this page's requests (see module comment).
  await context.addCookies([
    { name: 'sw_ux', value: 'stable', url: BASE_URL },
  ]);

  await page.goto('/settings');
  await page.waitForLoadState('load');

  // Sanity: land on a non-Users tab (default is General), so revealing Users
  // is a genuine client-side tab switch, not the tab that was active at load.
  const usersPanel = page.locator('[data-tab-panel="users"]');
  await expect(usersPanel).toHaveClass(/hidden/);

  const usersTableBody = page.locator('#users-table-body');
  const invitesList = page.locator('#invites-list');
  const initialUsersHTML = await usersTableBody.innerHTML();
  const initialInvitesHTML = await invitesList.innerHTML();

  // Arm both response waits before the click so the reveal-triggered fetches
  // (not any load-time fetch) are what get captured.
  const usersFetch = page.waitForResponse(
    resp => resp.request().method() === 'GET' && /\/api\/v1\/users$/.test(new URL(resp.url()).pathname),
  );
  const invitesFetch = page.waitForResponse(
    resp => resp.request().method() === 'GET' && /\/api\/v1\/users\/invites$/.test(new URL(resp.url()).pathname),
  );

  await page.locator('a[data-tab="users"]').click();

  // The click must be a real client-side reveal, not a navigation.
  expect(new URL(page.url()).pathname).toBe('/settings');
  await expect(usersPanel).not.toHaveClass(/hidden/);

  const [usersResp, invitesResp] = await Promise.all([usersFetch, invitesFetch]);
  expect(usersResp.ok()).toBe(true);
  expect(invitesResp.ok()).toBe(true);

  // The panel must actually swap away from its loading skeleton, not just
  // fire the request.
  await expect(usersTableBody).not.toHaveText('');
  await expect
    .poll(() => usersTableBody.innerHTML(), { timeout: 5_000 })
    .not.toBe(initialUsersHTML);
  await expect
    .poll(() => invitesList.innerHTML(), { timeout: 5_000 })
    .not.toBe(initialInvitesHTML);

  // At least the admin account itself must render as a real row.
  const userRows = usersTableBody.locator('tr[id^="user-row-"]');
  expect(await userRows.count()).toBeGreaterThanOrEqual(1);
});
