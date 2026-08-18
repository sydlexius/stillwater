# Automated restore of locked fields clobbered by past rule runs

Design for issue #3038 (v1.6.2, `priority: critical`). Repair half of #3037,
whose prevention half shipped in #3052 / #3053 / #3055 / #3060 / #3063 / #3065.

## Problem

#3037 established that the rule-fixer path never consulted `Artist.LockedFields`:
fixers mutated the artist struct directly and the pipeline persisted the whole
struct, so a field the operator had pinned was overwritten, and in one case
cleared outright. Prevention now holds at the persist chokepoint
(`internal/artist/lockguard.go`), but prevention does not help an artist that is
already damaged.

This unit puts those values back, automatically, without operator action.

## What makes the damage attributable

The issue's condition 3 assumes "a rule auto-fix audit row". There is no
`*_audit` table for rule fixes, and `rule_violations` / `rule_results` are both
current-state tables keyed `UNIQUE(rule_id, artist_id)` and rewritten on every
re-evaluation, so neither is a durable trail.

The trail exists in a different shape. `Pipeline.recordRuleFixHistory`
(`internal/rule/fixer.go:456`, issue #1106, shipped `4dc0fa52` on 2026-04-27)
writes a `metadata_changes` row for every SUCCESSFUL auto-fix:

| column | value |
|---|---|
| `field` | `"rule_fix"` (pseudo-field; `IsTrackableField` is false, so no Revert button renders) |
| `source` | `"rule:<rule_id>"` |
| `new_value` | the fixer's operator-facing message |
| `artist_id`, `created_at` | as written |

Two properties make it usable as the attribution key:

- **Successful fixes only.** It returns early unless `fr.Fixed`, and since #3065
  its sole call site is `grantFixCredits` (`internal/rule/lock_reverted_fix.go:246`),
  deliberately deferred so a lock-reverted fix emits no row at all.
- **Written AFTER the damage, not before.** The issue body states the audit row
  precedes the persist; that stopped being true when #3065 deferred it.
  `persistHealthAfterRun` writes the artist at `internal/rule/fixer.go:1097` and
  `grantFixCredits` writes the audit row at `:1101`. Any predicate keyed on the
  ordering is therefore wrong in the direction the issue assumed.

The trail predates every release including `v1.6.1`. That made it TEMPTING as an
attribution key, and the next section explains why it cannot serve as one
anyway.

### Rejected: parsing logs

Logs cannot carry this. `internal/logging/logging.go:395` defaults to 10 MB per
file / 30-day age with lumberjack rotation deleting the rest, so damage older
than a month is gone. The fixers also barely log field transitions: only
`fixers_language.go:118` logs old and new values, and `fixJunkBio`
(`internal/rule/fixers.go:460`) logs nothing about the biography it replaces --
the exact damage this unit targets. A log parser would be a fragile reader over
text that is both incomplete and expiring.

### REINSTATED: selecting on `source LIKE 'rule:%'` on the damage row itself

An earlier draft REJECTED this on coverage grounds, arguing that a damage row
carries a `rule:` source only inside the window between #3048 (`fdeb1b6f`) and
the prevention chokepoint (`2b50d3b5`) -- never released, so "approximately zero
rows on any real database".

**The coverage arithmetic was right; the conclusion was wrong**, because it
weighed coverage without weighing SAFETY. The alternative it chose in preference
-- the artist-wide `rule_fix` join -- buys its wider coverage by matching rows no
rule caused, which is not wider coverage at all but false positives that destroy
operator data. Per-row `source` is the only key that distinguishes a rule write
from an operator edit, so it is reinstated as the attribution condition.

Two corrections to that draft's reasoning, both worth keeping visible:

- The window is **not** bounded above by the chokepoint. The chokepoint stops a
  rule from changing a LOCKED field; it does not stop rule writes generally, and
  a rule that damages a field the operator locks LATER still writes a
  `rule:`-sourced row today. The upper bound was wrong.
- "Approximately zero rows" remains true for existing databases, and that is now
  stated as the design's accepted cost rather than used to justify a
  false-positive-prone predicate.

### The join alone is NOT a causal link (found in review, #3074)

An earlier draft of this design claimed the `rule_fix` join "gets attribution
WITHOUT widening to unattributed damage: an operator's own edit has no
`rule_fix` row on that artist naming a rule that writes that field." **That claim
was FALSE and the mechanism it described would have destroyed operator data.**

The counter-example is ordinary, not exotic:

1. `metadata_quality` runs on artist A at 10:00 and writes a `rule_fix` row.
2. At 11:00 the OPERATOR edits `biography` themselves, holding a grant on the
   locked field.
3. That operator row satisfies every damage predicate: `old_value != ''`,
   `old_value != new_value`, `source = 'manual'`, and it is rank 1.
4. The join matches, because a `rule_fix` row exists on that artist and
   `metadata_quality` declares `biography`.
5. The field is locked now.

The repair would restore the value the operator deliberately replaced. The join
proves only that a rule ran on this artist AT SOME POINT -- never that it caused
THIS row.

**THE DURABLE CAUSAL LINK IS THE DAMAGE ROW'S OWN SOURCE.**
`persistHealthAfterRun` wraps the write in `withRuleHistorySource`
(`internal/rule/fixer.go:2438`), so the damage row itself carries
`source = "rule:<id>"` on any build shipping #3048. That is per-row and cannot
name an operator edit, because an operator edit is written on a different path
with a different source.

So condition 3 requires BOTH:

- `damage.source LIKE 'rule:%'` -- the row itself says a rule wrote it, and
- `RuleFields(ruleID)` contains the damaged field, where `ruleID` is parsed
  from that row's own `source`.

The `rule_fix` join is then no longer load-bearing for attribution and is
DROPPED from the predicate entirely. It stays useful only as corroboration in the
report.

**THE HONEST COST, stated plainly.** This returns coverage to the post-#3048
window the `rule_fix` trail was reached for in order to escape. Damage written
before `fdeb1b6f` carries `source = "manual"` and is byte-identical to an
operator edit, so it is NOT recoverable by any safe predicate. The repair is
therefore FORWARD-LOOKING: it protects against a future write path that escapes
the chokepoint, and it does not repair the historical loss that motivated #3038.
That is a real reduction in scope and it is the correct one -- a mechanism that
silently reverts operator edits is worse than one that repairs nothing.

Damage the mechanism cannot attribute is REPORTED as unrecoverable, so the
operator can act on it through the blast-radius pane, which already surfaces it.

## Row selection: a positive allow-list

A row is restored only when all four are affirmatively true. Anything
unrecognized -- an unknown `rule_id`, an unparsable source, a field absent from
the catalog -- falls through to NOT restored and is counted as unrecoverable.

A predicate safe for deciding whether to WRITE turns destructive when inverted
to decide what to overwrite, so this is an allow-list and never a negated
safe-list.

1. The field is CURRENTLY in that artist's `locked_fields`.
2. The row is the newest change for its `(artist_id, field)` pair AND still
   reads as damage.
3. The damage row's OWN `source` is `rule:<id>` -- per-row causation, not an
   artist-wide coincidence.
4. `rule.RuleFields(ruleID)` contains the damaged field, where `ruleID` is the
   RULE id parsed out of that row's `source` (`"rule:metadata_quality"` ->
   `"metadata_quality"`), NOT the `metadata_changes` row's `id` column.

There is no longer a condition keyed on a SEPARATE `rule_fix` row, and no
timestamp-ordering condition at all. Both are gone deliberately: see "The join
alone is NOT a causal link" above for why the ordering condition was not merely
unnecessary but BACKWARDS.

### Why the old ordering condition was wrong in BOTH directions

The superseded design required `damage.created_at >= rule_fix.created_at`,
justified as "the audit row is written before the artist is persisted, so a rule
cannot have caused a change that predates its own audit entry".

**That ordering is now inverted, and the design never noticed.**
`persistHealthAfterRun` writes the artist -- and therefore the damage row -- at
`internal/rule/fixer.go:1097`, and `grantFixCredits` writes the `rule_fix` row
after it at `:1101`. #3065 deferred the audit row precisely so a lock-reverted
fix emits none, which moved it AFTER its own damage.

So the condition would have rejected every genuine candidate on a current build,
and the plan's `TestLockDamageCandidates_ExcludesDamageOlderThanTheRuleFix`
asserted the wrong direction. A predicate resting on a wall-clock ordering
between two rows written by different statements is fragile whichever way it
points, which is the deeper reason the per-row `source` is the right key.

### The SQL / Go seam

- **SQL answers 2 and 3:** ranking, the damage test, and
  `source LIKE 'rule:%'`. Returns candidates carrying the damaged field, the old
  value, and the `rule_id` parsed from that row's own source.
- **Go answers 1 and 4:** the field must currently be locked, and
  `RuleFields(rule_id)` must contain the damaged field.

`RuleFields` is a static Go map (`internal/rule/catalogue.go:421`) and
`locked_fields` is a JSON array on the artist row. Pushing either into SQL
re-implements Go logic in SQL -- what `024_retract_false_duplicate_passes.sql`
documents as the reason to avoid a migration ("A SQL re-implementation of the
... predicate would duplicate logic that lives in Go and drift from it").

`internal/artist` must NOT import `internal/rule` (that inverts the existing
direction). The query returns `rule_id` verbatim; the CALLER applies
`RuleFields`. The capability map crosses the boundary as data, not as an import.

## Placement

**The query is a new read-only `HistoryRepository` method in `internal/artist`**,
alongside `blast_radius.go`.

- `blastRadiusRankedCTE` and `blastRadiusDamageWhere` are unexported in
  `internal/artist/sqlite_history.go`. The issue requires reusing them rather
  than writing a second copy, and the CTE's header calls filtering-inward "the
  single most dangerous optimization available in this file". A
  maintenance-package implementation could only reuse them by exporting or
  copying -- and a copy is the drift the issue warns against.
- Precedent: `ListNFOMBIDWrites` / `CountNFOMBIDWrites` are read-only recovery
  queries built for one remediation flow and parked on this same interface.
- It keeps SQL out of the orchestration: the maintenance side holds no SQL.

**The orchestration is a new `internal/maintenance/lock_damage_repair.go`**,
invoked from the startup block in `cmd/stillwater/main.go` that already drives
the maintenance one-shots.

A migration is the wrong vehicle for the reason quoted above. This follows the
established one-shot pattern (the exists-flag scanner, the artwork reconciler,
the orphan cleanup at startup).

## Run-once guard

A `settings` row, key `lock_damage_repair.completed_at`, written ONLY after a
successful pass, read through the existing `getDBStringSetting` shape
(`cmd/stillwater/main.go:2262`).

- **Written after, never before.** A crash mid-run leaves it unset so the next
  boot retries. Writing it first converts a crash into a permanent silent skip.
- **The flag is an optimization; the QUERY is the real idempotence.** A restore
  writes a `source='revert'` row, which makes the repaired pair drop out of the
  damage query, so a second pass selects nothing on its own merits. The
  "clean no-op on second startup" criterion is therefore tested with the flag
  CLEARED, so a pass proves convergence rather than proving a boolean was set.

## The restore write

Each restored `(artist, field)` pair is its own `artist.Service.UpdateField`
call with `ctx = ContextWithSource(ctx, "revert")` -- the primitive the shipped
single undo uses.

- **Not blocked by the guard it repairs.** `lockguard.go:67-72` states that
  `UpdateField` / `ClearField` write their own targeted SQL and never pass
  through the chokepoint, because the operator's history revert and blast-radius
  restore are deliberately lock-blind. Restoring INTO a locked field therefore
  works -- required, since condition 1 demands the field be locked now. Routing
  through `Service.Update` instead would hit `enforceLocksBeforeUpdate` and have
  the restore reverted by the very guard that stopped the damage.
- **`source='revert'` is what makes the run converge.**
  `blastRadiusDamageWhere` excludes `source != 'revert'` and the CTE ranks by
  `created_at DESC`, so a restored pair's newest row is a revert row and the
  pair leaves both the report and the next run's candidate set.
- **Cannot re-trigger the damaging writer.** `UpdateField` records history and
  marks the artist dirty; it evaluates no rules and calls no providers.
- **Per-field, never a whole-artist persist.** A whole-row write drags along
  every other field AND lands in `Service.update` where the chokepoint lives.
  One field per call also isolates failures.

### The replay trap (#2750)

A revert row has a non-empty `old_value` and a different non-empty `new_value`,
so it structurally resembles damage. The single `source != 'revert'` exclusion
in `blastRadiusDamageWhere` is the only thing preventing a revert-of-a-revert,
and its comment marks it as the ONE deliberate place a source is excluded.
Reusing that predicate inherits the protection rather than reimplementing it --
but it still earns its own test.

## Reporting

- **A structured `slog` record per restore:** artist ID, field, attributing rule
  ID, and the timestamps of both the damage row and the `rule_fix` row. NOT the
  values -- an old biography is user library content and does not belong in a
  log line.
- **Recent Activity and the blast-radius pane, for free.** The write is an
  ordinary `source='revert'` history row, so both surfaces render it through
  existing machinery. This unit adds NO new UI; a conditional banner is #2678.
- **An explicit unrecoverable tally, by field.** A run that repairs nothing and
  says so is correct. A run reporting zero because the mechanism cannot observe
  the damage is the "unknown rendered as clean" defect the blast-radius work
  exists to prevent.

### Coverage, stated honestly

| Field | Recoverable? | Why |
|---|---|---|
| `biography` | YES, post-#3048 only | always in `trackableFields`; `metadata_quality` replaces a non-empty value, so it produces real damage |
| `origin` | NO damage exists to repair | always tracked and `origin_missing` declares it, but that rule fires ONLY when origin is already empty, so it destroys nothing. Repairable in principle if a future writer of `origin` damages it; nothing to recover today |
| `name`, `sort_name` | NO, before #3037 | entered `trackableFields` only in #3037, so earlier damage wrote no row |
| `musicbrainz_id`, provider IDs | NO | never in `trackableFields`; no row was ever written |
| ANY field damaged before #3048 (`fdeb1b6f`) | NO, by design | the row carries `source = "manual"`, byte-identical to an operator edit. Reported as unrecoverable so the operator can act via the pane |
| ANY field whose row carries a pseudo-source (`rule:multiple_rules`, the bulk jobs, the phash passes; see `rule.IsPseudoRuleSource`) | NO, by design | the row IS rule-engine damage, but the responsible rule -- and so its declared field set -- is not recorded on the row. Reported as unrecoverable with a batch-write reason, never the false "the attributing rule does not write this field" |

**The headline consequence, stated where nobody can miss it:** after the review
correction above, this mechanism repairs only damage written by a build carrying
#3048. No released build does. The repair is a FORWARD-LOOKING safety net, not a
recovery for the historical loss that motivated #3038, and the issue's premise
that damage "must be repaired on upgrade" is not achievable safely. The
blast-radius pane remains the recovery path for pre-#3048 damage.

Rules declaring fields (`internal/rule/catalogue.go`, verified by reading the
map keys that own each `Fields:` entry): `nfo_has_mbid` and `mbid_resolves`
(`musicbrainz_id`), `bio_exists` and `metadata_quality` (`biography`),
`name_language_pref` (`name`, `sort_name`), `origin_missing` (`origin`).

`provider_id_missing` declares NO fields, so `RuleFields` returns nil for it and
it can attribute nothing. That is a real coverage hole rather than an oversight
in this document: it is one of the rules #3037 named as damaging locked fields,
and its damage is unattributable by this mechanism. The damage was also to
provider-ID fields, which were never in `trackableFields`, so no history row
exists to restore from either way. Both facts point the same direction, and the
run reports such damage as unrecoverable rather than silently omitting it.

The fill-a-blank fixers (`bio_exists`, `origin_missing`) produce no damage --
they fire only when the field is already empty.

## Tests

Both packages use real SQLite via their established `setupTestDB` fixtures.

1. **The positive control pair (most important).** A locked field damaged by a
   rule IS restored; an otherwise identical UNLOCKED field damaged the same way
   is NOT. Same artist, same rule, same damage shape, differing only in
   `locked_fields`. Without the negative half the positive one can pass while
   the predicate matches nothing.
2. **The attribution control.** A locked field damaged with NO attributing
   `rule_fix` row is NOT restored and DOES appear in the unrecoverable tally.
   This pins the deliberate decision not to widen to unattributed damage.
3. **Revert does not revert itself.** Two passes over one fixture; the second
   restores nothing. Run with the settings flag CLEARED.
4. **An operator edit after the damage blocks the restore** for that field.
5. **The unrecoverable tally is non-zero when it should be:** seed
   `musicbrainz_id` damage on a locked field, assert it is reported rather than
   silently zero.
6. **Precondition assertions on every fixture:** the field really is locked, the
   `rule_fix` row really exists, the damage row really is newest -- asserted
   BEFORE trusting what the run reports. A fixture that silently fails to seed
   produces a green test that verifies nothing.

### Mutation proofs

Each must fail a test:

- Delete the `locked_fields` check -> the unlocked control fails.
- Delete the `rule_fix` join -> the unattributed control fails.

If either leaves the suite green, that control is decorative.

Test volume ceiling: cover the real branches and STOP. No test whose only
purpose is executing a line.

## Failure modes

- **A failed restore does not abort the run.** Each pair is independent;
  failures are logged, counted, and the run continues. The flag is written only
  on a completed pass, so a run with failures retries next boot -- and
  already-restored pairs are no longer candidates, making the retry incremental.
- **An unknown `rule_id`** (a deprecated rule) yields an empty `RuleFields`, so
  nothing matches and the row counts as unrecoverable. The allow-list direction
  holding: unrecognized input does not act.

## Validation against a production clone

Synthetic fixtures only prove the predicate does what the author THINKS damage
looks like. Before the PR opens, run the repair against a COPY of the
maintainer's production database.

**Handling rules, both mandatory:**

- **Work on a copy, never the live file.** Copy the database (and its `-wal` /
  `-shm` siblings) to a scratch path, point `SW_DB_PATH` at the copy, and never
  open the original read-write. SQLite recovery on an unclean copy can mutate
  the file it opens.
- **The clone is PRIVATE MEDIA-LIBRARY METADATA.** Artist names, biographies, and
  titles from it MUST NOT reach any outward surface: no GitHub issue, PR body,
  commit message, review comment, or log excerpt pasted into a bot thread. Keep
  the technical substance (row counts, field names, rule IDs, whether the
  predicate fired) and redact identifying titles. Local scratch files may hold
  real identifiers; the shared surface must be generic.

**What the clone run answers that fixtures cannot:**

- How many `(artist, field)` pairs the four-part predicate actually selects.
- Whether any selection looks like an operator edit rather than rule damage --
  the one failure mode the synthetic controls cannot surface, since they only
  contain damage the author constructed.
- The real distribution of the unrecoverable tally by field.

**Run it in report-only mode first** (select and report, write nothing), inspect
the candidate list by hand, and only then run the write pass against the clone
and diff the result. A predicate that selects a surprising row is a design
finding, not a tuning problem.

## Out of scope

- New UI (the pane and activity feed already render restores) -- #2678.
- Guarding the single-column write verbs / operator grants ("unit 3"), which is
  what closes `handlers_platform_state.go:142-175`, an automated writer wearing
  an operator's click.
- Any widening of the predicate to unattributed damage.

## Sequencing

#3038 carries `[mode: plan]`, so it is the design issue. A separate tracked
implementation issue references this document, per the repo rule that a
design/spec issue needing implementation gets its own impl issue.

The task-by-task implementation plan derived from this design is
`docs/architecture/lock-damage-repair-plan.md`.
