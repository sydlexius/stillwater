// bootstrap.js - Auth helper for Playwright a11y smoke tests.
//
// Follows the bruno-ci pattern: setup account -> login -> return session cookie.
// The server URL is read from SW_TEST_URL or derived from SW_PORT (set by the
// make test-a11y / CI job before Playwright runs).

const BASE_URL = process.env.SW_TEST_URL
  || `http://127.0.0.1:${process.env.SW_PORT || '1973'}`;

const ADMIN_USER = process.env.STILLWATER_ADMIN_USER || 'ci-a11y-admin';
const ADMIN_PASS = process.env.STILLWATER_ADMIN_PASSWORD || 'ci-a11y-ephemeral-pw';

/**
 * setupAndLogin creates the admin account (if first boot) and logs in.
 *
 * Returns { cookie, csrfToken } ready for use in Playwright storageState or
 * request headers.
 *
 * @param {import('playwright').APIRequestContext} request  Playwright API context.
 * @returns {Promise<{cookie: string, csrfToken: string}>}
 */
export async function setupAndLogin(request) {
  // Step 1: hit /api/v1/health to get the CSRF cookie.
  const health = await request.get(`${BASE_URL}/api/v1/health`);
  if (!health.ok()) {
    throw new Error(`health check failed: ${health.status()}`);
  }

  const setCookieHeader = health.headers()['set-cookie'] || '';
  const csrfMatch = setCookieHeader.match(/csrf_token=([^;]+)/);
  const csrfToken = csrfMatch ? csrfMatch[1] : '';

  // Step 2: register admin account (idempotent -- 409 on second call is fine).
  const setup = await request.post(`${BASE_URL}/api/v1/auth/setup`, {
    headers: {
      'Content-Type': 'application/json',
      'X-CSRF-Token': csrfToken,
      'Cookie': `csrf_token=${csrfToken}`,
    },
    data: JSON.stringify({ username: ADMIN_USER, password: ADMIN_PASS }),
  });
  if (!setup.ok() && setup.status() !== 409) {
    throw new Error(`setup failed: ${setup.status()}`);
  }

  // Step 3: login to get a session cookie.
  const login = await request.post(`${BASE_URL}/api/v1/auth/login`, {
    headers: {
      'Content-Type': 'application/json',
      'X-CSRF-Token': csrfToken,
      'Cookie': `csrf_token=${csrfToken}`,
    },
    data: JSON.stringify({ username: ADMIN_USER, password: ADMIN_PASS }),
  });
  if (!login.ok()) {
    const body = await login.text();
    throw new Error(`login failed: ${login.status()} ${body}`);
  }

  const loginSetCookie = login.headers()['set-cookie'] || '';
  const sessionMatch = loginSetCookie.match(/session=([^;]+)/);
  if (!sessionMatch) {
    throw new Error('login response did not set a session cookie');
  }
  const sessionCookie = `session=${sessionMatch[1]}`;

  // Step 4: Mark onboarding as complete so /next/ dashboard does not redirect to
  // the setup wizard on a fresh ephemeral DB. PUT /api/v1/settings is gated by
  // RequireAdmin (satisfied -- Setup always creates role=administrator) and CSRF
  // (satisfied -- the token from the health GET above is valid for 4h and
  // /api/v1/auth/setup + /api/v1/auth/login are CSRF-exempt so the token is
  // still the one issued at step 1).
  const onboardingMark = await request.put(`${BASE_URL}/api/v1/settings`, {
    headers: {
      'Content-Type': 'application/json',
      'X-CSRF-Token': csrfToken,
      'Cookie': sessionCookie,
    },
    data: JSON.stringify({ 'onboarding.completed': 'true' }),
  });
  if (!onboardingMark.ok()) {
    throw new Error(`failed to mark onboarding complete: ${onboardingMark.status()}`);
  }

  return { cookie: sessionCookie, csrfToken };
}
