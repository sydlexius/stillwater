package artist

// Coverage for the content_hash column and the UpdateHashes primitive added
// for issues #2341 and #2349.
//
// The property that matters most here is durability across a rescan. The whole
// point of persisting an image's hashes is that hashing becomes a
// once-per-file cost instead of a once-per-evaluation one; if a routine rescan
// silently cleared the hashes, the recomputation bug would come straight back
// while every test that merely checked "the hash was written" still passed.

import (
	"context"
	"errors"
	"testing"
)

func TestUpdateHashes_RoundTrips(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	repo := newSQLiteImageRepo(db)
	ctx := context.Background()

	a := testArtist("Hash Round Trip", "/music/Hash Round Trip")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := repo.Upsert(ctx, &ArtistImage{
		ArtistID: a.ID, ImageType: "fanart", SlotIndex: 1, Exists: true,
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if err := svc.UpdateImageHashes(ctx, a.ID, "fanart", 1, "0f0f0f0f0f0f0f0f", "sha256-of-bytes"); err != nil {
		t.Fatalf("UpdateImageHashes: %v", err)
	}

	imgs, err := repo.GetForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetForArtist: %v", err)
	}
	if len(imgs) != 1 {
		t.Fatalf("got %d image rows, want 1", len(imgs))
	}
	if imgs[0].PHash != "0f0f0f0f0f0f0f0f" {
		t.Errorf("PHash = %q, want the persisted perceptual hash", imgs[0].PHash)
	}
	if imgs[0].ContentHash != "sha256-of-bytes" {
		t.Errorf("ContentHash = %q, want the persisted content hash", imgs[0].ContentHash)
	}
}

// TestUpdateHashes_PreservesOtherProvenanceColumns is why UpdateHashes exists
// as its own method rather than reusing UpdateProvenance. The backfill hashes
// files that Stillwater did not necessarily write and knows nothing else
// about; routing it through UpdateProvenance would blank the source and format
// of a row that already had them.
func TestUpdateHashes_PreservesOtherProvenanceColumns(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	repo := newSQLiteImageRepo(db)
	ctx := context.Background()

	a := testArtist("Preserve Provenance", "/music/Preserve Provenance")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := repo.Upsert(ctx, &ArtistImage{
		ArtistID: a.ID, ImageType: "thumb", SlotIndex: 0, Exists: true,
		PHash: "1111111111111111", Source: "fanarttv", FileFormat: "jpeg",
		LastWrittenAt: "2026-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Backfill only the content hash.
	if err := svc.UpdateImageHashes(ctx, a.ID, "thumb", 0, "1111111111111111", "new-content-hash"); err != nil {
		t.Fatalf("UpdateImageHashes: %v", err)
	}

	imgs, err := repo.GetForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetForArtist: %v", err)
	}
	got := imgs[0]
	if got.ContentHash != "new-content-hash" {
		t.Errorf("ContentHash = %q; the backfill did not land", got.ContentHash)
	}
	if got.Source != "fanarttv" {
		t.Errorf("Source = %q, want %q; the hash backfill clobbered the recorded source", got.Source, "fanarttv")
	}
	if got.FileFormat != "jpeg" {
		t.Errorf("FileFormat = %q, want %q; the hash backfill clobbered it", got.FileFormat, "jpeg")
	}
	if got.LastWrittenAt != "2026-01-01T00:00:00Z" {
		t.Errorf("LastWrittenAt = %q; the hash backfill clobbered it", got.LastWrittenAt)
	}
}

// TestUpdateHashes_SurvivesRescan is the durability property. UpsertAll is the
// path a library rescan takes; it must re-sync the display fields WITHOUT
// wiping the hashes, or every scan would silently re-arm the recomputation
// bug.
func TestUpdateHashes_SurvivesRescan(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	repo := newSQLiteImageRepo(db)
	ctx := context.Background()

	a := testArtist("Rescan Durability", "/music/Rescan Durability")
	a.FanartExists = true
	a.FanartCount = 2
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := svc.UpdateImageHashes(ctx, a.ID, "fanart", 1, "2222222222222222", "content-hash-1"); err != nil {
		t.Fatalf("UpdateImageHashes: %v", err)
	}

	// Simulate a rescan re-syncing the same image set.
	if err := repo.UpsertAll(ctx, a.ID, extractImageMetadata(a)); err != nil {
		t.Fatalf("UpsertAll (rescan): %v", err)
	}

	imgs, err := repo.GetForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetForArtist: %v", err)
	}
	var found bool
	for _, img := range imgs {
		if img.ImageType != "fanart" || img.SlotIndex != 1 {
			continue
		}
		found = true
		if img.ContentHash != "content-hash-1" {
			t.Errorf("ContentHash = %q after rescan, want it preserved; a rescan that "+
				"clears hashes forces every image to be re-read and re-decoded again", img.ContentHash)
		}
		if img.PHash != "2222222222222222" {
			t.Errorf("PHash = %q after rescan, want it preserved", img.PHash)
		}
	}
	if !found {
		t.Fatal("fanart slot 1 row disappeared across the rescan")
	}
}

// TestUpdateHashes_VanishedRowIsErrNotFound: a slot removed or renumbered by a
// concurrent scan is a benign race, and callers distinguish it by sentinel so
// they can log and continue rather than failing the evaluation.
func TestUpdateHashes_VanishedRowIsErrNotFound(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist("Vanished Slot", "/music/Vanished Slot")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	err := svc.UpdateImageHashes(ctx, a.ID, "fanart", 7, "3333333333333333", "content-hash")
	if err == nil {
		t.Fatal("UpdateImageHashes on a nonexistent slot returned nil; the caller cannot " +
			"distinguish a lost write from a successful one")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v; want it to wrap ErrNotFound so the race can be told apart "+
			"from a real failure", err)
	}
}
