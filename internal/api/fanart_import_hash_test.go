package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	img "github.com/sydlexius/stillwater/internal/image"
)

// importJPEG returns a decodable JPEG. Seeds 0-3 are perceptually distinct
// patterns with a non-zero phash; seed -1 is a solid color, whose phash is 0.
func importJPEG(t *testing.T, seed int) []byte {
	t.Helper()
	m := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			var v uint8
			switch seed {
			case -1:
				v = 90
			case 0:
				v = uint8(x * 4)
			case 1:
				v = uint8((x*4 + y*2) % 256)
			case 2:
				v = uint8((x*x + y*3) % 256)
			default:
				v = uint8((255 - x*4 + (y*y)%64) % 256)
			}
			m.Set(x, y, color.RGBA{R: v, G: 255 - v, B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, m, nil); err != nil {
		t.Fatalf("encoding jpeg: %v", err)
	}
	return buf.Bytes()
}

func importPhash(t *testing.T, data []byte) string {
	t.Helper()
	h, err := img.PerceptualHash(bytes.NewReader(data))
	if err != nil || h == 0 {
		t.Fatalf("fixture phash unusable: %v", err)
	}
	return img.HashHex(h)
}

// populate runs the real populate pipeline against a fake platform serving
// platform[i] for backdrop index i, re-reading the artist first.
func populate(t *testing.T, r *Router, artistID string, platform [][]byte) populateResult {
	t.Helper()
	ctx := context.Background()
	a, err := r.artistService.GetByID(ctx, artistID)
	if err != nil {
		t.Fatalf("reading artist: %v", err)
	}
	dl := &mockImageDownloader{backdropFn: func(_ context.Context, _ string, i int) ([]byte, string, error) {
		if platform[i] == nil {
			return nil, "", errors.New("transient")
		}
		return platform[i], "image/jpeg", nil
	}}
	tags := make([]string, len(platform))
	for i := range tags {
		tags[i] = fmt.Sprintf("t%d", i)
	}
	var res populateResult
	r.downloadPlatformImages(ctx, dl, "item-1", nil, tags, a, "emby", &res)
	return res
}

// fanartFiles lists the artist's cache fanart in scanner (slot) order.
func fanartFiles(t *testing.T, r *Router, artistID string) []string {
	t.Helper()
	names, err := img.ResolveFanartNames(nil)
	if err != nil {
		t.Fatalf("names: %v", err)
	}
	paths, err := img.ResolveFanartFiles(context.Background(), filepath.Join(r.imageCacheDir, artistID), names)
	if err != nil {
		t.Fatalf("resolving fanart: %v", err)
	}
	return paths
}

// importFanart creates a path-less artist (optionally seeding extra SQL) and
// imports platform backdrops [A, A, B] through the real populate pipeline.
func importFanart(t *testing.T, seed string) (*Router, *artist.Artist, []byte, []byte, populateResult) {
	return importFanartAt(t, "", seed)
}

func importFanartAt(t *testing.T, path, seed string) (*Router, *artist.Artist, []byte, []byte, populateResult) {
	t.Helper()
	r := testRouterForLibraryOps(t)
	ctx := context.Background()
	a := &artist.Artist{Name: "Import Hash", SortName: "Import Hash", Path: path}
	if err := r.artistService.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	if seed != "" {
		var args []any
		if strings.Contains(seed, "?") {
			args = append(args, a.ID)
		}
		if _, err := r.db.ExecContext(ctx, seed, args...); err != nil {
			t.Fatalf("seeding: %v", err)
		}
	}
	imgA, imgB := importJPEG(t, 0), importJPEG(t, 1)
	return r, a, imgA, imgB, populate(t, r, a.ID, [][]byte{imgA, imgA, imgB})
}

func fanartHashes(t *testing.T, r *Router, artistID string) map[int][2]string {
	t.Helper()
	rows, err := r.db.QueryContext(context.Background(),
		`SELECT slot_index, phash, content_hash FROM artist_images WHERE artist_id = ? AND image_type = 'fanart'`, artistID)
	if err != nil {
		t.Fatalf("querying: %v", err)
	}
	defer rows.Close()
	out := map[int][2]string{}
	for rows.Next() {
		var s int
		var p, c string
		if err := rows.Scan(&s, &p, &c); err != nil {
			t.Fatalf("scanning: %v", err)
		}
		out[s] = [2]string{p, c}
	}
	return out
}

// TestHashImportedFanart_EqualCountCounterexample: platform [A, A, B] imports as
// local [A, B]; local slot 1 must carry B's hash (platform index 2), never A's.
func TestHashImportedFanart_EqualCountCounterexample(t *testing.T) {
	r, a, imgA, imgB, res := importFanart(t, "")
	if res.Images != 2 {
		t.Fatalf("precondition: want 2 backdrops written (duplicate skipped), got %d", res.Images)
	}
	got := fanartHashes(t, r, a.ID)
	if len(got) != 2 {
		t.Fatalf("want rows for slots 0 and 1, got %v", got)
	}
	if got[0][0] != importPhash(t, imgA) || got[0][1] == "" {
		t.Errorf("slot 0: want A's phash and a content hash, got %v", got[0])
	}
	if got[1][0] != importPhash(t, imgB) || got[1][1] == "" {
		t.Errorf("slot 1: want B's phash (platform index 2), got %v (A=%s)", got[1], importPhash(t, imgA))
	}
}

// TestHashImportedFanart_GappedSlotsWriteNothing: a stale live row at slot 5
// breaks the 0..n-1 mapping, so no row of the artist is hashed.
func TestHashImportedFanart_GappedSlotsWriteNothing(t *testing.T) {
	r, a, _, _, res := importFanart(t,
		`INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag) VALUES ('gap', ?, 'fanart', 5, 1)`)
	if res.Images != 2 {
		t.Fatalf("precondition: want 2 backdrops written, got %d", res.Images)
	}
	// Slot 0 is excluded: the primary's existing save-time provenance hashes it
	// independently of this pass.
	for slot, h := range fanartHashes(t, r, a.ID) {
		if slot != 0 && (h[0] != "" || h[1] != "") {
			t.Errorf("slot %d hashed despite gapped rows: %v", slot, h)
		}
	}
}

// TestHashImportedFanart_FailedWriteStillHashesOtherSlots: slot 0's hash write
// fails (a trigger aborts it); the import succeeds and slot 1 is still hashed,
// so one failure skips one slot rather than ending the pass.
func TestHashImportedFanart_FailedWriteStillHashesOtherSlots(t *testing.T) {
	r, a, _, imgB, res := importFanart(t,
		`CREATE TRIGGER no_hash BEFORE UPDATE OF phash ON artist_images WHEN NEW.slot_index = 0 BEGIN SELECT RAISE(ABORT, 'boom'); END`)
	if res.Images != 2 {
		t.Fatalf("import must still write both backdrops, got %d", res.Images)
	}
	got := fanartHashes(t, r, a.ID)
	if got[0][0] != "" {
		t.Fatalf("precondition: slot 0's write should have failed, got %v", got[0])
	}
	if got[1][0] != importPhash(t, imgB) {
		t.Errorf("slot 1 not hashed after slot 0's write failed: %v", got[1])
	}
}

// TestHashImportedFanart_ZeroPhashStoredEmpty: a solid image hashes to the
// zero "unknown" sentinel, which is stored as "" (content hash kept), never "000...0".
func TestHashImportedFanart_ZeroPhashStoredEmpty(t *testing.T) {
	r, a, _, _, _ := importFanart(t, "")
	solid := importJPEG(t, -1)
	if h, err := img.PerceptualHash(bytes.NewReader(solid)); err != nil || h != 0 {
		t.Fatalf("precondition: solid fixture must hash to 0, got %x (%v)", h, err)
	}
	populate(t, r, a.ID, [][]byte{nil, nil, nil, solid})
	got := fanartHashes(t, r, a.ID)
	if len(got) != 3 || got[2][0] != "" || got[2][1] == "" {
		t.Errorf("slot 2 (solid): want empty phash and a content hash, got %v", got)
	}
}

// TestHashImportedFanart_LibraryPathArtistNotHashed: artists with a library
// folder are left to the local pass (#3600).
func TestHashImportedFanart_LibraryPathArtistNotHashed(t *testing.T) {
	r, a, _, _, res := importFanartAt(t, t.TempDir(), "")
	if res.Images != 2 {
		t.Fatalf("precondition: want 2 backdrops written, got %d", res.Images)
	}
	if got := fanartHashes(t, r, a.ID); len(got) != 2 || got[1] != [2]string{"", ""} {
		t.Errorf("library-path artist's slot 1 hashed at import: %v", got)
	}
}

// TestHashImportedFanart_CompactionWithoutDownloadRehashes: the primary was
// evicted (the exists scan has since cleared the trailing row) and index 0
// fails this run, so populate downloads nothing but compaction renumbers,
// clearing every fanart hash. The rows are re-hashed from the files on disk.
func TestHashImportedFanart_CompactionWithoutDownloadRehashes(t *testing.T) {
	r, a, _, _, _ := importFanart(t, "")
	imgs := [][]byte{importJPEG(t, 0), importJPEG(t, 1), importJPEG(t, 2)}
	populate(t, r, a.ID, [][]byte{imgs[0], imgs[1], nil, imgs[2]}) // 3 slots: A, B, C
	if err := os.Remove(fanartFiles(t, r, a.ID)[0]); err != nil {
		t.Fatalf("evicting the primary: %v", err)
	}
	// What ScanExistsFlags does for a slot past the on-disk fanart count.
	if _, err := r.db.ExecContext(context.Background(),
		`UPDATE artist_images SET exists_flag = 0 WHERE artist_id = ? AND image_type = 'fanart' AND slot_index = 2`, a.ID); err != nil {
		t.Fatalf("clearing trailing row: %v", err)
	}
	if res := populate(t, r, a.ID, [][]byte{nil, nil, nil}); res.Images != 0 {
		t.Fatalf("precondition: want no download, got %d", res.Images)
	}
	got := fanartHashes(t, r, a.ID)
	if got[0][0] != importPhash(t, imgs[1]) || got[1][0] != importPhash(t, imgs[2]) || got[1][1] == "" {
		t.Errorf("after a compaction-only re-populate want slots 0,1 = B,C hashed, got %v", got)
	}
}

// TestHashImportedFanart_RedownloadedSlotRehashed: slot 2's file was evicted
// and the platform now serves a different picture there. The re-downloaded slot
// must get the NEW file's hashes, not keep its predecessor's. A non-fresh row
// with only a content hash is re-hashed whole, never mixing two files' values.
func TestHashImportedFanart_RedownloadedSlotRehashed(t *testing.T) {
	r, a, _, _, _ := importFanart(t, "")
	imgs := [][]byte{importJPEG(t, 0), importJPEG(t, 1), importJPEG(t, 2)}
	populate(t, r, a.ID, append(imgs[:1:1], imgs[1], nil, imgs[2])) // 3 slots: A, B, C
	files := fanartFiles(t, r, a.ID)
	before := fanartHashes(t, r, a.ID)
	if len(files) != 3 || before[2][0] != importPhash(t, imgs[2]) {
		t.Fatalf("precondition: want 3 hashed slots, got %v", before)
	}
	if err := os.Remove(files[2]); err != nil {
		t.Fatalf("evicting slot 2: %v", err)
	}
	if _, err := r.db.ExecContext(context.Background(),
		`UPDATE artist_images SET phash = '' , content_hash = 'stale' WHERE artist_id = ? AND slot_index = 1`, a.ID); err != nil {
		t.Fatalf("seeding partial row: %v", err)
	}
	replaced := importJPEG(t, 3)
	if res := populate(t, r, a.ID, [][]byte{nil, nil, nil, replaced}); res.Images != 1 {
		t.Fatalf("precondition: want 1 re-download, got %d", res.Images)
	}
	after := fanartHashes(t, r, a.ID)
	if after[2][0] != importPhash(t, replaced) || after[2][1] == before[2][1] {
		t.Errorf("slot 2 keeps its predecessor's hash: before %v after %v", before[2], after[2])
	}
	if after[1][1] == "stale" || after[1][0] != importPhash(t, imgs[1]) {
		t.Errorf("slot 1 mixes values from two files: %v", after[1])
	}
}

// TestHashImportedFanart_UnreadableDirWritesNothing: a directory that cannot be
// read is "cannot tell", so nothing is written. Note: with the error branch
// removed, nil paths also fail the contiguity guard, so this test pins the
// fail-closed outcome (and kills a name-guessing fallback), not that branch.
func TestHashImportedFanart_UnreadableDirWritesNothing(t *testing.T) {
	r, a, _, _, _ := importFanart(t, "")
	ctx := context.Background()
	if _, err := r.db.ExecContext(ctx, `UPDATE artist_images SET phash = '', content_hash = '' WHERE artist_id = ?`, a.ID); err != nil {
		t.Fatalf("resetting hashes: %v", err)
	}
	dir := filepath.Join(r.imageCacheDir, a.ID)
	// 0o300: files stay openable by name, only the listing fails, so a writer
	// that guessed names instead of failing closed would still find bytes.
	if err := os.Chmod(dir, 0o300); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := os.ReadDir(dir); err == nil {
		t.Fatal("precondition: directory still readable (running as root?)")
	}
	r.hashImportedFanart(ctx, a.ID, dir, nil)
	for slot, h := range fanartHashes(t, r, a.ID) {
		if h[0] != "" || h[1] != "" {
			t.Errorf("slot %d hashed from an unreadable dir: %v", slot, h)
		}
	}
}

// TestStoreImportedFanartHash_CASKeepsConcurrentHash: a hash another writer
// stored after the read is never overwritten.
func TestStoreImportedFanartHash_CASKeepsConcurrentHash(t *testing.T) {
	r, a, _, _, _ := importFanart(t, "")
	ctx := context.Background()
	if _, err := r.db.ExecContext(ctx,
		`UPDATE artist_images SET phash = 'other', content_hash = 'other' WHERE artist_id = ? AND slot_index = 1`, a.ID); err != nil {
		t.Fatalf("simulating concurrent writer: %v", err)
	}
	if err := r.storeImportedFanartHash(ctx, a.ID, 1, "", "", "mine", "mine"); err != nil {
		t.Fatalf("store: %v", err)
	}
	if got := fanartHashes(t, r, a.ID)[1]; got != [2]string{"other", "other"} {
		t.Errorf("concurrent hash clobbered: %v", got)
	}
}
