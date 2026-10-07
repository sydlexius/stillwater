// a11y-global-teardown-shard.test.js - pins #3442: the allowlist staleness
// check must not call an entry stale in a SHARDED run (its spec may have run in
// another shard), while an unsharded run still fails when nothing matched.

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

test('unsharded run: stale entry still fails', async () => {
  await assert.rejects(globalTeardown({ shard: null }), /Stale a11y allowance/);
});
