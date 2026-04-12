package artist

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/database"
)

func setupHistoryTestDB(t *testing.T) *HistoryService {
	t.Helper()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("migrating test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewHistoryService(db)
}

func TestHistoryService_Record(t *testing.T) {
	svc := setupHistoryTestDB(t)
	ctx := context.Background()

	artistID := "artist-001"

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
	svc := setupHistoryTestDB(t)
	ctx := context.Background()
	artistID := "artist-pag"

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
	svc := setupHistoryTestDB(t)
	ctx := context.Background()

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
	svc := setupHistoryTestDB(t)
	ctx := context.Background()

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
	svc := setupHistoryTestDB(t)
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

// TestHistoryService_ListGlobalOrderMixedTimestampFormats verifies that
// ListGlobal returns rows in true chronological order even when created_at
// values are stored in different formats. SQLite's metadata_changes column
// holds RFC 3339 strings for new rows ("2024-01-15T10:00:00Z") but legacy
// rows wrote the SQLite default ("2024-01-15 11:00:00"). Lexicographic sort
// puts the space-separated form after any RFC 3339 form ('T' < ' ' is false:
// ' ' = 0x20, 'T' = 0x54), so a raw ORDER BY mc.created_at would invert
// real-world ordering. The query wraps both the WHERE bounds and the
// ORDER BY in datetime() to normalise both representations.
func TestHistoryService_ListGlobalOrderMixedTimestampFormats(t *testing.T) {
	svc := setupHistoryTestDB(t)
	ctx := context.Background()
	db := svc.repo.(*sqliteHistoryRepo).db

	if _, err := db.ExecContext(ctx,
		`INSERT INTO artists (id, name, sort_name, path) VALUES (?, ?, ?, '')`,
		"artist-mixed", "Mixed", "Mixed",
	); err != nil {
		t.Fatalf("inserting artist: %v", err)
	}

	// Insert three rows directly so we can control the created_at format.
	// Expected DESC order (newest first): newer-rfc, middle-sqlite, oldest-rfc.
	rows := []struct {
		id, createdAt string
	}{
		{"oldest-rfc", "2024-01-15T08:00:00Z"},
		{"middle-sqlite", "2024-01-15 09:00:00"}, // legacy SQLite format
		{"newer-rfc", "2024-01-15T10:00:00Z"},
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

	want := []string{"newer-rfc", "middle-sqlite", "oldest-rfc"}
	got := []string{changes[0].ID, changes[1].ID, changes[2].ID}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("position %d: got %q, want %q (full order: %v)", i, got[i], want[i], got)
		}
	}
}

func TestHistoryService_ListGlobal(t *testing.T) {
	svc := setupHistoryTestDB(t)
	ctx := context.Background()

	// Need artists in the database for the JOIN.
	db := svc.repo.(*sqliteHistoryRepo).db
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

func TestIsTrackableField(t *testing.T) {
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
