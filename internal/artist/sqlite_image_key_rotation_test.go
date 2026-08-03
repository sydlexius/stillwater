package artist

import (
	"context"
	"testing"
)

// TestUpsert_DoesNotRotateThePrimaryKey is the regression guard for #2643.
//
// Upsert fills an empty ID with a freshly generated UUID before executing, and
// its conflict clause included `id = excluded.id`. A refresh-shaped call -- one
// that identifies the row by (artist_id, image_type, slot_index) and leaves ID
// empty, which is how every reconcile writes -- therefore ROTATED the primary
// key of an existing row.
//
// The row survived intact in every other respect, which is what made this
// invisible: nothing looked broken. But SetLock matches on `id` alone, and
// since #2639 made SetLock the exclusive owner of lock state, it is the only
// path that can set or clear a lock. An operator with an artist page open
// holds the pre-refresh ID; a background refresh rotates it; the lock toggle
// then resolves to zero rows and 404s, and the lock is silently never applied.
// A lock is what protects pinned artwork from the auto-fix rules that delete
// files, so a lock that quietly fails to apply is the worst shape of this bug.
func TestUpsert_DoesNotRotateThePrimaryKey(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	ctx := context.Background()
	svc := NewService(db)
	repo := newSQLiteImageRepo(db)

	a := testArtist("Key Rotation", "/music/Key Rotation")
	a.ThumbExists = true
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	images, err := repo.GetForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetForArtist: %v", err)
	}
	if len(images) != 1 {
		t.Fatalf("expected 1 image row, got %d", len(images))
	}
	origID := images[0].ID
	if origID == "" {
		t.Fatal("precondition: the seeded row must carry an ID for rotation to be observable")
	}

	// A refresh-shaped write: addressed by (artist_id, image_type, slot_index),
	// carrying no ID of its own. This is the shape every reconcile uses.
	if err := repo.Upsert(ctx, &ArtistImage{
		ArtistID:  a.ID,
		ImageType: "thumb",
		SlotIndex: 0,
		Exists:    true,
		Width:     800,
		Height:    800,
	}); err != nil {
		t.Fatalf("Upsert (refresh-shaped): %v", err)
	}

	images, err = repo.GetForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetForArtist after upsert: %v", err)
	}
	if len(images) != 1 {
		t.Fatalf("expected the upsert to update in place, got %d rows", len(images))
	}
	if images[0].ID != origID {
		t.Errorf("upsert rotated the primary key: %s -> %s; every ID a caller holds is now stale",
			origID, images[0].ID)
	}

	// The consequence, asserted end to end rather than inferred: the ID a
	// caller captured before the refresh must still address the row. Without
	// this the test would pin the mechanism while saying nothing about the
	// operator-visible failure it causes.
	if err := repo.SetLock(ctx, origID, true); err != nil {
		t.Fatalf("SetLock with the pre-refresh ID failed, so an operator's lock toggle would 404: %v", err)
	}

	images, err = repo.GetForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetForArtist after SetLock: %v", err)
	}
	if !images[0].Locked {
		t.Error("the lock did not apply, so pinned artwork is left unprotected from the auto-fix rules")
	}

	// The upsert must still have done its job -- a fix that stopped updating
	// the row entirely would satisfy every assertion above.
	if images[0].Width != 800 || images[0].Height != 800 {
		t.Errorf("the upsert did not update the row: geometry = %dx%d, want 800x800",
			images[0].Width, images[0].Height)
	}
}
