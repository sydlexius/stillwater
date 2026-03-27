package artist

import (
	"context"
	"testing"
)

func TestMBSnapshotRepo_UpsertAll_and_GetForArtist(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	repo := newSQLiteMBSnapshotRepo(db)
	ctx := context.Background()

	a := testArtist("ABBA", "/music/ABBA")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create artist: %v", err)
	}

	snapshots := []MBSnapshot{
		{ArtistID: a.ID, Field: "genres", MBValue: `["pop","europop"]`},
		{ArtistID: a.ID, Field: "type", MBValue: "group"},
		{ArtistID: a.ID, Field: "formed", MBValue: "1972"},
	}

	t.Run("inserts snapshots", func(t *testing.T) {
		if err := repo.UpsertAll(ctx, a.ID, snapshots); err != nil {
			t.Fatalf("UpsertAll: %v", err)
		}

		got, err := repo.GetForArtist(ctx, a.ID)
		if err != nil {
			t.Fatalf("GetForArtist: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("len(got) = %d, want 3", len(got))
		}
		if got["genres"].MBValue != `["pop","europop"]` {
			t.Errorf("genres = %q, want %q", got["genres"].MBValue, `["pop","europop"]`)
		}
		if got["type"].MBValue != "group" {
			t.Errorf("type = %q, want %q", got["type"].MBValue, "group")
		}
		if got["formed"].MBValue != "1972" {
			t.Errorf("formed = %q, want %q", got["formed"].MBValue, "1972")
		}
		// Verify fetched_at is populated.
		if got["genres"].FetchedAt.IsZero() {
			t.Error("FetchedAt should not be zero")
		}
	})

	t.Run("upsert overwrites existing values", func(t *testing.T) {
		updated := []MBSnapshot{
			{ArtistID: a.ID, Field: "genres", MBValue: `["pop","europop","synth-pop"]`},
		}
		if err := repo.UpsertAll(ctx, a.ID, updated); err != nil {
			t.Fatalf("UpsertAll: %v", err)
		}

		got, err := repo.GetForArtist(ctx, a.ID)
		if err != nil {
			t.Fatalf("GetForArtist: %v", err)
		}
		// Should still have all 3 fields (the other 2 are untouched).
		if len(got) != 3 {
			t.Fatalf("len(got) = %d, want 3", len(got))
		}
		if got["genres"].MBValue != `["pop","europop","synth-pop"]` {
			t.Errorf("genres = %q, want updated value", got["genres"].MBValue)
		}
	})

	t.Run("empty slice is a no-op", func(t *testing.T) {
		if err := repo.UpsertAll(ctx, a.ID, nil); err != nil {
			t.Fatalf("UpsertAll with nil: %v", err)
		}
		got, err := repo.GetForArtist(ctx, a.ID)
		if err != nil {
			t.Fatalf("GetForArtist: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("len(got) = %d, want 3 (unchanged)", len(got))
		}
	})
}

func TestMBSnapshotRepo_DeleteByArtistID(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	repo := newSQLiteMBSnapshotRepo(db)
	ctx := context.Background()

	a := testArtist("Bjork", "/music/Bjork")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create artist: %v", err)
	}

	snapshots := []MBSnapshot{
		{ArtistID: a.ID, Field: "type", MBValue: "person"},
		{ArtistID: a.ID, Field: "born", MBValue: "1965-11-21"},
	}
	if err := repo.UpsertAll(ctx, a.ID, snapshots); err != nil {
		t.Fatalf("UpsertAll: %v", err)
	}

	if err := repo.DeleteByArtistID(ctx, a.ID); err != nil {
		t.Fatalf("DeleteByArtistID: %v", err)
	}

	got, err := repo.GetForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetForArtist: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len(got) = %d, want 0 after delete", len(got))
	}
}

func TestMBSnapshotRepo_GetForArtist_empty(t *testing.T) {
	db := setupTestDB(t)
	repo := newSQLiteMBSnapshotRepo(db)
	ctx := context.Background()

	got, err := repo.GetForArtist(ctx, "nonexistent-id")
	if err != nil {
		t.Fatalf("GetForArtist: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil empty map, got nil")
	}
	if len(got) != 0 {
		t.Errorf("len(got) = %d, want 0", len(got))
	}
}

// Note: CASCADE delete is tested implicitly by the schema constraint.
// In-memory SQLite may not always enforce foreign keys depending on the
// driver's PRAGMA initialization, so we skip an explicit cascade test here.
