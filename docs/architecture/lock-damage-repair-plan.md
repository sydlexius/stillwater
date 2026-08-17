# Locked-Field Damage Repair Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Automatically restore locked artist fields that past rule runs
overwrote, once per database, at startup.

**Architecture:** A read-only query on `HistoryRepository` selects damage rows
whose OWN `source` names the rule that wrote them (`rule:<id>`, #3048), reusing
the existing blast-radius ranking CTE and damage predicate. A maintenance
one-shot applies the two Go-side conditions (the field is currently locked; the
naming rule declares that field) and restores each surviving pair through
`Service.UpdateField` with `source="revert"`.

**Scope, corrected in review (#3074):** attribution comes from the damage row's
own source, NOT from a join to a `rule_fix` row on the same artist. That join
proved only that a rule ran on the artist at some point and would have restored
over an operator's own later edit. The cost is that this repairs only damage
written by a build carrying #3048 -- no released build -- so it is a
forward-looking safety net rather than a recovery for historical loss. See the
design doc's "The join alone is NOT a causal link".

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
| `internal/artist/sqlite_history.go` (modify) | The SQL implementation, reusing `blastRadiusRankedCTE` plus a `source LIKE 'rule:%'` test |
| `internal/artist/history.go:55-87` (modify) | Add the method to the `HistoryRepository` interface |
| `internal/maintenance/lock_damage_repair.go` (create) | The one-shot: Go-side conditions, the restore loop, the report |
| `cmd/stillwater/main.go` (modify) | Startup wiring guarded by the settings key, plus the `-lock-damage-dry-run` entry point |
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
      RuleID     string    // parsed from the damage row's OWN source
      DamagedAt  time.Time
  }

  // On HistoryRepository:
  LockDamageCandidates(ctx context.Context) ([]LockDamageCandidate, error)
  ```

- [ ] **Step 1: Write the failing test**

In `internal/artist/lock_damage_test.go`:

```go
func TestLockDamageCandidates_SelectsRuleSourcedDamage(t *testing.T) {
    db := newTestDB(t)
    repo := newSQLiteHistoryRepo(db)
    ctx := context.Background()

    seedArtist(t, db, "a1", "Test Artist")

    // The damage row NAMES the rule that wrote it. That is the whole
    // attribution: no rule_fix row is seeded, because none is consulted.
    seedChange(t, db, "d1", "a1", "biography", "curated bio", "junk bio",
        "rule:metadata_quality", "2026-05-01T10:00:01Z")

    // PRECONDITIONS. Assert the fixture's DEFINING properties, not just that
    // rows exist: a seeded row with the wrong source would make the assertion
    // below pass for the wrong reason.
    requireChange(t, db, "d1", map[string]string{
        "field":     "biography",
        "source":    "rule:metadata_quality",
        "old_value": "curated bio",
        "new_value": "junk bio",
    })

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

// requireChange asserts a seeded row's DEFINING columns. Every fixture in this
// file calls it before the assertion that matters: a row seeded with the wrong
// source or field makes the candidate assertion pass or fail for a reason that
// has nothing to do with the code under test.
func requireChange(t *testing.T, db *sql.DB, id string, want map[string]string) {
    t.Helper()
    for col, expect := range want {
        var got string
        // #nosec G202 -- col comes from this file's own literal maps, never input.
        q := "SELECT " + col + " FROM metadata_changes WHERE id = ?"
        if err := db.QueryRow(q, id).Scan(&got); err != nil {
            t.Fatalf("fixture: reading %s of change %s: %v", col, id, err)
        }
        if got != expect {
            t.Fatalf("fixture: change %s has %s = %q, want %q", id, col, got, expect)
        }
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/artist/ -run TestLockDamageCandidates_SelectsRuleSourcedDamage -v
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
// WHY ATTRIBUTION IS THE DAMAGE ROW'S OWN SOURCE (#3074 review).
// An earlier design joined each damage row to any earlier rule_fix row on the
// same artist. That proves a rule ran on the artist AT SOME POINT, never that it
// caused THIS row, so an operator's own edit made after any rule run matched the
// join and would have been restored over -- destroying the operator data the
// lock exists to protect. The per-row source is the only key that separates a
// rule write from an operator edit: persistHealthAfterRun stamps it via
// withRuleHistorySource (internal/rule/fixer.go:2438).
//
// The accepted cost is coverage. A damage row carries a rule: source only on a
// build shipping #3048 (fdeb1b6f), which no release does, so this repairs
// nothing on existing databases and exists to catch a FUTURE write path that
// escapes the chokepoint. Pre-#3048 damage is reported as unrecoverable.
//
// THIS QUERY IS DELIBERATELY INCOMPLETE. It answers conditions 2 and 3 of the
// spec's four-part allow-list. Conditions 1 (the field is CURRENTLY locked) and
// 4 (the naming rule declares that field) are answered in Go by the caller,
// because locked_fields is a JSON array and
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
    // RuleID is the attributing rule, taken from THIS ROW'S OWN source with the
    // "rule:" prefix removed. The caller resolves it against
    // rule.RuleFields; an id absent from the catalogue yields no fields and so
    // restores nothing.
    RuleID string
    // DamagedAt is the damage row's created_at.
    DamagedAt time.Time
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
// lockDamageQuery selects damage rows that SAY THEY WERE WRITTEN BY A RULE.
//
// NO JOIN TO rule_fix, DELIBERATELY (#3074 review). An earlier design joined
// each damage row to any earlier rule_fix row on the same artist. That proves a
// rule ran on the artist AT SOME POINT, never that it caused THIS row -- so an
// operator's own edit, made after any rule ever ran, matched and would have been
// restored over. The per-row source is the only key that distinguishes a rule
// write from an operator edit.
//
// It is also why there is NO timestamp condition here. The old design ordered
// damage after its rule_fix row; #3065 deferred that row to grantFixCredits
// (fixer.go:1101), which runs AFTER the persist at :1097, so the ordering is now
// inverted and the condition would reject every genuine candidate.
//
// EXACTLY ONE ROW PER (artist_id, field). The ranking CTE partitions on that
// pair and the damage clause keeps rn = 1, so no duplicate candidate can reach
// the caller and no field can be restored twice in a pass.
//
// The ranking CTE and the damage predicate are REUSED VERBATIM. Both are shared
// with the blast-radius report, and blastRadiusRankedCTE's own header explains
// why no damage or source predicate may move inside the frame: it would promote
// an old damage row to rank 1 and report a recovered field as broken forever.
// The source test below is therefore applied in the OUTER select, appended to
// the damage clause rather than pushed into the frame.
//
// The source test is capability-blind on purpose: whether the naming rule can
// write this field is decided in Go against rule.RuleFields.
const lockDamageQuery = blastRadiusRankedCTE + `
    SELECT r.id, r.artist_id, r.artist_name, r.field, r.old_value, r.new_value,
           SUBSTR(r.source, 6) AS rule_id, r.created_at
    FROM ranked r
    %s
      AND r.source LIKE 'rule:%%'
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
        var damagedAt string
        if err := rows.Scan(&c.ChangeID, &c.ArtistID, &c.ArtistName, &c.Field,
            &c.OldValue, &c.NewValue, &c.RuleID, &damagedAt); err != nil {
            return nil, fmt.Errorf("scanning locked-field damage candidate: %w", err)
        }
        c.DamagedAt = parseHistoryTimestamp(c.ChangeID, damagedAt)
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
// THE OPERATOR-EDIT CONTROL. This is the test the superseded design would have
// FAILED: it seeded a rule_fix row on the artist and then an operator edit, and
// the artist-wide join matched the edit. Attribution by the row's own source
// cannot, because an operator edit is written on a different path.
func TestLockDamageCandidates_ExcludesAnOperatorEdit(t *testing.T) {
    db := newTestDB(t)
    repo := newSQLiteHistoryRepo(db)

    seedArtist(t, db, "a1", "Test Artist")
    // A rule DID run on this artist earlier, and wrote its audit row.
    seedChange(t, db, "f1", "a1", "rule_fix", "", "replaced junk biography",
        "rule:metadata_quality", "2026-05-01T10:00:00Z")
    // Then the OPERATOR edited the same field themselves.
    seedChange(t, db, "d1", "a1", "biography", "curated bio", "operator value",
        "manual", "2026-05-01T11:00:00Z")

    // PRECONDITIONS: both halves of the trap must really be present, or this
    // passes without ever exercising the case it exists for.
    requireChange(t, db, "f1", map[string]string{"source": "rule:metadata_quality"})
    requireChange(t, db, "d1", map[string]string{"source": "manual"})

    got, err := repo.LockDamageCandidates(context.Background())
    if err != nil {
        t.Fatalf("LockDamageCandidates: %v", err)
    }
    if len(got) != 0 {
        t.Fatalf("got %d candidates, want 0 -- an operator edit must never be "+
            "attributed to a rule that merely ran on the same artist", len(got))
    }
}

func TestLockDamageCandidates_ExcludesAlreadyRestored(t *testing.T) {
    db := newTestDB(t)
    repo := newSQLiteHistoryRepo(db)

    seedArtist(t, db, "a1", "Test Artist")
    seedChange(t, db, "d1", "a1", "biography", "curated bio", "junk bio",
        "rule:metadata_quality", "2026-05-01T10:00:01Z")
    // The restore. Its source is what drops the pair from the damage predicate.
    seedChange(t, db, "r1", "a1", "biography", "junk bio", "curated bio",
        "revert", "2026-05-01T10:00:02Z")

    // PRECONDITIONS: the damage is rule-sourced (so it WOULD be a candidate)
    // and the revert row is newer. Without both, "0 candidates" proves nothing.
    requireChange(t, db, "d1", map[string]string{"source": "rule:metadata_quality"})
    requireChange(t, db, "r1", map[string]string{"source": "revert"})

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
        env.seedDamage(id, "biography", "curated bio", "junk bio",
            "metadata_quality", "2026-05-01T10:00:01Z")
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
    env.seedDamageWithSource("a1", "biography", "curated bio", "something else",
        "manual", "2026-05-01T10:00:01Z")

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
    env.seedDamage("a1", "biography", "curated bio", "junk bio",
        "origin_missing", "2026-05-01T10:00:01Z")

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

        // STALENESS RECHECK, immediately before the write (#3074 review).
        // Everything above was decided from a candidate list read at the top of
        // the pass, and the artist read is a separate statement from the write.
        // An operator can edit or unlock the field in between, and this repair
        // runs in a goroutine at startup while the server is already serving.
        // Re-verify the field still holds the DAMAGED value: if it does not,
        // something changed it after the candidate was selected and this row is
        // no longer the newest change, so restoring would overwrite newer data.
        //
        // This is a compare-and-set expressed with the reads available at this
        // layer, not a transaction. It narrows the window rather than closing
        // it; closing it needs a conditional UPDATE in the repository, which is
        // its own unit of work. Narrowing is worth doing because the pass reads
        // ALL candidates up front, so the window is the whole run, not one row.
        if live := artist.FieldValueFromArtist(a, c.Field); live != c.NewValue {
            res.Unrecoverable = append(res.Unrecoverable, LockDamageSkip{
                ArtistID: c.ArtistID, Field: c.Field, RuleID: c.RuleID,
                Reason: "the field changed after the candidate was selected",
            })
            continue
        }

        // A failure here is isolated: it is counted and the loop continues, so
        // one bad row never aborts the pass.
        writeCtx := artist.ContextWithSource(ctx, "revert")
        changed, err := s.artistService.UpdateField(writeCtx, c.ArtistID, c.Field, c.OldValue)
        if err != nil {
            res.Failed = append(res.Failed, LockDamageSkip{
                ArtistID: c.ArtistID, Field: c.Field, RuleID: c.RuleID,
                Reason: "the restore write failed",
            })
            continue
        }
        // UpdateField returns (false, nil) when the NORMALIZED value is
        // unchanged -- it wrote nothing. Counting that as Restored would report
        // a repair that never happened, which is the same "success credit for a
        // write that did not land" defect #3065 fixed in the rule pipeline.
        // It is FAILED rather than unrecoverable: the candidate said the field
        // held the damaged value and the write disagreed, so the two views are
        // inconsistent and the pass should not record completion.
        if !changed {
            res.Failed = append(res.Failed, LockDamageSkip{
                ArtistID: c.ArtistID, Field: c.Field, RuleID: c.RuleID,
                Reason: "the restore wrote nothing; the stored value already matched",
            })
            continue
        }

        // Values are never logged: an old biography is user library content.
        s.logger.Info("restored a locked field a rule had overwritten",
            slog.String("artist_id", c.ArtistID),
            slog.String("field", c.Field),
            slog.String("rule_id", c.RuleID),
            slog.Time("damaged_at", c.DamagedAt))

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

// seedRuleFix writes a rule_fix audit row. NO test asserts on it any more --
// attribution moved to the damage row's own source (#3074) -- but the helper
// stays because the operator-edit control in internal/artist deliberately
// seeds one to prove it does NOT cause a match. Delete it only if that control
// is also deleted.
func (e *lockDamageEnv) seedRuleFix(artistID, ruleID, at string) {
    e.t.Helper()
    e.insertChange(artistID+"-fix-"+ruleID, artistID, "rule_fix", "",
        "fixed something", "rule:"+ruleID, at)
}

// seedDamage writes RULE-SOURCED damage: the shape the repair is designed to
// attribute. Use seedDamageWithSource for anything else.
func (e *lockDamageEnv) seedDamage(artistID, field, oldV, newV, ruleID, at string) {
    e.t.Helper()
    e.insertChange(artistID+"-dmg-"+field, artistID, field, oldV, newV,
        "rule:"+ruleID, at)
}

func (e *lockDamageEnv) seedDamageWithSource(artistID, field, oldV, newV, source, at string) {
    e.t.Helper()
    e.insertChange(artistID+"-dmg-"+field, artistID, field, oldV, newV, source, at)
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

- [ ] **Step 8: Mutation-proof all four controls**

Run each mutation, confirm the named test FAILS, then REVERT it. A mutation that
leaves the suite green means that control is decorative.

Four mutations, one per condition in the allow-list. The ATTRIBUTION mutation
(C) is the one that matters most: it is the condition whose absence would
destroy operator data, and an earlier draft of this plan omitted it (#3074).

```bash
# Mutation A: delete the lock check (the `if !s.artistService.IsFieldLocked` block).
go test ./internal/maintenance/ -run TestRepairLockDamage_RestoresLockedNotUnlocked -v
# Expected: FAIL (a2 gets restored).

# Mutation B: delete the RuleFields capability check.
go test ./internal/maintenance/ -run TestRepairLockDamage_SkipsFieldTheRuleCannotWrite -v
# Expected: FAIL.

# Mutation C: drop `AND r.source LIKE 'rule:%'` from lockDamageQuery -- i.e.
# attribute damage by nothing at all, which is what the superseded rule_fix join
# effectively did. Run BOTH the query-level and orchestration-level controls.
go test ./internal/artist/      -run TestLockDamageCandidates_ExcludesAnOperatorEdit -v
go test ./internal/maintenance/ -run TestRepairLockDamage_SkipsDamageWithNoAttributingRuleFix -v
# Expected: BOTH FAIL. If either passes, the operator-edit guarantee is unproven.

# Mutation D: delete the `if !changed` block after UpdateField.
go test ./internal/maintenance/ -run TestRepairLockDamage_ANoOpWriteIsNotARestore -v
# Expected: FAIL (a write that changed nothing is counted as Restored).
```

Record all four outcomes in the commit message. If any PASSES under mutation,
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
    env.seedDamage("a1", "biography", "curated bio", "junk bio",
        "metadata_quality", "2026-05-01T10:00:01Z")
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
    env.seedDamage("a1", "biography", "curated bio", "junk bio",
        "metadata_quality", "2026-05-01T10:00:01Z")
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
// The documented unrecoverable path is PRE-#3048 damage: a manual-sourced row
// the mechanism cannot attribute. An earlier draft of this test used the
// capability case instead, which duplicated
// TestRepairLockDamage_SkipsFieldTheRuleCannotWrite and left the headline
// coverage claim untested (#3074 review).
//
// NOTE the boundary this pins: an unattributable row is filtered by the QUERY,
// so it never reaches the repair loop's Unrecoverable tally. Reporting it is
// therefore the QUERY's job, via a companion count. If the implementation
// cannot report it without a second query, that is a real design gap -- surface
// it rather than deleting the assertion.
func TestRepairLockDamage_ReportsPre3048DamageAsUnrecoverable(t *testing.T) {
    env := newLockDamageEnv(t)
    env.seedArtist("a1", "Locked Artist", []string{"biography"})
    // Manual source: exactly what a pre-#3048 rule write looks like on disk,
    // and byte-identical to an operator edit. No attribution is possible.
    env.seedDamageWithSource("a1", "biography", "curated bio", "junk bio",
        "manual", "2026-05-01T10:00:01Z")

    env.requireLocked("a1", "biography")

    res, err := env.svc.RepairLockDamage(context.Background(), LockDamageOpts{})
    if err != nil {
        t.Fatalf("RepairLockDamage: %v", err)
    }
    if len(res.Restored) != 0 {
        t.Fatalf("restored %d, want 0 -- unattributable damage must never be "+
            "restored, it is indistinguishable from an operator edit", len(res.Restored))
    }
    if len(res.Unrecoverable) != 1 {
        t.Fatalf("unrecoverable = %d, want 1 -- reporting a silent zero for "+
            "damage the mechanism cannot see is the defect this exists to stop",
            len(res.Unrecoverable))
    }
    if res.Unrecoverable[0].Field != "biography" {
        t.Errorf("unrecoverable field = %q, want biography", res.Unrecoverable[0].Field)
    }
}
```

- [ ] **Step 3b: Write the no-op-write test**

```go
// UpdateField returns (false, nil) when the normalized value is unchanged. A
// restore that wrote nothing must not be counted as a repair (#3074 review) --
// the same "success credit for a write that did not land" defect #3065 fixed in
// the rule pipeline.
func TestRepairLockDamage_ANoOpWriteIsNotARestore(t *testing.T) {
    env := newLockDamageEnv(t)
    env.seedArtist("a1", "Locked Artist", []string{"biography"})
    env.seedDamage("a1", "biography", "curated bio", "junk bio",
        "metadata_quality", "2026-05-01T10:00:01Z")
    // The stored value ALREADY equals what the restore would write, so the
    // write normalizes to a no-op even though the candidate looks restorable.
    env.setBiography("a1", "curated bio")

    env.requireLocked("a1", "biography")
    if got := env.biography("a1"); got != "curated bio" {
        t.Fatalf("fixture: biography = %q, want the restore target", got)
    }

    res, err := env.svc.RepairLockDamage(context.Background(), LockDamageOpts{})
    if err != nil {
        t.Fatalf("RepairLockDamage: %v", err)
    }
    if len(res.Restored) != 0 {
        t.Fatalf("restored %d, want 0 -- nothing was written", len(res.Restored))
    }
}
```

Add the fixture helper:

```go
func (e *lockDamageEnv) setBiography(artistID, bio string) {
    e.t.Helper()
    if _, err := e.db.Exec(
        `UPDATE artists SET biography = ? WHERE id = ?`, bio, artistID); err != nil {
        e.t.Fatalf("setting biography for %s: %v", artistID, err)
    }
}
```

NOTE FOR THE IMPLEMENTER: this test asserts on `Restored` only. Whether the row
lands in `Failed` or is skipped silently depends on the staleness recheck
running first -- the recheck compares the live value to `c.NewValue` and will
divert this fixture before the write. EITHER outcome satisfies the invariant
that matters (it is not a Restore); assert the one the implementation actually
produces, and say which in the commit message.

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
                    // Log the TYPE, never the recovered value. A panic from the
                    // restore path can carry a field value in its message, and
                    // an old biography is user library content that must not
                    // reach the log (this file's own logging constraint).
                    logger.Error("locked-field damage repair panicked",
                        "panic_type", fmt.Sprintf("%T", r))
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
            // COMPLETION IS RECORDED ONLY ON A CLEAN PASS. A row-level failure
            // means that pair was neither restored nor proven unrecoverable, so
            // stamping the key here would retire the one-shot with work
            // outstanding and nothing would ever retry it. Unrecoverable rows do
            // NOT block completion: they are a decided outcome, and they can
            // never become recoverable on a later boot.
            if len(res.Failed) > 0 {
                logger.Warn("locked-field damage repair had row-level failures; "+
                    "not recording completion, the next start retries them",
                    "failed", len(res.Failed))
                return
            }
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

**THE DRY-RUN NEEDS AN ENTRY POINT, AND TASK 4 DOES NOT PROVIDE ONE** (#3074
review). `SW_DB_PATH` selects the database but the startup call passes
`LockDamageOpts{}`, which WRITES. Booting the server against a clone would run
the real repair, not a preview. Add the entry point as part of Task 4:

- [ ] Add a `-lock-damage-dry-run` flag to `cmd/stillwater`. When set, the
      process opens the database, calls
      `RepairLockDamage(ctx, maintenance.LockDamageOpts{DryRun: true})`, prints
      the candidate report, and EXITS before any listener starts. It must not
      fall through into the normal startup path, and it must not stamp
      `lockDamageRepairKey`.
- [ ] Assert in a test that the flag path performs no write: run it against a
      seeded fixture, then verify the artist row and `metadata_changes` are
      byte-identical to before. A dry run that writes is worse than no dry run.

Then the validation itself:

- [ ] Copy the production database and its `-wal` / `-shm` siblings to a scratch
      path. NEVER open the live file read-write: SQLite recovery on an unclean
      copy can mutate the file it opens.
- [ ] Point `SW_DB_PATH` at the copy and run with `-lock-damage-dry-run`.
- [ ] Inspect the candidate list by hand.
- [ ] Confirm no selected row looks like an operator edit. This is the one
      failure mode synthetic controls cannot surface, since they contain only
      damage the author constructed.
- [ ] Then run the write pass against the clone and diff the result.

**EXPECT ZERO CANDIDATES, and treat that as the PASS condition rather than a
bug.** After the attribution correction, a candidate requires a damage row whose
own source is `rule:<id>`, which only a build carrying #3048 writes. A clone of
any released deployment should therefore select nothing. A NON-ZERO count on
such a clone means the predicate is matching something it should not -- stop and
investigate before running the write pass.

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

- Conditions 1-4 each have an implementing task and a mutation that kills a test
  (Tasks 1-3, Task 2 Step 8).
- The seven acceptance criteria in #3038 map to: Task 2 Step 1 (automatic
  restore), Task 2 Steps 1-3 (allow-list with each condition asserted), Task 1
  Step 6 (revert does not revert itself), Task 2 Step 1 (locked/unlocked
  control pair), Task 3 Step 2 (operator edit blocks), Task 3 Step 1 (clean
  second pass), Task 3 Step 3 (unrecoverable tally).
- Two placeholder names are flagged inline as IMPLEMENTER NOTES rather than left
  silent (`NewHistoryRepoForTest`, `parseHistoryTimestamp`); both are "use the
  existing one, do not add a second" instructions.
- `maintenance.Service` widening is called out in Task 2 Step 5 because it
  touches existing call sites.

### Corrections applied from the #3074 review

- **Attribution rewritten.** The artist-wide `rule_fix` join is gone; the damage
  row's own `source` is the key. The join matched an operator's later edit and
  would have restored over it.
- **The ordering condition is deleted, not inverted.** It was backwards after
  #3065, and a wall-clock comparison between two separately-written rows is the
  wrong shape regardless.
- **Duplicate candidates cannot occur.** With the join gone, the ranking CTE's
  `(artist_id, field)` partition plus `rn = 1` yields exactly one row per pair.
- **The write is guarded by a staleness recheck and honors `changed`.**
- **Completion is recorded only on a pass with no row-level failures.**
- **The panic log records the type, never the recovered value.**
- **Four mutations, not two** -- attribution and the no-op write were unproven.
- **A `-lock-damage-dry-run` entry point is required**, since the clone
  validation had no non-writing way to run.
- **Coverage restated honestly:** `origin` has no damage to repair today, and
  the mechanism repairs nothing on any released build.
