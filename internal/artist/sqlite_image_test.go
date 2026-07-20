package artist

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
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

// --- destructive-image record capture (issue #2636) ---------------------------
//
// The image repository logs row deletions and exists_flag clears through the
// package-level slog default so the destructive paths are attributable after
// the fact. These helpers install a capturing handler for the duration of a
// test and expose the records for assertion. Tests using them must NOT call
// t.Parallel(): slog.SetDefault is process-global, and Go only runs parallel
// tests after every sequential test in the package has finished, so a
// sequential test is the only way to own the default logger exclusively.

type imageLogEntry struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

type imageLogState struct {
	mu      sync.Mutex
	entries []imageLogEntry
}

type imageLogHandler struct {
	state *imageLogState
}

func (h *imageLogHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *imageLogHandler) Handle(_ context.Context, r slog.Record) error {
	entry := imageLogEntry{level: r.Level, msg: r.Message, attrs: map[string]string{}}
	r.Attrs(func(a slog.Attr) bool {
		entry.attrs[a.Key] = a.Value.String()
		return true
	})
	h.state.mu.Lock()
	h.state.entries = append(h.state.entries, entry)
	h.state.mu.Unlock()
	return nil
}

func (h *imageLogHandler) WithAttrs(_ []slog.Attr) slog.Handler {
	return &imageLogHandler{state: h.state}
}
func (h *imageLogHandler) WithGroup(_ string) slog.Handler { return h }

// captureImageLogs redirects the default logger into a fresh state for the
// remainder of the test and restores the previous default on cleanup.
func captureImageLogs(t *testing.T) *imageLogState {
	t.Helper()
	state := &imageLogState{}
	prev := slog.Default()
	slog.SetDefault(slog.New(&imageLogHandler{state: state}))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return state
}

// matching returns every captured entry with the given message that also
// carries the given artist_id, so assertions cannot be satisfied by an
// unrelated record emitted elsewhere in the same test.
func (s *imageLogState) matching(msg, artistID string) []imageLogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []imageLogEntry
	for _, e := range s.entries {
		if e.msg == msg && e.attrs["artist_id"] == artistID {
			out = append(out, e)
		}
	}
	return out
}

// requireOne asserts exactly one record with the given message/artist exists
// and that its level, image_type, slot_index and source match exactly. Every
// attribute is checked, so an empty source or a wrong slot index fails.
func requireOne(t *testing.T, s *imageLogState, msg, artistID, imageType, slotIndex, source string) {
	t.Helper()
	got := s.matching(msg, artistID)
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 %q record for artist %s, got %d (all entries: %+v)",
			msg, artistID, len(got), s.entries)
	}
	e := got[0]
	// Warn, not Info: Info is silenceable by a routine operator log-level bump,
	// which would disable the whole attribution mechanism (issue #2636).
	if e.level != slog.LevelWarn {
		t.Errorf("%s: level = %v, want WARN", msg, e.level)
	}
	if e.attrs["image_type"] != imageType {
		t.Errorf("%s: image_type = %q, want %q", msg, e.attrs["image_type"], imageType)
	}
	if e.attrs["slot_index"] != slotIndex {
		t.Errorf("%s: slot_index = %q, want %q", msg, e.attrs["slot_index"], slotIndex)
	}
	if e.attrs["source"] != source {
		t.Errorf("%s: source = %q, want %q", msg, e.attrs["source"], source)
	}
}

// seedImageSlots installs an exact set of image rows for an artist via
// UpsertAll. Any records the seeding emits go to whatever logger is installed
// at call time; call this BEFORE captureImageLogs so the seeding noise lands
// outside the captured state.
func seedImageSlots(t *testing.T, repo *sqliteImageRepo, artistID string, images []ArtistImage) {
	t.Helper()
	if err := repo.UpsertAll(context.Background(), artistID, images); err != nil {
		t.Fatalf("seeding image slots: %v", err)
	}
}

// imageSlot builds an ArtistImage for one (image_type, slot_index) pair. Named
// to avoid shadowing confusion with the `img` loop variable in UpsertAll.
func imageSlot(imageType string, slot int, exists bool) ArtistImage {
	return ArtistImage{ImageType: imageType, SlotIndex: slot, Exists: exists, Width: 100, Height: 100}
}

// TestUpsertAll_LogsStaleRowDeletion asserts that dropping a slot from the
// incoming set emits exactly one attributable deletion record for that slot,
// and no record for the slots that survive.
func TestUpsertAll_LogsStaleRowDeletion(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	repo := newSQLiteImageRepo(db)

	a := testArtist("Deletion Probe", "/music/DeletionProbe")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}
	seedImageSlots(t, repo, a.ID, []ArtistImage{
		imageSlot("thumb", 0, true), imageSlot("fanart", 0, true), imageSlot("fanart", 1, true),
	})

	// Capture only the second call: fanart slot 1 disappears from the incoming
	// set, so its row must be deleted and recorded.
	state := captureImageLogs(t)
	if err := repo.UpsertAll(ctx, a.ID, []ArtistImage{
		imageSlot("thumb", 0, true), imageSlot("fanart", 0, true),
	}); err != nil {
		t.Fatalf("UpsertAll: %v", err)
	}

	requireOne(t, state, "artist image row deleted", a.ID, "fanart", "1", unattributedSource)

	// The surviving slots must not be reported as deleted, and no exists_flag
	// clear happened on this call.
	if n := len(state.matching("artist image row deleted", a.ID)); n != 1 {
		t.Errorf("expected 1 deletion record total, got %d", n)
	}
	if n := len(state.matching("artist image exists flag cleared", a.ID)); n != 0 {
		t.Errorf("expected no exists-flag records, got %d", n)
	}

	// The record must describe reality: the row is actually gone.
	images, err := repo.GetForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetForArtist: %v", err)
	}
	if len(images) != 2 {
		t.Fatalf("expected 2 surviving rows, got %d", len(images))
	}
	for _, im := range images {
		if im.ImageType == "fanart" && im.SlotIndex == 1 {
			t.Error("fanart slot 1 should have been deleted")
		}
	}
}

// TestUpsertAll_LogsExistsFlagCleared asserts that a 1 -> 0 exists_flag
// transition is recorded with the exact slot that flipped, and that slots
// which stay true or were already false emit nothing.
func TestUpsertAll_LogsExistsFlagCleared(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	repo := newSQLiteImageRepo(db)

	a := testArtist("Flag Probe", "/music/FlagProbe")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}
	seedImageSlots(t, repo, a.ID, []ArtistImage{
		imageSlot("thumb", 0, true), imageSlot("fanart", 0, true), imageSlot("banner", 0, false),
	})

	state := captureImageLogs(t)
	// fanart/0 flips true -> false. thumb/0 stays true. banner/0 stays false,
	// so it is not a transition and must not be recorded.
	if err := repo.UpsertAll(ctx, a.ID, []ArtistImage{
		imageSlot("thumb", 0, true), imageSlot("fanart", 0, false), imageSlot("banner", 0, false),
	}); err != nil {
		t.Fatalf("UpsertAll: %v", err)
	}

	requireOne(t, state, "artist image exists flag cleared", a.ID, "fanart", "0", unattributedSource)
	if n := len(state.matching("artist image row deleted", a.ID)); n != 0 {
		t.Errorf("expected no deletion records, got %d", n)
	}

	// Assert the DB agrees: only fanart/0 is now false among the two that
	// started true, and nothing was deleted.
	images, err := repo.GetForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetForArtist: %v", err)
	}
	if len(images) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(images))
	}
	for _, im := range images {
		wantExists := im.ImageType == "thumb"
		if im.Exists != wantExists {
			t.Errorf("%s/%d exists = %v, want %v", im.ImageType, im.SlotIndex, im.Exists, wantExists)
		}
	}
}

// TestUpsertAll_RecordsSourceFromContext proves the source attribute reflects
// the context tag rather than the "manual" default, which is what makes the
// records attributable to the scanner in production.
func TestUpsertAll_RecordsSourceFromContext(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	repo := newSQLiteImageRepo(db)

	a := testArtist("Source Probe", "/music/SourceProbe")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}
	seedImageSlots(t, repo, a.ID, []ArtistImage{
		imageSlot("thumb", 0, true), imageSlot("fanart", 2, true),
	})

	state := captureImageLogs(t)
	scanCtx := ContextWithSource(ctx, "scan")
	// thumb/0 clears its flag; fanart/2 is dropped entirely.
	if err := repo.UpsertAll(scanCtx, a.ID, []ArtistImage{imageSlot("thumb", 0, false)}); err != nil {
		t.Fatalf("UpsertAll: %v", err)
	}

	requireOne(t, state, "artist image exists flag cleared", a.ID, "thumb", "0", "scan")
	// slot_index 2 (not 0) is the deleted slot: a record that reported the
	// wrong slot would fail here.
	requireOne(t, state, "artist image row deleted", a.ID, "fanart", "2", "scan")
}

// TestUpsertAll_NoRecordsWhenTransactionRollsBack is the anti-phantom
// regression test. Records describe destruction, so they must share the
// transaction's atomicity: if the transaction rolls back, nothing was
// destroyed and nothing may be recorded. A record that outlives its rollback
// is worse than no record at all, because the next incident investigation
// will believe it.
//
// The failure is induced with a duplicate artist_images.id in the incoming
// slice: the second row carrying that id violates the primary-key UNIQUE
// constraint, so UpsertAll returns an error and the whole transaction is
// rolled back -- but only AFTER the thumb/0 exists-flag clear has already
// been decided.
func TestUpsertAll_NoRecordsWhenTransactionRollsBack(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	repo := newSQLiteImageRepo(db)

	a := testArtist("Rollback Probe", "/music/RollbackProbe")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}
	seedImageSlots(t, repo, a.ID, []ArtistImage{
		imageSlot("thumb", 0, true), imageSlot("fanart", 0, true),
	})

	// thumb/0 clears its flag (a record would be decided here) and fanart/0 is
	// dropped from the incoming set (a deletion would be decided later). The
	// two fanart/5 and fanart/6 rows share one id, so the second one fails.
	dup1 := imageSlot("fanart", 5, true)
	dup1.ID = "duplicate-row-id"
	dup2 := imageSlot("fanart", 6, true)
	dup2.ID = "duplicate-row-id"

	state := captureImageLogs(t)
	err := repo.UpsertAll(ctx, a.ID, []ArtistImage{
		imageSlot("thumb", 0, false), dup1, dup2,
	})
	if err == nil {
		t.Fatal("UpsertAll should have failed on the duplicate row id; " +
			"without a failure this test proves nothing")
	}
	if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Fatalf("expected a UNIQUE constraint failure, got: %v", err)
	}

	if n := len(state.matching("artist image exists flag cleared", a.ID)); n != 0 {
		t.Errorf("emitted %d exists-flag record(s) for a rolled-back transaction, want 0 (all entries: %+v)",
			n, state.entries)
	}
	if n := len(state.matching("artist image row deleted", a.ID)); n != 0 {
		t.Errorf("emitted %d deletion record(s) for a rolled-back transaction, want 0 (all entries: %+v)",
			n, state.entries)
	}

	// The DB must agree that nothing was destroyed: both seeded rows survive
	// with exists_flag intact. This is what makes any emitted record a lie.
	images, err := repo.GetForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetForArtist: %v", err)
	}
	if len(images) != 2 {
		t.Fatalf("expected the 2 seeded rows to survive the rollback, got %d", len(images))
	}
	for _, im := range images {
		if !im.Exists {
			t.Errorf("%s/%d exists = false after rollback, want true", im.ImageType, im.SlotIndex)
		}
	}
}

// TestUpsertAll_UntaggedContextRecordsUnattributed pins the distinction that
// makes the records trustworthy: an untagged context must record
// "unattributed", never "manual". Most automated callers (provider refresh,
// notification and identify handlers) reach UpsertAll via Service.Update
// without tagging a source, so a "manual" default would positively assert
// that a human cleared the flag. For an incident investigation that false
// claim is strictly worse than an explicit unknown (issue #2636).
func TestUpsertAll_UntaggedContextRecordsUnattributed(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	repo := newSQLiteImageRepo(db)

	a := testArtist("Unattributed Probe", "/music/UnattributedProbe")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}
	seedImageSlots(t, repo, a.ID, []ArtistImage{
		imageSlot("thumb", 0, true), imageSlot("fanart", 3, true),
	})

	state := captureImageLogs(t)
	// Deliberately an untagged context: thumb/0 clears, fanart/3 is dropped.
	if err := repo.UpsertAll(ctx, a.ID, []ArtistImage{imageSlot("thumb", 0, false)}); err != nil {
		t.Fatalf("UpsertAll: %v", err)
	}

	requireOne(t, state, "artist image exists flag cleared", a.ID, "thumb", "0", "unattributed")
	requireOne(t, state, "artist image row deleted", a.ID, "fanart", "3", "unattributed")

	// Spelled out rather than deferred to requireOne: "manual" is the specific
	// wrong answer this test exists to prevent regressing to.
	for _, msg := range []string{"artist image exists flag cleared", "artist image row deleted"} {
		for _, e := range state.matching(msg, a.ID) {
			if e.attrs["source"] == "manual" {
				t.Errorf("%s: source = \"manual\" for an untagged context; "+
					"an automated path must not be recorded as a human edit", msg)
			}
		}
	}
}

// TestUpsertAll_CanceledContextDestroysNothing is the non-destruction
// guarantee under cancellation. The #2636 incident's damage window is dense
// with "context canceled" events, and mass mid-flight cancellation across the
// Update/UpsertAll path has never been ruled out as a contributor to the row
// loss. UpsertAll is the only code path that both deletes artist_images rows
// and clears exists_flag, so if cancellation can leave a partially-applied
// destruction behind, this is where it happens.
//
// The contract asserted here is total: a canceled call must fail, must leave
// every seeded row byte-for-byte intact, and must emit no destructive record.
// Assertions read the database directly rather than trusting UpsertAll's own
// return value, because a path that reports failure while having already
// destroyed rows is exactly the failure mode being guarded against.
func TestUpsertAll_CanceledContextDestroysNothing(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	repo := newSQLiteImageRepo(db)

	a := testArtist("Cancellation Probe", "/music/CancellationProbe")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// A full registry spanning several types, with one slot already false so
	// the assertion below distinguishes "unchanged" from "all true".
	seeded := []ArtistImage{
		imageSlot("banner", 0, true),
		imageSlot("fanart", 0, true),
		imageSlot("fanart", 1, true),
		imageSlot("logo", 0, false),
		imageSlot("thumb", 0, true),
	}
	seedImageSlots(t, repo, a.ID, seeded)

	canceledCtx, cancel := context.WithCancel(ContextWithSource(ctx, "scan"))
	cancel()

	state := captureImageLogs(t)
	// The incoming set would, if applied, clear thumb/0 and delete every other
	// slot. Cancellation must prevent all of it.
	err := repo.UpsertAll(canceledCtx, a.ID, []ArtistImage{imageSlot("thumb", 0, false)})
	if err == nil {
		t.Fatal("UpsertAll returned nil for a canceled context; a canceled " +
			"destructive write must fail rather than silently proceed")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("UpsertAll error = %v, want one wrapping context.Canceled; "+
			"callers distinguish cancellation from real failures", err)
	}

	// No record may be emitted, because nothing was destroyed. A record here
	// would be a phantom that a future investigation would trust.
	if n := len(state.matching("artist image row deleted", a.ID)); n != 0 {
		t.Errorf("emitted %d deletion record(s) for a canceled call, want 0 (all entries: %+v)",
			n, state.entries)
	}
	if n := len(state.matching("artist image exists flag cleared", a.ID)); n != 0 {
		t.Errorf("emitted %d exists-flag record(s) for a canceled call, want 0 (all entries: %+v)",
			n, state.entries)
	}

	// The authoritative check: every seeded row still exists with its original
	// exists_flag. Read with an uncanceled context so this observes the
	// database rather than the cancellation.
	images, err := repo.GetForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetForArtist: %v", err)
	}
	if len(images) != len(seeded) {
		t.Fatalf("expected all %d seeded rows to survive cancellation, got %d", len(seeded), len(images))
	}
	want := make(map[slotKey]bool, len(seeded))
	for _, im := range seeded {
		want[slotKey{im.ImageType, im.SlotIndex}] = im.Exists
	}
	for _, im := range images {
		k := slotKey{im.ImageType, im.SlotIndex}
		wantExists, ok := want[k]
		if !ok {
			t.Errorf("unexpected row %s/%d after cancellation", im.ImageType, im.SlotIndex)
			continue
		}
		if im.Exists != wantExists {
			t.Errorf("%s/%d exists = %v after cancellation, want %v (original value)",
				im.ImageType, im.SlotIndex, im.Exists, wantExists)
		}
		delete(want, k)
	}
	for k := range want {
		t.Errorf("row %s/%d was destroyed by a canceled call", k.imageType, k.slotIndex)
	}
}

// TestUpsertAll_StaleDeletionOrderIsStableAcrossTypes covers the reason
// deleteStaleSlots sorts at all. The removal set is built by ranging over a
// map, and Go randomizes map iteration order, so without the sort the emitted
// records would arrive in a different order on every run. That matters because
// the records exist to be read by a human reconstructing an incident: an
// unstable order makes two log excerpts of the same event impossible to
// compare.
//
// The existing deletion tests all drop a single slot, which never invokes the
// comparator. This one drops five slots spanning four image types, which is
// the only shape that exercises the cross-type branch, and asserts the exact
// emitted sequence. Run with -count=5 to confirm the order does not vary.
func TestUpsertAll_StaleDeletionOrderIsStableAcrossTypes(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	repo := newSQLiteImageRepo(db)

	a := testArtist("Ordering Probe", "/music/OrderingProbe")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Seeded deliberately out of sorted order so a pass-through implementation
	// that preserved insertion order would not accidentally match.
	seedImageSlots(t, repo, a.ID, []ArtistImage{
		imageSlot("thumb", 0, true),
		imageSlot("fanart", 1, true),
		imageSlot("logo", 0, true),
		imageSlot("banner", 0, true),
		imageSlot("fanart", 0, true),
		imageSlot("poster", 0, true),
	})

	state := captureImageLogs(t)
	// poster/0 is the sole survivor; the other five slots go stale at once.
	if err := repo.UpsertAll(ctx, a.ID, []ArtistImage{imageSlot("poster", 0, true)}); err != nil {
		t.Fatalf("UpsertAll: %v", err)
	}

	// Sorted by image_type, then slot_index.
	wantOrder := []struct {
		imageType string
		slotIndex string
	}{
		{"banner", "0"},
		{"fanart", "0"},
		{"fanart", "1"},
		{"logo", "0"},
		{"thumb", "0"},
	}

	got := state.matching("artist image row deleted", a.ID)
	if len(got) != len(wantOrder) {
		t.Fatalf("expected %d deletion records, got %d (all entries: %+v)",
			len(wantOrder), len(got), state.entries)
	}
	for i, want := range wantOrder {
		if got[i].attrs["image_type"] != want.imageType || got[i].attrs["slot_index"] != want.slotIndex {
			t.Errorf("record %d = %s/%s, want %s/%s (records must be sorted by type then slot)",
				i, got[i].attrs["image_type"], got[i].attrs["slot_index"], want.imageType, want.slotIndex)
		}
	}

	// The records must describe reality: only the survivor is left.
	images, err := repo.GetForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetForArtist: %v", err)
	}
	if len(images) != 1 || images[0].ImageType != "poster" {
		t.Fatalf("expected only poster/0 to survive, got %d rows: %+v", len(images), images)
	}
}

// TestUpsertAll_StaleDeletionFailureRollsBackEverything covers the recovery
// path for a delete that the database refuses. deleteStaleSlots runs last,
// after the upserts have already been applied inside the transaction and after
// exists-flag records have already been decided, so a failure there is the
// case most likely to leave the database half-changed and the log describing a
// destruction that did not happen.
//
// The refusal is induced with a BEFORE DELETE trigger that aborts, which is
// how a real schema-level guard (a foreign key, a protective trigger) would
// present. Nothing in production code is stubbed or restructured.
func TestUpsertAll_StaleDeletionFailureRollsBackEverything(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	repo := newSQLiteImageRepo(db)

	a := testArtist("Delete Refusal Probe", "/music/DeleteRefusalProbe")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}
	seedImageSlots(t, repo, a.ID, []ArtistImage{
		imageSlot("thumb", 0, true),
		imageSlot("fanart", 0, true),
		imageSlot("logo", 0, true),
	})

	// Installed after seeding so the seed itself is unaffected.
	if _, err := db.ExecContext(ctx, `
		CREATE TRIGGER refuse_image_delete BEFORE DELETE ON artist_images
		BEGIN
			SELECT RAISE(ABORT, 'image row deletion refused');
		END`); err != nil {
		t.Fatalf("creating refusal trigger: %v", err)
	}

	state := captureImageLogs(t)
	// thumb/0 clears its flag (a record is decided early), and fanart/0 plus
	// logo/0 go stale (the delete that will be refused). The refusal must undo
	// the flag clear too.
	err := repo.UpsertAll(ctx, a.ID, []ArtistImage{imageSlot("thumb", 0, false)})
	if err == nil {
		t.Fatal("UpsertAll should have failed on the refused delete; " +
			"without a failure this test proves nothing")
	}
	if !strings.Contains(err.Error(), "image row deletion refused") {
		t.Fatalf("expected the trigger's abort to surface, got: %v", err)
	}

	if n := len(state.matching("artist image row deleted", a.ID)); n != 0 {
		t.Errorf("emitted %d deletion record(s) for a refused delete, want 0 (all entries: %+v)",
			n, state.entries)
	}
	// The flag clear was decided before the failure, so this is the assertion
	// that proves records are buffered until commit rather than emitted inline.
	if n := len(state.matching("artist image exists flag cleared", a.ID)); n != 0 {
		t.Errorf("emitted %d exists-flag record(s) for a rolled-back transaction, want 0 (all entries: %+v)",
			n, state.entries)
	}

	// Every seeded row survives with exists_flag intact, including the thumb
	// whose clear was rolled back.
	images, err := repo.GetForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetForArtist: %v", err)
	}
	if len(images) != 3 {
		t.Fatalf("expected the 3 seeded rows to survive the rollback, got %d", len(images))
	}
	for _, im := range images {
		if !im.Exists {
			t.Errorf("%s/%d exists = false after rollback, want true", im.ImageType, im.SlotIndex)
		}
	}
}

// TestLogSourceFromContext_DoesNotAlterHistoryDefault guards the constraint
// that the two source helpers stay independent. The history layer's "manual"
// default is load-bearing -- metadata_changes validates against a source set
// that does not contain "unattributed" -- so widening the shared helper would
// corrupt history rows. The logging helper must diverge only on the untagged
// case, and agree everywhere else.
func TestLogSourceFromContext_DoesNotAlterHistoryDefault(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	if got := sourceFromContext(ctx); got != "manual" {
		t.Errorf("sourceFromContext(untagged) = %q, want %q (history default must be unchanged)", got, "manual")
	}
	if got := logSourceFromContext(ctx); got != unattributedSource {
		t.Errorf("logSourceFromContext(untagged) = %q, want %q", got, unattributedSource)
	}

	// An explicitly tagged context must read identically through both helpers,
	// so records and history rows never disagree about a known source.
	for _, source := range []string{"scan", "manual", "import", "revert", "provider:musicbrainz", "rule:r-42"} {
		tagged := ContextWithSource(ctx, source)
		if got := sourceFromContext(tagged); got != source {
			t.Errorf("sourceFromContext(%q) = %q", source, got)
		}
		if got := logSourceFromContext(tagged); got != source {
			t.Errorf("logSourceFromContext(%q) = %q", source, got)
		}
	}
}
