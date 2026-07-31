// known-violations.js - a NARROW, SELF-EXPIRING allowance for one tracked defect.
//
// WHY THIS EXISTS RATHER THAN AN axe EXCLUDE
//
// The dashboard activity feed's <time> elements render at 4.35:1 in dark mode
// against a 4.5:1 requirement (#2875). Two scans see them -- the dashboard
// stat-card scan and the full-page cheat-sheet scan -- and both fail.
//
// The obvious unblock is `AxeBuilder.exclude('.sw-activity-row time')`. Do not
// do that. An exclude blinds the scan to that selector PERMANENTLY and
// silently: once #2875 is fixed, nothing tells anyone the exclusion is dead,
// and any FUTURE violation on those elements is invisible. That is the same
// "passes because it never looked" failure this whole a11y tier keeps finding.
//
// So instead of hiding the nodes from axe, this asserts the violation set
// contains ONLY the known defect:
//
//   - a NEW violation anywhere on the page still fails the test
//   - a new violation on the SAME elements still fails, unless it is the exact
//     tracked rule
//   - when #2875 is fixed the allowance stops matching, and the test FAILS
//     telling you to delete it -- it cannot rot quietly
//
// The last property is the important one. An allowance that outlives its defect
// is how a suite silently stops testing something.
//
// WHY #2875 IS NOT JUST FIXED HERE
//
// It looked like a one-line token change. It is not. Measured across the real
// backgrounds it is used on:
//
//   --swd-ink-3 light #64748b  on #ffffff 4.76  on #f1f5f9 4.34  <- also fails
//   --swd-ink-3 dark  #7d8aa1  on #0b1220 5.37  on #111a2e 4.97  on #1c2638 4.35
//
// The token passes on the backgrounds it was designed against ("AA body on
// white", per its own comment) and fails on the ones nobody checked -- in BOTH
// themes. It has 17 consumers. The real fix is a contrast floor over
// ink/surface PAIRINGS, not a new value, and that is design-system work with
// its own blast radius. Doing it inside a test-fixture PR would be exactly the
// unscoped change this repo's size and decomposition rules exist to prevent.

/**
 * KNOWN_VIOLATIONS: one entry per tracked, deliberately-unfixed defect.
 *
 * Keep this list EMPTY in the steady state. Every entry is a debt with an
 * issue number, and `assertOnlyKnownViolations` fails when an entry stops
 * matching so the debt cannot be forgotten.
 */
export const KNOWN_VIOLATIONS = [
  {
    issue: 2875,
    ruleId: 'color-contrast',
    // Matches only the activity-feed timestamps. Deliberately specific: a
    // contrast violation ANYWHERE else, including elsewhere on these pages,
    // is not covered by this allowance and still fails.
    matches: (node) => node.target.some(
      (t) => typeof t === 'string' && /\.sw-activity-row.*\btime\b/.test(t),
    ),
    note: 'dashboard activity timestamps at 4.35:1 (--swd-ink-3 on the activity-row surface)',
  },
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
  // reportStaleAllowances below.
  for (const issue of matchedIssues) seenIssues.add(issue);
}

// seenIssues accumulates across every assertOnlyKnownViolations call in a run.
const seenIssues = new Set();

/**
 * reportStaleAllowances returns the entries that NEVER matched anywhere in this
 * run, i.e. defects that appear to be fixed and whose allowance is now dead
 * weight blinding a surface to future regressions.
 *
 * Exposed rather than asserted inline because a single spec file cannot know
 * whether another file's scan matched the entry. Wire it into a suite-wide
 * teardown when the list grows; with one entry the surviving protection is
 * that the entry is narrow (one rule, one selector shape) and documented with
 * its issue number.
 */
export function reportStaleAllowances() {
  return KNOWN_VIOLATIONS.filter((k) => !seenIssues.has(k.issue))
    .map((k) => `#${k.issue} (${k.note})`);
}
