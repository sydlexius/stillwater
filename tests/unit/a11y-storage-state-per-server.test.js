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
// The fix keys the path on a DIGEST of the canonical server origin plus the
// per-invocation run id, so distinct servers and distinct runs never share a
// file while every process in ONE run derives the same path from the same env
// with no handshake.
//
// WHAT THIS TEST PINS
//
// That two DIFFERENT server URLs produce two DIFFERENT storage-state paths --
// including URL pairs a character-substitution key COLLIDES on, which is what
// the first implementation used ("a.b" vs "a-b", trailing slash vs none) --
// and that the same URL under the same run id is stable across imports (the
// config, globalSetup and each worker process must all agree). Reverting to a
// fixed path, or back to the lossy substitution, fails these.
//
// The run-id component is pinned in a11y-global-setup-run-id.test.js, which
// needs its own process to observe an UNSET SW_A11Y_SEEN_RUN_ID; the guards
// inside globalSetup itself are pinned in a11y-global-setup-guards.test.js.
//
// The module is imported with a cache-busting query so each case observes a
// fresh module-level evaluation of SW_PORT, matching what separate Playwright
// processes do.

import test from 'node:test';
import assert from 'node:assert/strict';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

// Pin the run id for this file. global-setup.js mints one only when the var is
// unset (`||=`), so fixing it here holds the per-invocation component CONSTANT
// and isolates the SERVER component, which is what every case below varies.
process.env.SW_A11Y_SEEN_RUN_ID = 'unit-test-fixed-run-id';

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

test('SW_TEST_URL keys the path, with SW_PORT unset', async () => {
  // Two SW_TEST_URL values with NO SW_PORT set. The previous form of this test
  // compared a URL-only case against a port-only one, which passes even if
  // SW_TEST_URL is ignored entirely and the URL case falls back to the default
  // port -- the two would still differ. Varying only SW_TEST_URL cannot.
  const one = await storageStateFor({ url: 'http://127.0.0.1:40004' }, 'e');
  const two = await storageStateFor({ url: 'http://127.0.0.1:40005' }, 'f');

  assert.notEqual(one, two, 'SW_TEST_URL does not key the path at all');

  // The path must stay inside the gitignored .auth dir and remain a plain
  // filename: a URL is interpolated into it, so any surviving separator or
  // colon would escape the directory or break on a case-insensitive FS.
  const base = path.basename(one);
  assert.match(base, /^state-[A-Za-z0-9-]+\.json$/, `unsafe storage-state filename: ${base}`);
  assert.equal(path.basename(path.dirname(one)), '.auth');
});

test('SW_TEST_URL takes precedence over SW_PORT when both are set', async () => {
  // playwright.config.js, bootstrap.js and global-setup.js each resolve
  // `SW_TEST_URL || http://127.0.0.1:${SW_PORT}` independently. If the key
  // disagreed about which wins, the config and globalSetup would derive
  // different paths and the workers would load a file nothing wrote.
  const both = await storageStateFor({ url: 'http://127.0.0.1:40006', port: '40007' }, 'g');
  const urlOnly = await storageStateFor({ url: 'http://127.0.0.1:40006' }, 'h');
  const portOnly = await storageStateFor({ port: '40007' }, 'i');

  assert.equal(both, urlOnly, 'SW_TEST_URL must win over SW_PORT');
  assert.notEqual(both, portOnly, 'SW_PORT keyed the path while SW_TEST_URL was set');
});

test('distinct hosts a lossy substitution collides on stay distinct', async () => {
  // "a.b" and "a-b" are DIFFERENT valid hosts that map to ONE key under
  // `BASE_URL.replace(/[^a-zA-Z0-9]+/g, '-')`, the first implementation. A
  // collision means two servers share a state file and one run's rmSync
  // deletes the other's session -- the exact defect this PR fixes.
  const dotted = await storageStateFor({ url: 'http://a.b:40004' }, 'l');
  const hyphen = await storageStateFor({ url: 'http://a-b:40004' }, 'm');

  assert.ok(dotted && hyphen, 'STORAGE_STATE did not resolve for both hosts');
  assert.notEqual(dotted, hyphen, 'a.b and a-b collide on one storage-state file');
});

test('a trailing slash names the SAME server and so the SAME file', async () => {
  // The complement: the key digests the CANONICAL origin, so a cosmetic URL
  // difference denoting one server must not fork into two files -- globalSetup
  // writing one while the workers read the other is the same 401 by another
  // route.
  const bare = await storageStateFor({ url: 'http://127.0.0.1:8080' }, 'j');
  const slash = await storageStateFor({ url: 'http://127.0.0.1:8080/' }, 'k');

  assert.ok(bare && slash, 'STORAGE_STATE did not resolve for both URL forms');
  assert.equal(bare, slash, 'a trailing slash forked one server into two state files');
});
