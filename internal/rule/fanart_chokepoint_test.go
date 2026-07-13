package rule

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	img "github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/provider"
)

// makeChokepointPNG encodes a real PNG. The fanart originals in these tests MUST be
// genuinely PNG-encoded, not JPEG bytes in a .png file: Save picks the output extension
// from the DECODED format, so a mislabelled original would be restored to fanart.jpg and
// the test would report a data-loss failure that is really a fixture bug.
//
// It is also what makes the destruction real. A png original replaced by a jpg is a
// DELETE (CleanupConflictingFormats removes fanart.png), not an overwrite -- which is why
// the atomic write never protected anyone.
func makeChokepointPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			m.Set(x, y, color.RGBA{R: 12, G: 240, B: 96, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, m); err != nil {
		t.Fatalf("encoding test png: %v", err)
	}
	return buf.Bytes()
}

// The rule engine's fanart writes are the WORST case of #2413, and they are what #2433
// closes.
//
// They run UNATTENDED, ACROSS THE WHOLE LIBRARY. ruleToImageType maps fanart_exists,
// fanart_min_res and fanart_aspect to "fanart", and the latter two fire precisely BECAUSE
// a fanart already exists -- it is too small, or the wrong shape. So the fixer downloads a
// replacement and overwrites the user's primary backdrop. In bulk auto-fix mode
// (BulkExecutor, Mode "auto") that happens for every artist in the library with no
// confirmation and, before this change, with no backup and no rollback: SaveImageFromData
// called img.Save directly.
//
// These tests assert the OUTCOME on disk -- the user's original bytes -- never an error
// value or a "fixed" flag.

// TestSaveImageFromData_RollsBackAFailedFanartOverwrite proves the rule engine's shared
// save funnel now restores the user's fanart when the write fails.
//
// The fault is the one that actually reaches the rollback: naming[1] cannot be written
// ("blocked" is a regular file, so the write fails ENOTDIR), and by the time it fails,
// Save's CleanupConflictingFormats has ALREADY DELETED the fanart.png original to make
// way for fanart.jpg. Undecodable bytes would be useless here -- ConvertFormat rejects
// them long before anything is destroyed, so such a test passes against the broken code.
//
// Routed through img.Save (main), this leaves the artwork deleted and unrecoverable.
func TestSaveImageFromData_RollsBackAFailedFanartOverwrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	original := makeChokepointPNG(t, 400, 225)
	if err := os.WriteFile(filepath.Join(dir, "fanart.png"), original, 0o644); err != nil {
		t.Fatalf("seeding the user's fanart: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blocked"), []byte("a regular file, not a directory"), 0o644); err != nil {
		t.Fatalf("seeding the write barrier: %v", err)
	}

	a := &artist.Artist{Name: "Rollback", Path: dir}
	naming := []string{"fanart.jpg", "blocked/fanart.jpg"}

	_, err := SaveImageFromData(context.Background(), a, "fanart", makeTestJPEG(t, 1920, 1080),
		naming, false, nil, nil, testLogger())
	if err == nil {
		t.Fatal("expected the unwritable second filename to fail the save; without a failure this test is vacuous")
	}

	// THE ASSERTION: the user's backdrop is back, byte for byte. The save deleted it as a
	// conflicting format and then failed.
	got, readErr := os.ReadFile(filepath.Join(dir, "fanart.png"))
	if readErr != nil {
		t.Fatalf("the user's fanart is GONE after a failed rule-engine overwrite (%v). The rule engine "+
			"wrote through img.Save with no backup and no rollback, so a failed auto-fix destroys the "+
			"artwork outright -- library-wide, unattended (#2433)", readErr)
	}
	if !bytes.Equal(got, original) {
		t.Errorf("the restored fanart is not the user's original: got %d bytes, want %d", len(got), len(original))
	}
}

// TestImageFixer_AutoFix_BacksUpTheUsersFanartBeforeReplacingIt is the end-to-end #2433
// guard: it drives the REAL auto-fix path (ImageFixer.Fix -> downloadAndPersist ->
// SaveImageFromData) for fanart_min_res, the rule that fires only when a fanart is
// ALREADY on disk.
//
// The fix SUCCEEDS here, which is the point. There is no rollback to lean on on the happy
// path, so if the replacement is written with no backup the user's original backdrop is
// simply gone -- silently, with no error and no way to revert. A successful auto-fix must
// still leave the original recoverable.
func TestImageFixer_AutoFix_BacksUpTheUsersFanartBeforeReplacingIt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// The user's existing backdrop: real, and too small, which is why fanart_min_res fires.
	original := makeChokepointPNG(t, 400, 225)
	if err := os.WriteFile(filepath.Join(dir, "fanart.png"), original, 0o644); err != nil {
		t.Fatalf("seeding the user's fanart: %v", err)
	}

	replacement := makeTestJPEG(t, 1920, 1080)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		if _, err := w.Write(replacement); err != nil {
			t.Errorf("serving the replacement image: %v", err)
		}
	}))
	defer srv.Close()

	mock := &mockImageProvider{
		result: &provider.FetchResult{
			Images: []provider.ImageResult{
				{URL: srv.URL + "/fanart.jpg", Type: "fanart", Width: 1920, Height: 1080, Source: "fanarttv"},
			},
		},
	}

	f := NewImageFixer(mock, nil, nonSharedFSCheck(), testLogger())
	// The SSRF-safe default client blocks loopback, so the httptest server needs a plain one.
	f.httpClient = &http.Client{}

	a := &artist.Artist{
		Name:          "Auto Fix",
		MusicBrainzID: "mbid-fanart",
		Path:          dir,
		LibraryID:     "lib-test",
	}
	v := &Violation{
		RuleID: RuleFanartMinRes,
		Config: RuleConfig{MinWidth: 1280, MinHeight: 720},
	}

	fr, err := f.Fix(context.Background(), a, v)
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	// Non-vacuity: if the fix did not actually write, everything below is meaningless.
	if !fr.Fixed {
		t.Fatalf("the auto-fix did not run (Fixed = false, message %q); this test would assert nothing", fr.Message)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "fanart.jpg")); statErr != nil {
		t.Fatalf("the replacement fanart was never written (%v); the fix reported success while doing nothing", statErr)
	}

	// THE ASSERTION: the overwritten original is recoverable. The replacement's
	// conflicting-format cleanup DELETED fanart.png; only a backup can bring it back.
	backup, readErr := os.ReadFile(filepath.Join(dir, img.BackupDirName, "fanart", "fanart.png"))
	if readErr != nil {
		t.Fatalf("an unattended auto-fix REPLACED the user's backdrop and kept NO backup of it (%v). "+
			"fanart_min_res fires because a fanart already exists, so this destroys real artwork across "+
			"the whole library in bulk auto mode, with no way to revert (#2433)", readErr)
	}
	if !bytes.Equal(backup, original) {
		t.Errorf("the backup does not hold the user's original bytes: got %d, want %d", len(backup), len(original))
	}
	if !img.HasBackup(dir, "fanart") {
		t.Error("HasBackup reports no fanart backup, so the UI will not offer the user a revert")
	}
}

// TestSaveImageToDisk_NonFanartTypesAreStillUnprotected pins the KNOWN, DELIBERATE residual
// so it is a recorded fact rather than a surprise.
//
// #2433 routes FANART through the protected chokepoint. thumb/logo/banner still go to a
// bare img.Save from the rule engine: no backup, no rollback. That is the same bug class,
// and it is NOT closed here -- the single-slot types use a different backup mechanism
// (img.BackupSingleSlot, one-deep PER TYPE) whose prune would corrupt the slot-scoped
// backups this chokepoint writes, so mixing them in one .sw-backup/<type>/ dir would break
// the Router's revert. Closing it means lifting the Router's single-slot policy into
// internal/image as a second shared chokepoint.
//
// This test asserts the CURRENT truth. When that follow-up lands it should go RED and be
// replaced by a backup assertion -- that is the intent, not a regression.
func TestSaveImageToDisk_NonFanartTypesAreStillUnprotected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	original := makeTestJPEG(t, 100, 100)
	if err := os.WriteFile(filepath.Join(dir, "folder.jpg"), original, 0o644); err != nil {
		t.Fatalf("seeding the thumb: %v", err)
	}

	if _, err := saveImageToDisk(dir, "thumb", makeTestJPEG(t, 500, 500),
		[]string{"folder.jpg"}, false, nil, testLogger()); err != nil {
		t.Fatalf("saveImageToDisk: %v", err)
	}

	if img.HasBackup(dir, "thumb") {
		t.Error("a thumb backup now exists: the rule engine's single-slot writes have become protected. " +
			"That is the desired end state -- delete this test and assert the backup instead")
	}
}
