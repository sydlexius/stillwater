package database

import (
	"context"
	"path/filepath"
	"testing"
)

// The version probes exist for read-only inspection bootstraps (the
// -lock-damage-dry-run preview) that must refuse a behind-on-migrations
// database rather than migrate it. Their contract is therefore that they
// mutate NOTHING: AppliedMigrationVersion in particular must answer 0 for a
// tracker-less database instead of creating the tracker the way goose would.

func TestAppliedMigrationVersion(t *testing.T) {
	ctx := context.Background()

	t.Run("no tracker table answers zero without creating it", func(t *testing.T) {
		db, err := Open(filepath.Join(t.TempDir(), "bare.db"))
		if err != nil {
			t.Fatalf("opening db: %v", err)
		}
		defer func() { _ = db.Close() }()

		v, err := AppliedMigrationVersion(ctx, db)
		if err != nil {
			t.Fatalf("AppliedMigrationVersion: %v", err)
		}
		if v != 0 {
			t.Errorf("version = %d, want 0 on a tracker-less database", v)
		}
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='goose_db_version'`).Scan(&n); err != nil {
			t.Fatalf("probing sqlite_master: %v", err)
		}
		if n != 0 {
			t.Error("the probe CREATED goose_db_version; it must be read-only")
		}
	})

	t.Run("migrated database answers the embedded latest", func(t *testing.T) {
		db, err := Open(filepath.Join(t.TempDir(), "migrated.db"))
		if err != nil {
			t.Fatalf("opening db: %v", err)
		}
		defer func() { _ = db.Close() }()
		if err := Migrate(db); err != nil {
			t.Fatalf("migrating: %v", err)
		}

		applied, err := AppliedMigrationVersion(ctx, db)
		if err != nil {
			t.Fatalf("AppliedMigrationVersion: %v", err)
		}
		latest, err := LatestMigrationVersion()
		if err != nil {
			t.Fatalf("LatestMigrationVersion: %v", err)
		}
		if applied != latest || latest == 0 {
			t.Errorf("applied = %d, latest = %d; a freshly migrated database must be at the embedded latest", applied, latest)
		}
	})

	t.Run("rolled-back tracker reads as behind", func(t *testing.T) {
		db, err := Open(filepath.Join(t.TempDir(), "behind.db"))
		if err != nil {
			t.Fatalf("opening db: %v", err)
		}
		defer func() { _ = db.Close() }()
		if err := Migrate(db); err != nil {
			t.Fatalf("migrating: %v", err)
		}
		if _, err := db.ExecContext(ctx,
			`DELETE FROM goose_db_version WHERE version_id > (SELECT MAX(version_id) - 2 FROM goose_db_version)`); err != nil {
			t.Fatalf("rolling back tracker: %v", err)
		}

		applied, err := AppliedMigrationVersion(ctx, db)
		if err != nil {
			t.Fatalf("AppliedMigrationVersion: %v", err)
		}
		latest, err := LatestMigrationVersion()
		if err != nil {
			t.Fatalf("LatestMigrationVersion: %v", err)
		}
		if applied >= latest {
			t.Errorf("applied = %d, latest = %d; the rolled-back tracker must read as behind", applied, latest)
		}
	})

	t.Run("closed database surfaces the error", func(t *testing.T) {
		db, err := Open(filepath.Join(t.TempDir(), "closed.db"))
		if err != nil {
			t.Fatalf("opening db: %v", err)
		}
		_ = db.Close()
		if _, err := AppliedMigrationVersion(ctx, db); err == nil {
			t.Error("want an error from a closed database, got nil")
		}
	})
}
