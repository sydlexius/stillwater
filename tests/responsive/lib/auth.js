// auth.js - thin adapter over the a11y tier's existing login helper so the
// responsive harness does not duplicate auth/CSRF logic (single source of
// truth: tests/a11y/helpers/bootstrap.js).
//
// bootstrap.js reads its target server from SW_TEST_URL/SW_PORT at MODULE
// LOAD time, so this uses a dynamic import: env vars must be set before the
// first call to authenticateOnce() in the process (run.js does this before
// calling in).
//
// IMPORTANT: /api/v1/auth/login sits behind the production login
// brute-force rate limiter (5 req/min/IP, shared across all auth endpoints
// -- see tests/a11y/global-setup.js). Logging in once per browser context
// (one per viewport x theme combo) blows through that budget in a couple of
// iterations and 429s the rest of the run. Authenticate EXACTLY ONCE per
// process via authenticateOnce(), capture the resulting storageState, and
// hand that state to every browser.newContext({ storageState }) call
// instead of logging in again.
export async function authenticateOnce({ baseURL, adminUser, adminPass } = {}) {
  if (baseURL) process.env.SW_TEST_URL = baseURL;
  if (adminUser) process.env.STILLWATER_ADMIN_USER = adminUser;
  if (adminPass) process.env.STILLWATER_ADMIN_PASSWORD = adminPass;

  const { setupAndLogin } = await import('../../a11y/helpers/bootstrap.js');
  const { request } = await import('playwright');

  const ctx = await request.newContext({ baseURL });
  try {
    await setupAndLogin(ctx);
    return ctx.storageState();
  } finally {
    await ctx.dispose();
  }
}
