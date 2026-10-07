// global-teardown.js - fails the run when a known-violation allowance outlived
// the defect it tracks.
//
// WHY THIS FILE EXISTS
//
// helpers/known-violations.js promises, in its own header, that "when the defect
// is fixed the allowance stops matching, and the test FAILS telling you to
// delete it -- it cannot rot quietly". That property was delivered entirely by
// reportStaleAllowances(), which was exported and never called (#2875): the
// comment deferred wiring it to "a suite-wide teardown when the list grows". So
// the guarantee the file documented was not actually in force, and a fixed
// defect would have left a dead allowance silently blinding two scans to any
// FUTURE violation on those same elements -- the exact "passes because it never
// looked" failure the allowlist was written to prevent.
//
// The check belongs here rather than in a spec because staleness is a property
// of the WHOLE RUN: whether a given surface exhibits a tracked defect depends on
// data and engine, so no single spec can tell that an entry never matched
// anywhere. Only a run-level hook sees every scan.
//
// Playwright runs globalTeardown once per invocation, AFTER all projects. A
// throw here fails the run.

import { reportStaleAllowances, KNOWN_VIOLATIONS } from './helpers/known-violations.js';

export default async function globalTeardown(config) {
  // An empty list is the steady state and is trivially not stale.
  if (KNOWN_VIOLATIONS.length === 0) return;

  // SHARDED RUNS (#3442): CI splits each engine's suite across runners with
  // `--shard`, and each shard's teardown sees only the seen-marks of the spec
  // files IT ran. An entry whose spec ran in another shard would look dead
  // here, so a verdict would be a false "stale" failure. KNOWN_VIOLATIONS
  // entries carry no spec file, so which shard owns an entry cannot be decided
  // from the data at hand; the verdict is skipped, loudly, not silently. The
  // unsharded run sees every scan, but it only happens locally (`make
  // test-a11y`, or the opt-in RUN_A11Y=1 gate); the default gate skips the tier
  // and CI is sharded, so CI does NOT run this check. tests/unit/
  // a11y-known-violations-tripwire.test.js keeps the list empty until a
  // fan-in staleness check exists (#3442).
  if (config && config.shard) {
    console.warn(`known-violations: staleness check skipped in sharded run `
      + `(shard ${config.shard.current}/${config.shard.total}): this shard saw only `
      + 'its own scans; an unsharded run (make test-a11y) is authoritative.');
    return;
  }

  const stale = reportStaleAllowances();

  // null means "cannot tell" (no marks file: no allowlist-consulting scan ran),
  // NOT "everything is stale". Reporting stale here would fail a run that simply
  // skipped those specs -- the same false positive the cross-process fix removed.
  if (stale === null) {
    console.warn('known-violations: no scan recorded a seen-mark this run, so '
      + 'allowance staleness could not be evaluated.');
    return;
  }

  if (stale.length === 0) return;

  throw new Error(
    'Stale a11y allowance(s) in tests/a11y/helpers/known-violations.js.\n\n'
    + `${stale.map((s) => `  - ${s}`).join('\n')}\n\n`
    + 'These entries matched NO violation anywhere in this run, which means the\n'
    + 'defect they track appears to be FIXED. Delete the entry (and close the\n'
    + 'issue). Leaving it in place blinds the scans that consult it to any future\n'
    + 'violation on those elements.\n\n'
    + 'If instead the surface simply was not rendered this run (a data-dependent\n'
    + 'scan that found no rows, or a spec that was skipped), fix the FIXTURE so\n'
    + 'the surface is actually exercised -- do not relax this check, or the\n'
    + 'allowlist goes back to being unable to expire.',
  );
}
