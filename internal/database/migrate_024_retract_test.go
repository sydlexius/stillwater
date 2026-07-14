package database

// Migration 024 clears the false "passed" rows the two image-duplicate rules
// recorded for path-less (API-only) artists before the capability gate existed
// (issue #2509). The runtime retraction only fires when an artist is next
// evaluated, and rules_evaluated_at can keep an artist out of the dirty set for
// a long time, so the one-shot delete is what makes the dashboards honest on
// upgrade day.
//
// The test builds a database at the PRE-024 schema, seeds it the way the old
// code left a real library (stale pass rows for path-less artists, genuine rows
// for everything else), and only then applies 024 -- the exact path every
// existing install takes on upgrade.

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// ruleResultExists reports whether a (artist, rule) row is present.
func ruleResultExists(t *testing.T, db *sql.DB, artistID, ruleID string) bool {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM rule_results WHERE artist_id = ? AND rule_id = ?`,
		artistID, ruleID).Scan(&n); err != nil {
		t.Fatalf("counting rule_results (%s, %s): %v", artistID, ruleID, err)
	}
	return n > 0
}

// TestMigration024_MakesRetractedArtistsDirty is the other half of the
// migration's job.
//
// Deleting the false pass rows is not enough on its own. A path-less artist that
// IS capable under the new gate (no directory, but two or more comparable stored
// hashes) keeps both duplicate rules in its eligible set. Delete its rows and
// leave it CLEAN, and Pipeline.offlineHealthScore sees two rules with no result
// row, calls the artist un-scoreable and refuses to rescore -- while
// artist.ListDirtyIDs never returns it, because the migration bumps neither
// rules.updated_at nor artists.dirty_since. Its health score would simply stop
// updating until someone forced a full re-evaluation.
//
// The migration therefore clears rules_evaluated_at for every artist it touched,
// which is the never-evaluated branch of the dirty query. This test asserts that
// against the REAL ListDirtyIDs, not against a hand-copied version of its SQL.
//
// Mutant this kills: dropping the UPDATE from 024 (the delete alone re-freezes
// health), or scoping it to artists the migration did not actually touch.
func TestMigration024_MakesRetractedArtistsDirty(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "dirty.db")
	ctx := context.Background()

	migrateUpTo(t, dbPath, 23)

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopening db: %v", err)
	}
	defer db.Close()

	// The rules were last touched long ago, so the "an enabled rule changed since
	// this artist was evaluated" branch of the dirty query cannot fire and mask
	// the result.
	for _, r := range []struct{ id, name, category string }{
		{"image_duplicate", "Duplicate Images", "image"},
		{"nfo_exists", "NFO Exists", "nfo"},
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO rules (id, name, category, enabled, updated_at)
			 VALUES (?, ?, ?, 1, '2020-01-01 00:00:00')`,
			r.id, r.name, r.category); err != nil {
			t.Fatalf("seeding rule %s: %v", r.id, err)
		}
	}

	// Earlier migrations seed rules of their own, stamped updated_at = now. Push
	// every rule into the past so the "an enabled rule changed since this artist
	// was evaluated" branch cannot fire for anyone: this test is about the
	// never-evaluated branch and nothing else.
	if _, err := db.ExecContext(ctx,
		`UPDATE rules SET updated_at = '2020-01-01 00:00:00'`); err != nil {
		t.Fatalf("aging rules.updated_at: %v", err)
	}

	// Both artists were evaluated AFTER that, so both are CLEAN going in.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO artists (id, name, sort_name, path, rules_evaluated_at) VALUES
			('api-empty', 'API Empty Path', 'API Empty Path', '',                    '2026-01-05T00:00:00Z'),
			('local',     'Local Artist',   'Local Artist',   '/music/Local Artist', '2026-01-05T00:00:00Z')
	`); err != nil {
		t.Fatalf("seeding artists: %v", err)
	}
	// Only the path-less artist carries a false duplicate pass; the artist with a
	// path carries a genuine one the migration must not touch.
	for _, aid := range []string{"api-empty", "local"} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO rule_results (artist_id, rule_id, passed, evaluated_at, last_passed_at)
			 VALUES (?, 'image_duplicate', 1, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
			aid); err != nil {
			t.Fatalf("seeding rule_results for %s: %v", aid, err)
		}
	}

	svc := artist.NewService(db)

	// PRECONDITION / POSITIVE CONTROL: nobody is dirty before the migration. If
	// they already were, "they are dirty afterwards" would be vacuously true.
	before, err := svc.ListDirtyIDs(ctx)
	if err != nil {
		t.Fatalf("ListDirtyIDs before migration: %v", err)
	}
	if len(before) != 0 {
		t.Fatalf("precondition: %v is already dirty before the migration runs; this test cannot "+
			"prove the migration made anyone dirty", before)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("migrating a populated database to 024: %v", err)
	}

	after, err := svc.ListDirtyIDs(ctx)
	if err != nil {
		t.Fatalf("ListDirtyIDs after migration: %v", err)
	}
	if !slices.Contains(after, "api-empty") {
		t.Errorf("ListDirtyIDs = %v; the artist whose duplicate rows migration 024 deleted is "+
			"NOT dirty, so the next incremental pass never re-walks it. If it is capable under "+
			"the new gate its rules stay in the eligible denominator with no result rows and "+
			"offlineHealthScore refuses to score it: its health silently stops updating.", after)
	}
	if slices.Contains(after, "local") {
		t.Errorf("ListDirtyIDs = %v; the artist WITH a path was made dirty. The migration "+
			"deleted none of its rows, so it has no reason to be re-evaluated -- marking it "+
			"dirty forces needless work on every library on upgrade day.", after)
	}
}

// TestMigration024_ArmsExcludedAndLockedArtistsForReEvaluation pins the cohort it
// would be easiest to get exactly backwards.
//
// ListDirtyIDs selects WHERE is_excluded = 0 AND locked = 0 AND (rules_evaluated_at
// IS NULL OR ...). It is tempting to conclude that NULLing the timestamp for an
// excluded or locked artist is pointless, since the dirty query cannot return it
// today, and therefore to scope the UPDATE away from that cohort. That reasoning is
// exactly wrong, and scoping it is what strands the artist.
//
// NULLing the column while the artist is still locked or excluded schedules nothing
// NOW, but it ARMS the first freshness branch, so the artist is re-walked the moment
// it is unlocked or re-included. Leaving the column intact is what strands it:
// unlocking does NOT stamp dirty_since (SetLock in internal/artist/sqlite_artist.go
// writes only locked, lock_source, locked_at and updated_at). An unlocked artist
// whose rows this migration deleted would then satisfy NO freshness branch, would
// never be re-evaluated, and would hold a frozen health score indefinitely.
//
// So this test asserts the timestamp IS cleared, and then proves the point that
// actually matters: once unlocked, the artist really does come back dirty.
//
// Mutant this kills: scoping the UPDATE to is_excluded = 0 AND locked = 0.
func TestMigration024_ArmsExcludedAndLockedArtistsForReEvaluation(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "excluded.db")
	ctx := context.Background()

	migrateUpTo(t, dbPath, 23)

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopening db: %v", err)
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO rules (id, name, category, enabled, updated_at)
		 VALUES ('image_duplicate', 'Duplicate Images', 'image', 1, '2020-01-01 00:00:00')`); err != nil {
		t.Fatalf("seeding rule: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE rules SET updated_at = '2020-01-01 00:00:00'`); err != nil {
		t.Fatalf("aging rules.updated_at: %v", err)
	}

	const evaluatedAt = "2026-01-05T00:00:00Z"
	// locked = 1 carries a CHECK that locked_at is set.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO artists (id, name, sort_name, path, rules_evaluated_at, is_excluded, locked, locked_at) VALUES
			('api-excluded', 'API Excluded', 'API Excluded', '', ?, 1, 0, NULL),
			('api-locked',   'API Locked',   'API Locked',   '', ?, 0, 1, '2026-01-02T00:00:00Z')
	`, evaluatedAt, evaluatedAt); err != nil {
		t.Fatalf("seeding artists: %v", err)
	}
	for _, aid := range []string{"api-excluded", "api-locked"} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO rule_results (artist_id, rule_id, passed, evaluated_at, last_passed_at)
			 VALUES (?, 'image_duplicate', 1, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
			aid); err != nil {
			t.Fatalf("seeding rule_results for %s: %v", aid, err)
		}
		// PRECONDITION: the false row this migration must delete is really there.
		if !ruleResultExists(t, db, aid, "image_duplicate") {
			t.Fatalf("precondition: rule_results (%s, image_duplicate) was not seeded", aid)
		}
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("migrating a populated database to 024: %v", err)
	}

	for _, aid := range []string{"api-excluded", "api-locked"} {
		if ruleResultExists(t, db, aid, "image_duplicate") {
			t.Errorf("the false image_duplicate pass row for %s survived; it is a false pass "+
				"whether or not the artist is ever re-evaluated", aid)
		}
		var evaluated sql.NullString
		if err := db.QueryRowContext(ctx,
			`SELECT rules_evaluated_at FROM artists WHERE id = ?`, aid).Scan(&evaluated); err != nil {
			t.Fatalf("reading rules_evaluated_at for %s: %v", aid, err)
		}
		if evaluated.Valid {
			t.Errorf("migration 024 left rules_evaluated_at = %q for %s. It deleted that "+
				"artist's duplicate rows, so leaving the timestamp intact strands it: unlocking "+
				"does not stamp dirty_since, so no freshness branch of ListDirtyIDs would ever "+
				"hold again and the artist would keep a frozen health score forever. The column "+
				"must be NULLed to arm the never-evaluated branch for when it is unlocked.",
				evaluated.String, aid)
		}
	}

	// The assertion that actually matters: once the artist is unlocked, the armed
	// NULL really does bring it back through the dirty query. Without this, the
	// test above only pins a column value and proves nothing about the outcome.
	if _, err := db.ExecContext(ctx,
		`UPDATE artists SET locked = 0, locked_at = NULL WHERE id = 'api-locked'`); err != nil {
		t.Fatalf("unlocking the artist: %v", err)
	}

	dirtyIDs, err := artist.NewService(db).ListDirtyIDs(ctx)
	if err != nil {
		t.Fatalf("ListDirtyIDs after unlocking: %v", err)
	}
	if !slices.Contains(dirtyIDs, "api-locked") {
		t.Errorf("ListDirtyIDs = %v after unlocking; want it to contain %q. The artist is NOT "+
			"dirty, so the pipeline will never re-walk it and the rows migration 024 deleted are "+
			"never rebuilt: its health score is frozen permanently.", dirtyIDs, "api-locked")
	}
}

func TestMigration024_RetractsOnlyPathlessDuplicatePasses(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "stale.db")
	ctx := context.Background()

	// 1. A database at the schema that shipped BEFORE this migration.
	migrateUpTo(t, dbPath, 23)

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopening db: %v", err)
	}
	defer db.Close()

	// 2. Three rules and two artists: one API-only (artists.path is NOT NULL, so
	//    "" is the only spelling of "no local directory", and it is exactly what
	//    the old checkers tested), and one with a real directory. nfo_exists is
	//    the control rule the migration must not touch.
	for _, r := range []struct{ id, name, category string }{
		{"image_duplicate", "Duplicate Images", "image"},
		{"image_duplicate_exact", "Exact Duplicate Images", "image"},
		{"nfo_exists", "NFO Exists", "metadata"},
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO rules (id, name, category) VALUES (?, ?, ?)`,
			r.id, r.name, r.category); err != nil {
			t.Fatalf("seeding rule %s: %v", r.id, err)
		}
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO artists (id, name, sort_name, path) VALUES
			('api-empty', 'API Empty Path', 'API Empty Path', ''),
			('local',     'Local Artist',   'Local Artist',   '/music/Local Artist')
	`); err != nil {
		t.Fatalf("seeding artists: %v", err)
	}

	// 3. Seed exactly what the old code wrote: a passed=1 row for every artist
	//    and every rule. For the path-less artist the duplicate-rule rows are
	//    the false passes (the checkers returned nil without looking at a single
	//    image); everything else is a genuine outcome.
	for _, aid := range []string{"api-empty", "local"} {
		for _, rid := range []string{"image_duplicate", "image_duplicate_exact", "nfo_exists"} {
			if _, err := db.ExecContext(ctx,
				`INSERT INTO rule_results (artist_id, rule_id, passed, evaluated_at, last_passed_at)
				 VALUES (?, ?, 1, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
				aid, rid); err != nil {
				t.Fatalf("seeding rule_results (%s, %s): %v", aid, rid, err)
			}
		}
	}

	// PRECONDITION / POSITIVE CONTROL: every row this test reasons about is
	// actually in the database before the migration runs. Without this the
	// "the false rows are gone" assertions pass vacuously.
	for _, aid := range []string{"api-empty", "local"} {
		for _, rid := range []string{"image_duplicate", "image_duplicate_exact", "nfo_exists"} {
			if !ruleResultExists(t, db, aid, rid) {
				t.Fatalf("precondition: rule_results (%s, %s) was not seeded", aid, rid)
			}
		}
	}

	// 4. Upgrade.
	if err := Migrate(db); err != nil {
		t.Fatalf("migrating a populated database to 024: %v", err)
	}

	// 5. Exactly the false passes are gone.
	for _, aid := range []string{"api-empty"} {
		for _, rid := range []string{"image_duplicate", "image_duplicate_exact"} {
			if ruleResultExists(t, db, aid, rid) {
				t.Errorf("rule_results (%s, %s) survived migration 024. A path-less artist could "+
					"never produce a violation for this rule under the old code, so the row is a "+
					"false pass by construction and must be deleted.", aid, rid)
			}
		}
	}

	// 6. Nothing else was touched: the same artists keep their other rules, and
	//    an artist with a path keeps its duplicate rows (its rules genuinely ran).
	for _, aid := range []string{"api-empty", "local"} {
		if !ruleResultExists(t, db, aid, "nfo_exists") {
			t.Errorf("migration 024 deleted the nfo_exists row for %s; it must only touch the "+
				"two duplicate rules", aid)
		}
	}
	for _, rid := range []string{"image_duplicate", "image_duplicate_exact"} {
		if !ruleResultExists(t, db, "local", rid) {
			t.Errorf("migration 024 deleted the %s row for an artist WITH a local path; that rule "+
				"really did run against its files and its outcome is authoritative", rid)
		}
	}
}
