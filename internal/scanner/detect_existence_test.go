package scanner

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
)

// writeTestFile writes a small placeholder file at dir/name so the
// detectFilesExistenceOnly cheap-existence path can find it without an
// image-decode probe.
func writeTestFile(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
}

// TestDetectFilesExistenceOnly_AllPatterns drives detectFilesExistenceOnly --
// the mtime fast-path's existence-only probe -- through every recognized
// canonical filename pattern at once. It verifies that the cheap path sets
// the right *Exists flags from disk reality (the contract pinned by issue
// #1225's registry-vs-disk reconciliation) and reuses the supplied existing
// Artist's dimensions / placeholders / low-res flags (which the expensive
// probe would otherwise regenerate).
func TestDetectFilesExistenceOnly_AllPatterns(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTestFile(t, dir, "artist.nfo")
	writeTestFile(t, dir, "folder.jpg") // matches thumbPatterns
	writeTestFile(t, dir, "fanart.jpg")
	writeTestFile(t, dir, "logo.png")
	writeTestFile(t, dir, "banner.jpg")

	// LastScannedAt is set far in the future so the per-file mtime check
	// passes; the in-place rewrite case is covered separately by
	// TestDetectFilesExistenceOnly_InPlaceFileRewrite.
	future := time.Now().Add(time.Hour)
	existing := &artist.Artist{
		LastScannedAt: &future,
		ThumbWidth:    1000, ThumbHeight: 1500, ThumbLowRes: false,
		ThumbPlaceholder:  "thumb-placeholder",
		FanartWidth:       1920,
		FanartHeight:      1080,
		FanartLowRes:      true,
		FanartPlaceholder: "fanart-placeholder",
		LogoWidth:         400, LogoHeight: 200, LogoPlaceholder: "logo-placeholder",
		BannerWidth: 800, BannerHeight: 200, BannerPlaceholder: "banner-placeholder",
	}
	got, err := detectFilesExistenceOnly(dir, existing)
	if err != nil {
		t.Fatalf("detectFilesExistenceOnly: %v", err)
	}

	// Existence flags must come from disk reality.
	if !got.NFOExists || !got.ThumbExists || !got.FanartExists ||
		!got.LogoExists || !got.BannerExists {
		t.Errorf("expected every *Exists=true; got %+v", got)
	}
	// FanartCount falls back to 1 when DiscoverFanart finds no variants.
	if got.FanartCount < 1 {
		t.Errorf("FanartCount=%d; expected >=1", got.FanartCount)
	}
	// Dimensions / placeholders / low-res must be reused from `existing`
	// because the fast path skips the expensive probe.
	if got.ThumbWidth != 1000 || got.ThumbHeight != 1500 || got.ThumbPlaceholder != "thumb-placeholder" {
		t.Errorf("thumb fields not reused from existing; got %+v", got)
	}
	if got.FanartWidth != 1920 || got.FanartHeight != 1080 || !got.FanartLowRes ||
		got.FanartPlaceholder != "fanart-placeholder" {
		t.Errorf("fanart fields not reused from existing; got %+v", got)
	}
	if got.LogoWidth != 400 || got.LogoHeight != 200 || got.LogoPlaceholder != "logo-placeholder" {
		t.Errorf("logo fields not reused from existing; got %+v", got)
	}
	if got.BannerWidth != 800 || got.BannerHeight != 200 || got.BannerPlaceholder != "banner-placeholder" {
		t.Errorf("banner fields not reused from existing; got %+v", got)
	}
}

// TestDetectFilesExistenceOnly_EmptyDir verifies the no-file branch: every
// *Exists flag stays false and the function returns a zero detectedFiles
// instead of an error. The scanner relies on this to surface file removals
// (the inverse of the registry-vs-disk drift case): when a file disappears,
// the fast-path must report Exists=false so processExistingArtist can clear
// the stored flag.
func TestDetectFilesExistenceOnly_EmptyDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	future := time.Now().Add(time.Hour)
	existing := &artist.Artist{
		LastScannedAt: &future,
		ThumbExists:   true, FanartExists: true,
		ThumbPlaceholder: "stale-placeholder",
	}
	got, err := detectFilesExistenceOnly(dir, existing)
	if err != nil {
		t.Fatalf("detectFilesExistenceOnly: %v", err)
	}
	if got.NFOExists || got.ThumbExists || got.FanartExists ||
		got.LogoExists || got.BannerExists {
		t.Errorf("empty dir should yield all-false flags; got %+v", got)
	}
}

// TestDetectFilesExistenceOnly_ReadDirError surfaces a missing-directory
// error -- the scanner uses this to short-circuit per-file work when the
// underlying directory has been removed mid-scan.
func TestDetectFilesExistenceOnly_ReadDirError(t *testing.T) {
	t.Parallel()
	future := time.Now().Add(time.Hour)
	got, err := detectFilesExistenceOnly("/this/path/does/not/exist/i/hope", &artist.Artist{LastScannedAt: &future})
	if err == nil {
		t.Fatalf("expected error for missing dir; got %+v", got)
	}
}

// TestDetectFilesExistenceOnly_InPlaceFileRewrite pins the POSIX-aware
// per-file mtime check: when a canonical file (here, fanart.jpg) is
// rewritten in place AFTER existing.LastScannedAt, the parent directory's
// mtime does NOT advance on Linux/POSIX, so the directory-mtime gate
// alone would miss the change. detectFilesExistenceOnly catches the
// in-place rewrite by checking each canonical file's own mtime and
// returns errFastPathFileTouched so the caller falls back to the full
// detectFiles probe.
func TestDetectFilesExistenceOnly_InPlaceFileRewrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTestFile(t, dir, "fanart.jpg")

	// LastScannedAt is in the past; the file we just wrote has mtime
	// "now", so it must be detected as a fresh in-place rewrite.
	past := time.Now().Add(-time.Hour)
	existing := &artist.Artist{LastScannedAt: &past}

	_, err := detectFilesExistenceOnly(dir, existing)
	if !errors.Is(err, errFastPathFileTouched) {
		t.Fatalf("expected errFastPathFileTouched for file newer than LastScannedAt; got %v", err)
	}
}

// TestDetectFilesExistenceOnly_FileMtimeBeforeLastScan covers the happy
// path of the per-file mtime check: when every canonical file's mtime is
// at or before LastScannedAt, the fast path proceeds with cached
// dimensions and no error.
func TestDetectFilesExistenceOnly_FileMtimeBeforeLastScan(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTestFile(t, dir, "fanart.jpg")

	// LastScannedAt is far in the future, so every file mtime is "before"
	// the last scan and the fast path is applicable.
	future := time.Now().Add(time.Hour)
	existing := &artist.Artist{LastScannedAt: &future, FanartWidth: 1920, FanartHeight: 1080}

	got, err := detectFilesExistenceOnly(dir, existing)
	if err != nil {
		t.Fatalf("detectFilesExistenceOnly: %v", err)
	}
	if !got.FanartExists {
		t.Errorf("FanartExists should be true; got %+v", got)
	}
	if got.FanartWidth != 1920 || got.FanartHeight != 1080 {
		t.Errorf("dimensions not reused from existing; got %+v", got)
	}
}

// TestDetectFilesExistenceOnly_MultipleFanart pins the fanart-count branch:
// when fanart-N.jpg variants exist alongside fanart.jpg, img.DiscoverFanart
// counts them and FanartCount reflects the total -- otherwise the registry
// would silently believe there is only one extraneous-images-eligible file.
func TestDetectFilesExistenceOnly_MultipleFanart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTestFile(t, dir, "fanart.jpg")
	writeTestFile(t, dir, "fanart1.jpg") // DiscoverFanart format: {base}{N}, not {base}-{N}
	writeTestFile(t, dir, "fanart2.jpg")

	future := time.Now().Add(time.Hour)
	got, err := detectFilesExistenceOnly(dir, &artist.Artist{LastScannedAt: &future})
	if err != nil {
		t.Fatalf("detectFilesExistenceOnly: %v", err)
	}
	if !got.FanartExists {
		t.Errorf("FanartExists should be true; got %+v", got)
	}
	if got.FanartCount < 3 {
		t.Errorf("FanartCount=%d; expected >=3 (fanart + 2 variants)", got.FanartCount)
	}
}
