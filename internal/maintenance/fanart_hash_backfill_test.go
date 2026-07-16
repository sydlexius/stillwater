package maintenance

import (
	"bytes"
	"context"
	"database/sql"
	"image"
	"image/color"
	"image/jpeg"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	img "github.com/sydlexius/stillwater/internal/image"
)

const testPrimary = "backdrop.jpg"

// embyPrimary resolves the Emby primary fanart name. Using a NON-default name
// on purpose: the default is "fanart.jpg", so a regression that quietly falls
// back to the defaults (as ScanExistsFlags legitimately does) would discover
// zero files here and be caught, instead of passing because the fixture happened
// to agree with the fallback.
func embyPrimary(context.Context) string { return testPrimary }

// backfillJPEG encodes a JPEG with block structure so its perceptual hash is
// non-trivial and variant-specific. A flat fill hashes to all zeros for every
// image, which would make "the right file's hash" indistinguishable from any
// other file's.
func backfillJPEG(t *testing.T, variant int) []byte {
	t.Helper()
	const (
		blocks = 8
		w      = 640
		h      = 360
	)
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			bx, by := x*blocks/w, y*blocks/h
			hsh := uint32(bx)*374761393 + uint32(by)*668265263 + uint32(variant)*2246822519
			hsh ^= hsh >> 13
			hsh *= 1274126177
			hsh ^= hsh >> 16
			v := uint8(hsh >> 8)
			m.Set(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, m, nil); err != nil {
		t.Fatalf("encoding JPEG: %v", err)
	}
	return buf.Bytes()
}

// seedFanartArtist writes `count` fanart files (primary + numbered variants,
// Emby numbering) and inserts the artist plus one artist_images row per slot
// with an EMPTY phash -- the starved state under test.
func seedFanartArtist(t *testing.T, db *sql.DB, artistID string, count int) string {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	for i := range count {
		name := img.FanartFilename(testPrimary, i, false)
		if err := os.WriteFile(filepath.Join(dir, name), backfillJPEG(t, i), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO artists (id, name, path) VALUES (?, ?, ?)`, artistID, artistID, dir); err != nil {
		t.Fatalf("seeding artist: %v", err)
	}
	for i := range count {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, phash, content_hash)
			 VALUES (?, ?, 'fanart', ?, 1, '', '')`,
			artistID+"-fanart-"+string(rune('0'+i)), artistID, i); err != nil {
			t.Fatalf("seeding image row slot %d: %v", i, err)
		}
	}
	return dir
}

func fanartHashes(t *testing.T, db *sql.DB, artistID string, slot int) (phash, content string) {
	t.Helper()
	err := db.QueryRowContext(context.Background(),
		`SELECT phash, content_hash FROM artist_images
		 WHERE artist_id = ? AND image_type = 'fanart' AND slot_index = ?`, artistID, slot).Scan(&phash, &content)
	if err != nil {
		t.Fatalf("reading hashes for slot %d: %v", slot, err)
	}
	return phash, content
}

// TestBackfillFanartHashes_FillsStarvedSlots is the #2564 guard for the
// backfill.
//
// REVERT-AND-RERUN: narrowing the SELECT to slot_index = 0, or dropping the
// UPDATE, must turn this RED on the per-slot hash assertions.
func TestBackfillFanartHashes_FillsStarvedSlots(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := NewService(db, dbPath, "", slog.Default())
	dir := seedFanartArtist(t, db, "artist-starved", 3)

	// PRECONDITION: every slot starts starved. Without this the test could pass
	// against rows that were never empty, proving nothing about backfilling.
	for slot := range 3 {
		if ph, _ := fanartHashes(t, db, "artist-starved", slot); ph != "" {
			t.Fatalf("precondition: slot %d phash = %q, want empty", slot, ph)
		}
	}

	if err := svc.BackfillFanartHashes(context.Background(), embyPrimary, 0); err != nil {
		t.Fatalf("BackfillFanartHashes: %v", err)
	}

	// Each slot must receive the hash of ITS OWN file. Asserting merely
	// "non-empty" would pass even if every slot got slot 0's hash -- the exact
	// cross-slot mix-up this whole change exists to prevent.
	seen := map[string]int{}
	for slot := range 3 {
		wantPath := filepath.Join(dir, img.FanartFilename(testPrimary, slot, false))
		fh, err := img.HashFile(wantPath, true)
		if err != nil {
			t.Fatalf("hashing expected file for slot %d: %v", slot, err)
		}
		want := img.HashHex(fh.Perceptual)

		gotPH, gotContent := fanartHashes(t, db, "artist-starved", slot)
		if gotPH == "" {
			t.Errorf("slot %d phash is empty -- backfill left the slot starved", slot)
			continue
		}
		if gotPH != want {
			t.Errorf("slot %d phash = %q, want its own file's %q", slot, gotPH, want)
		}
		if gotContent != fh.Content {
			t.Errorf("slot %d content_hash = %q, want %q", slot, gotContent, fh.Content)
		}
		seen[gotPH]++
	}

	// PRECONDITION on the fixture: the three files must hash distinctly, or the
	// per-slot assertions above could not have discriminated anything.
	if len(seen) != 3 {
		t.Errorf("fixture produced %d distinct phashes across 3 slots, want 3; the test cannot discriminate slots", len(seen))
	}
}

// TestBackfillFanartHashes_PreservesExistingHashes proves the pass is
// convergent: a row that already has a phash is not selected, so a healed
// library costs no work and the save path's better (EXIF-derived) value is never
// overwritten by this slower re-read.
func TestBackfillFanartHashes_PreservesExistingHashes(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := NewService(db, dbPath, "", slog.Default())
	seedFanartArtist(t, db, "artist-healed", 2)

	const sentinel = "1234567890abcdef"
	if _, err := db.ExecContext(context.Background(),
		`UPDATE artist_images SET phash = ?, content_hash = 'kept'
		 WHERE artist_id = 'artist-healed' AND slot_index = 0`, sentinel); err != nil {
		t.Fatalf("pre-filling slot 0: %v", err)
	}

	if err := svc.BackfillFanartHashes(context.Background(), embyPrimary, 0); err != nil {
		t.Fatalf("BackfillFanartHashes: %v", err)
	}

	ph, content := fanartHashes(t, db, "artist-healed", 0)
	if ph != sentinel {
		t.Errorf("slot 0 phash = %q, want the pre-existing %q -- backfill overwrote a hash it should not have selected", ph, sentinel)
	}
	if content != "kept" {
		t.Errorf("slot 0 content_hash = %q, want %q", content, "kept")
	}

	// The genuinely starved sibling must still have been filled, proving the
	// preservation above is selectivity and not the task no-opping entirely.
	if ph1, _ := fanartHashes(t, db, "artist-healed", 1); ph1 == "" {
		t.Error("slot 1 phash is empty -- backfill skipped a starved slot")
	}
}

// TestBackfillFanartHashes_RejectsMissingResolver pins the loud-failure
// contract. A nil or empty resolver must error rather than fall back to the
// default naming: on an Emby library the default discovers nothing, so the pass
// would report success having silently healed zero rows.
func TestBackfillFanartHashes_RejectsMissingResolver(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := NewService(db, dbPath, "", slog.Default())

	if err := svc.BackfillFanartHashes(context.Background(), nil, 0); err == nil {
		t.Error("nil resolver: expected an error, got nil -- a silent no-op here heals nothing while reporting success")
	}
	empty := func(context.Context) string { return "" }
	if err := svc.BackfillFanartHashes(context.Background(), empty, 0); err == nil {
		t.Error("empty primary name: expected an error, got nil")
	}
}

// TestBackfillFanartHashes_SkipsVanishedSlot covers a row that outlived its
// file (or whose slots were renumbered between the SELECT and the read). It
// must be skipped, not fail the pass and not fill the row with a neighbour's
// hash.
func TestBackfillFanartHashes_SkipsVanishedSlot(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := NewService(db, dbPath, "", slog.Default())
	seedFanartArtist(t, db, "artist-vanished", 1)

	// A row for a slot with no file behind it.
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, phash, content_hash)
		 VALUES ('ghost', 'artist-vanished', 'fanart', 5, 1, '', '')`); err != nil {
		t.Fatalf("seeding ghost row: %v", err)
	}

	if err := svc.BackfillFanartHashes(context.Background(), embyPrimary, 0); err != nil {
		t.Fatalf("BackfillFanartHashes: %v", err)
	}

	if ph, _ := fanartHashes(t, db, "artist-vanished", 5); ph != "" {
		t.Errorf("ghost slot 5 phash = %q, want empty -- a slot with no file was given some other file's hash", ph)
	}
	// The real slot must still be healed: one bad row must not poison the pass.
	if ph, _ := fanartHashes(t, db, "artist-vanished", 0); ph == "" {
		t.Error("slot 0 phash is empty -- a vanished sibling slot aborted the whole pass")
	}
}

// TestBackfillFanartHashes_RespectsMaxPerPass pins the bound, and that the
// remainder is still reachable on a later pass (convergence, not loss).
func TestBackfillFanartHashes_RespectsMaxPerPass(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := NewService(db, dbPath, "", slog.Default())
	seedFanartArtist(t, db, "artist-capped", 4)

	if err := svc.BackfillFanartHashes(context.Background(), embyPrimary, 2); err != nil {
		t.Fatalf("BackfillFanartHashes: %v", err)
	}

	filled := 0
	for slot := range 4 {
		if ph, _ := fanartHashes(t, db, "artist-capped", slot); ph != "" {
			filled++
		}
	}
	if filled != 2 {
		t.Errorf("filled %d slots in a capped pass, want exactly 2", filled)
	}

	// A second pass picks up the remainder: the cap defers work, never drops it.
	if err := svc.BackfillFanartHashes(context.Background(), embyPrimary, 2); err != nil {
		t.Fatalf("second BackfillFanartHashes: %v", err)
	}
	for slot := range 4 {
		if ph, _ := fanartHashes(t, db, "artist-capped", slot); ph == "" {
			t.Errorf("slot %d still starved after two capped passes -- the bound is dropping work rather than deferring it", slot)
		}
	}
}
