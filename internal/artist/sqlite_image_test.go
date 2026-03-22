package artist

import (
	"context"
	"strings"
	"testing"
)

func TestUpsert_ProvenanceFields(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	// Create an artist so foreign key constraint is satisfied.
	a := testArtist("Radiohead", "/music/Radiohead")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Upsert an image with provenance fields populated.
	img := &ArtistImage{
		ArtistID:      a.ID,
		ImageType:     "thumb",
		SlotIndex:     0,
		Exists:        true,
		LowRes:        false,
		Width:         1000,
		Height:        1000,
		PHash:         "a1b2c3d4e5f6g7h8",
		FileFormat:    "jpeg",
		Source:        "fanarttv",
		LastWrittenAt: "2026-03-21T12:00:00Z",
	}
	repo := newSQLiteImageRepo(db)
	if err := repo.Upsert(ctx, img); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Read it back and verify all provenance fields.
	images, err := repo.GetForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetForArtist: %v", err)
	}
	if len(images) != 1 {
		t.Fatalf("expected 1 image, got %d", len(images))
	}

	got := images[0]
	if got.PHash != "a1b2c3d4e5f6g7h8" {
		t.Errorf("PHash = %q, want %q", got.PHash, "a1b2c3d4e5f6g7h8")
	}
	if got.FileFormat != "jpeg" {
		t.Errorf("FileFormat = %q, want %q", got.FileFormat, "jpeg")
	}
	if got.Source != "fanarttv" {
		t.Errorf("Source = %q, want %q", got.Source, "fanarttv")
	}
	if got.LastWrittenAt != "2026-03-21T12:00:00Z" {
		t.Errorf("LastWrittenAt = %q, want %q", got.LastWrittenAt, "2026-03-21T12:00:00Z")
	}
}

func TestUpdateProvenance(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	// Create an artist and an initial image row without provenance.
	a := testArtist("Tool", "/music/Tool")
	a.ThumbExists = true
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Verify the initial row exists but has empty provenance.
	repo := newSQLiteImageRepo(db)
	images, err := repo.GetForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetForArtist: %v", err)
	}
	if len(images) != 1 {
		t.Fatalf("expected 1 image, got %d", len(images))
	}
	if images[0].PHash != "" {
		t.Errorf("initial PHash should be empty, got %q", images[0].PHash)
	}
	if images[0].LastWrittenAt != "" {
		t.Errorf("initial LastWrittenAt should be empty, got %q", images[0].LastWrittenAt)
	}

	// Now update provenance fields via the targeted update.
	err = svc.UpdateImageProvenance(ctx, a.ID, "thumb", 0,
		"ff00ff00ff00ff00", "musicbrainz", "jpeg", "2026-03-21T15:30:00Z")
	if err != nil {
		t.Fatalf("UpdateImageProvenance: %v", err)
	}

	// Read back and verify provenance was updated.
	images, err = repo.GetForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetForArtist after update: %v", err)
	}
	if len(images) != 1 {
		t.Fatalf("expected 1 image, got %d", len(images))
	}

	got := images[0]
	if got.PHash != "ff00ff00ff00ff00" {
		t.Errorf("PHash = %q, want %q", got.PHash, "ff00ff00ff00ff00")
	}
	if got.Source != "musicbrainz" {
		t.Errorf("Source = %q, want %q", got.Source, "musicbrainz")
	}
	if got.FileFormat != "jpeg" {
		t.Errorf("FileFormat = %q, want %q", got.FileFormat, "jpeg")
	}
	if got.LastWrittenAt != "2026-03-21T15:30:00Z" {
		t.Errorf("LastWrittenAt = %q, want %q", got.LastWrittenAt, "2026-03-21T15:30:00Z")
	}

	// Verify display fields were not affected by the provenance update.
	if !got.Exists {
		t.Error("Exists should still be true after provenance update")
	}
	if got.ImageType != "thumb" {
		t.Errorf("ImageType = %q, want %q", got.ImageType, "thumb")
	}
}

func TestUpdateProvenance_NoRow(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	// Create an artist with no images.
	a := testArtist("Deftones", "/music/Deftones")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// UpdateProvenance on a non-existent row should return an error indicating
	// no matching row was found.
	err := svc.UpdateImageProvenance(ctx, a.ID, "thumb", 0,
		"deadbeef", "user", "png", "2026-03-21T10:00:00Z")
	if err == nil {
		t.Fatal("UpdateImageProvenance on missing row should return an error")
	}
	if !strings.Contains(err.Error(), "no image row found") {
		t.Errorf("expected 'no image row found' error, got: %v", err)
	}

	// Verify no rows were created (update only, not insert).
	repo := newSQLiteImageRepo(db)
	images, err := repo.GetForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetForArtist: %v", err)
	}
	if len(images) != 0 {
		t.Errorf("expected 0 images (update should not create rows), got %d", len(images))
	}
}

func TestUpsertAll_PreservesProvenance(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := newSQLiteImageRepo(db)

	// Create an artist.
	svc := NewService(db)
	a := testArtist("Nine Inch Nails", "/music/Nine Inch Nails")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Step 1: UpsertAll to create the initial image row (display fields only).
	images := []ArtistImage{
		{
			ArtistID:  a.ID,
			ImageType: "fanart",
			SlotIndex: 0,
			Exists:    true,
		},
	}
	if err := repo.UpsertAll(ctx, a.ID, images); err != nil {
		t.Fatalf("UpsertAll (initial): %v", err)
	}

	// Step 2: Set provenance via UpdateProvenance (the normal flow after image save).
	if err := repo.UpdateProvenance(ctx, a.ID, "fanart", 0,
		"1234567890abcdef", "fanarttv", "jpeg", "2026-01-15T08:00:00Z"); err != nil {
		t.Fatalf("UpdateProvenance: %v", err)
	}

	// Step 3: Call UpsertAll again with only display fields (simulates an
	// artistService.Update call). Provenance should survive.
	images[0].LowRes = true // change a display field
	if err := repo.UpsertAll(ctx, a.ID, images); err != nil {
		t.Fatalf("UpsertAll (second): %v", err)
	}

	// Read back and verify provenance was preserved.
	got, err := repo.GetForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetForArtist: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 image, got %d", len(got))
	}
	if got[0].LastWrittenAt != "2026-01-15T08:00:00Z" {
		t.Errorf("LastWrittenAt = %q, want %q", got[0].LastWrittenAt, "2026-01-15T08:00:00Z")
	}
	if got[0].PHash != "1234567890abcdef" {
		t.Errorf("PHash = %q, want %q", got[0].PHash, "1234567890abcdef")
	}
	if got[0].Source != "fanarttv" {
		t.Errorf("Source = %q, want %q", got[0].Source, "fanarttv")
	}
	if got[0].FileFormat != "jpeg" {
		t.Errorf("FileFormat = %q, want %q", got[0].FileFormat, "jpeg")
	}
	// Verify display field was updated.
	if !got[0].LowRes {
		t.Error("LowRes should be true after second UpsertAll")
	}
}
