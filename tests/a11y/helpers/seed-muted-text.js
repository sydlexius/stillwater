// seed-muted-text.js - throwaway servers for muted-text-contrast.spec.js (#3474).
//
// The shared a11y server is bootstrapped with onboarding already complete, so
// /setup/wizard redirects away from it, and it has multi_user off, so /register
// is a 404. Flipping either on the shared server would change what every later
// spec sees. This spec therefore boots its OWN servers (the base-path-server
// pattern): nothing it seeds is visible to, or outlives, any other spec.
//
//   startSettingsFixture()    admin + onboarding complete + multi-user on + one
//                             invite code, for /settings and /register.
//   addConnection()/removeConnection()
//                             a platform connection (skip_test, no network). It is
//                             NOT part of the fixture's base state: a media-server
//                             connection engages the write-back conflict gate,
//                             which greys out and disables the rule cards
//                             (opacity .55, pointer-events none) and would corrupt
//                             every other settings measurement. The one test that
//                             needs it adds it and removes it in a finally.
//   startOnboardingFixture()  admin created and logged in, onboarding NOT marked
//                             complete, so /setup/wizard serves the wizard.

import { startBasePathServer } from './base-path-server.js';

const ADMIN_USER = 'ci-a11y-muted-admin';
const ADMIN_PASS = 'ci-a11y-muted-ephemeral-pw';

function headers(csrfToken, session) {
  const cookie = session ? `csrf_token=${csrfToken}; session=${session}` : `csrf_token=${csrfToken}`;
  return { 'Content-Type': 'application/json', 'X-CSRF-Token': csrfToken, Cookie: cookie };
}

async function call(server, method, path, body, session) {
  const resp = await fetch(`${server.baseURL}${path}`, {
    method,
    headers: headers(server.csrfToken, session ?? server.sessionCookie),
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (!resp.ok) throw new Error(`seed-muted-text: ${method} ${path} -> ${resp.status} ${await resp.text()}`);
  return resp;
}

export async function startSettingsFixture() {
  const server = await startBasePathServer('', { env: { SW_UX: 'next' } });
  try {
    await call(server, 'PUT', '/api/v1/settings', { 'multi_user.enabled': 'true' });
    const invite = await (await call(server, 'POST', '/api/v1/users/invites', { role: 'operator', expires_in: '1h' })).json();
    return { server, inviteCode: invite.code };
  } catch (err) {
    server.stop();
    throw err;
  }
}

// addConnection stores an Emby connection without contacting it (skip_test) and
// returns its id.
export async function addConnection(server) {
  const resp = await call(server, 'POST', '/api/v1/connections', {
    name: 'Muted text fixture', type: 'emby', url: 'http://127.0.0.1:9', api_key: 'fixture', enabled: true, skip_test: true,
  });
  return (await resp.json()).id;
}

export async function removeConnection(server, id) {
  await call(server, 'DELETE', `/api/v1/connections/${id}`);
}

export async function startOnboardingFixture() {
  const server = await startBasePathServer('', { skipBootstrap: true, env: { SW_UX: 'next' } });
  try {
    const health = await fetch(`${server.baseURL}/api/v1/health`);
    if (!health.ok) throw new Error(`seed-muted-text: GET /api/v1/health -> ${health.status}`);
    const csrfToken = (health.headers.get('set-cookie') || '').match(/csrf_token=([^;]+)/)?.[1] || '';
    if (!csrfToken) throw new Error('seed-muted-text: health response carried no csrf_token cookie');
    server.csrfToken = csrfToken;
    await call(server, 'POST', '/api/v1/auth/setup', { username: ADMIN_USER, password: ADMIN_PASS }, null);
    const login = await call(server, 'POST', '/api/v1/auth/login', { username: ADMIN_USER, password: ADMIN_PASS }, null);
    const session = (login.headers.get('set-cookie') || '').match(/session=([^;]+)/)?.[1];
    if (!session) throw new Error('seed-muted-text: login response carried no session cookie');
    server.sessionCookie = session;
    return { server };
  } catch (err) {
    server.stop();
    throw err;
  }
}
