// a11y-global-teardown-shard.test.js - pins #3442: the allowlist staleness
// check must not call an entry stale in a SHARDED run (its spec may have run in
// another shard, whether --shard or a CI file-list shard marked by SW_A11Y_SHARD),
// while an unsharded run still fails when nothing matched.

import test, { after } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import globalTeardown from '../a11y/global-teardown.js';
import { KNOWN_VIOLATIONS } from '../a11y/helpers/known-violations.js';

// A scan ran ('.' sentinel) but matched nothing: the "defect is fixed" shape.
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'sw-seen-'));
process.env.SW_A11Y_SEEN_DIR = dir;
process.env.SW_A11Y_SEEN_RUN_ID = 'teardown-shard-test';
fs.writeFileSync(path.join(dir, '.known-violations-seen-teardown-shard-test'), '.\n');
after(() => fs.rmSync(dir, { recursive: true, force: true }));
KNOWN_VIOLATIONS.push({ issue: 999999, ruleId: 'fake', note: 'fake entry' });

test('sharded run: no stale verdict, one warning', async () => {
  const warns = [];
  const orig = console.warn;
  console.warn = (m) => warns.push(m);
  try {
    await globalTeardown({ shard: { current: 1, total: 3 } });
  } finally { console.warn = orig; }
  assert.equal(warns.length, 1);
  assert.match(warns[0], /sharded run/);
});

// A firefox file-list shard (no --shard, so config.shard is null) is still a
// slice of the suite: CI marks it with SW_A11Y_SHARD.
test('file-list shard (SW_A11Y_SHARD, no config.shard): no stale verdict', async () => {
  const warns = [];
  const orig = console.warn;
  console.warn = (m) => warns.push(m);
  process.env.SW_A11Y_SHARD = '2/4';
  try {
    await globalTeardown({ shard: null });
  } finally { console.warn = orig; delete process.env.SW_A11Y_SHARD; }
  assert.equal(warns.length, 1);
  assert.match(warns[0], /sharded run \(shard 2\/4\)/);
});

test('unsharded run: stale entry still fails', async () => {
  delete process.env.SW_A11Y_SHARD;
  await assert.rejects(globalTeardown({ shard: null }), /Stale a11y allowance/);
});
