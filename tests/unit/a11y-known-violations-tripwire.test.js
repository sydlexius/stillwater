// Tripwire (#3442): CI shards the a11y run and a sharded teardown skips the
// allowlist staleness check, so nothing in CI would notice a dead entry.
import test from 'node:test';
import assert from 'node:assert/strict';
import { KNOWN_VIOLATIONS } from '../a11y/helpers/known-violations.js';

test('KNOWN_VIOLATIONS stays empty until CI can check staleness', () => {
  assert.equal(KNOWN_VIOLATIONS.length, 0,
    'CI is sharded and does not evaluate allowlist staleness. Before adding an '
    + 'entry, wire a fan-in staleness check across the shards (each leg uploads '
    + 'its seen-marks, one step merges them and calls reportStaleAllowances), '
    + 'see #3442.');
});
