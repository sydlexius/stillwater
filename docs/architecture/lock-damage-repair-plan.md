# Locked-Field Damage Repair Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Automatically restore locked artist fields that past rule runs
overwrote, once per database, at startup.

**Architecture:** A read-only query on `HistoryRepository` selects candidate
damage by reusing the existing blast-radius ranking CTE and damage predicate,
joined to the `rule_fix` audit rows that `recordRuleFixHistory` has written
since #1106. A maintenance one-shot applies the two Go-side conditions (the
field is currently locked; the attributing rule declares that field) and
restores each surviving pair through `Service.UpdateField` with
`source="revert"`.

**Tech Stack:** Go 1.26, `modernc.org/sqlite` (pure Go, no CGO), `log/slog`,
stdlib `database/sql`. Tests use real SQLite via each package's existing
`setupTestDB` fixture.

**Spec:** `docs/architecture/lock-damage-repair.md`

## Global Constraints

- Go 1.26+, `net/http` stdlib routing, `log/slog` for all logging.
- `internal/artist` MUST NOT import `internal/rule`. The dependency runs the
  other way. The query returns `rule_id` verbatim; the caller applies
  `rule.RuleFields`.
- Row selection is a POSITIVE ALLOW-LIST. Anything unrecognized falls through
  to not-restored and is counted as unrecoverable. Never a negated safe-list.
- Never log field VALUES (an old biography is user library content). Log artist
  ID, field name, rule ID, timestamps.
- No emoji, no em-dashes in code, comments, or docs.
- Test ceiling: cover the real branches and STOP. No test whose only purpose is
  executing a line. Every fixture asserts its own preconditions before the
  assertion that matters.
- Run `bash scripts/pre-push-gate.sh` before any push; every check in its
  default path is blocking.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/artist/lock_damage.go` (create) | `LockDamageCandidate` type + the `LockDamageCandidates` repository method contract |
| `internal/artist/sqlite_history.go` (modify) | The SQL implementation, reusing `blastRadiusRankedCTE` |
| `internal/artist/history.go:55-87` (modify) | Add the method to the `HistoryRepository` interface |
| `internal/maintenance/lock_damage_repair.go` (create) | The one-shot: Go-side conditions, the restore loop, the report |
| `cmd/stillwater/main.go` (modify) | Startup wiring, guarded by the settings key |
| `internal/artist/lock_damage_test.go` (create) | Query-level tests |
| `internal/maintenance/lock_damage_repair_test.go` (create) | Orchestration, controls, mutation proofs |

---

### Task 1: The candidate query

**Files:**
- Create: `internal/artist/lock_damage.go`
- Modify: `internal/artist/history.go` (add to `HistoryRepository`, after
  `CountNFOMBIDWrites`)
- Modify: `internal/artist/sqlite_history.go` (append the implementation)
- Test: `internal/artist/lock_damage_test.go`

**Interfaces:**
- Consumes: `blastRadiusRankedCTE`, `blastRadiusDamageWhere` (both unexported,
  same package, do not modify them).
- Produces:
  ```go
  type LockDamageCandidate struct {
      ChangeID   string
      ArtistID   string
      ArtistName string
      Field      string
      OldValue   string
      NewValue   string
      RuleID     string    // extracted from the joined rule_fix row's source
      DamagedAt  time.Time
      FixedAt    time.Time // the attributing rule_fix row's created_at
  }

  // On HistoryRepository:
  LockDamageCandidates(ctx context.Context) ([]LockDamageCandidate, error)
  ```

- [ ] **Step 1: Write the failing test**

In `internal/artist/lock_damage_test.go`:

```go
func TestLockDamageCandidates_JoinsAttributingRuleFixRow(t *testing.T) {
    db := newTestDB(t)
    repo := newSQLiteHistoryRepo(db)
    ctx := context.Background()

    seedArtist(t, db, "a1", "Test Artist")

    // The rule ran first, then the damage landed. Both orderings matter:
    // condition 4 requires damage.created_at >= rule_fix.created_at.
    seedChange(t, db, "f1", "a1", "rule_fix", "", "replaced junk biography",
        "rule:metadata_quality", "2026-05-01T10:00:00Z")
    seedChange(t, db, "d1", "a1", "biography", "curated bio", "junk bio",
        "manual", "2026-05-01T10:00:01Z")

    // PRECONDITION: the fixture really did seed both rows, or the assertion
    // below would pass vacuously against an empty table.
    var n int
    if err := db.QueryRowContext(ctx,
        `SELECT COUNT(*) FROM metadata_changes WHERE artist_id = 'a1'`).Scan(&n); err != nil {
        t.Fatalf("counting seeded rows: %v", err)
    }
    if n != 2 {
        t.Fatalf("fixture seeded %d rows, want 2", n)
    }

    got, err := repo.LockDamageCandidates(ctx)
    if err != nil {
        t.Fatalf("LockDamageCandidates: %v", err)
    }
    if len(got) != 1 {
        t.Fatalf("got %d candidates, want 1", len(got))
    }
    if got[0].Field != "biography" {
        t.Errorf("Field = %q, want %q", got[0].Field, "biography")
    }
    if got[0].OldValue != "curated bio" {
        t.Errorf("OldValue = %q, want %q", got[0].OldValue, "curated bio")
    }
    if got[0].RuleID != "metadata_quality" {
        t.Errorf("RuleID = %q, want %q", got[0].RuleID, "metadata_quality")
    }
}
```

Add the two fixture helpers in the same file:

```go
func seedArtist(t *testing.T, db *sql.DB, id, name string) {
    t.Helper()
    if _, err := db.Exec(
        `INSERT INTO artists (id, name, sort_name) VALUES (?, ?, ?)`,
        id, name, name); err != nil {
        t.Fatalf("seeding artist %s: %v", id, err)
    }
}

func seedChange(t *testing.T, db *sql.DB, id, artistID, field, oldV, newV, source, at string) {
    t.Helper()
    if _, err := db.Exec(
        `INSERT INTO metadata_changes (id, artist_id, field, old_value, new_value, source, created_at)
         VALUES (?, ?, ?, ?, ?, ?, ?)`,
        id, artistID, field, oldV, newV, source, at); err != nil {
        t.Fatalf("seeding change %s: %v", id, err)
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/artist/ -run TestLockDamageCandidates_JoinsAttributingRuleFixRow -v
```

Expected: FAIL, `repo.LockDamageCandidates undefined`.

- [ ] **Step 3: Write the type and interface method**

Create `internal/artist/lock_damage.go`:

```go
package artist

import (
    "context"
    "time"
)

// lock_damage.go -- candidate selection for the automated repair of locked
// fields a past rule run overwrote (#3038, the repair half of #3037).
//
// WHY THE ATTRIBUTION COMES FROM A JOIN AND NOT THE DAMAGE ROW'S OWN SOURCE.
// Stamping rule writes with a "rule:<id>" source landed in #3048, and the
// prevention chokepoint landed the same day. A damage row therefore carries a
// rule: source only if it was written inside that window, which no released
// build ever shipped. The durable trail is the rule_fix pseudo-field row that
// Pipeline.recordRuleFixHistory has written for every SUCCESSFUL auto-fix since
// #1106 (2026-04-27), which predates every release.
//
// THIS QUERY IS DELIBERATELY INCOMPLETE. It answers conditions 2, 3 (the join)
// and 4 of the spec's four-part allow-list. Conditions 1 (the field is
// CURRENTLY locked) and the capability half of 3 (the rule declares that field)
// are answered in Go by the caller, because locked_fields is a JSON array and
// rule.RuleFields is a static Go map -- re-implementing either in SQL is the
// drift that 024_retract_false_duplicate_passes.sql documents as the reason to
// avoid a migration. internal/artist must not import internal/rule, so the
// rule id crosses the boundary as data.

// LockDamageCandidate is one (artist, field) pair whose newest change reads as
// damage and which a rule fix on the same artist could have caused.
//
// It is a CANDIDATE, never a decision: the caller still applies the lock check
// and the rule-capability check before restoring anything.
type LockDamageCandidate struct {
    // ChangeID is the damage row's primary key.
    ChangeID string
    // ArtistID and ArtistName identify the affected artist.
    ArtistID   string
    ArtistName string
    // Field is the damaged metadata field.
    Field string
    // OldValue is the operator's value, and what a restore writes back.
    OldValue string
    // NewValue is what replaced it. Carried for the report, never restored.
    NewValue string
    // RuleID is the attributing rule, taken from the joined rule_fix row's
    // source with the "rule:" prefix removed. The caller resolves it against
    // rule.RuleFields; an id absent from the catalogue yields no fields and so
    // restores nothing.
    RuleID string
    // DamagedAt is the damage row's created_at.
    DamagedAt time.Time
    // FixedAt is the attributing rule_fix row's created_at. DamagedAt is never
    // earlier: the audit row is written in the same pass as the persist.
    FixedAt time.Time
}
```

In `internal/artist/history.go`, add to the `HistoryRepository` interface after
`CountNFOMBIDWrites`:

```go
    // LockDamageCandidates returns every (artist, field) pair whose newest
    // change reads as damage and which a rule fix on the same artist could
    // have caused. Read-only, and a candidate list rather than a decision:
    // see LockDamageCandidate.
    LockDamageCandidates(ctx context.Context) ([]LockDamageCandidate, error)
```

- [ ] **Step 4: Write the SQL implementation**

Append to `internal/artist/sqlite_history.go`:

```go
// lockDamageQuery selects candidate damage joined to its attributing rule_fix
// row.
//
// The ranking CTE and the damage predicate are REUSED VERBATIM. Both are shared
// with the blast-radius report, and blastRadiusRankedCTE's own header explains
// why no damage or source predicate may move inside the frame: it would promote
// an old damage row to rank 1 and report a recovered field as broken forever.
// The self-join below is applied in the OUTER select for that reason.
//
// The join is capability-blind on purpose. It proves only that a rule ran on
// this artist before the damage; whether that rule can write this field is
// decided in Go against rule.RuleFields.
const lockDamageQuery = blastRadiusRankedCTE + `
    SELECT r.id, r.artist_id, r.artist_name, r.field, r.old_value, r.new_value,
           SUBSTR(fix.source, 6) AS rule_id, r.created_at, fix.created_at
    FROM ranked r
    JOIN metadata_changes fix
      ON fix.artist_id = r.artist_id
     AND fix.field = 'rule_fix'
     AND fix.source LIKE 'rule:%'
     AND fix.created_at <= r.created_at
    %s
    ORDER BY r.created_at DESC, r.id DESC`

func (r *sqliteHistoryRepo) LockDamageCandidates(ctx context.Context) ([]LockDamageCandidate, error) {
    //nolint:gosec // G202: both fragments are server-built constants. The CTE
    // takes no filter here (empty string) and the damage clause is composed
    // from validated constants, so no caller-supplied text reaches the query.
    q := fmt.Sprintf(lockDamageQuery, "",
        blastRadiusDamageWhere(BlastScopeAll, ""))

    rows, err := r.db.QueryContext(ctx, q)
    if err != nil {
        return nil, fmt.Errorf("querying locked-field damage candidates: %w", err)
    }
    defer func() { _ = rows.Close() }()

    out := make([]LockDamageCandidate, 0)
    for rows.Next() {
        var c LockDamageCandidate
        var damagedAt, fixedAt string
        if err := rows.Scan(&c.ChangeID, &c.ArtistID, &c.ArtistName, &c.Field,
            &c.OldValue, &c.NewValue, &c.RuleID, &damagedAt, &fixedAt); err != nil {
            return nil, fmt.Errorf("scanning locked-field damage candidate: %w", err)
        }
        c.DamagedAt = parseHistoryTimestamp(c.ChangeID, damagedAt)
        c.FixedAt = parseHistoryTimestamp(c.ChangeID, fixedAt)
        out = append(out, c)
    }
    if err := rows.Err(); err != nil {
        return nil, fmt.Errorf("iterating locked-field damage candidates: %w", err)
    }
    return out, nil
}
```

NOTE FOR THE IMPLEMENTER: `lockDamageQuery` has TWO `%s` verbs (the CTE's own
filter slot at `sqlite_history.go:435`, then the damage WHERE), so the
`fmt.Sprintf` above passes both in that order. `parseHistoryTimestamp` is the
existing decoder at `sqlite_history.go:815`; it takes `(changeID, raw)` and
falls back from RFC3339 to `time.DateTime`. Use it. Do not add a second time
parser.

- [ ] **Step 5: Run test to verify it passes**

```bash
go test ./internal/artist/ -run TestLockDamageCandidates -v
```

Expected: PASS.

- [ ] **Step 6: Add the ordering and exclusion tests**

```go
func TestLockDamageCandidates_ExcludesDamageOlderThanTheRuleFix(t *testing.T) {
    db := newTestDB(t)
    repo := newSQLiteHistoryRepo(db)

    seedArtist(t, db, "a1", "Test Artist")
    // Damage FIRST, rule fix after: the rule cannot have caused it.
    seedChange(t, db, "d1", "a1", "biography", "curated bio", "junk bio",
        "manual", "2026-05-01T10:00:00Z")
    seedChange(t, db, "f1", "a1", "rule_fix", "", "replaced junk biography",
        "rule:metadata_quality", "2026-05-01T10:00:01Z")

    got, err := repo.LockDamageCandidates(context.Background())
    if err != nil {
        t.Fatalf("LockDamageCandidates: %v", err)
    }
    if len(got) != 0 {
        t.Fatalf("got %d candidates, want 0 (damage predates the rule fix)", len(got))
    }
}

func TestLockDamageCandidates_ExcludesAlreadyRestored(t *testing.T) {
    db := newTestDB(t)
    repo := newSQLiteHistoryRepo(db)

    seedArtist(t, db, "a1", "Test Artist")
    seedChange(t, db, "f1", "a1", "rule_fix", "", "replaced junk biography",
        "rule:metadata_quality", "2026-05-01T10:00:00Z")
    seedChange(t, db, "d1", "a1", "biography", "curated bio", "junk bio",
        "manual", "2026-05-01T10:00:01Z")
    // The restore. Its source is what drops the pair from the damage predicate.
    seedChange(t, db, "r1", "a1", "biography", "junk bio", "curated bio",
        "revert", "2026-05-01T10:00:02Z")

    got, err := repo.LockDamageCandidates(context.Background())
    if err != nil {
        t.Fatalf("LockDamageCandidates: %v", err)
    }
    if len(got) != 0 {
        t.Fatalf("got %d candidates, want 0 (already restored)", len(got))
    }
}
```

The second test is the replay-trap guard from #2750: a revert row has a
non-empty old_value and a different non-empty new_value, so it resembles damage,
and only the `source != 'revert'` exclusion stops a revert-of-a-revert.

- [ ] **Step 7: Run the full package**

```bash
go test ./internal/artist/ -run TestLockDamageCandidates -v
```

Expected: 3 tests PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/artist/lock_damage.go internal/artist/lock_damage_test.go \
        internal/artist/history.go internal/artist/sqlite_history.go
git commit -m "feat(artist): select locked-field damage candidates attributable to a rule fix"
```

---

### Task 2: The repair one-shot

**Files:**
- Create: `internal/maintenance/lock_damage_repair.go`
- Test: `internal/maintenance/lock_damage_repair_test.go`

**Interfaces:**
- Consumes: `artist.LockDamageCandidate`, `LockDamageCandidates` (Task 1);
  `rule.RuleFields(id string) []string`;
  `(*artist.Service).UpdateField(ctx, id, field, value string) (bool, error)`;
  `(*artist.Service).GetByID` via the repository;
  `artist.ContextWithSource(ctx, "revert")`.
- Produces:
  ```go
  type LockDamageResult struct {
      Restored      []LockDamageRestore
      Unrecoverable []LockDamageSkip
      Failed        []LockDamageSkip
  }
  func (s *Service) RepairLockDamage(ctx context.Context, opts LockDamageOpts) (*LockDamageResult, error)
  type LockDamageOpts struct{ DryRun bool }
  ```

- [ ] **Step 1: Write the positive control PAIR**

This is the most important test in the plan. The negative half is what proves
the predicate is not matching everything.

```go
func TestRepairLockDamage_RestoresLockedNotUnlocked(t *testing.T) {
    env := newLockDamageEnv(t)

    // Two artists, identical damage from the same rule. Only the lock differs.
    env.seedArtist("a1", "Locked Artist", []string{"biography"})
    env.seedArtist("a2", "Unlocked Artist", nil)
    for _, id := range []string{"a1", "a2"} {
        env.seedRuleFix(id, "metadata_quality", "2026-05-01T10:00:00Z")
        env.seedDamage(id, "biography", "curated bio", "junk bio",
            "2026-05-01T10:00:01Z")
    }

    // PRECONDITIONS. Without these the assertions can pass for the wrong
    // reason: an unseeded lock makes the negative half trivially true.
    env.requireLocked("a1", "biography")
    env.requireNotLocked("a2", "biography")

    res, err := env.svc.RepairLockDamage(context.Background(), LockDamageOpts{})
    if err != nil {
        t.Fatalf("RepairLockDamage: %v", err)
    }

    if len(res.Restored) != 1 {
        t.Fatalf("restored %d pairs, want exactly 1", len(res.Restored))
    }
    if res.Restored[0].ArtistID != "a1" {
        t.Errorf("restored artist = %q, want a1", res.Restored[0].ArtistID)
    }
    if got := env.biography("a1"); got != "curated bio" {
        t.Errorf("a1 biography = %q, want the restored value", got)
    }
    if got := env.biography("a2"); got != "junk bio" {
        t.Errorf("a2 biography = %q, want it left damaged", got)
    }
}
```

- [ ] **Step 2: Write the attribution control**

```go
func TestRepairLockDamage_SkipsDamageWithNoAttributingRuleFix(t *testing.T) {
    env := newLockDamageEnv(t)

    env.seedArtist("a1", "Locked Artist", []string{"biography"})
    // Damage, but NO rule_fix row: indistinguishable from an operator edit.
    env.seedDamage("a1", "biography", "curated bio", "something else",
        "2026-05-01T10:00:01Z")

    env.requireLocked("a1", "biography")

    res, err := env.svc.RepairLockDamage(context.Background(), LockDamageOpts{})
    if err != nil {
        t.Fatalf("RepairLockDamage: %v", err)
    }

    if len(res.Restored) != 0 {
        t.Fatalf("restored %d pairs, want 0 (no attribution)", len(res.Restored))
    }
    if got := env.biography("a1"); got != "something else" {
        t.Errorf("biography = %q, want it untouched", got)
    }
}
```

This test pins the deliberate decision NOT to widen to unattributed damage. A
future change that drops the rule_fix join to "repair more" must fail here.

- [ ] **Step 3: Write the capability control**

```go
func TestRepairLockDamage_SkipsFieldTheRuleCannotWrite(t *testing.T) {
    env := newLockDamageEnv(t)

    env.seedArtist("a1", "Locked Artist", []string{"biography"})
    // origin_missing declares "origin", never "biography".
    env.seedRuleFix("a1", "origin_missing", "2026-05-01T10:00:00Z")
    env.seedDamage("a1", "biography", "curated bio", "junk bio",
        "2026-05-01T10:00:01Z")

    env.requireLocked("a1", "biography")

    res, err := env.svc.RepairLockDamage(context.Background(), LockDamageOpts{})
    if err != nil {
        t.Fatalf("RepairLockDamage: %v", err)
    }
    if len(res.Restored) != 0 {
        t.Fatalf("restored %d pairs, want 0 (rule cannot write this field)", len(res.Restored))
    }
}
```

- [ ] **Step 4: Run all three to verify they fail**

```bash
go test ./internal/maintenance/ -run TestRepairLockDamage -v
```

Expected: FAIL, `env.svc.RepairLockDamage undefined`.

- [ ] **Step 5: Implement the one-shot**

Create `internal/maintenance/lock_damage_repair.go`:

```go
package maintenance

import (
    "context"
    "fmt"
    "log/slog"
    "slices"
    "time"

    "github.com/sydlexius/stillwater/internal/artist"
    "github.com/sydlexius/stillwater/internal/rule"
)

// lock_damage_repair.go -- the repair half of #3037 (#3038).
//
// Restores locked fields a past rule run overwrote. Prevention now holds at the
// persist chokepoint, but prevention does nothing for an artist already
// damaged.
//
// ROW SELECTION IS A POSITIVE ALLOW-LIST, in four parts. The query answers
// conditions 2, 3 (a rule ran on this artist before the damage) and 4; this
// file answers 1 and the capability half of 3:
//
//  1. the field is CURRENTLY in the artist's locked_fields
//  2. the row is the newest change for its (artist, field) pair AND reads as damage
//  3. an attributing rule_fix row exists whose rule DECLARES that field
//  4. the damage is not older than that rule_fix row
//
// Anything unrecognized -- an unknown rule id, a field absent from the
// catalogue -- falls through to NOT restored and is counted as unrecoverable. A
// predicate safe for deciding whether to WRITE turns destructive when inverted
// to decide what to overwrite, which is why this direction is not negotiable.

// LockDamageRestore records one repaired (artist, field) pair.
type LockDamageRestore struct {
    ArtistID   string
    ArtistName string
    Field      string
    RuleID     string
    DamagedAt  time.Time
}

// LockDamageSkip records a pair that was NOT repaired, and why. Reason is a
// hand-authored literal and never carries a field value.
type LockDamageSkip struct {
    ArtistID string
    Field    string
    RuleID   string
    Reason   string
}

// LockDamageResult reports what a pass did. Unrecoverable is as load-bearing as
// Restored: a run that repairs nothing and says so is correct, but a run
// reporting zero because the mechanism cannot SEE the damage is the "unknown
// rendered as clean" defect the blast-radius work exists to prevent.
type LockDamageResult struct {
    Restored      []LockDamageRestore
    Unrecoverable []LockDamageSkip
    Failed        []LockDamageSkip
}

// LockDamageOpts controls a pass.
type LockDamageOpts struct {
    // DryRun selects and reports without writing. Used for the production-clone
    // validation described in the design doc.
    DryRun bool
}

// RepairLockDamage restores every locked field a rule overwrote.
//
// Each pair is restored INDEPENDENTLY through artist.Service.UpdateField. That
// verb is deliberately lock-blind (see the comment block in
// internal/artist/lockguard.go): the operator's history revert and blast-radius
// restore use it for exactly this reason. Routing a restore through
// Service.Update instead would hit enforceLocksBeforeUpdate and have the
// restore reverted by the very guard that stopped the damage.
//
// The write records source="revert", which is also what makes a second pass a
// no-op: the revert row becomes the newest row for the pair, so the damage
// predicate stops matching it. The settings flag is an optimization; this
// convergence is the real idempotence.
func (s *Service) RepairLockDamage(ctx context.Context, opts LockDamageOpts) (*LockDamageResult, error) {
    candidates, err := s.history.LockDamageCandidates(ctx)
    if err != nil {
        return nil, fmt.Errorf("selecting locked-field damage candidates: %w", err)
    }

    res := &LockDamageResult{}
    for i := range candidates {
        c := candidates[i]

        // Condition 3, capability half. An unknown rule id yields no fields, so
        // an unrecognized value restores nothing rather than everything.
        if !slices.Contains(rule.RuleFields(c.RuleID), c.Field) {
            res.Unrecoverable = append(res.Unrecoverable, LockDamageSkip{
                ArtistID: c.ArtistID, Field: c.Field, RuleID: c.RuleID,
                Reason: "the attributing rule does not write this field",
            })
            continue
        }

        // Condition 1. Read the STORED artist, never a cached struct: the lock
        // set is the operator's current intent, not what it was at damage time.
        a, err := s.artists.GetByID(ctx, c.ArtistID)
        if err != nil {
            res.Failed = append(res.Failed, LockDamageSkip{
                ArtistID: c.ArtistID, Field: c.Field, RuleID: c.RuleID,
                Reason: "could not read the artist",
            })
            continue
        }
        if !s.artistService.IsFieldLocked(a, artist.FieldName(c.Field)) {
            res.Unrecoverable = append(res.Unrecoverable, LockDamageSkip{
                ArtistID: c.ArtistID, Field: c.Field, RuleID: c.RuleID,
                Reason: "the field is not currently locked",
            })
            continue
        }

        if opts.DryRun {
            res.Restored = append(res.Restored, LockDamageRestore{
                ArtistID: c.ArtistID, ArtistName: c.ArtistName,
                Field: c.Field, RuleID: c.RuleID, DamagedAt: c.DamagedAt,
            })
            continue
        }

        // A failure here is isolated: it is counted and the loop continues, so
        // one bad row never aborts the pass.
        writeCtx := artist.ContextWithSource(ctx, "revert")
        if _, err := s.artistService.UpdateField(writeCtx, c.ArtistID, c.Field, c.OldValue); err != nil {
            res.Failed = append(res.Failed, LockDamageSkip{
                ArtistID: c.ArtistID, Field: c.Field, RuleID: c.RuleID,
                Reason: "the restore write failed",
            })
            continue
        }

        // Values are never logged: an old biography is user library content.
        s.logger.Info("restored a locked field a rule had overwritten",
            slog.String("artist_id", c.ArtistID),
            slog.String("field", c.Field),
            slog.String("rule_id", c.RuleID),
            slog.Time("damaged_at", c.DamagedAt),
            slog.Time("rule_ran_at", c.FixedAt))

        res.Restored = append(res.Restored, LockDamageRestore{
            ArtistID: c.ArtistID, ArtistName: c.ArtistName,
            Field: c.Field, RuleID: c.RuleID, DamagedAt: c.DamagedAt,
        })
    }
    return res, nil
}
```

NOTE FOR THE IMPLEMENTER: `maintenance.Service` currently holds only
`db`, `dbPath`, `imageCacheDir`, `logger` (`internal/maintenance/maintenance.go:41`).
This task adds `history` (an `artist.HistoryRepository`), `artists` (an
`artist.Repository`) and `artistService` (`*artist.Service`). Add them to the
struct and to `NewService`, and update every existing `NewService` call site
(`cmd/stillwater/main.go:646` and the maintenance tests). If that widening makes
`NewService` unwieldy, an options struct is acceptable, but do NOT change the
existing parameters' meaning.

- [ ] **Step 6: Write the test env helper**

In `internal/maintenance/lock_damage_repair_test.go`:

```go
type lockDamageEnv struct {
    t   *testing.T
    db  *sql.DB
    svc *Service
}

func newLockDamageEnv(t *testing.T) *lockDamageEnv {
    t.Helper()
    db, dbPath := setupTestDB(t)
    // Wire the same real services the startup path uses: these tests assert
    // against stored rows, so a fake repository would prove nothing about the
    // SQL in Task 1.
    histRepo := artist.NewHistoryRepoForTest(db) // see note below
    artistRepo := artist.NewSQLiteRepositoryForTest(db)
    artistSvc := artist.NewService(artistRepo /* ...existing deps... */)
    svc := NewService(db, dbPath, t.TempDir(), slog.Default(),
        histRepo, artistRepo, artistSvc)
    return &lockDamageEnv{t: t, db: db, svc: svc}
}

func (e *lockDamageEnv) seedArtist(id, name string, locked []string) {
    e.t.Helper()
    lf := "[]"
    if len(locked) > 0 {
        b, err := json.Marshal(locked)
        if err != nil {
            e.t.Fatalf("marshalling locked fields: %v", err)
        }
        lf = string(b)
    }
    if _, err := e.db.Exec(
        `INSERT INTO artists (id, name, sort_name, biography, locked_fields)
         VALUES (?, ?, ?, ?, ?)`,
        id, name, name, "junk bio", lf); err != nil {
        e.t.Fatalf("seeding artist %s: %v", id, err)
    }
}

func (e *lockDamageEnv) seedRuleFix(artistID, ruleID, at string) {
    e.t.Helper()
    e.insertChange(artistID+"-fix-"+ruleID, artistID, "rule_fix", "",
        "fixed something", "rule:"+ruleID, at)
}

func (e *lockDamageEnv) seedDamage(artistID, field, oldV, newV, at string) {
    e.t.Helper()
    e.insertChange(artistID+"-dmg-"+field, artistID, field, oldV, newV,
        "manual", at)
}

func (e *lockDamageEnv) insertChange(id, artistID, field, oldV, newV, source, at string) {
    e.t.Helper()
    if _, err := e.db.Exec(
        `INSERT INTO metadata_changes (id, artist_id, field, old_value, new_value, source, created_at)
         VALUES (?, ?, ?, ?, ?, ?, ?)`,
        id, artistID, field, oldV, newV, source, at); err != nil {
        e.t.Fatalf("seeding change %s: %v", id, err)
    }
}

// requireLocked asserts the fixture's defining property. A test whose lock was
// never seeded passes vacuously.
func (e *lockDamageEnv) requireLocked(artistID, field string) {
    e.t.Helper()
    if !e.lockedFields(artistID)[field] {
        e.t.Fatalf("fixture: %s is not locked on %s", field, artistID)
    }
}

func (e *lockDamageEnv) requireNotLocked(artistID, field string) {
    e.t.Helper()
    if e.lockedFields(artistID)[field] {
        e.t.Fatalf("fixture: %s is unexpectedly locked on %s", field, artistID)
    }
}

func (e *lockDamageEnv) lockedFields(artistID string) map[string]bool {
    e.t.Helper()
    var raw string
    if err := e.db.QueryRow(
        `SELECT locked_fields FROM artists WHERE id = ?`, artistID).Scan(&raw); err != nil {
        e.t.Fatalf("reading locked_fields for %s: %v", artistID, err)
    }
    var fields []string
    if err := json.Unmarshal([]byte(raw), &fields); err != nil {
        e.t.Fatalf("parsing locked_fields for %s: %v", artistID, err)
    }
    out := make(map[string]bool, len(fields))
    for _, f := range fields {
        out[f] = true
    }
    return out
}

func (e *lockDamageEnv) biography(artistID string) string {
    e.t.Helper()
    var bio string
    if err := e.db.QueryRow(
        `SELECT biography FROM artists WHERE id = ?`, artistID).Scan(&bio); err != nil {
        e.t.Fatalf("reading biography for %s: %v", artistID, err)
    }
    return bio
}
```

NOTE FOR THE IMPLEMENTER: `NewHistoryRepoForTest` / `NewSQLiteRepositoryForTest`
are PLACEHOLDER NAMES for whatever constructors `internal/artist` already
exports. `newSQLiteHistoryRepo` is unexported, so from `internal/maintenance`
use `artist.NewHistoryService(db)` and whatever exported repository constructor
exists (grep `func New.*Repository` in `internal/artist`). If the maintenance
service needs the repository interface rather than the service, thread it
through `NewService` rather than exporting new test-only constructors.

- [ ] **Step 7: Run all three controls**

```bash
go test ./internal/maintenance/ -run TestRepairLockDamage -v
```

Expected: 3 PASS.

- [ ] **Step 8: Mutation-proof the two controls**

Run each mutation, confirm the named test FAILS, then REVERT it. A mutation that
leaves the suite green means that control is decorative.

```bash
# Mutation A: delete the lock check (the `if !s.artistService.IsFieldLocked` block).
go test ./internal/maintenance/ -run TestRepairLockDamage_RestoresLockedNotUnlocked -v
# Expected: FAIL (a2 gets restored).

# Mutation B: delete the RuleFields capability check.
go test ./internal/maintenance/ -run TestRepairLockDamage_SkipsFieldTheRuleCannotWrite -v
# Expected: FAIL.
```

Record both outcomes in the commit message. If either PASSES under mutation,
STOP and fix the test before continuing.

- [ ] **Step 9: Commit**

```bash
git add internal/maintenance/lock_damage_repair.go \
        internal/maintenance/lock_damage_repair_test.go \
        internal/maintenance/maintenance.go
git commit -m "feat(maintenance): restore locked fields a past rule run overwrote"
```

---

### Task 3: Idempotence and the operator-edit guard

**Files:**
- Test: `internal/maintenance/lock_damage_repair_test.go` (extend)

**Interfaces:**
- Consumes: everything from Task 2. No new production code unless a test fails.

- [ ] **Step 1: Write the second-pass test**

```go
func TestRepairLockDamage_SecondPassRestoresNothing(t *testing.T) {
    env := newLockDamageEnv(t)
    env.seedArtist("a1", "Locked Artist", []string{"biography"})
    env.seedRuleFix("a1", "metadata_quality", "2026-05-01T10:00:00Z")
    env.seedDamage("a1", "biography", "curated bio", "junk bio",
        "2026-05-01T10:00:01Z")
    env.requireLocked("a1", "biography")

    first, err := env.svc.RepairLockDamage(context.Background(), LockDamageOpts{})
    if err != nil {
        t.Fatalf("first pass: %v", err)
    }
    if len(first.Restored) != 1 {
        t.Fatalf("first pass restored %d, want 1", len(first.Restored))
    }

    // NO settings flag is consulted here. This asserts the QUERY converged --
    // the restore's source="revert" row is now newest for the pair, so the
    // damage predicate stops matching. A test that relied on the flag would
    // pass even if the predicate had not converged.
    second, err := env.svc.RepairLockDamage(context.Background(), LockDamageOpts{})
    if err != nil {
        t.Fatalf("second pass: %v", err)
    }
    if len(second.Restored) != 0 {
        t.Fatalf("second pass restored %d, want 0", len(second.Restored))
    }
    if got := env.biography("a1"); got != "curated bio" {
        t.Errorf("biography = %q after two passes, want the restored value once", got)
    }
}
```

- [ ] **Step 2: Write the operator-edit guard test**

```go
func TestRepairLockDamage_OperatorEditAfterDamageBlocksRestore(t *testing.T) {
    env := newLockDamageEnv(t)
    env.seedArtist("a1", "Locked Artist", []string{"biography"})
    env.seedRuleFix("a1", "metadata_quality", "2026-05-01T10:00:00Z")
    env.seedDamage("a1", "biography", "curated bio", "junk bio",
        "2026-05-01T10:00:01Z")
    // The operator then wrote their own value. It is now the newest row for the
    // pair, so the damage row is no longer rank 1 and must not be restored over.
    env.insertChange("a1-edit", "a1", "biography", "junk bio", "operator value",
        "manual", "2026-05-01T11:00:00Z")

    env.requireLocked("a1", "biography")

    res, err := env.svc.RepairLockDamage(context.Background(), LockDamageOpts{})
    if err != nil {
        t.Fatalf("RepairLockDamage: %v", err)
    }
    if len(res.Restored) != 0 {
        t.Fatalf("restored %d pairs, want 0 (operator edited after the damage)", len(res.Restored))
    }
}
```

- [ ] **Step 3: Write the unrecoverable-tally test**

```go
func TestRepairLockDamage_ReportsUnrecoverableRatherThanSilentZero(t *testing.T) {
    env := newLockDamageEnv(t)
    env.seedArtist("a1", "Locked Artist", []string{"biography"})
    // origin_missing cannot write biography, so this is unrecoverable, not absent.
    env.seedRuleFix("a1", "origin_missing", "2026-05-01T10:00:00Z")
    env.seedDamage("a1", "biography", "curated bio", "junk bio",
        "2026-05-01T10:00:01Z")

    res, err := env.svc.RepairLockDamage(context.Background(), LockDamageOpts{})
    if err != nil {
        t.Fatalf("RepairLockDamage: %v", err)
    }
    if len(res.Restored) != 0 {
        t.Fatalf("restored %d, want 0", len(res.Restored))
    }
    if len(res.Unrecoverable) != 1 {
        t.Fatalf("unrecoverable = %d, want 1 (a silent zero hides the damage)",
            len(res.Unrecoverable))
    }
    if res.Unrecoverable[0].Field != "biography" {
        t.Errorf("unrecoverable field = %q, want biography", res.Unrecoverable[0].Field)
    }
}
```

- [ ] **Step 4: Run the whole file**

```bash
go test ./internal/maintenance/ -run TestRepairLockDamage -v
```

Expected: 6 PASS.

- [ ] **Step 5: Run the race detector on both packages**

```bash
. scripts/lib/run-paths.sh
go test -race -count=1 ./internal/artist/ ./internal/maintenance/ 2>&1 | tee "$SW_RUN_DIR/lockdamage-race.log"
grep -nE 'WARNING: DATA RACE|--- FAIL' "$SW_RUN_DIR/lockdamage-race.log"
```

Expected: no matches. Capture once, grep the file; never re-run a long suite to
re-filter it.

- [ ] **Step 6: Commit**

```bash
git add internal/maintenance/lock_damage_repair_test.go
git commit -m "test(maintenance): pin idempotence, the operator-edit guard, and the unrecoverable tally"
```

---

### Task 4: Startup wiring

**Files:**
- Modify: `cmd/stillwater/main.go` (the startup block near the maintenance
  scheduler, around line 1304)

**Interfaces:**
- Consumes: `(*maintenance.Service).RepairLockDamage` (Task 2),
  `getDBStringSetting` (`cmd/stillwater/main.go:2262`).
- Produces: nothing other packages consume.

- [ ] **Step 1: Add the guarded startup call**

Insert after the maintenance scheduler block:

```go
    // One-shot repair of locked fields a past rule run overwrote (#3038).
    //
    // The settings key is written only AFTER a successful pass, so a crash
    // mid-run retries next boot rather than being permanently skipped. It is an
    // optimization only: a restore writes a source="revert" row that drops the
    // pair from the damage query, so a second pass selects nothing on its own
    // merits even with the key absent.
    if getDBStringSetting(ctx, db, lockDamageRepairKey, "") == "" {
        go func() {
            defer func() {
                if r := recover(); r != nil {
                    logger.Error("locked-field damage repair panicked", "panic", r)
                }
            }()
            res, err := a.maintenanceService.RepairLockDamage(ctx, maintenance.LockDamageOpts{})
            if err != nil {
                logger.Error("locked-field damage repair failed; will retry next start",
                    "error", err)
                return
            }
            logger.Info("locked-field damage repair complete",
                "restored", len(res.Restored),
                "unrecoverable", len(res.Unrecoverable),
                "failed", len(res.Failed))
            if _, err := db.ExecContext(ctx,
                `INSERT INTO settings (key, value) VALUES (?, ?)
                 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
                lockDamageRepairKey, time.Now().UTC().Format(time.RFC3339)); err != nil {
                logger.Error("recording locked-field damage repair completion", "error", err)
            }
        }()
    }
```

Add the key constant near the other settings keys:

```go
// lockDamageRepairKey guards the one-shot locked-field damage repair (#3038).
// Its VALUE is the completion timestamp; only its presence is consulted.
const lockDamageRepairKey = "lock_damage_repair.completed_at"
```

- [ ] **Step 2: Verify it compiles and vet is clean**

```bash
go build ./... && go vet ./...
```

Expected: no output.

- [ ] **Step 3: Run the app and confirm the one-shot fires once**

```bash
bash scripts/dev-restart.sh
```

Expected in the log: one `locked-field damage repair complete` line on the first
start, and NONE on a second restart. Confirm with a second
`bash scripts/dev-restart.sh`.

- [ ] **Step 4: Commit**

```bash
git add cmd/stillwater/main.go
git commit -m "feat(startup): run the locked-field damage repair once per database"
```

---

### Task 5: Docs and the gate

**Files:**
- Modify: `docs/site/src/core-concepts/field-locks.md`

**Interfaces:** none.

- [ ] **Step 1: Document the repair**

Add a section to the field-locks page stating: what the repair restores
(biography and origin damaged by a rule while the field was locked), what it
CANNOT (`musicbrainz_id` and the provider IDs, never tracked; `name` /
`sort_name` damaged before #3037), that it runs once per database at startup,
and that restores appear in Recent Activity and the blast-radius pane.

Every claim must be backed by code. Do NOT state that the repair covers damage
it cannot see: that is the "unknown rendered as clean" defect the whole feature
exists to avoid.

- [ ] **Step 2: Run the full gate**

```bash
bash scripts/pre-push-gate.sh 2>&1 | tee /tmp/lockdamage-gate.log
```

Expected: exit 0, "All hard checks passed", and patch coverage at or above the
floor. Every check in the default path is BLOCKING; do not argue one down.

- [ ] **Step 3: Commit**

```bash
git add docs/site/src/core-concepts/field-locks.md
git commit -m "docs(field-locks): state what the automated repair does and does not restore"
```

---

## Production-clone validation

REQUIRED before the implementation PR opens. Synthetic fixtures only prove the
predicate does what the author THINKS damage looks like.

- [ ] Copy the production database and its `-wal` / `-shm` siblings to a scratch
      path. NEVER open the live file read-write: SQLite recovery on an unclean
      copy can mutate the file it opens.
- [ ] Point `SW_DB_PATH` at the copy.
- [ ] Run with `LockDamageOpts{DryRun: true}` and inspect the candidate list by
      hand.
- [ ] Confirm no selected row looks like an operator edit. This is the one
      failure mode synthetic controls cannot surface, since they contain only
      damage the author constructed.
- [ ] Then run the write pass against the clone and diff the result.

THE CLONE IS PRIVATE MEDIA-LIBRARY METADATA. Artist names, biographies, and
titles from it MUST NOT reach any outward surface: no GitHub issue, PR body,
commit message, review comment, or log excerpt pasted into a bot thread. Row
counts, field names and rule IDs are fine. Local scratch files may hold real
identifiers; the shared surface must be generic.

A predicate that selects a surprising row is a DESIGN finding, not a tuning
problem: stop and revisit the spec.

---

## Self-review notes

Checked against the spec:

- Conditions 1-4 each have an implementing task and a test that fails without it
  (Tasks 1-3).
- The seven acceptance criteria in #3038 map to: Task 2 Step 1 (automatic
  restore), Task 2 Steps 1-3 (allow-list with each condition asserted), Task 1
  Step 6 (revert does not revert itself), Task 2 Step 1 (locked/unlocked
  control pair), Task 3 Step 2 (operator edit blocks), Task 3 Step 1 (clean
  second pass), Task 3 Step 3 (unrecoverable tally).
- Two placeholder names are flagged inline as IMPLEMENTER NOTES rather than left
  silent (`NewHistoryRepoForTest`, `parseHistoryTime`); both are "use the
  existing one, do not add a second" instructions.
- `maintenance.Service` widening is called out in Task 2 Step 5 because it
  touches existing call sites.
