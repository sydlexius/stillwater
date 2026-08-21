// platform-backdrop-fixture-hygiene.spec.js - proves the platform-backdrop
// fixture cannot leak Connection rows into the shared ephemeral database.
//
// WHAT THIS PINS, AND WHY IT IS A SPEC RATHER THAN AN ARGUMENT
//
// seed-platform-backdrop-duplicates.js stands up a fake Emby on port 0, so
// EVERY attempt gets a different port and therefore a different connection
// url. handleCreateConnection dedupes on type+url (GetByTypeAndURL), so that
// dedup could never match a previous attempt -- each attempt created a BRAND
// NEW connection row. Playwright retries a failed beforeAll hook twice
// (retries: 2 in playwright.config.js) and the whole tier shares ONE
// ephemeral database, so a run whose hook failed three times left three live
// connections behind, each pointing at a listener that had since been closed.
//
// Those rows are not inert. A connection whose server is gone reports its NFO
// check as unavailable, and the app's write guard is fail-closed on an
// indeterminate check -- which is how that debris made nfo-mbid.spec.js fail
// with a 409 nfo_write_blocked in a run that had nothing to do with platform
// backdrops. Fixture pollution, not a flake.
//
// Reasoning about the fix is not evidence, so these tests DRIVE it: two fakes
// on two different ports, the real ensureConnection against each, and a count
// read back off the live API. They use their own connection name so they can
// never reap a row the perceptual spec is depending on -- and because that
// name is the only identifier that survives a changing port, exercising the
// name path IS exercising the fix.

import { test, expect } from 'playwright/test';
import {
  startFakeEmby,
  ensureConnection,
  fixtureConnectionIDs,
  deleteFixtureConnections,
} from './helpers/seed-platform-backdrop-duplicates.js';

// Deliberately NOT the seeder's own FIXTURE_CONNECTION_NAME: this spec and
// platform-backdrop-perceptual.spec.js run against the same server, and a
// shared name would let either one delete the other's row.
const NAME = 'a11y platform-backdrop-duplicates hygiene probe';

test.afterAll(async ({ request }) => {
  // A failed assertion below must not itself become the debris this file
  // exists to forbid.
  await deleteFixtureConnections(request, NAME);
});

test('a retry on a fresh port reuses one row instead of stacking a second', async ({ request }) => {
  await deleteFixtureConnections(request, NAME);

  const first = await startFakeEmby();
  const second = await startFakeEmby();
  try {
    // Precondition, asserted rather than assumed: this test is only
    // meaningful if the two fakes really are on different ports, because a
    // shared port would let handleCreateConnection's type+url dedup pass the
    // test for a reason that has nothing to do with the fix.
    expect(
      second.port,
      'both fakes bound the same port, so this test would pass on the url dedup rather than on the name-based reuse it exists to check',
    ).not.toBe(first.port);

    await ensureConnection(request, first.port, NAME);
    expect(
      await fixtureConnectionIDs(request, NAME),
      'the first seeding attempt did not create exactly one connection',
    ).toHaveLength(1);

    // This is the retry. Before the fix it produced a SECOND row.
    await ensureConnection(request, second.port, NAME);
    const after = await fixtureConnectionIDs(request, NAME);
    expect(
      after,
      `a second seeding attempt on a different port left ${after.length} connections; `
      + 'each extra one is permanent debris that fails unrelated specs later in the run',
    ).toHaveLength(1);
  } finally {
    await first.close();
    await second.close();
  }
});

test('teardown removes the connection, not just the loopback listener', async ({ request }) => {
  await deleteFixtureConnections(request, NAME);

  const fake = await startFakeEmby();
  try {
    await ensureConnection(request, fake.port, NAME);
    // Precondition: there is actually something to delete. Without this the
    // assertion below passes vacuously on an empty database.
    expect(
      await fixtureConnectionIDs(request, NAME),
      'nothing was created, so the teardown assertion below would be vacuous',
    ).toHaveLength(1);
  } finally {
    await fake.close();
  }

  // The OLD teardown was exactly this close() and nothing else. Pin what that
  // left behind, so the assertions below are measuring a real removal rather
  // than a row that was never there.
  expect(
    await fixtureConnectionIDs(request, NAME),
    'closing the fake server removed the DB row on its own, so this test no longer measures anything',
  ).toHaveLength(1);

  const deleted = await deleteFixtureConnections(request, NAME);
  expect(deleted, 'teardown reported deleting no connection').toBe(1);
  expect(
    await fixtureConnectionIDs(request, NAME),
    'the connection survived teardown -- closing the fake server does not remove the DB row',
  ).toHaveLength(0);
});
