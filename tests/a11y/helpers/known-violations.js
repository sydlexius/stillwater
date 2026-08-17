// known-violations.js - NARROW, SELF-EXPIRING allowances for tracked defects.
//
// The list is currently EMPTY, which is the steady state. What follows is how to
// use it when a defect genuinely has to ship unfixed for a while.
//
// WHY THIS EXISTS RATHER THAN AN axe EXCLUDE
//
// The tempting unblock for a known violation is
// `AxeBuilder.exclude('<selector>')`. Do not do that. An exclude blinds the scan
// to that selector PERMANENTLY and silently: once the defect is fixed, nothing
// tells anyone the exclusion is dead, and any FUTURE violation on those elements
// is invisible. That is the same "passes because it never looked" failure this
// whole a11y tier keeps finding.
//
// So instead of hiding nodes from axe, an entry here asserts the violation set
// contains ONLY the known defect:
//
//   - a NEW violation anywhere on the page still fails the test
//   - a new violation on the SAME elements still fails, unless it is the exact
//     tracked rule
//   - when the defect is fixed the allowance stops matching, and the RUN fails
//     telling you to delete it -- it cannot rot quietly
//
// The last property is the important one, and it is the one that is easy to
// ship broken. An allowance that outlives its defect is how a suite silently
// stops testing something.
//
// It is delivered by reportStaleAllowances() below, called from
// tests/a11y/global-teardown.js. That wiring is load-bearing: the function
// existed here, exported and NEVER CALLED, while this header claimed the
// guarantee was in force (#2875). It was not.
//
// The seen-marks cross a PROCESS boundary through a file -- see the
// CROSS-PROCESS SEEN-SET note below. A module-level Set does not work here and
// fails in the dangerous direction: specs run in workers, teardown runs in the
// coordinator, so teardown read an always-empty Set and would have called every
// LIVE allowance stale.
//
// WHEN AN ENTRY IS THE RIGHT CALL
//
// When the real fix is bigger than the PR that surfaced it. The former #2875
// entry is the worked example: what looked like a one-line token change was a
// token that passed on the one background it was designed against and failed on
// the others in BOTH themes, across dozens of consumers. The fix was a contrast
// floor over ink/surface PAIRINGS (now tests/a11y/token-pairings.spec.js), which
// is design-system work with its own blast radius -- doing it inside a
// test-fixture PR would have been exactly the unscoped change this repo's size
// and decomposition rules exist to prevent.
//
// An entry is NOT the right call for a defect you simply do not want to fix
// today. Every entry needs an open issue.

/**
 * KNOWN_VIOLATIONS: one entry per tracked, deliberately-unfixed defect.
 *
 * Keep this list EMPTY in the steady state. Every entry is a debt with an
 * issue number, and `assertOnlyKnownViolations` fails when an entry stops
 * matching so the debt cannot be forgotten.
 */
export const KNOWN_VIOLATIONS = [
  // EMPTY, and that is the steady state. #2875 was the sole entry and is fixed:
  // --swd-ink-3 was retuned in design-tokens.css so the quiet ink clears 4.5:1
  // on every surface it is painted on, in both themes, and the floor is now
  // enforced directly by tests/a11y/token-pairings.spec.js rather than tracked
  // as debt here.
];

/**
 * assertOnlyKnownViolations fails unless the violation set is exactly the
 * known-and-tracked ones.
 *
 * @param {object} expect      Playwright's expect, passed in so this helper has
 *                             no import cycle with the specs.
 * @param {Array}  violations  axe results.violations
 * @param {string} label       surface name, for the failure message
 * @param {Function} format    the spec's formatViolations
 */
export function assertOnlyKnownViolations(expect, violations, label, format) {
  const unexpected = [];
  const matchedIssues = new Set();

  for (const v of violations) {
    const known = KNOWN_VIOLATIONS.filter((k) => k.ruleId === v.id);
    // Split this violation's NODES: tracked ones are allowed, the rest are not.
    // Splitting per node rather than per rule matters -- a second, unrelated
    // color-contrast failure on the same page must still fail the test.
    const untracked = v.nodes.filter(
      (n) => !known.some((k) => {
        if (!k.matches(n)) return false;
        matchedIssues.add(k.issue);
        return true;
      }),
    );
    if (untracked.length) unexpected.push({ ...v, nodes: untracked });
  }

  expect(
    unexpected,
    `${label} a11y violations (excluding tracked defects):\n${format(unexpected)}`,
  ).toHaveLength(0);

  // Record that this entry was seen. Staleness is deliberately NOT asserted
  // per-surface: whether a given page renders the offending element depends on
  // data and engine (the cheat-sheet full-page scan sees the dashboard activity
  // feed on Firefox but not reliably on Chromium), so "this surface must
  // exhibit this defect" is not a property that holds run to run. Asserting it
  // per-surface failed exactly that way in review.
  //
  // The staleness check belongs to the SUITE, not the scan -- see
  // reportStaleAllowances below. Record that a scan RAN (the `.` sentinel)
  // separately from which issues it
  // MATCHED. Those are different facts and the distinction is load-bearing: a
  // run with no marks at all could mean "no scan consulted the allowlist"
  // (nothing can be concluded) or "scans ran and matched nothing" (the defect
  // is FIXED -- exactly what this mechanism exists to catch). Without the
  // sentinel the second case is indistinguishable from the first and gets
  // silently swallowed.
  recordSeen('.');
  for (const issue of matchedIssues) recordSeen(issue);
}

// ---------------------------------------------------------------------------
// CROSS-PROCESS SEEN-SET
//
// Playwright runs specs in WORKER processes and globalTeardown in the
// COORDINATOR. They do not share module state: a module-level `Set` written by
// a worker is a DIFFERENT Set from the one teardown reads, so teardown always
// saw an empty one and reported every allowance stale -- including allowances
// that had just matched. Measured on playwright 1.62.0:
//
//   SPEC     pid=98399 seen.size=1
//   TEARDOWN pid=98385 seen.size=0
//
// That is the dangerous direction: a LIVE allowance doing its job would fail
// the run with a message instructing the reader to delete it.
//
// So the seen-set crosses the boundary through a file. Append-only, one issue
// number per line: appends are small and each worker opens with 'a', so
// concurrent workers interleave lines rather than clobbering a shared value,
// and the reader only needs the SET of numbers (order and duplicates are
// irrelevant). No locking needed for that shape.
// ---------------------------------------------------------------------------

import fs from 'node:fs';
import path from 'node:path';

// PER-INVOCATION, not merely per-directory. Two Playwright runs started from
// the SAME checkout (a local run beside a CI one, or two `make test-a11y`
// invocations) would otherwise share test-results/.known-violations-seen, and
// run B's globalSetup reset would delete run A's marks mid-flight -- making run
// A call a LIVE allowance stale. That is the same false positive the
// cross-process fix removed, reintroduced through a different door.
//
// SW_A11Y_SEEN_RUN_ID is stamped once at MODULE level by global-setup.js -- in
// the coordinator, when the config graph loads, before any worker is forked --
// and inherited by every worker and by globalTeardown through the environment.
// (It moved out of the globalSetup function body in #3057 because the
// storage-state path is now keyed on it too, and that path is a module-level
// const the config imports.) A run that somehow has no id falls
// back to a shared path rather than inventing a per-PROCESS id: workers must
// agree on the file, and a per-process id would silently give each worker its
// own, which reads as "nothing was ever seen".
/**
 * seenFile resolves the marks path AT CALL TIME, never at module load.
 *
 * That distinction is load-bearing and was got wrong once: globalTeardown
 * imports this module, and an ES import is evaluated when the config graph
 * loads -- BEFORE globalSetup runs and stamps the run id. A module-level
 * `const` therefore captured the id-less fallback path in the coordinator while
 * the workers (forked later, after the stamp) wrote to the id-bearing one, so
 * teardown read a file nothing had written and reported "no scan ran" on every
 * run. Measured: env id present, SEEN_FILE still `.known-violations-seen`.
 *
 * Resolving per call costs nothing here (a handful of calls per run) and cannot
 * go stale.
 */
function seenFile() {
  const dir = process.env.SW_A11Y_SEEN_DIR
    || path.join(process.cwd(), 'test-results');
  const id = process.env.SW_A11Y_SEEN_RUN_ID;
  return path.join(dir, id ? `.known-violations-seen-${id}` : '.known-violations-seen');
}

/**
 * newRunId returns an id for this invocation. Called by globalSetup only.
 */
export function newRunId() {
  return `${process.pid}-${Date.now().toString(36)}`;
}

// recordSeen and resetSeen deliberately do NOT catch. An I/O failure here is not
// cosmetic: a lost mark makes a live allowance look stale (a confusing failure)
// and a failed reset makes a dead one look alive (a SILENT one, which is worse).
// Swallowing either would resurrect exactly the class of bug this file exists to
// prevent, so the run fails on the real error instead.
function recordSeen(issue) {
  const file = seenFile();
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.appendFileSync(file, `${issue}\n`);
}

/**
 * resetSeen clears marks from any previous run. Called from globalSetup: a
 * stale file would otherwise make a genuinely-dead allowance look alive.
 *
 * `force: true` already makes a missing file a no-op, so anything that still
 * throws is a real filesystem problem and is allowed to propagate.
 */
export function resetSeen() {
  fs.rmSync(seenFile(), { force: true });
}

/**
 * reportStaleAllowances returns the entries that NEVER matched anywhere in this
 * run, i.e. defects that appear to be fixed and whose allowance is now dead
 * weight blinding a surface to future regressions.
 *
 * Returns null -- meaning "cannot tell", distinct from "nothing is stale" -- only
 * when NO SCAN RAN at all (no marks file, or a file with no `.` sentinel). That
 * happens when every allowlist-consulting spec skipped or the run died early,
 * and in that state nothing can be concluded.
 *
 * A run where scans DID run and matched nothing is NOT that case: it is the
 * defect-is-fixed signal, and it returns the full list so the run fails. The
 * sentinel is what separates the two -- without it, the conclusive case looked
 * identical to the inconclusive one and was silently swallowed.
 */
export function reportStaleAllowances() {
  let marks;
  try {
    marks = fs.readFileSync(seenFile(), 'utf8').split('\n').filter(Boolean);
  } catch (err) {
    // ENOENT is the expected "no scan ran" case and is the ONLY error that maps
    // to null. Anything else (EACCES, EIO, a directory in the way) is a real
    // failure to READ state, and reporting it as "cannot tell" would let a run
    // pass while the staleness check silently did nothing.
    if (err.code === 'ENOENT') return null;
    throw err;
  }
  // No sentinel: the file exists but no scan completed a consult. Inconclusive.
  if (!marks.includes('.')) return null;

  const seenIssues = new Set(marks.filter((m) => m !== '.').map(Number));
  return KNOWN_VIOLATIONS.filter((k) => !seenIssues.has(k.issue))
    .map((k) => `#${k.issue} (${k.note})`);
}
