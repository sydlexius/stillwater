// a11y-global-setup-guards.test.js - pins what the sibling
// a11y-storage-state-per-server.test.js cannot see (#3057): the two hardening
// guards INSIDE globalSetup, and the PER-INVOCATION half of the key. The
// sibling never EXECUTES globalSetup, so deleting either guard survived it, and
// it fixes the run id to isolate the server component.
//
// Its own FILE: helpers/bootstrap.js reads SW_TEST_URL at MODULE load and a
// cache-busting query on global-setup.js does not re-evaluate it, so only a
// fresh process (node --test gives each file one) can point it at a stub -- and
// the run-id case needs SW_A11Y_SEEN_RUN_ID genuinely unset.
//
// The stub implements just enough auth to reach each guard and answers POST
// /api/v1/libraries with 500, so the blast-radius seeder dies with an
// unmistakably DIFFERENT message -- that is what makes "the session guard was
// deleted" distinguishable from "it fired".

import test, { after } from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import path from 'node:path';
import fs from 'node:fs';
import os from 'node:os';

let loginMode = 'ok';
const srv = http.createServer((req, res) => {
  if (req.url.startsWith('/api/v1/health')) {
    res.setHeader('Set-Cookie', 'csrf_token=tok; Path=/');
  } else if (req.url.startsWith('/api/v1/auth/login')) {
    if (loginMode === 'fail') { res.statusCode = 401; res.end('nope'); return; }
    // 'nosession' sends an ALREADY-EXPIRED session cookie: bootstrap.js's
    // Set-Cookie regex still matches, so login "succeeds", but Playwright's
    // cookie jar drops it -- exactly the state the guard exists to catch.
    // (Verified against the real jar, not assumed.)
    res.setHeader('Set-Cookie', loginMode === 'nosession'
      ? 'session=expired; Path=/; Max-Age=0'
      : 'session=live; Path=/');
  } else if (req.url.startsWith('/api/v1/libraries') && req.method === 'POST') {
    res.statusCode = 500; res.end('stub: no seeding here'); return;
  }
  res.end('{}');
});
await new Promise((r) => srv.listen(0, '127.0.0.1', r));
process.env.SW_TEST_URL = `http://127.0.0.1:${srv.address().port}`;

// SW_A11Y_SEEN_RUN_ID is UNSET here, so this import is what mints it.
const {
  default: globalSetup, STORAGE_STATE, pruneStaleStateFiles, STATE_TTL_MS,
} = await import('../a11y/global-setup.js');
const MINTED_RUN_ID = process.env.SW_A11Y_SEEN_RUN_ID;

after(() => srv.close());

/** Invokes globalSetup and returns the error it threw (the stub always forces one). */
async function runExpectingThrow(mode) {
  loginMode = mode;
  try {
    await globalSetup();
  } catch (err) {
    return err;
  }
  return assert.fail('globalSetup resolved; the stub cannot seed, so it must throw');
}

/** Re-imports global-setup.js under a given run id, returning STORAGE_STATE. */
async function stateForRunId(runId, bust) {
  const prev = process.env.SW_A11Y_SEEN_RUN_ID;
  process.env.SW_A11Y_SEEN_RUN_ID = runId;
  try {
    return (await import(`../a11y/global-setup.js?bust=${bust}`)).STORAGE_STATE;
  } finally {
    process.env.SW_A11Y_SEEN_RUN_ID = prev;
  }
}

test('globalSetup REPLACES a pre-existing state file rather than inheriting it', async () => {
  fs.mkdirSync(path.dirname(STORAGE_STATE), { recursive: true });
  fs.writeFileSync(STORAGE_STATE, '{"cookies":[{"name":"session","value":"STALE"}]}');
  // Precondition, or the assertion below passes vacuously against a path that
  // never existed.
  assert.ok(fs.existsSync(STORAGE_STATE), 'stale fixture file was not created');

  // Login FAILS, so globalSetup throws BEFORE ctx.storageState() writes. Only
  // the up-front rmSync can have removed the stale file; an overwrite cannot,
  // because no write happens. Drop the rmSync and the stale session survives
  // into a run that believes it authenticated.
  const err = await runExpectingThrow('fail');
  assert.match(String(err.message), /login failed/);
  assert.equal(
    fs.existsSync(STORAGE_STATE), false,
    "a previous run's state file survived globalSetup, so this run would proceed "
    + 'on a session no live server ever minted',
  );
});

test('globalSetup REJECTS a state file with no usable session cookie', async () => {
  const err = await runExpectingThrow('nosession');
  // The MESSAGE matters, not merely that it threw: without the guard the run
  // continues into the seeder and dies on the stub's 500 instead, tens of
  // failures away from anything naming auth.
  assert.match(
    String(err.message), /no session cookie was serialized/,
    `expected the auth guard to fire, got: ${err.message}`,
  );
});

test('the run id is stamped at MODULE load, before the path is computed', () => {
  // The ORDERING is the claim: STORAGE_STATE is a module-level const that
  // playwright.config.js imports, so an id minted later (inside globalSetup, as
  // it was before this fix) is too late to key the path.
  assert.ok(MINTED_RUN_ID, 'importing global-setup.js left SW_A11Y_SEEN_RUN_ID unset');
});

test('an INHERITED run id is reused, and two runs on one server differ', async () => {
  // Workers are forked after the coordinator stamped the env; re-minting there
  // would give each worker its own file and none of them the one globalSetup
  // wrote. So the same id must be stable...
  const a1 = await stateForRunId('run-a', 'r1');
  const a2 = await stateForRunId('run-a', 'r2');
  assert.equal(a2, a1, 'one run+server did not resolve to one path');

  // ...and a DIFFERENT invocation against the same server (SW_TEST_URL is fixed
  // for this whole file) must not. Drop the run id from the digest and these
  // are equal -- the recycled-port collision, verbatim.
  const b = await stateForRunId('run-b', 'r3');
  assert.ok(a1 && b, 'STORAGE_STATE did not resolve for both runs');
  assert.notEqual(
    b, a1,
    'two invocations against one server share a state file, so a later run '
    + "deletes an earlier run's session and its remaining tests 401",
  );
});

// --- pruneStaleStateFiles -------------------------------------------------
//
// The per-invocation key makes `.auth/` grow one file per run, so globalSetup
// sweeps it. The hazard is the OPPOSITE of the one the sweep solves: deleting a
// file a live run is still reading reinstates the 401 bug by a new route. So
// the survival cases are the load-bearing ones and get the most weight, and the
// rules are asserted directly rather than inferred from TTL arithmetic.

/** Creates a temp .auth-alike holding files at the given ages, in ms. */
function stateDirWith(files) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'sw-prune-'));
  for (const [name, ageMs] of Object.entries(files)) {
    const file = path.join(dir, name);
    fs.writeFileSync(file, '{}');
    const when = new Date(Date.now() - ageMs);
    fs.utimesSync(file, when, when);
  }
  return dir;
}

const DAY = 24 * 60 * 60 * 1000;

test('prune: a state file older than the TTL is removed', () => {
  const dir = stateDirWith({ 'state-old-aaaa.json': 3 * DAY });
  // Precondition, or "it is gone" would be true of a file never created.
  assert.ok(fs.existsSync(path.join(dir, 'state-old-aaaa.json')), 'fixture not created');

  pruneStaleStateFiles(dir, { keep: 'state-current-zzzz.json' });

  assert.equal(
    fs.existsSync(path.join(dir, 'state-old-aaaa.json')), false,
    'a state file well past the TTL was not swept, so .auth/ grows without bound',
  );
});

test('prune: a file NEWER than the TTL survives (a concurrent run is reading it)', () => {
  // THE case this whole guard exists for. A run in flight is minutes old; the
  // TTL is a day. Asserted explicitly, never inferred from the arithmetic.
  const dir = stateDirWith({ 'state-live-bbbb.json': 90 * 1000 });

  pruneStaleStateFiles(dir, { keep: 'state-current-zzzz.json' });

  assert.ok(
    fs.existsSync(path.join(dir, 'state-live-bbbb.json')),
    "the sweep deleted a live run's session file, so that run's remaining tests 401 "
    + '-- the original bug, by a new route',
  );
  assert.ok(STATE_TTL_MS > 60 * 60 * 1000, 'the TTL is too short to outlast a run');
});

test("prune: the CURRENT run's own file survives an ancient mtime", () => {
  // Spared by NAME, not by age. A clock skew, a restored mtime or a machine
  // resuming from sleep must not let the sweep eat the file THIS suite is about
  // to read. Age is set well past the TTL precisely so only the name can save it.
  const dir = stateDirWith({ 'state-current-zzzz.json': 30 * DAY });

  pruneStaleStateFiles(dir, { keep: 'state-current-zzzz.json' });

  assert.ok(
    fs.existsSync(path.join(dir, 'state-current-zzzz.json')),
    "the sweep deleted the running suite's OWN state file; every context that "
    + 'loads it afterwards authenticates as nobody',
  );
});

test('prune: only state-named files inside .auth are touched', () => {
  // A positive allow-list, never a negated safe-list. Every file here is far
  // past the TTL, so age alone would condemn all of them; only the naming rule
  // spares the strays.
  const dir = stateDirWith({
    'state-old-cccc.json': 5 * DAY,
    'README.md': 5 * DAY,
    'notes.json': 5 * DAY,
    'state-old-cccc.json.bak': 5 * DAY,
  });

  pruneStaleStateFiles(dir, { keep: 'state-current-zzzz.json' });

  assert.equal(fs.existsSync(path.join(dir, 'state-old-cccc.json')), false,
    'the matching stale file should have been swept');
  for (const stray of ['README.md', 'notes.json', 'state-old-cccc.json.bak']) {
    assert.ok(
      fs.existsSync(path.join(dir, stray)),
      `the sweep deleted ${stray}, which it does not own`,
    );
  }
});

test('prune: FAIL-SAFE on a missing directory and an unstattable entry', () => {
  // Housekeeping must never fail a run. Two real failure modes, both exercised
  // rather than asserted from the shape of the try/catch.
  assert.doesNotThrow(
    () => pruneStaleStateFiles(path.join(os.tmpdir(), 'sw-prune-does-not-exist')),
    'a missing .auth directory threw, failing a run over housekeeping',
  );

  // A broken symlink: readdir lists it, statSync throws ENOENT on it. This is
  // also the benign race -- another process removing a file between the two.
  const dir = stateDirWith({ 'state-real-dddd.json': 5 * DAY });
  fs.symlinkSync(path.join(dir, 'gone'), path.join(dir, 'state-broken-eeee.json'));
  assert.doesNotThrow(
    () => pruneStaleStateFiles(dir, { keep: 'state-current-zzzz.json' }),
    'an unstattable entry threw instead of being skipped',
  );
  // ...and it kept going: the real stale file after it was still swept.
  assert.equal(
    fs.existsSync(path.join(dir, 'state-real-dddd.json')), false,
    'the sweep aborted on the bad entry instead of continuing',
  );
});
