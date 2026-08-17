// a11y-storage-state-per-server.test.js - regression test for #3057.
//
// THE DEFECT
//
// tests/a11y/global-setup.js exported STORAGE_STATE as a FIXED path
// (`.auth/state.json`) while `make test-a11y` boots an ephemeral server on a
// RANDOM FREE PORT with a fresh database each invocation. A session cookie is
// only valid against the server that minted it (auth.ValidateSession reads
// that server's own `sessions` table), so two concurrent runs from one
// checkout - the second overwriting the file the first is still using -
// handed the first run a foreign session. Playwright re-reads storageState
// from DISK at every context creation, so every subsequent test in the first
// run authenticated as nobody: authenticated writes 401'd (POST
// /api/v1/libraries, PUT /api/v1/settings) while unauthenticated page GETs
// still rendered, which is why the symptom looked like several unrelated
// specs failing late rather than like an auth failure.
//
// The fix keys the path on the base URL. Two servers cannot share a port, so
// two runs cannot collide, and every process derives the same path from the
// same env with no handshake.
//
// WHAT THIS TEST PINS
//
// That two DIFFERENT server URLs produce two DIFFERENT storage-state paths,
// and that the same URL is stable across imports (the config, globalSetup and
// each worker process must all agree). Reverting to a fixed path fails the
// first assertion.
//
// The module is imported with a cache-busting query so each case observes a
// fresh module-level evaluation of SW_PORT, matching what separate Playwright
// processes do.

import test from 'node:test';
import assert from 'node:assert/strict';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const GLOBAL_SETUP = path.join(
  path.dirname(fileURLToPath(import.meta.url)),
  '..', '..', 'tests', 'a11y', 'global-setup.js',
);

// Import global-setup.js with a given SW_PORT / SW_TEST_URL, returning its
// STORAGE_STATE. The query string defeats the ESM module cache so the
// module-level path computation runs again under the new environment.
async function storageStateFor({ port, url }, bust) {
  const prevPort = process.env.SW_PORT;
  const prevURL = process.env.SW_TEST_URL;
  if (port === undefined) delete process.env.SW_PORT; else process.env.SW_PORT = port;
  if (url === undefined) delete process.env.SW_TEST_URL; else process.env.SW_TEST_URL = url;
  try {
    const mod = await import(`${GLOBAL_SETUP}?bust=${bust}`);
    return mod.STORAGE_STATE;
  } finally {
    if (prevPort === undefined) delete process.env.SW_PORT; else process.env.SW_PORT = prevPort;
    if (prevURL === undefined) delete process.env.SW_TEST_URL; else process.env.SW_TEST_URL = prevURL;
  }
}

test('two ephemeral servers get two different storage-state files', async () => {
  const a = await storageStateFor({ port: '40001' }, 'a');
  const b = await storageStateFor({ port: '40002' }, 'b');

  // Precondition: both really resolved to a path (an undefined export would
  // make the inequality below pass vacuously).
  assert.ok(a && b, 'STORAGE_STATE did not resolve for both ports');

  assert.notEqual(
    a, b,
    'both ports share one storage-state file, so a second concurrent a11y run '
    + "overwrites the first run's session and every later authenticated request 401s",
  );
});

test('the same server resolves to the same storage-state file across imports', async () => {
  const first = await storageStateFor({ port: '40003' }, 'c');
  const second = await storageStateFor({ port: '40003' }, 'd');

  assert.equal(
    first, second,
    'globalSetup and the worker processes must derive the same path for one server, '
    + 'or the workers load a state file nothing ever wrote',
  );
});

test('SW_TEST_URL and SW_PORT both key the path', async () => {
  const viaURL = await storageStateFor({ url: 'http://127.0.0.1:40004' }, 'e');
  const viaPort = await storageStateFor({ port: '40005' }, 'f');

  assert.notEqual(viaURL, viaPort, 'SW_TEST_URL must key the path the same way SW_PORT does');

  // The path must stay inside the gitignored .auth dir and remain a plain
  // filename: a URL is interpolated into it, so any surviving separator or
  // colon would escape the directory or break on a case-insensitive FS.
  const base = path.basename(viaURL);
  assert.match(base, /^state-[A-Za-z0-9-]+\.json$/, `unsafe storage-state filename: ${base}`);
  assert.equal(path.basename(path.dirname(viaURL)), '.auth');
});
