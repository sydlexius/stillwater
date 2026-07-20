package artist

import (
	"context"
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
	if e.level != slog.LevelInfo {
		t.Errorf("%s: level = %v, want INFO", msg, e.level)
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
// UpsertAll, discarding any records the seeding itself emits.
func seedImageSlots(t *testing.T, repo *sqliteImageRepo, artistID string, images []ArtistImage) {
	t.Helper()
	if err := repo.UpsertAll(context.Background(), artistID, images); err != nil {
		t.Fatalf("seeding image slots: %v", err)
	}
}

func img(imageType string, slot int, exists bool) ArtistImage {
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
		img("thumb", 0, true), img("fanart", 0, true), img("fanart", 1, true),
	})

	// Capture only the second call: fanart slot 1 disappears from the incoming
	// set, so its row must be deleted and recorded.
	state := captureImageLogs(t)
	if err := repo.UpsertAll(ctx, a.ID, []ArtistImage{
		img("thumb", 0, true), img("fanart", 0, true),
	}); err != nil {
		t.Fatalf("UpsertAll: %v", err)
	}

	requireOne(t, state, "artist image row deleted", a.ID, "fanart", "1", "manual")

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
		img("thumb", 0, true), img("fanart", 0, true), img("banner", 0, false),
	})

	state := captureImageLogs(t)
	// fanart/0 flips true -> false. thumb/0 stays true. banner/0 stays false,
	// so it is not a transition and must not be recorded.
	if err := repo.UpsertAll(ctx, a.ID, []ArtistImage{
		img("thumb", 0, true), img("fanart", 0, false), img("banner", 0, false),
	}); err != nil {
		t.Fatalf("UpsertAll: %v", err)
	}

	requireOne(t, state, "artist image exists flag cleared", a.ID, "fanart", "0", "manual")
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
		img("thumb", 0, true), img("fanart", 2, true),
	})

	state := captureImageLogs(t)
	scanCtx := ContextWithSource(ctx, "scan")
	// thumb/0 clears its flag; fanart/2 is dropped entirely.
	if err := repo.UpsertAll(scanCtx, a.ID, []ArtistImage{img("thumb", 0, false)}); err != nil {
		t.Fatalf("UpsertAll: %v", err)
	}

	requireOne(t, state, "artist image exists flag cleared", a.ID, "thumb", "0", "scan")
	// slot_index 2 (not 0) is the deleted slot: a record that reported the
	// wrong slot would fail here.
	requireOne(t, state, "artist image row deleted", a.ID, "fanart", "2", "scan")
}
