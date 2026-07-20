package artist

import (
	"context"
	"strings"
	"testing"
)

func TestUpsert_ProvenanceFields(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
		"ff00ff00ff00ff00", "sha-thumb", "musicbrainz", "jpeg", "2026-03-21T15:30:00Z")
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
	if got.ContentHash != "sha-thumb" {
		t.Errorf("ContentHash = %q, want %q", got.ContentHash, "sha-thumb")
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
	t.Parallel()
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
		"deadbeef", "sha-missing", "user", "png", "2026-03-21T10:00:00Z")
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

func TestNewestWriteTimesByArtist(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	repo := newSQLiteImageRepo(db)

	// Seed the two libraries the artists will belong to so Service.Create's
	// derive-source membership insert lands a real artist_libraries row.
	for _, lid := range []string{"lib-mtime", "lib-other"} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO libraries (id, name, type, source, created_at, updated_at)
				VALUES (?, ?, 'regular', 'manual', datetime('now'), datetime('now'))`,
			lid, lid); err != nil {
			t.Fatalf("seeding library %s: %v", lid, err)
		}
	}

	// Create two artists in the same library with different write times.
	artistA := testArtist("Artist A", "/music/Artist A")
	artistA.LibraryID = "lib-mtime"
	artistA.ThumbExists = true
	if err := svc.Create(ctx, artistA); err != nil {
		t.Fatalf("Create A: %v", err)
	}

	artistB := testArtist("Artist B", "/music/Artist B")
	artistB.LibraryID = "lib-mtime"
	artistB.ThumbExists = true
	artistB.FanartExists = true
	if err := svc.Create(ctx, artistB); err != nil {
		t.Fatalf("Create B: %v", err)
	}

	// Create a third artist in a different library (should not appear in results).
	artistC := testArtist("Artist C", "/music/Artist C")
	artistC.LibraryID = "lib-other"
	artistC.ThumbExists = true
	if err := svc.Create(ctx, artistC); err != nil {
		t.Fatalf("Create C: %v", err)
	}

	// Set provenance with different timestamps for each artist.
	if err := repo.UpdateProvenance(ctx, artistA.ID, "thumb", 0,
		"aaa", "sha-a", "musicbrainz", "jpeg", "2026-03-20T10:00:00Z"); err != nil {
		t.Fatalf("UpdateProvenance A: %v", err)
	}

	// Artist B has two images with different timestamps -- the MAX should be returned.
	if err := repo.UpdateProvenance(ctx, artistB.ID, "thumb", 0,
		"bbb", "sha-b-thumb", "fanarttv", "jpeg", "2026-03-21T08:00:00Z"); err != nil {
		t.Fatalf("UpdateProvenance B thumb: %v", err)
	}
	if err := repo.UpdateProvenance(ctx, artistB.ID, "fanart", 0,
		"ccc", "sha-b-fanart", "fanarttv", "jpeg", "2026-03-21T15:30:00Z"); err != nil {
		t.Fatalf("UpdateProvenance B fanart: %v", err)
	}

	// Artist C is in a different library -- set provenance to verify filtering.
	if err := repo.UpdateProvenance(ctx, artistC.ID, "thumb", 0,
		"ddd", "sha-c", "user", "png", "2026-03-22T00:00:00Z"); err != nil {
		t.Fatalf("UpdateProvenance C: %v", err)
	}

	// Query per-artist newest write times for the target library.
	result, err := repo.NewestWriteTimesByArtist(ctx, "lib-mtime")
	if err != nil {
		t.Fatalf("NewestWriteTimesByArtist: %v", err)
	}

	if len(result) != 2 {
		t.Fatalf("expected 2 artists in result, got %d", len(result))
	}

	// Verify Artist A's timestamp.
	if got := result[artistA.ID]; got != "2026-03-20T10:00:00Z" {
		t.Errorf("Artist A write time = %q, want %q", got, "2026-03-20T10:00:00Z")
	}

	// Verify Artist B's timestamp is the MAX of their two images.
	if got := result[artistB.ID]; got != "2026-03-21T15:30:00Z" {
		t.Errorf("Artist B write time = %q, want %q", got, "2026-03-21T15:30:00Z")
	}

	// Verify Artist C (different library) is not included.
	if _, ok := result[artistC.ID]; ok {
		t.Error("Artist C (different library) should not appear in results")
	}
}

func TestNewestWriteTimesByArtist_NoWrites(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	repo := newSQLiteImageRepo(db)

	// Seed lib-empty so Service.Create's derive-source membership insert
	// lands a real artist_libraries row (matches the membership-backed
	// precondition the sibling TestNewestWriteTimesByArtist test enforces).
	if _, err := db.ExecContext(ctx,
		`INSERT INTO libraries (id, name, type, source, created_at, updated_at)
			VALUES (?, ?, 'regular', 'manual', datetime('now'), datetime('now'))`,
		"lib-empty", "lib-empty"); err != nil {
		t.Fatalf("seeding library lib-empty: %v", err)
	}

	// Create an artist with an image but no provenance (empty last_written_at).
	a := testArtist("Silent Artist", "/music/Silent")
	a.LibraryID = "lib-empty"
	a.ThumbExists = true
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Query should return an empty map since no writes have been recorded.
	result, err := repo.NewestWriteTimesByArtist(ctx, "lib-empty")
	if err != nil {
		t.Fatalf("NewestWriteTimesByArtist: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected empty map for library with no writes, got %d entries", len(result))
	}
}

func TestNewestWriteTimesByArtist_EmptyLibrary(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	repo := newSQLiteImageRepo(db)
	ctx := context.Background()

	// Query a nonexistent library -- should return an empty map with no error.
	result, err := repo.NewestWriteTimesByArtist(ctx, "lib-nonexistent")
	if err != nil {
		t.Fatalf("NewestWriteTimesByArtist: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected empty map for nonexistent library, got %d entries", len(result))
	}
}

func TestUpsertAll_PreservesProvenance(t *testing.T) {
	t.Parallel()
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
		"1234567890abcdef", "sha-fanart", "fanarttv", "jpeg", "2026-01-15T08:00:00Z"); err != nil {
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
	if got[0].ContentHash != "sha-fanart" {
		t.Errorf("ContentHash = %q, want %q", got[0].ContentHash, "sha-fanart")
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

// TestUpsert_PreservesLock verifies that the singular Upsert's conflict path
// leaves the locked column alone. A refresh-shaped caller that leaves Locked
// at its zero value must not clear an operator's lock: a cleared lock exposes
// pinned artwork to the auto-fix rules that delete files. The deliberate
// unlock path through SetLock must still work.
func TestUpsert_PreservesLock(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	ctx := context.Background()
	repo := newSQLiteImageRepo(db)

	svc := NewService(db)
	a := testArtist("Lock Preservation", "/music/Lock Preservation")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Seed the row via Upsert.
	img := &ArtistImage{
		ArtistID:  a.ID,
		ImageType: "fanart",
		SlotIndex: 0,
		Exists:    true,
		Width:     1920,
		Height:    1080,
	}
	if err := repo.Upsert(ctx, img); err != nil {
		t.Fatalf("Upsert (seed): %v", err)
	}

	// Confirm the precondition: the seeded row starts unlocked, so a later
	// "still locked" assertion cannot pass vacuously.
	seeded, err := repo.GetForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetForArtist (seed): %v", err)
	}
	if len(seeded) != 1 {
		t.Fatalf("expected 1 image after seed, got %d", len(seeded))
	}
	if seeded[0].Locked {
		t.Fatal("seeded row should start unlocked")
	}
	imageID := seeded[0].ID

	// The operator locks the image.
	if err := repo.SetLock(ctx, imageID, true); err != nil {
		t.Fatalf("SetLock(true): %v", err)
	}
	locked, err := repo.GetForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetForArtist (after lock): %v", err)
	}
	if !locked[0].Locked {
		t.Fatal("precondition failed: SetLock(true) did not lock the row")
	}

	// A refresh-shaped Upsert: same slot, Locked left at its zero value.
	// This is the shape that used to silently clear the lock.
	refresh := &ArtistImage{
		ArtistID:  a.ID,
		ImageType: "fanart",
		SlotIndex: 0,
		Exists:    true,
		Width:     3840,
		Height:    2160,
	}
	if err := repo.Upsert(ctx, refresh); err != nil {
		t.Fatalf("Upsert (refresh): %v", err)
	}

	got, err := repo.GetForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetForArtist (after refresh): %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 image, got %d", len(got))
	}
	if !got[0].Locked {
		t.Error("Locked = false after a refresh-shaped Upsert, want true: " +
			"the conflict path must not clear an operator-set lock")
	}
	// The rest of the row is a full overwrite by design, so the refresh's
	// dimensions must have landed. This also proves the Upsert actually took
	// effect rather than silently no-opping.
	if got[0].Width != 3840 || got[0].Height != 2160 {
		t.Errorf("dimensions = %dx%d, want 3840x2160 (refresh should overwrite)",
			got[0].Width, got[0].Height)
	}

	// PINS A KNOWN DEFECT, not desired behavior. The conflict path sets
	// id = excluded.id, and Upsert fills an empty ID with a fresh UUID, so the
	// refresh above rotated this row's primary key out from under any caller
	// holding the pre-refresh ID -- including SetLock, which matches on id
	// alone. Asserting the rotation keeps the defect visible: a change to
	// id = excluded.id in either direction trips this test instead of passing
	// silently. When the rotation is fixed (tracked separately), invert this to
	// require got[0].ID == imageID and drop this comment.
	if got[0].ID == imageID {
		t.Errorf("image ID did not rotate across the refresh (still %s); "+
			"if the id = excluded.id rotation was fixed, invert this assertion",
			imageID)
	}

	// The deliberate unlock path must still clear the lock. Note this uses the
	// post-refresh ID; the pre-refresh imageID would fail with ErrNotFound for
	// the reason pinned above.
	t.Run("ExplicitUnlock", func(t *testing.T) {
		if err := repo.SetLock(ctx, got[0].ID, false); err != nil {
			t.Fatalf("SetLock(false): %v", err)
		}
		after, err := repo.GetForArtist(ctx, a.ID)
		if err != nil {
			t.Fatalf("GetForArtist (after unlock): %v", err)
		}
		if after[0].Locked {
			t.Error("Locked = true after SetLock(false), want false")
		}
	})

	// The omission of locked from the conflict SET list cuts BOTH ways: the
	// conflict path can no longer clear a lock, and equally can no longer set
	// one. A caller passing Locked: true against an existing row gets a nil
	// error and no lock, which is easy to mistake for success. Pinning it here
	// keeps the asymmetry with the INSERT path (which does honor Locked: true)
	// an asserted contract rather than an undocumented surprise.
	t.Run("CannotSetLockOnExistingRow", func(t *testing.T) {
		before, err := repo.GetForArtist(ctx, a.ID)
		if err != nil {
			t.Fatalf("GetForArtist (before set attempt): %v", err)
		}
		if before[0].Locked {
			t.Fatal("precondition failed: row should be unlocked entering this subtest")
		}

		if err := repo.Upsert(ctx, &ArtistImage{
			ArtistID:  a.ID,
			ImageType: "fanart",
			SlotIndex: 0,
			Exists:    true,
			Width:     1280,
			Height:    720,
			Locked:    true,
		}); err != nil {
			t.Fatalf("Upsert (lock attempt): %v", err)
		}

		after, err := repo.GetForArtist(ctx, a.ID)
		if err != nil {
			t.Fatalf("GetForArtist (after set attempt): %v", err)
		}
		if after[0].Locked {
			t.Error("Locked = true after an Upsert carrying Locked: true; " +
				"the conflict path must not set a lock either -- SetLock owns both directions")
		}
		// Proves the Upsert genuinely executed, so the unlocked result above
		// is the conflict path declining to set the lock rather than a no-op.
		if after[0].Width != 1280 || after[0].Height != 720 {
			t.Errorf("dimensions = %dx%d, want 1280x720 (the lock-attempt Upsert should have landed)",
				after[0].Width, after[0].Height)
		}
	})
}

// TestReconcileImages verifies that ReconcileImages converges the
// artist_images registry to filesystem-truth without touching other artist
// columns, and rejects a nil/empty-ID Artist.
func TestReconcileImages(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist("Reconcile Test", "/music/ReconcileTest")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	a.ThumbExists = true
	a.FanartExists = true
	repaired, err := svc.ReconcileImages(ctx, a)
	if err != nil {
		t.Fatalf("ReconcileImages: %v", err)
	}
	if !repaired {
		t.Error("expected repaired=true on first reconciliation from empty registry")
	}

	images, err := svc.GetImagesForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetImagesForArtist: %v", err)
	}
	var sawThumb, sawFanart bool
	for _, im := range images {
		if im.ImageType == "thumb" && im.SlotIndex == 0 && im.Exists {
			sawThumb = true
		}
		if im.ImageType == "fanart" && im.SlotIndex == 0 && im.Exists {
			sawFanart = true
		}
	}
	if !sawThumb || !sawFanart {
		t.Errorf("registry not converged: thumb=%v fanart=%v", sawThumb, sawFanart)
	}

	// Idempotent replay must report no drift, since the registry already
	// matches the Artist's image fields.
	repaired, err = svc.ReconcileImages(ctx, a)
	if err != nil {
		t.Fatalf("ReconcileImages (replay): %v", err)
	}
	if repaired {
		t.Error("expected repaired=false on idempotent replay")
	}

	// Guard rejects nil and empty ID.
	if _, err := svc.ReconcileImages(ctx, nil); err == nil {
		t.Error("expected error for nil Artist")
	}
	if _, err := svc.ReconcileImages(ctx, &Artist{}); err == nil {
		t.Error("expected error for empty Artist ID")
	}
}

// TestClearExistsFlag verifies that ClearExistsFlag sets the exists_flag to 0
// for the targeted image slot without affecting other slots.
func TestClearExistsFlag(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist("Flag Test", "/music/FlagTest")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Set the thumb flag.
	a.ThumbExists = true
	a.FanartExists = true
	if err := svc.Update(ctx, a); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Clear only the thumb flag.
	if err := svc.ClearImageFlag(ctx, a.ID, "thumb", 0); err != nil {
		t.Fatalf("ClearImageFlag: %v", err)
	}

	// Verify thumb is cleared but fanart is still set.
	images, err := svc.GetImagesForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetImagesForArtist: %v", err)
	}

	foundThumb := false
	foundFanart := false
	for _, im := range images {
		if im.ImageType == "thumb" && im.SlotIndex == 0 {
			foundThumb = true
			if im.Exists {
				t.Error("thumb exists_flag should be false after ClearImageFlag")
			}
		}
		if im.ImageType == "fanart" && im.SlotIndex == 0 {
			foundFanart = true
			if !im.Exists {
				t.Error("fanart exists_flag should still be true")
			}
		}
	}
	if !foundThumb {
		t.Error("expected thumb image slot 0 to be present")
	}
	if !foundFanart {
		t.Error("expected fanart image slot 0 to be present")
	}
}
