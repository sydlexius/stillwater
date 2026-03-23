package artist

import (
	"context"
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

	t.Run("clamps limit to maximum of 200", func(t *testing.T) {
		// Should not error; limit is silently clamped.
		changes, _, err := svc.List(ctx, artistID, 999, 0)
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		if len(changes) > 200 {
			t.Errorf("len(changes) = %d, want <= 200", len(changes))
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
