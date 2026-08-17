package artist

import (
	"context"
	"database/sql"
	"testing"
)

// lockDamageArtistID is the single artist every fixture in this file uses.
// The queries under test partition per (artist_id, field), so varying the
// field is what exercises the partitioning; a second artist would add nothing.
const lockDamageArtistID = "a1"

// seedLockDamageArtist inserts the fixture artist row. path is NOT NULL with
// no default, so it is seeded empty explicitly.
func seedLockDamageArtist(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO artists (id, name, sort_name, path) VALUES (?, ?, ?, '')`,
		lockDamageArtistID, "Test Artist", "Test Artist"); err != nil {
		t.Fatalf("seeding artist %s: %v", lockDamageArtistID, err)
	}
}

func seedLockDamageChange(t *testing.T, db *sql.DB, id, field, oldV, newV, source, at string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO metadata_changes (id, artist_id, field, old_value, new_value, source, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, lockDamageArtistID, field, oldV, newV, source, at); err != nil {
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

func TestLockDamageCandidates_SelectsRuleSourcedDamage(t *testing.T) {
	db := newTestDB(t)
	repo := newSQLiteHistoryRepo(db)
	ctx := context.Background()

	seedLockDamageArtist(t, db)

	// The damage row NAMES the rule that wrote it. That is the whole
	// attribution: no rule_fix row is seeded, because none is consulted.
	seedLockDamageChange(t, db, "d1", "biography", "curated bio", "junk bio",
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

// THE OPERATOR-EDIT CONTROL. This is the test the superseded design would have
// FAILED: it seeded a rule_fix row on the artist and then an operator edit, and
// the artist-wide join matched the edit. Attribution by the row's own source
// cannot, because an operator edit is written on a different path.
//
// MUTATION PROOF (mutation C, #3075): drop `AND r.source LIKE 'rule:%'` from
// lockDamageQuery -- attributing damage by nothing at all, which is what the
// superseded rule_fix join effectively did -- and this test FAILS (the manual
// row d1 becomes a candidate). If it stays green under that mutation, the
// operator-edit guarantee is unproven.
func TestLockDamageCandidates_ExcludesAnOperatorEdit(t *testing.T) {
	db := newTestDB(t)
	repo := newSQLiteHistoryRepo(db)

	seedLockDamageArtist(t, db)
	// A rule DID run on this artist earlier, and wrote its audit row.
	seedLockDamageChange(t, db, "f1", "rule_fix", "", "replaced junk biography",
		"rule:metadata_quality", "2026-05-01T10:00:00Z")
	// Then the OPERATOR edited the same field themselves.
	seedLockDamageChange(t, db, "d1", "biography", "curated bio", "operator value",
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

	seedLockDamageArtist(t, db)
	seedLockDamageChange(t, db, "d1", "biography", "curated bio", "junk bio",
		"rule:metadata_quality", "2026-05-01T10:00:01Z")
	// The restore. Its source is what drops the pair from the damage predicate.
	// This is the replay-trap guard from #2750: a revert row has a non-empty
	// old_value and a different non-empty new_value, so it resembles damage,
	// and only the `source != 'revert'` exclusion stops a revert-of-a-revert.
	seedLockDamageChange(t, db, "r1", "biography", "junk bio", "curated bio",
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

// The unattributed companion. A manual-sourced damage row -- the shape of all
// pre-#3048 damage, byte-identical to an operator edit -- must be REPORTED by
// LockDamageUnattributed while never becoming a candidate: a silent zero for
// damage the mechanism cannot see is the "unknown rendered as clean" defect
// the blast-radius work exists to prevent.
func TestLockDamageUnattributed_ReportsNonRuleDamageOnly(t *testing.T) {
	db := newTestDB(t)
	repo := newSQLiteHistoryRepo(db)

	seedLockDamageArtist(t, db)
	// Unattributable damage: manual source.
	seedLockDamageChange(t, db, "d1", "biography", "curated bio", "junk bio",
		"manual", "2026-05-01T10:00:01Z")
	// Rule-sourced damage on a different field: a CANDIDATE, so it must NOT
	// also appear in the unattributed report.
	seedLockDamageChange(t, db, "d2", "origin", "Seattle", "Tacoma",
		"rule:origin_missing", "2026-05-01T10:00:02Z")
	// Already-restored pair: excluded from both reports.
	seedLockDamageChange(t, db, "d3", "type", "group", "solo",
		"manual", "2026-05-01T10:00:03Z")
	seedLockDamageChange(t, db, "r3", "type", "solo", "group",
		"revert", "2026-05-01T10:00:04Z")

	requireChange(t, db, "d1", map[string]string{"source": "manual"})
	requireChange(t, db, "d2", map[string]string{"source": "rule:origin_missing"})
	requireChange(t, db, "r3", map[string]string{"source": "revert"})

	got, err := repo.LockDamageUnattributed(context.Background())
	if err != nil {
		t.Fatalf("LockDamageUnattributed: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d unattributed rows, want 1", len(got))
	}
	if got[0].ArtistID != "a1" || got[0].Field != "biography" {
		t.Errorf("unattributed row = %s/%s, want a1/biography", got[0].ArtistID, got[0].Field)
	}
	if got[0].Source != "manual" {
		t.Errorf("Source = %q, want %q", got[0].Source, "manual")
	}
}

// The error contract: a failed query surfaces as a wrapped error, never as an
// empty (and therefore "clean-looking") result. Driven with a canceled
// context, the one failure mode reachable without a fault-injecting driver.
func TestLockDamageQueries_SurfaceQueryErrors(t *testing.T) {
	db := newTestDB(t)
	repo := newSQLiteHistoryRepo(db)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := repo.LockDamageCandidates(ctx); err == nil {
		t.Error("LockDamageCandidates returned nil error on a canceled context")
	}
	if _, err := repo.LockDamageUnattributed(ctx); err == nil {
		t.Error("LockDamageUnattributed returned nil error on a canceled context")
	}
}

// The scan-error contract, same rationale as the query-error test above: a
// malformed row must surface as a wrapped error, never be silently dropped
// from a RECOVERY-REPORTING result -- a row that vanishes from both the
// candidate list and the unattributed report is the "unknown rendered as
// clean" defect this feature exists to prevent. The live schema's NOT NULL
// constraints make a NULL unreachable through any production writer, so the
// fixture recreates metadata_changes without them: the test pins how the
// scanner behaves IF the invariant ever breaks (a hand-edited database, a
// future migration bug), which is exactly when the answer matters.
func TestLockDamageQueries_SurfaceScanErrors(t *testing.T) {
	db := newTestDB(t)
	repo := newSQLiteHistoryRepo(db)
	ctx := context.Background()

	seedLockDamageArtist(t, db)
	if _, err := db.Exec(`DROP TABLE metadata_changes`); err != nil {
		t.Fatalf("dropping metadata_changes: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE metadata_changes (
		id TEXT PRIMARY KEY, artist_id TEXT, field TEXT,
		old_value TEXT, new_value TEXT, source TEXT, created_at TEXT)`); err != nil {
		t.Fatalf("recreating metadata_changes without NOT NULL: %v", err)
	}

	// A rule-sourced damage row whose created_at is NULL: passes the damage
	// predicate, fails the candidate scan (DamagedAt reads a string).
	if _, err := db.Exec(
		`INSERT INTO metadata_changes (id, artist_id, field, old_value, new_value, source, created_at)
		 VALUES ('bad-c', ?, 'biography', 'old', 'new', 'rule:metadata_quality', NULL)`,
		lockDamageArtistID); err != nil {
		t.Fatalf("seeding NULL-created_at row: %v", err)
	}
	if _, err := repo.LockDamageCandidates(ctx); err == nil {
		t.Error("LockDamageCandidates returned nil error for an unscannable row")
	}

	// A manual damage row whose field is NULL: passes the unattributed
	// predicate, fails its scan (Field reads a string).
	if _, err := db.Exec(`DELETE FROM metadata_changes`); err != nil {
		t.Fatalf("clearing rows: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO metadata_changes (id, artist_id, field, old_value, new_value, source, created_at)
		 VALUES ('bad-u', ?, NULL, 'old', 'new', 'manual', '2026-05-01T10:00:01Z')`,
		lockDamageArtistID); err != nil {
		t.Fatalf("seeding NULL-field row: %v", err)
	}
	if _, err := repo.LockDamageUnattributed(ctx); err == nil {
		t.Error("LockDamageUnattributed returned nil error for an unscannable row")
	}
}
