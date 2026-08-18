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

// noDBRepo wraps a Repository and hides any DB accessor, standing in for a
// deployment where the service was built over a repository that owns no raw
// handle.
type noDBRepo struct{ Repository }
