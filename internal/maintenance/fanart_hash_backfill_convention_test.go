package maintenance

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	img "github.com/sydlexius/stillwater/internal/image"
)

// TestBackfillFanartHashes_OtherConventionFiles is the #2632 guard for the
// backfill: the files on disk use the fanart* convention while the active
// profile's primary is backdrop.jpg. The scanner registers those files under
// every convention, so the backfill must resolve them the same way instead of
// finding nothing and skipping every row.
//
// REVERT-AND-RERUN: resolving with DiscoverFanart(primary) again must turn this
// RED on the per-slot hash assertions.
func TestBackfillFanartHashes_OtherConventionFiles(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := NewService(db, dbPath, "", slog.Default())
	ctx := context.Background()
	dir := t.TempDir()
	const id = "artist-other-convention"

	// PRECONDITION: the files use a convention the profile primary does NOT name.
	if testPrimary == "fanart.jpg" {
		t.Fatal("precondition: test needs a primary that differs from the on-disk convention")
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO artists (id, name, path) VALUES (?, ?, ?)`, id, id, dir); err != nil {
		t.Fatalf("seeding artist: %v", err)
	}
	for slot := range 3 {
		name := img.FanartFilename("fanart.jpg", slot, false)
		if err := os.WriteFile(filepath.Join(dir, name), backfillJPEG(t, slot), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, phash, content_hash)
			 VALUES (?, ?, 'fanart', ?, 1, '', '')`, id+"-"+name, id, slot); err != nil {
			t.Fatalf("seeding image row slot %d: %v", slot, err)
		}
		if _, err := os.Stat(filepath.Join(dir, testPrimary)); err == nil {
			t.Fatalf("precondition: %s must not exist on disk", testPrimary)
		}
	}

	if err := svc.BackfillFanartHashes(ctx, embyPrimary, 0); err != nil {
		t.Fatalf("BackfillFanartHashes: %v", err)
	}

	for slot := range 3 {
		fh, err := img.HashFile(ctx, filepath.Join(dir, img.FanartFilename("fanart.jpg", slot, false)), true)
		if err != nil {
			t.Fatalf("hashing expected file for slot %d: %v", slot, err)
		}
		gotPH, gotContent := fanartHashes(t, db, id, slot)
		if gotPH != img.HashHex(fh.Perceptual) || gotContent != fh.Content {
			t.Errorf("slot %d hashes = (%q, %q), want (%q, %q)", slot, gotPH, gotContent, img.HashHex(fh.Perceptual), fh.Content)
		}
	}
}

// TestBackfillFanartHashes_MixedDirectoryUsesScannerOrder pins the backfill's
// resolution in a directory holding BOTH conventions under an Emby profile
// (primary backdrop.jpg): the scanner resolves fanart*, so slots 0 and 1 must
// be hashed from fanart.jpg / fanart2.jpg, never from the backdrop pair a
// profile-first walk would pick.
//
// REVERT-AND-RERUN: resolving profile-first (backdrop before fanart) must turn
// this RED on the per-slot hash assertions.
func TestBackfillFanartHashes_MixedDirectoryUsesScannerOrder(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := NewService(db, dbPath, "", slog.Default())
	ctx := context.Background()
	dir := t.TempDir()
	const id = "artist-mixed-convention"

	if _, err := db.ExecContext(ctx, `INSERT INTO artists (id, name, path) VALUES (?, ?, ?)`, id, id, dir); err != nil {
		t.Fatalf("seeding artist: %v", err)
	}
	files := []string{"fanart.jpg", "fanart2.jpg", "backdrop.jpg", "backdrop2.jpg"}
	for i, name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), backfillJPEG(t, i), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	for slot := range 2 {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, phash, content_hash)
			 VALUES (?, ?, 'fanart', ?, 1, '', '')`, id+"-"+files[slot], id, slot); err != nil {
			t.Fatalf("seeding image row slot %d: %v", slot, err)
		}
	}

	if err := svc.BackfillFanartHashes(ctx, embyPrimary, 0); err != nil {
		t.Fatalf("BackfillFanartHashes: %v", err)
	}

	for slot := range 2 {
		fh, err := img.HashFile(ctx, filepath.Join(dir, files[slot]), true)
		if err != nil {
			t.Fatalf("hashing expected file for slot %d: %v", slot, err)
		}
		gotPH, gotContent := fanartHashes(t, db, id, slot)
		if gotPH != img.HashHex(fh.Perceptual) || gotContent != fh.Content {
			t.Errorf("slot %d hashes = (%q, %q), want those of %s (%q, %q)",
				slot, gotPH, gotContent, files[slot], img.HashHex(fh.Perceptual), fh.Content)
		}
	}
}
