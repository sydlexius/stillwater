package artist

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// lock_restore_test.go -- unit tests for RestoreLockedFieldGuarded, the
// guarded conditional write the locked-field damage repair uses (#3075 fix
// round). The repair-level tests in internal/maintenance exercise the verb
// through the full pass; these pin the verb's own contract branch by branch,
// against real SQLite, because the guarantees under test (the transactional
// lock recheck, the repeated equality condition, the collision scan) are
// properties of the SQL this package owns.

type lockRestoreEnv struct {
	t   *testing.T
	db  *sql.DB
	svc *Service
}

func newLockRestoreEnv(t *testing.T) *lockRestoreEnv {
	t.Helper()
	db := newTestDB(t)
	svc := NewService(db)
	svc.SetHistoryService(NewHistoryService(db))
	return &lockRestoreEnv{t: t, db: db, svc: svc}
}

func (e *lockRestoreEnv) seedArtist(id, name, bio, lockedFieldsJSON string) {
	e.t.Helper()
	if _, err := e.db.Exec(
		`INSERT INTO artists (id, name, sort_name, path, biography, locked_fields)
		 VALUES (?, ?, ?, '', ?, ?)`,
		id, name, name, bio, lockedFieldsJSON); err != nil {
		e.t.Fatalf("seeding artist %s: %v", id, err)
	}
}

// biography reads a1's stored biography, the artist every fixture here seeds.
func (e *lockRestoreEnv) biography() string {
	e.t.Helper()
	var bio string
	if err := e.db.QueryRow(`SELECT biography FROM artists WHERE id = 'a1'`).Scan(&bio); err != nil {
		e.t.Fatalf("reading biography of a1: %v", err)
	}
	return bio
}

func TestRestoreLockedFieldGuarded_AppliesAndRecordsHistory(t *testing.T) {
	env := newLockRestoreEnv(t)
	env.seedArtist("a1", "Artist", "junk bio", `["biography"]`)
	ctx := ContextWithSource(context.Background(), "revert")

	outcome, err := env.svc.RestoreLockedFieldGuarded(ctx, "a1", "biography", "junk bio", "curated bio")
	if err != nil {
		t.Fatalf("RestoreLockedFieldGuarded: %v", err)
	}
	if outcome != LockedFieldRestoreApplied {
		t.Fatalf("outcome = %v, want applied", outcome)
	}
	if got := env.biography(); got != "curated bio" {
		t.Errorf("biography = %q, want the restored value", got)
	}
	// The history row carries the context's source and the real old/new pair.
	var oldV, newV, source string
	if err := env.db.QueryRow(
		`SELECT old_value, new_value, source FROM metadata_changes WHERE artist_id = 'a1' AND field = 'biography'`).
		Scan(&oldV, &newV, &source); err != nil {
		t.Fatalf("reading history row: %v", err)
	}
	if oldV != "junk bio" || newV != "curated bio" || source != "revert" {
		t.Errorf("history row = (%q, %q, %q), want (junk bio, curated bio, revert)", oldV, newV, source)
	}
}

func TestRestoreLockedFieldGuarded_DivertsWhenValueDiverged(t *testing.T) {
	env := newLockRestoreEnv(t)
	env.seedArtist("a1", "Artist", "operator edit", `["biography"]`)

	outcome, err := env.svc.RestoreLockedFieldGuarded(context.Background(), "a1", "biography", "junk bio", "curated bio")
	if err != nil {
		t.Fatalf("RestoreLockedFieldGuarded: %v", err)
	}
	if outcome != LockedFieldRestoreValueDiverged {
		t.Fatalf("outcome = %v, want diverged", outcome)
	}
	if got := env.biography(); got != "operator edit" {
		t.Errorf("biography = %q, want the newer value untouched", got)
	}
}

func TestRestoreLockedFieldGuarded_DivertsWhenUnlocked(t *testing.T) {
	env := newLockRestoreEnv(t)
	env.seedArtist("a1", "Artist", "junk bio", `[]`)

	outcome, err := env.svc.RestoreLockedFieldGuarded(context.Background(), "a1", "biography", "junk bio", "curated bio")
	if err != nil {
		t.Fatalf("RestoreLockedFieldGuarded: %v", err)
	}
	if outcome != LockedFieldRestoreUnlocked {
		t.Fatalf("outcome = %v, want unlocked", outcome)
	}
	if got := env.biography(); got != "junk bio" {
		t.Errorf("biography = %q, want it untouched", got)
	}
}

// The two DETERMINISTIC refusals the repair retires via FailedPermanent: both
// must travel as typed errors so errors.Is classification works, and neither
// may write anything.
func TestRestoreLockedFieldGuarded_DeterministicRefusalsAreTyped(t *testing.T) {
	t.Run("a field with no artists column is refused as invalid", func(t *testing.T) {
		env := newLockRestoreEnv(t)
		env.seedArtist("a1", "Artist", "junk bio", `["musicbrainz_id"]`)

		_, err := env.svc.RestoreLockedFieldGuarded(context.Background(), "a1", "musicbrainz_id", "old-id", "new-id")
		if !errors.Is(err, ErrInvalidFieldValue) {
			t.Fatalf("err = %v, want ErrInvalidFieldValue", err)
		}
	})

	t.Run("a name restore that would recreate a collision is refused", func(t *testing.T) {
		env := newLockRestoreEnv(t)
		env.seedArtist("a1", "Damaged Name", "", `["name"]`)
		// The identity the restore would write back now belongs to a2.
		env.seedArtist("a2", "Original Name", "", `[]`)

		_, err := env.svc.RestoreLockedFieldGuarded(context.Background(), "a1", "name", "Damaged Name", "Original Name")
		if !errors.Is(err, ErrNameCollision) {
			t.Fatalf("err = %v, want ErrNameCollision", err)
		}
		var name string
		if err := env.db.QueryRow(`SELECT name FROM artists WHERE id = 'a1'`).Scan(&name); err != nil {
			t.Fatalf("reading name: %v", err)
		}
		if name != "Damaged Name" {
			t.Errorf("name = %q, want the refused restore to have written nothing", name)
		}
	})
}

// A collision-free name restore lands, proving the collision scan refuses
// only genuine second identities rather than every name write.
func TestRestoreLockedFieldGuarded_CollisionFreeNameRestoreApplies(t *testing.T) {
	env := newLockRestoreEnv(t)
	env.seedArtist("a1", "Damaged Name", "", `["name"]`)

	outcome, err := env.svc.RestoreLockedFieldGuarded(context.Background(), "a1", "name", "Damaged Name", "Original Name")
	if err != nil {
		t.Fatalf("RestoreLockedFieldGuarded: %v", err)
	}
	if outcome != LockedFieldRestoreApplied {
		t.Fatalf("outcome = %v, want applied", outcome)
	}
	var name string
	if err := env.db.QueryRow(`SELECT name FROM artists WHERE id = 'a1'`).Scan(&name); err != nil {
		t.Fatalf("reading name: %v", err)
	}
	if name != "Original Name" {
		t.Errorf("name = %q, want the restored value", name)
	}
}

// Slice fields store JSON but history rows carry the joined form, which is
// what the candidate's values are. The compare and the write must both round
// through that representation.
func TestRestoreLockedFieldGuarded_SliceFieldComparesJoinedForm(t *testing.T) {
	env := newLockRestoreEnv(t)
	env.seedArtist("a1", "Artist", "", `["genres"]`)
	if _, err := env.db.Exec(
		`UPDATE artists SET genres = '["Junk","Noise"]' WHERE id = 'a1'`); err != nil {
		t.Fatalf("seeding genres: %v", err)
	}

	outcome, err := env.svc.RestoreLockedFieldGuarded(context.Background(), "a1", "genres", "Junk, Noise", "Rock, Alternative")
	if err != nil {
		t.Fatalf("RestoreLockedFieldGuarded: %v", err)
	}
	if outcome != LockedFieldRestoreApplied {
		t.Fatalf("outcome = %v, want applied", outcome)
	}
	var raw string
	if err := env.db.QueryRow(`SELECT genres FROM artists WHERE id = 'a1'`).Scan(&raw); err != nil {
		t.Fatalf("reading genres: %v", err)
	}
	if raw != `["Rock","Alternative"]` {
		t.Errorf("genres = %s, want the restored JSON array", raw)
	}
}

// TestRestoreLockedFieldGuarded_SliceFieldFormattingOnlyDiffStillRecordsHistory
// reproduces the hostile-review F1 finding (#3088 fix round). The verb makes
// TWO different comparisons that must not be conflated:
//
//  1. The candidate-selection check (line ~130) compares
//     normalizeFieldValue(damagedValue) against normalizeFieldValue(stored) --
//     both round-tripped through splitTags+join, so a raw formatting
//     difference (extra whitespace, comma spacing) between damagedValue and
//     the actual column is tolerated: it still counts as "this is the damage
//     the candidate was selected for" and the guarded UPDATE fires.
//  2. recordHistoryTx's (now-removed) no-op skip compared oldValue==newValue
//     as RAW strings -- oldValue is `stored` (the joined form of the CURRENT
//     column), newValue is `restoreValue` UNNORMALIZED, exactly as the caller
//     passed it in.
//
// Those two comparisons can disagree: damagedValue can raw-differ from stored
// (satisfying check 1 as a normalize-match) while restoreValue happens to
// raw-STRING-EQUAL stored's joined form (triggering the old check-2 skip),
// even though the UPDATE statement itself still reports affected == 1 (SQLite
// counts a WHERE-matched row as affected regardless of whether SET actually
// changes its value). Before the fix that combination committed the artist
// write, then recordHistoryTx's skip silently ate the insert: outcome reports
// Applied with ZERO history rows, and with no revert row to exclude the pair
// from lockDamageQuery, the next boot selects and "restores" it again,
// forever.
//
// genres is stored as ["rock","pop"] (joined form "rock, pop"). damagedValue
// is "rock,pop" (no space) -- normalize-equal to stored, so the UPDATE fires.
// restoreValue is "rock, pop" -- the exact joined form already stored, so the
// old raw-string skip fired on it.
func TestRestoreLockedFieldGuarded_SliceFieldFormattingOnlyDiffStillRecordsHistory(t *testing.T) {
	env := newLockRestoreEnv(t)
	env.seedArtist("a1", "Artist", "", `["genres"]`)
	if _, err := env.db.Exec(
		`UPDATE artists SET genres = '["rock","pop"]' WHERE id = 'a1'`); err != nil {
		t.Fatalf("seeding genres: %v", err)
	}

	outcome, err := env.svc.RestoreLockedFieldGuarded(context.Background(),
		"a1", "genres", "rock,pop", "rock, pop")
	if err != nil {
		t.Fatalf("RestoreLockedFieldGuarded: %v", err)
	}
	if outcome != LockedFieldRestoreApplied {
		t.Fatalf("outcome = %v, want applied", outcome)
	}
	var n int
	if err := env.db.QueryRow(
		`SELECT COUNT(*) FROM metadata_changes WHERE artist_id = 'a1' AND field = 'genres'`).
		Scan(&n); err != nil {
		t.Fatalf("counting history rows: %v", err)
	}
	if n != 1 {
		t.Fatalf("history rows = %d, want 1 -- an outcome=Applied write must always leave an audit row "+
			"(F1: the old recordHistoryTx no-op skip silently ate this one, reporting Applied with zero "+
			"history rows, so the pair stayed a damage candidate forever)", n)
	}
}

func TestRestoreLockedFieldGuarded_MissingArtistIsNotFound(t *testing.T) {
	env := newLockRestoreEnv(t)

	_, err := env.svc.RestoreLockedFieldGuarded(context.Background(), "absent", "biography", "junk", "curated")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// The verb's failure branches, driven with real SQLite faults rather than
// mocks: each is an error path the repair loop classifies (transient vs
// deterministic), so each earns a pin that it surfaces as an ERROR or a
// decided outcome, never as a false "applied".
func TestRestoreLockedFieldGuarded_FailureBranches(t *testing.T) {
	t.Run("a side-table field with no artists column is a typed refusal", func(t *testing.T) {
		env := newLockRestoreEnv(t)
		// audiodb_id carries no validation rule, so this reaches the column
		// map rather than being refused earlier; it lives in the provider
		// side table, not on the artists row.
		_, err := env.svc.RestoreLockedFieldGuarded(context.Background(), "a1", "audiodb_id", "old", "new")
		if !errors.Is(err, ErrInvalidFieldValue) {
			t.Fatalf("err = %v, want ErrInvalidFieldValue for a side-table field", err)
		}
	})

	t.Run("a repository without a DB accessor is an error", func(t *testing.T) {
		db := newTestDB(t)
		artists, providers, members, aliases, images, platformIDs, completeness := NewDefaultRepos(db)
		// noDBRepo hides the DB() accessor the verb's transaction needs.
		svc := NewServiceWithRepos(noDBRepo{artists}, providers, members, aliases, images, platformIDs, completeness)
		if _, err := svc.RestoreLockedFieldGuarded(context.Background(), "a1", "biography", "junk", "curated"); err == nil {
			t.Fatal("want an error when the repository exposes no raw handle")
		}
	})

	t.Run("a closed database fails at BeginTx", func(t *testing.T) {
		env := newLockRestoreEnv(t)
		env.seedArtist("a1", "Artist", "junk bio", `["biography"]`)
		if err := env.db.Close(); err != nil {
			t.Fatalf("closing db: %v", err)
		}
		if _, err := env.svc.RestoreLockedFieldGuarded(context.Background(), "a1", "biography", "junk bio", "curated bio"); err == nil {
			t.Fatal("want an error on a closed database")
		}
	})

	t.Run("a load failure that is not no-rows is surfaced", func(t *testing.T) {
		env := newLockRestoreEnv(t)
		if _, err := env.db.Exec(`DROP TABLE artists`); err != nil {
			t.Fatalf("dropping artists: %v", err)
		}
		_, err := env.svc.RestoreLockedFieldGuarded(context.Background(), "a1", "biography", "junk", "curated")
		if err == nil {
			t.Fatal("want an error when the artists table is unreadable")
		}
		if errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v; a broken table must not read as a missing artist", err)
		}
	})

	t.Run("a refused write surfaces as an error, not applied", func(t *testing.T) {
		env := newLockRestoreEnv(t)
		env.seedArtist("a1", "Artist", "junk bio", `["biography"]`)
		if _, err := env.db.Exec(
			`CREATE TRIGGER refuse_bio_update BEFORE UPDATE OF biography ON artists
			 BEGIN SELECT RAISE(ABORT, 'refused by test trigger'); END`); err != nil {
			t.Fatalf("creating trigger: %v", err)
		}
		if _, err := env.svc.RestoreLockedFieldGuarded(context.Background(), "a1", "biography", "junk bio", "curated bio"); err == nil {
			t.Fatal("want the trigger's refusal surfaced as an error")
		}
		if got := env.biography(); got != "junk bio" {
			t.Errorf("biography = %q, want it untouched by the refused write", got)
		}
	})

	t.Run("a write the engine silently skips reports diverged, never applied", func(t *testing.T) {
		// RAISE(IGNORE) makes SQLite skip the row without erroring: the one
		// reachable shape of "UPDATE succeeded but wrote 0 rows", which is
		// exactly what the repeated WHERE condition reports when the value
		// moved. The verb must read RowsAffected and answer diverged.
		env := newLockRestoreEnv(t)
		env.seedArtist("a1", "Artist", "junk bio", `["biography"]`)
		if _, err := env.db.Exec(
			`CREATE TRIGGER skip_bio_update BEFORE UPDATE OF biography ON artists
			 BEGIN SELECT RAISE(IGNORE); END`); err != nil {
			t.Fatalf("creating trigger: %v", err)
		}
		outcome, err := env.svc.RestoreLockedFieldGuarded(context.Background(), "a1", "biography", "junk bio", "curated bio")
		if err != nil {
			t.Fatalf("RestoreLockedFieldGuarded: %v", err)
		}
		if outcome != LockedFieldRestoreValueDiverged {
			t.Fatalf("outcome = %v, want diverged for a 0-row write", outcome)
		}
		if got := env.biography(); got != "junk bio" {
			t.Errorf("biography = %q, want it untouched", got)
		}
	})

	t.Run("a history-insert failure rolls back the artist write too (#3088)", func(t *testing.T) {
		// This is the atomicity guarantee #3088 adds: before the fix, the
		// artist UPDATE and the metadata_changes INSERT were two separate
		// statements (the artist write committed, then a best-effort history
		// insert followed). A trigger that refuses every insert into
		// metadata_changes proves the two now share one transaction --
		// without the fix this trigger would fire AFTER the artist row had
		// already committed, so the biography would read "curated bio" with
		// no history row to explain it. With the fix, the whole transaction
		// rolls back and the stored value is untouched.
		env := newLockRestoreEnv(t)
		env.seedArtist("a1", "Artist", "junk bio", `["biography"]`)
		if _, err := env.db.Exec(
			`CREATE TRIGGER refuse_history_insert BEFORE INSERT ON metadata_changes
			 BEGIN SELECT RAISE(ABORT, 'refused by test trigger'); END`); err != nil {
			t.Fatalf("creating trigger: %v", err)
		}
		_, err := env.svc.RestoreLockedFieldGuarded(context.Background(), "a1", "biography", "junk bio", "curated bio")
		if err == nil {
			t.Fatal("want the history trigger's refusal surfaced as an error")
		}
		if got := env.biography(); got != "junk bio" {
			t.Errorf("biography = %q, want the artist write rolled back with the failed history insert", got)
		}
		var n int
		if err := env.db.QueryRow(
			`SELECT COUNT(*) FROM metadata_changes WHERE artist_id = 'a1' AND field = 'biography'`).
			Scan(&n); err != nil {
			t.Fatalf("counting history rows: %v", err)
		}
		if n != 0 {
			t.Errorf("history rows = %d, want 0 -- no partial history row from the rolled-back transaction", n)
		}
	})
}

// TestRecordHistoryTx_ValidationAndSkipBranches pins recordHistoryTx's own
// contract directly, branch by branch, against a real *sql.Tx -- the
// validation and skip rules it mirrors from HistoryService.Record but cannot
// share code with (recordHistoryTx exists purely because HistoryRepository
// has no INSERT-on-a-transaction method; see the doc comment on
// recordHistoryTx). RestoreLockedFieldGuarded's own tests exercise these only
// indirectly through a full restore, which cannot reach the empty-artistID /
// empty-field / invalid-source branches because ValidateFieldUpdate and the
// caller's own id argument already rule those inputs out before
// recordHistoryTx is ever called.
func TestRecordHistoryTx_ValidationAndSkipBranches(t *testing.T) {
	// metadata_changes.artist_id carries a foreign key onto artists, so every
	// case that reaches the actual INSERT needs artist "a1" seeded first --
	// the validation-only cases below (empty artist_id/field/source, invalid
	// source) never reach the INSERT and do not need it.
	newTx := func(t *testing.T) (*sql.DB, *sql.Tx) {
		t.Helper()
		env := newLockRestoreEnv(t)
		env.seedArtist("a1", "Artist", "junk bio", `["biography"]`)
		tx, err := env.db.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatalf("beginning tx: %v", err)
		}
		t.Cleanup(func() { _ = tx.Rollback() })
		return env.db, tx
	}

	t.Run("empty artist_id is refused", func(t *testing.T) {
		_, tx := newTx(t)
		err := recordHistoryTx(context.Background(), tx, "", "biography", "old", "new", "revert")
		if err == nil {
			t.Fatal("want an error for an empty artist_id")
		}
	})

	t.Run("empty field is refused", func(t *testing.T) {
		_, tx := newTx(t)
		err := recordHistoryTx(context.Background(), tx, "a1", "", "old", "new", "revert")
		if err == nil {
			t.Fatal("want an error for an empty field")
		}
	})

	t.Run("empty source is refused", func(t *testing.T) {
		_, tx := newTx(t)
		err := recordHistoryTx(context.Background(), tx, "a1", "biography", "old", "new", "")
		if err == nil {
			t.Fatal("want an error for an empty source")
		}
	})

	t.Run("an invalid source is refused", func(t *testing.T) {
		_, tx := newTx(t)
		err := recordHistoryTx(context.Background(), tx, "a1", "biography", "old", "new", "bogus")
		if err == nil {
			t.Fatal("want an error for a source outside the valid set")
		}
	})

	// UNLIKE HistoryService.Record, recordHistoryTx does NOT skip an
	// identical non-empty old/new pair (#3088 fix round, F1). At this call
	// site the caller (RestoreLockedFieldGuarded) has already decided the
	// change via the guarded UPDATE's affected-row count before ever calling
	// this function, so a string-equality skip here can eat a row the UPDATE
	// legitimately counted as applied -- see
	// TestRestoreLockedFieldGuarded_SliceFieldFormattingOnlyDiffStillRecordsHistory
	// for the reproduction. This subtest pins the function's OWN contract
	// directly: an identical pair must always insert.
	t.Run("identical non-empty old and new values still insert", func(t *testing.T) {
		db, tx := newTx(t)
		if err := recordHistoryTx(context.Background(), tx, "a1", "biography", "same", "same", "revert"); err != nil {
			t.Fatalf("recordHistoryTx: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("committing: %v", err)
		}
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM metadata_changes WHERE artist_id = 'a1'`).Scan(&n); err != nil {
			t.Fatalf("counting rows: %v", err)
		}
		if n != 1 {
			t.Errorf("rows = %d, want 1 -- recordHistoryTx has no no-op skip, unlike HistoryService.Record", n)
		}
	})

	t.Run("an empty old value with an empty new value still inserts", func(t *testing.T) {
		// Mirrors HistoryService.Record's own comment: the skip only fires
		// when oldValue is non-empty, so a fix result whose old and new are
		// both "" must still leave an audit trail rather than being silently
		// dropped by the same guard that skips a genuine no-op.
		db, tx := newTx(t)
		if err := recordHistoryTx(context.Background(), tx, "a1", "biography", "", "", "revert"); err != nil {
			t.Fatalf("recordHistoryTx: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("committing: %v", err)
		}
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM metadata_changes WHERE artist_id = 'a1'`).Scan(&n); err != nil {
			t.Fatalf("counting rows: %v", err)
		}
		if n != 1 {
			t.Errorf("rows = %d, want 1 -- an empty/empty pair must still be recorded", n)
		}
	})

	t.Run("a pre-assigned history ID from context is used as-is", func(t *testing.T) {
		db, tx := newTx(t)
		ctx := ContextWithHistoryID(context.Background(), "preset-id")
		if err := recordHistoryTx(ctx, tx, "a1", "biography", "old", "new", "revert"); err != nil {
			t.Fatalf("recordHistoryTx: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("committing: %v", err)
		}
		var id string
		if err := db.QueryRow(`SELECT id FROM metadata_changes WHERE artist_id = 'a1'`).Scan(&id); err != nil {
			t.Fatalf("reading id: %v", err)
		}
		if id != "preset-id" {
			t.Errorf("id = %q, want the pre-assigned context ID", id)
		}
	})
}

// noDBRepo wraps a Repository and hides any DB accessor, standing in for a
// deployment where the service was built over a repository that owns no raw
// handle.
type noDBRepo struct{ Repository }
