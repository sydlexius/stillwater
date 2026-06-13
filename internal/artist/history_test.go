package artist

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

// setupHistoryTestDB opens a pre-migrated test database and returns the
// HistoryService plus the underlying *sql.DB for direct SQL access (e.g.
// seeding parent artist rows required by the FK on metadata_changes.artist_id).
func setupHistoryTestDB(t *testing.T) (*HistoryService, *sql.DB) {
	t.Helper()
	db := newTestDB(t)
	return NewHistoryService(db), db
}

// seedTestArtist inserts a minimal artists row so that history records can
// reference it without violating the FK constraint on metadata_changes.artist_id.
// Uses INSERT OR IGNORE so duplicate calls are idempotent.
func seedTestArtist(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(),
		`INSERT OR IGNORE INTO artists (id, name, sort_name, path) VALUES (?, ?, ?, '')`,
		id, id, id,
	)
	if err != nil {
		t.Fatalf("seedTestArtist(%q): %v", id, err)
	}
}

func TestHistoryService_Record(t *testing.T) {
	t.Parallel()
	svc, db := setupHistoryTestDB(t)
	ctx := context.Background()

	artistID := "artist-001"
	seedTestArtist(t, db, artistID)

	t.Run("records a change successfully", func(t *testing.T) {
		err := svc.Record(ctx, artistID, "biography", "old bio", "new bio", "manual")
		if err != nil {
			t.Fatalf("Record() error = %v", err)
		}

		changes, total, err := svc.List(ctx, artistID, 10, 0)
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		if total != 1 {
			t.Errorf("total = %d, want 1", total)
		}
		if len(changes) != 1 {
			t.Fatalf("len(changes) = %d, want 1", len(changes))
		}
		c := changes[0]
		if c.ArtistID != artistID {
			t.Errorf("ArtistID = %q, want %q", c.ArtistID, artistID)
		}
		if c.Field != "biography" {
			t.Errorf("Field = %q, want %q", c.Field, "biography")
		}
		if c.OldValue != "old bio" {
			t.Errorf("OldValue = %q, want %q", c.OldValue, "old bio")
		}
		if c.NewValue != "new bio" {
			t.Errorf("NewValue = %q, want %q", c.NewValue, "new bio")
		}
		if c.Source != "manual" {
			t.Errorf("Source = %q, want %q", c.Source, "manual")
		}
		if c.ID == "" {
			t.Error("ID should not be empty")
		}
		if c.CreatedAt.IsZero() {
			t.Error("CreatedAt should not be zero")
		}
	})

	t.Run("returns error when artist_id is empty", func(t *testing.T) {
		err := svc.Record(ctx, "", "biography", "old", "new", "manual")
		if err == nil {
			t.Fatal("expected error for empty artist_id, got nil")
		}
	})

	t.Run("returns error when field is empty", func(t *testing.T) {
		err := svc.Record(ctx, artistID, "", "old", "new", "manual")
		if err == nil {
			t.Fatal("expected error for empty field, got nil")
		}
	})

	t.Run("records all source types", func(t *testing.T) {
		artistID2 := "artist-002"
		seedTestArtist(t, db, artistID2)
		sources := []string{
			"manual",
			"provider:musicbrainz",
			"provider:audiodb",
			"rule:missing_mbid",
			"scan",
			"import",
			"revert",
		}
		for _, src := range sources {
			err := svc.Record(ctx, artistID2, "biography", "", "value", src)
			if err != nil {
				t.Errorf("Record() with source %q: error = %v", src, err)
			}
		}
		_, total, err := svc.List(ctx, artistID2, 50, 0)
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		if total != len(sources) {
			t.Errorf("total = %d, want %d", total, len(sources))
		}
	})
}

func TestHistoryService_List(t *testing.T) {
	t.Parallel()
	svc, db := setupHistoryTestDB(t)
	ctx := context.Background()
	artistID := "artist-pag"
	seedTestArtist(t, db, artistID)

	// Insert 15 changes with slight time separation to guarantee ordering.
	for i := 0; i < 15; i++ {
		field := "biography"
		if i%2 == 0 {
			field = "genres"
		}
		if err := svc.Record(ctx, artistID, field, "", "value", "manual"); err != nil {
			t.Fatalf("Record() i=%d: %v", i, err)
		}
	}

	t.Run("returns paginated results", func(t *testing.T) {
		changes, total, err := svc.List(ctx, artistID, 5, 0)
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		if total != 15 {
			t.Errorf("total = %d, want 15", total)
		}
		if len(changes) != 5 {
			t.Errorf("len(changes) = %d, want 5", len(changes))
		}
	})

	t.Run("second page returns correct records", func(t *testing.T) {
		changes, total, err := svc.List(ctx, artistID, 5, 5)
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		if total != 15 {
			t.Errorf("total = %d, want 15", total)
		}
		if len(changes) != 5 {
			t.Errorf("len(changes) = %d, want 5", len(changes))
		}
	})

	t.Run("last page returns remaining records", func(t *testing.T) {
		changes, total, err := svc.List(ctx, artistID, 5, 10)
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		if total != 15 {
			t.Errorf("total = %d, want 15", total)
		}
		if len(changes) != 5 {
			t.Errorf("len(changes) = %d, want 5", len(changes))
		}
	})

	t.Run("offset beyond total returns empty slice", func(t *testing.T) {
		changes, total, err := svc.List(ctx, artistID, 5, 100)
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		if total != 15 {
			t.Errorf("total = %d, want 15", total)
		}
		if len(changes) != 0 {
			t.Errorf("len(changes) = %d, want 0", len(changes))
		}
	})

	t.Run("returns empty slice for artist with no changes", func(t *testing.T) {
		changes, total, err := svc.List(ctx, "no-such-artist", 10, 0)
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		if total != 0 {
			t.Errorf("total = %d, want 0", total)
		}
		if len(changes) != 0 {
			t.Errorf("len(changes) = %d, want 0", len(changes))
		}
	})

	t.Run("returns error for empty artist_id", func(t *testing.T) {
		_, _, err := svc.List(ctx, "", 10, 0)
		if err == nil {
			t.Fatal("expected error for empty artist_id, got nil")
		}
	})

	t.Run("clamps limit to maximum of 500", func(t *testing.T) {
		// Should not error; limit is silently clamped.
		changes, _, err := svc.List(ctx, artistID, 999, 0)
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		if len(changes) > 500 {
			t.Errorf("len(changes) = %d, want <= 500", len(changes))
		}
	})

	t.Run("results are ordered most recent first", func(t *testing.T) {
		changes, _, err := svc.List(ctx, artistID, 15, 0)
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		for i := 1; i < len(changes); i++ {
			if changes[i].CreatedAt.After(changes[i-1].CreatedAt) {
				t.Errorf("changes[%d].CreatedAt (%v) is after changes[%d].CreatedAt (%v); want DESC order",
					i, changes[i].CreatedAt, i-1, changes[i-1].CreatedAt)
			}
		}
	})

	t.Run("does not return changes for other artists", func(t *testing.T) {
		otherID := "artist-other"
		seedTestArtist(t, db, otherID)
		if err := svc.Record(ctx, otherID, "biography", "", "value", "manual"); err != nil {
			t.Fatalf("Record() for other artist: %v", err)
		}
		changes, total, err := svc.List(ctx, artistID, 50, 0)
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		if total != 15 {
			t.Errorf("total = %d after inserting for other artist, want 15", total)
		}
		for _, c := range changes {
			if c.ArtistID != artistID {
				t.Errorf("change.ArtistID = %q, want %q", c.ArtistID, artistID)
			}
		}
	})
}

func TestHistoryService_RecordPreservesTimestamp(t *testing.T) {
	t.Parallel()
	svc, db := setupHistoryTestDB(t)
	ctx := context.Background()
	seedTestArtist(t, db, "artist-ts")

	before := time.Now().UTC().Truncate(time.Second)
	if err := svc.Record(ctx, "artist-ts", "biography", "", "value", "manual"); err != nil {
		t.Fatalf("Record() error = %v", err)
	}
	after := time.Now().UTC().Add(time.Second)

	changes, _, err := svc.List(ctx, "artist-ts", 1, 0)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(changes) == 0 {
		t.Fatal("expected 1 change, got 0")
	}
	ts := changes[0].CreatedAt
	if ts.Before(before) || ts.After(after) {
		t.Errorf("CreatedAt %v not within expected range [%v, %v]", ts, before, after)
	}
}

func TestHistoryService_GetByID(t *testing.T) {
	t.Parallel()
	svc, db := setupHistoryTestDB(t)
	ctx := context.Background()
	seedTestArtist(t, db, "artist-get")

	t.Run("returns recorded change", func(t *testing.T) {
		if err := svc.Record(ctx, "artist-get", "biography", "old", "new", "manual"); err != nil {
			t.Fatalf("Record() error = %v", err)
		}
		changes, _, err := svc.List(ctx, "artist-get", 1, 0)
		if err != nil || len(changes) == 0 {
			t.Fatalf("List() error = %v, len = %d", err, len(changes))
		}

		got, err := svc.GetByID(ctx, changes[0].ID)
		if err != nil {
			t.Fatalf("GetByID() error = %v", err)
		}
		if got.Field != "biography" {
			t.Errorf("Field = %q, want %q", got.Field, "biography")
		}
		if got.OldValue != "old" {
			t.Errorf("OldValue = %q, want %q", got.OldValue, "old")
		}
	})

	t.Run("returns ErrChangeNotFound for missing ID", func(t *testing.T) {
		_, err := svc.GetByID(ctx, "nonexistent-id")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !errors.Is(err, ErrChangeNotFound) {
			t.Errorf("error = %v, want ErrChangeNotFound", err)
		}
	})

	t.Run("returns error for empty ID", func(t *testing.T) {
		_, err := svc.GetByID(ctx, "")
		if err == nil {
			t.Fatal("expected error for empty ID, got nil")
		}
	})
}

// TestHistoryService_RecordUsesContextHistoryID verifies that Record honors a
// pre-assigned change ID supplied via ContextWithHistoryID. This locks in the
// race-free revert flow: the handler pre-generates the change ID, injects it
// via context, then fetches the resulting row by ID instead of doing a "most
// recent revert for field X" lookup that races against concurrent writers.
func TestHistoryService_RecordUsesContextHistoryID(t *testing.T) {
	t.Parallel()
	svc, db := setupHistoryTestDB(t)
	seedTestArtist(t, db, "artist-ctxid")
	preID := "00000000-0000-4000-8000-000000000abc"

	ctx := ContextWithHistoryID(context.Background(), preID)
	if err := svc.Record(ctx, "artist-ctxid", "biography", "old", "new", "revert"); err != nil {
		t.Fatalf("Record() error = %v", err)
	}

	got, err := svc.GetByID(context.Background(), preID)
	if err != nil {
		t.Fatalf("GetByID(preID) error = %v", err)
	}
	if got.ID != preID {
		t.Errorf("recorded ID = %q, want pre-assigned %q", got.ID, preID)
	}

	// A second Record call without a context ID must generate a fresh UUID,
	// not collide with the pre-assigned one.
	if err := svc.Record(context.Background(), "artist-ctxid", "biography", "new", "newer", "manual"); err != nil {
		t.Fatalf("second Record() error = %v", err)
	}
	changes, _, err := svc.List(context.Background(), "artist-ctxid", 10, 0)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("len(changes) = %d, want 2", len(changes))
	}
}

// TestHistoryService_ListGlobalOrderRFC3339 verifies that ListGlobal
// returns rows in true chronological order. Migration 004 normalised any
// pre-existing space-separator legacy rows to RFC3339, so the production
// table holds a single uniform format and a plain TEXT ORDER BY is
// monotonic without datetime() normalization. This test guards the order
// against an accidental sort regression.
func TestHistoryService_ListGlobalOrderRFC3339(t *testing.T) {
	t.Parallel()
	svc, db := setupHistoryTestDB(t)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO artists (id, name, sort_name, path) VALUES (?, ?, ?, '')`,
		"artist-mixed", "Mixed", "Mixed",
	); err != nil {
		t.Fatalf("inserting artist: %v", err)
	}

	// Three RFC3339 rows; expected DESC order (newest first):
	// newer, middle, oldest.
	rows := []struct {
		id, createdAt string
	}{
		{"oldest", "2024-01-15T08:00:00Z"},
		{"middle", "2024-01-15T09:00:00Z"},
		{"newer", "2024-01-15T10:00:00Z"},
	}
	for _, r := range rows {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO metadata_changes (id, artist_id, field, old_value, new_value, source, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			r.id, "artist-mixed", "biography", "", "v", "manual", r.createdAt,
		); err != nil {
			t.Fatalf("inserting %s: %v", r.id, err)
		}
	}

	changes, total, err := svc.ListGlobal(ctx, GlobalHistoryFilter{Limit: 50})
	if err != nil {
		t.Fatalf("ListGlobal() error = %v", err)
	}
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}
	if len(changes) != 3 {
		t.Fatalf("len(changes) = %d, want 3", len(changes))
	}

	want := []string{"newer", "middle", "oldest"}
	got := []string{changes[0].ID, changes[1].ID, changes[2].ID}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("position %d: got %q, want %q (full order: %v)", i, got[i], want[i], got)
		}
	}
}

func TestHistoryService_ListGlobal(t *testing.T) {
	t.Parallel()
	svc, db := setupHistoryTestDB(t)
	ctx := context.Background()

	// Need artists in the database for the JOIN.
	for _, a := range []struct{ id, name string }{
		{"artist-a", "Alpha"},
		{"artist-b", "Beta"},
	} {
		_, err := db.ExecContext(ctx,
			`INSERT INTO artists (id, name, sort_name, path) VALUES (?, ?, ?, '')`,
			a.id, a.name, a.name)
		if err != nil {
			t.Fatalf("inserting artist %s: %v", a.id, err)
		}
	}

	// Record changes for both artists.
	if err := svc.Record(ctx, "artist-a", "biography", "", "bio A", "manual"); err != nil {
		t.Fatalf("recording artist-a biography change: %v", err)
	}
	if err := svc.Record(ctx, "artist-a", "genres", "", "Rock", "provider:musicbrainz"); err != nil {
		t.Fatalf("recording artist-a genres change: %v", err)
	}
	if err := svc.Record(ctx, "artist-b", "biography", "", "bio B", "scan"); err != nil {
		t.Fatalf("recording artist-b biography change: %v", err)
	}

	t.Run("returns all changes with artist names", func(t *testing.T) {
		changes, total, err := svc.ListGlobal(ctx, GlobalHistoryFilter{Limit: 50})
		if err != nil {
			t.Fatalf("ListGlobal() error = %v", err)
		}
		if total != 3 {
			t.Errorf("total = %d, want 3", total)
		}
		if len(changes) != 3 {
			t.Errorf("len(changes) = %d, want 3", len(changes))
		}
		// Verify artist names are populated.
		for _, c := range changes {
			if c.ArtistName == "" {
				t.Errorf("ArtistName empty for change %s", c.ID)
			}
		}
	})

	t.Run("filters by artist_id", func(t *testing.T) {
		changes, total, err := svc.ListGlobal(ctx, GlobalHistoryFilter{
			ArtistID: "artist-a",
			Limit:    50,
		})
		if err != nil {
			t.Fatalf("ListGlobal() error = %v", err)
		}
		if total != 2 {
			t.Errorf("total = %d, want 2", total)
		}
		if len(changes) != 2 {
			t.Fatalf("len(changes) = %d, want 2", len(changes))
		}
		for _, c := range changes {
			if c.ArtistID != "artist-a" {
				t.Errorf("ArtistID = %q, want artist-a", c.ArtistID)
			}
		}
	})

	t.Run("filters by field", func(t *testing.T) {
		changes, total, err := svc.ListGlobal(ctx, GlobalHistoryFilter{
			Fields: []string{"biography"},
			Limit:  50,
		})
		if err != nil {
			t.Fatalf("ListGlobal() error = %v", err)
		}
		if total != 2 {
			t.Errorf("total = %d, want 2", total)
		}
		if len(changes) != 2 {
			t.Fatalf("len(changes) = %d, want 2", len(changes))
		}
		for _, c := range changes {
			if c.Field != "biography" {
				t.Errorf("Field = %q, want biography", c.Field)
			}
		}
	})

	t.Run("filters by source", func(t *testing.T) {
		changes, total, err := svc.ListGlobal(ctx, GlobalHistoryFilter{
			Sources: []string{"scan"},
			Limit:   50,
		})
		if err != nil {
			t.Fatalf("ListGlobal() error = %v", err)
		}
		if total != 1 {
			t.Errorf("total = %d, want 1", total)
		}
		if len(changes) != 1 {
			t.Fatalf("len(changes) = %d, want 1", len(changes))
		}
		if changes[0].Source != "scan" {
			t.Errorf("Source = %q, want scan", changes[0].Source)
		}
	})

	t.Run("pagination works", func(t *testing.T) {
		changes, total, err := svc.ListGlobal(ctx, GlobalHistoryFilter{
			Limit:  2,
			Offset: 0,
		})
		if err != nil {
			t.Fatalf("ListGlobal() error = %v", err)
		}
		if total != 3 {
			t.Errorf("total = %d, want 3", total)
		}
		if len(changes) != 2 {
			t.Errorf("len(changes) = %d, want 2", len(changes))
		}
	})

	t.Run("empty result returns empty slice", func(t *testing.T) {
		changes, total, err := svc.ListGlobal(ctx, GlobalHistoryFilter{
			ArtistID: "nonexistent",
			Limit:    50,
		})
		if err != nil {
			t.Fatalf("ListGlobal() error = %v", err)
		}
		if total != 0 {
			t.Errorf("total = %d, want 0", total)
		}
		if changes == nil {
			t.Error("expected empty slice, got nil")
		}
	})

	t.Run("filters by source prefix", func(t *testing.T) {
		changes, total, err := svc.ListGlobal(ctx, GlobalHistoryFilter{
			SourcePrefixes: []string{"provider:"},
			Limit:          50,
		})
		if err != nil {
			t.Fatalf("ListGlobal() error = %v", err)
		}
		if total != 1 {
			t.Errorf("total = %d, want 1", total)
		}
		if len(changes) != 1 {
			t.Fatalf("len(changes) = %d, want 1", len(changes))
		}
		if changes[0].Source != "provider:musicbrainz" {
			t.Errorf("Source = %q, want provider:musicbrainz", changes[0].Source)
		}
	})

	t.Run("filters by date range", func(t *testing.T) {
		// All test data was inserted moments ago, so filtering with a range
		// that spans "now" should return all 3 changes.
		now := time.Now().UTC()
		from := now.Add(-1 * time.Minute)
		to := now.Add(1 * time.Minute)
		changes, total, err := svc.ListGlobal(ctx, GlobalHistoryFilter{
			From:  from,
			To:    to,
			Limit: 50,
		})
		if err != nil {
			t.Fatalf("ListGlobal() error = %v", err)
		}
		if total != 3 {
			t.Errorf("total = %d, want 3", total)
		}
		if len(changes) != 3 {
			t.Errorf("len(changes) = %d, want 3", len(changes))
		}
	})

	t.Run("date range excludes future changes", func(t *testing.T) {
		// Use a range entirely in the past to exclude all changes.
		past := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
		changes, total, err := svc.ListGlobal(ctx, GlobalHistoryFilter{
			From:  past,
			To:    past.Add(1 * time.Hour),
			Limit: 50,
		})
		if err != nil {
			t.Fatalf("ListGlobal() error = %v", err)
		}
		if total != 0 {
			t.Errorf("total = %d, want 0", total)
		}
		if len(changes) != 0 {
			t.Errorf("len(changes) = %d, want 0", len(changes))
		}
	})

	t.Run("combines prefix and date range filters", func(t *testing.T) {
		now := time.Now().UTC()
		changes, total, err := svc.ListGlobal(ctx, GlobalHistoryFilter{
			SourcePrefixes: []string{"provider:"},
			From:           now.Add(-1 * time.Minute),
			To:             now.Add(1 * time.Minute),
			Limit:          50,
		})
		if err != nil {
			t.Fatalf("ListGlobal() error = %v", err)
		}
		if total != 1 {
			t.Errorf("total = %d, want 1", total)
		}
		if len(changes) != 1 {
			t.Fatalf("len(changes) = %d, want 1", len(changes))
		}
		if changes[0].Source != "provider:musicbrainz" {
			t.Errorf("Source = %q, want provider:musicbrainz", changes[0].Source)
		}
	})
}

// TestHistoryService_ListGlobal_PerFieldLimit verifies that the windowed CTE
// path (PerFieldLimit > 0) returns at most N rows per field and that a field
// with fewer than N entries is not truncated. This exercises the correctness
// guarantee that a single heavily-edited field cannot starve other fields.
func TestHistoryService_ListGlobal_PerFieldLimit(t *testing.T) {
	t.Parallel()
	svc, db := setupHistoryTestDB(t)
	ctx := context.Background()

	seedTestArtist(t, db, "artist-pfl")

	// biography: 7 changes (more than the cap of 6)
	for i := 0; i < 7; i++ {
		if err := svc.Record(ctx, "artist-pfl", "biography", "", "bio", "manual"); err != nil {
			t.Fatalf("recording biography change %d: %v", i, err)
		}
	}
	// genres: 2 changes (fewer than the cap)
	for i := 0; i < 2; i++ {
		if err := svc.Record(ctx, "artist-pfl", "genres", "", "Rock", "manual"); err != nil {
			t.Fatalf("recording genres change %d: %v", i, err)
		}
	}

	changes, total, err := svc.ListGlobal(ctx, GlobalHistoryFilter{
		ArtistID:      "artist-pfl",
		Fields:        []string{"biography", "genres"},
		PerFieldLimit: 6,
		Limit:         1,
		Offset:        10,
	})
	if err != nil {
		t.Fatalf("ListGlobal(PerFieldLimit=6) error = %v", err)
	}
	// Windowed path returns total=0 (no meaningful pagination total).
	if total != 0 {
		t.Errorf("total = %d, want 0 for per-field-capped path", total)
	}

	// Count per field.
	bioCount := 0
	genreCount := 0
	for _, c := range changes {
		switch c.Field {
		case "biography":
			bioCount++
		case "genres":
			genreCount++
		}
	}
	// biography: 7 entries, cap=6 -> expect exactly 6
	if bioCount != 6 {
		t.Errorf("biography count = %d, want 6 (cap at PerFieldLimit)", bioCount)
	}
	// genres: 2 entries, cap=6 -> expect all 2
	if genreCount != 2 {
		t.Errorf("genres count = %d, want 2 (not truncated when under cap)", genreCount)
	}
	// Total: 6 + 2 = 8
	if len(changes) != 8 {
		t.Errorf("len(changes) = %d, want 8 (6 biography + 2 genres)", len(changes))
	}
}

func TestIsTrackableField(t *testing.T) {
	t.Parallel()
	trackable := []string{"biography", "genres", "styles", "moods", "formed", "born", "disbanded", "died", "years_active", "type", "gender"}
	for _, f := range trackable {
		if !IsTrackableField(f) {
			t.Errorf("IsTrackableField(%q) = false, want true", f)
		}
	}

	notTrackable := []string{"", "name", "id", "path", "nonexistent"}
	for _, f := range notTrackable {
		if IsTrackableField(f) {
			t.Errorf("IsTrackableField(%q) = true, want false", f)
		}
	}
}
