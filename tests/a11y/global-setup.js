// global-setup.js - one-time auth for the a11y smoke tier.
//
// Authenticates ONCE for the whole Playwright run (setup + login via the
// bruno-ci bootstrap) and persists the session into a storageState file that
// every test context loads (see `use.storageState` in playwright.config.js).
//
// Why this exists: /api/v1/auth/setup and /api/v1/auth/login are behind the
// production login brute-force rate limiter (5 req/min/IP, burst 5, shared
// across all auth endpoints). The previous per-spec-file beforeAll login meant
// each spec file spent two of those tokens; with more than two spec files the
// rapid loopback bursts exceeded the burst and the suite failed with a 429
// "too many requests" before any test ran. Logging in exactly once keeps the
// whole run at two auth calls regardless of how many spec files exist, without
// touching the production rate limiter.

import { request } from 'playwright/test';
import { fileURLToPath } from 'node:url';
import { createHash } from 'node:crypto';
import path from 'node:path';
import fs from 'node:fs';

import { setupAndLogin } from './helpers/bootstrap.js';
import { seedBlastRadius } from './helpers/seed-blast-radius.js';
import { newRunId } from './helpers/known-violations.js';

const dirname = path.dirname(fileURLToPath(import.meta.url));

const BASE_URL = process.env.SW_TEST_URL
  || `http://127.0.0.1:${process.env.SW_PORT || '1973'}`;

// Per-invocation run id, read by the known-violation seen-marks
// (helpers/known-violations.js) AND by the storage-state path below.
//
// At MODULE level, not inside globalSetup: STORAGE_STATE is a module-level
// const that playwright.config.js imports, so the id must exist before it is
// computed. The coordinator loads this module before globalSetup runs and
// every worker is forked afterwards, inheriting the value through the
// environment, so all of them derive the same paths with no handshake. `||=`
// rather than a plain assign for the same reason: the config, globalSetup and
// a spec each import this module and must not re-mint.
process.env.SW_A11Y_SEEN_RUN_ID ||= newRunId();

// The storage-state path is keyed by the SERVER the session belongs to AND by
// this invocation, not fixed at `.auth/state.json` (#3057).
//
// A session cookie is only meaningful against the server that minted it:
// `sessions` is a table in that server's own SQLite file, and
// middleware.Auth -> auth.ValidateSession looks the token up there
// (internal/api/middleware/auth.go, internal/auth/auth.go:207). Present a
// session from a DIFFERENT ephemeral server and the lookup misses, so every
// authenticated write 401s -- while unauthenticated GETs still render, which
// is why the symptom reads as unrelated specs failing rather than as an auth
// problem.
//
// `make test-a11y` picks a RANDOM FREE PORT and a fresh database per
// invocation, so two runs FROM THE SAME CHECKOUT (a local one beside CI, or a
// re-run started before the first finished) are two different servers sharing
// one `.auth/` directory. With one fixed path the second run's globalSetup
// overwrites the file the first run is still using, and Playwright re-reads
// storageState FROM DISK at every context creation -- so the first run's
// remaining tests all pick up a foreign session and 401. Measured: a run
// started 12s behind another failed 44 of its tests from contrast.spec.js
// onward, in file order, exactly the "several unrelated specs, all late"
// shape.
//
// Sibling WORKTREES are NOT a trigger and never were: this path is anchored to
// path.dirname(fileURLToPath(import.meta.url)), so each checkout has its own
// tests/a11y/.auth (verified: two live worktrees' .auth dirs are distinct
// inodes). Only same-checkout runs share a directory.
//
// TWO components, because neither alone suffices:
//
//   - the SERVER, so a session is never presented to a server that did not
//     mint it. A SHA-256 of the canonical ORIGIN, not a character
//     substitution: `BASE_URL.replace(/[^a-zA-Z0-9]+/g, '-')` is lossy and
//     collides on inputs that reach here in practice ("http://a.b:x" vs
//     "http://a-b:x"). The readable prefix is only for eyeballing `ls .auth/`.
//
//   - the INVOCATION, because a port is unique at one INSTANT, not over time.
//     Ports get recycled: run A boots on 55000, run A's server dies while its
//     Playwright process is still mid-suite, run B's bind(0) draws 55000
//     again, and run B's rmSync deletes the file run A is still reading. Run
//     A's remaining contexts authenticate as nobody -- the original bug.
//
// So one invocation against one server always resolves to one path (which is
// what lets the config, globalSetup and every worker agree), and two distinct
// invocations never share a path whatever the ports did.
//
// Origin only: the URL parser normalizes away a trailing slash, a default port
// and any path/query, so a cosmetic difference cannot fork one server into two
// files. An unparsable SW_TEST_URL must still yield a path; the digest keeps
// it unique and login fails loudly against it anyway.
let canonicalBase = BASE_URL;
try { canonicalBase = new URL(BASE_URL).origin; } catch { /* keep the raw value */ }

const serverDigest = createHash('sha256')
  .update(`${canonicalBase}\n${process.env.SW_A11Y_SEEN_RUN_ID}`)
  .digest('hex')
  .slice(0, 16);

// Readable, and deliberately lossy (bounded, filename-safe). Two servers may
// share it; the digest is what keeps the full filename distinct.
const readable = canonicalBase
  .replace(/^https?:\/\//, '')
  .replace(/[^a-zA-Z0-9]+/g, '-')
  .replace(/^-|-$/g, '')
  .slice(0, 40);

// Shared with playwright.config.js (`use.storageState`). Kept under the a11y
// test dir and gitignored.
export const STORAGE_STATE = path.join(
  dirname, '.auth', `state-${readable}-${serverDigest}.json`,
);

// A per-invocation key means `.auth/` accumulates one file per run. Sweep
// files older than a day -- far longer than any run (the tier is minutes), so
// a CONCURRENT run's file is never a candidate, which is the one thing this
// must not delete.
const STATE_TTL_MS = 24 * 60 * 60 * 1000;

function pruneStaleStateFiles(dir, now = Date.now()) {
  let entries;
  try {
    entries = fs.readdirSync(dir);
  } catch {
    return; // No directory yet: nothing to prune.
  }
  for (const name of entries) {
    if (!/^state-.*\.json$/.test(name)) continue;
    const file = path.join(dir, name);
    try {
      if (now - fs.statSync(file).mtimeMs < STATE_TTL_MS) continue;
      fs.rmSync(file, { force: true });
    } catch {
      // A file another process removed between readdir and stat is the wanted
      // outcome; anything else is housekeeping, never worth failing a run over.
    }
  }
}

export default async function globalSetup() {
  fs.mkdirSync(path.dirname(STORAGE_STATE), { recursive: true });

  // Never inherit a file at this path. With the run id in the key nothing else
  // should have written here, so this is belt-and-braces against a globalSetup
  // that died after mkdir but before storageState(): that leaves a state file
  // naming a server that no longer exists, and the run proceeds on a dead
  // session instead of failing at login. Removing it first means the only way
  // a state file exists below is that THIS run wrote it.
  fs.rmSync(STORAGE_STATE, { force: true });

  pruneStaleStateFiles(path.dirname(STORAGE_STATE));

  const ctx = await request.newContext({ baseURL: BASE_URL });
  try {
    // setupAndLogin receives the Set-Cookie headers on the request context;
    // storageState() then serializes those cookies (session + csrf) for reuse.
    await setupAndLogin(ctx);
    const state = await ctx.storageState({ path: STORAGE_STATE });

    // Fail LOUDLY here rather than let every spec discover it. Without a
    // session cookie in the file, `use.storageState` loads an anonymous
    // context and each authenticated call 401s one spec at a time, tens of
    // failures deep, with nothing naming auth as the cause.
    if (!state.cookies.some((c) => c.name === 'session' && c.value)) {
      throw new Error(
        `global-setup: no session cookie was serialized into ${STORAGE_STATE}. `
        + 'Every spec would run unauthenticated. This is an auth/harness failure, '
        + 'not an a11y defect.',
      );
    }

    // Seed the blast-radius fixture. The a11y target runs against a fresh
    // empty database every time, so without this the pane renders its empty
    // state and blast-radius.spec.js cannot reach the restore dialog or the
    // pager -- it throws rather than passing vacuously, which is correct
    // behaviour and exactly why the data has to exist.
    //
    // Seeded here rather than in a beforeAll so it happens ONCE for the whole
    // run: the fixture is shared, idempotent-per-run state, and re-seeding per
    // spec file would multiply both the rows and the wall-clock cost.
    //
    // A failure here is fatal on purpose. A half-seeded fixture would surface
    // as unrelated a11y failures ("pager absent", "no rows") that read as
    // defects in the pane rather than as a broken fixture.
    const seeded = await seedBlastRadius(ctx);
    console.log(
      `[a11y] blast-radius fixture: ${seeded.rows} rows `
      + `(${seeded.automated} automated, ${seeded.unknown} unknown)`,
    );

    // The nfo-has-mbid fixture (#2809) is seeded by nfo-mbid.spec.js's own
    // beforeAll, not here -- the repo rule is that an a11y spec owns its own
    // fixture against the freshly started harness (see
    // tests/a11y/backdrop-perceptual.spec.js for the same pattern). Seeding
    // it globally would seed it on behalf of a spec file that never asked for
    // it here and duplicate ownership of the same fixture in two places.
  } finally {
    await ctx.dispose();
  }
}
