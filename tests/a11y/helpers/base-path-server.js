// base-path-server.js - spawns a SECOND, throwaway Stillwater server with a
// non-empty SW_BASE_PATH, for the one spec that needs it (#3094).
//
// WHY A SECOND SERVER, NOT THE SHARED ONE
//
// The shared a11y server (global-setup.js) boots with an empty base path --
// every other spec's URLs, storageState, and known-violation baselines assume
// root-relative routing. Retrofitting SW_BASE_PATH onto that server would
// move every other spec's URL, which is a much bigger blast radius than one
// issue warrants. A dedicated ephemeral server, built from the same binary
// `make test-a11y` already produces, is cheaper and cannot perturb any other
// spec.
//
// THE BINARY IS NOT BUILT HERE. It resolves ../../../stillwater (repo root),
// the exact path `make build` / `make test-a11y` / the CI a11y-test job all
// produce before Playwright runs. If it is missing, this throws a clear error
// naming the missing path rather than silently trying to build it itself --
// building it here would race the harness's own build step and could mask a
// real build failure as a spawn failure.
//
// EVERYTHING IS ISOLATED: its own random port, own empty SQLite file, own
// empty music dir, own admin account. Nothing it does is visible to the
// shared a11y server or to any other spec.

import { spawn } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import net from 'node:net';
import os from 'node:os';
import { fileURLToPath } from 'node:url';

const dirname = path.dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = path.resolve(dirname, '..', '..', '..');
const BINARY = path.join(REPO_ROOT, 'stillwater');

const ADMIN_USER = 'ci-a11y-basepath-admin';
const ADMIN_PASS = 'ci-a11y-basepath-ephemeral-pw';

/** freePort resolves a currently-unused TCP port by binding to :0 and releasing it. */
function freePort() {
  return new Promise((resolve, reject) => {
    const srv = net.createServer();
    srv.listen(0, '127.0.0.1', () => {
      const { port } = srv.address();
      srv.close(() => resolve(port));
    });
    srv.on('error', reject);
  });
}

/** waitForHealth polls /<basePath>/api/v1/health until it reports ok or times out. */
async function waitForHealth(baseURL, deadlineMs) {
  const deadline = Date.now() + deadlineMs;
  while (Date.now() < deadline) {
    try {
      const resp = await fetch(`${baseURL}/api/v1/health`);
      if (resp.ok) {
        const body = await resp.json();
        if (body.status === 'ok') return;
      }
    } catch {
      // Connection refused while the process is still starting; keep polling.
    }
    await new Promise((r) => setTimeout(r, 200));
  }
  throw new Error(`base-path-server: /api/v1/health did not report ok within ${deadlineMs}ms`);
}

/**
 * startBasePathServer boots an isolated Stillwater instance under basePath
 * (e.g. "/sw-basepath-test") and returns { baseURL, csrfToken, sessionCookie, stop }.
 *
 * baseURL already includes the base path prefix (e.g.
 * "http://127.0.0.1:54321/sw-basepath-test"), so callers can navigate/fetch
 * against `${baseURL}/reports` etc. without re-deriving the prefix.
 */
export async function startBasePathServer(basePath) {
  if (!fs.existsSync(BINARY)) {
    throw new Error(
      `base-path-server: binary not found at ${BINARY}. Run \`make build\` (or the harness's `
      + 'equivalent build step) before this spec runs.',
    );
  }

  const port = await freePort();
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'sw-basepath-'));

  // stop/cleanup is defined BEFORE anything that can fail, and every
  // subsequent throw path below runs through the single try/catch that
  // calls it. Six separate `child.kill()` call sites (one per bootstrap
  // step) each forgot the matching `fs.rmSync(tmpDir, ...)`, so a bootstrap
  // failure (e.g. the admin-setup POST 500s) leaked a `sw-basepath-*` temp
  // dir with a SQLite file in os.tmpdir() once per failed run. Centralizing
  // the cleanup here means a future SEVENTH bootstrap step that throws
  // cannot reintroduce the leak by omission -- there is only one place left
  // to call, and the catch below calls it unconditionally.
  let child = null;
  let exited = false;
  const cleanup = () => {
    if (child && !exited) child.kill();
    fs.rmSync(tmpDir, { recursive: true, force: true });
  };

  try {
    const dbPath = path.join(tmpDir, 'sw-basepath.db');
    const musicDir = path.join(tmpDir, 'empty-library');
    fs.mkdirSync(musicDir, { recursive: true });
    const logPath = path.join(tmpDir, 'server.log');
    const logFd = fs.openSync(logPath, 'w');

    child = spawn(BINARY, [], {
      cwd: REPO_ROOT,
      env: {
        ...process.env,
        SW_DB_PATH: dbPath,
        SW_PORT: String(port),
        SW_BASE_PATH: basePath,
        SW_LOG_FORMAT: 'text',
        SW_LOG_LEVEL: 'warn',
        SW_BACKUP_ENABLED: 'false',
        SW_MUSIC_PATH: musicDir,
      },
      stdio: ['ignore', logFd, logFd],
    });
    child.on('exit', () => { exited = true; });

    const rootURL = `http://127.0.0.1:${port}`;
    const baseURL = `${rootURL}${basePath}`;

    try {
      await waitForHealth(baseURL, 20_000);
    } catch (err) {
      const log = fs.existsSync(logPath) ? fs.readFileSync(logPath, 'utf8') : '(no log)';
      throw new Error(`${err.message}\nexited=${exited}\nserver log:\n${log}`);
    }

    // Bootstrap: CSRF cookie from health, admin account, session cookie,
    // mark onboarding complete -- the same four steps as bootstrap.js, but
    // scoped under basePath since every route on this server lives there.
    const healthResp = await fetch(`${baseURL}/api/v1/health`);
    const setCookie = healthResp.headers.get('set-cookie') || '';
    const csrfMatch = setCookie.match(/csrf_token=([^;]+)/);
    const csrfToken = csrfMatch ? csrfMatch[1] : '';
    if (!csrfToken) {
      throw new Error('base-path-server: health response carried no csrf_token cookie');
    }

    const setupResp = await fetch(`${baseURL}/api/v1/auth/setup`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'X-CSRF-Token': csrfToken,
        Cookie: `csrf_token=${csrfToken}`,
      },
      body: JSON.stringify({ username: ADMIN_USER, password: ADMIN_PASS }),
    });
    if (!setupResp.ok && setupResp.status !== 409) {
      throw new Error(`base-path-server: admin setup failed: ${setupResp.status}`);
    }

    const loginResp = await fetch(`${baseURL}/api/v1/auth/login`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'X-CSRF-Token': csrfToken,
        Cookie: `csrf_token=${csrfToken}`,
      },
      body: JSON.stringify({ username: ADMIN_USER, password: ADMIN_PASS }),
    });
    if (!loginResp.ok) {
      throw new Error(`base-path-server: login failed: ${loginResp.status}`);
    }
    const loginSetCookie = loginResp.headers.get('set-cookie') || '';
    const sessionMatch = loginSetCookie.match(/session=([^;]+)/);
    if (!sessionMatch) {
      throw new Error('base-path-server: login response carried no session cookie');
    }
    const sessionCookie = sessionMatch[1];

    const onboardResp = await fetch(`${baseURL}/api/v1/settings`, {
      method: 'PUT',
      headers: {
        'Content-Type': 'application/json',
        'X-CSRF-Token': csrfToken,
        Cookie: `session=${sessionCookie}`,
      },
      body: JSON.stringify({ 'onboarding.completed': 'true' }),
    });
    if (!onboardResp.ok) {
      throw new Error(`base-path-server: marking onboarding complete failed: ${onboardResp.status}`);
    }

    const stop = () => {
      if (!exited) child.kill();
      fs.rmSync(tmpDir, { recursive: true, force: true });
    };

    return {
      baseURL, rootURL, csrfToken, sessionCookie, stop,
    };
  } catch (err) {
    cleanup();
    throw err;
  }
}
