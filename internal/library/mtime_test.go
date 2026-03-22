package library

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCheckArtistDirMtimes_NewerFile(t *testing.T) {
	dir := t.TempDir()

	// Write an image file.
	imgPath := filepath.Join(dir, "folder.jpg")
	if err := os.WriteFile(imgPath, []byte("fake-jpg"), 0o644); err != nil {
		t.Fatalf("writing test file: %v", err)
	}

	// Set lastWrittenAt to 10 seconds ago so the file's mtime is newer.
	lastWrittenAt := time.Now().Add(-10 * time.Second)

	found, err := CheckArtistDirMtimes(dir, lastWrittenAt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Error("expected mtime mismatch to be detected")
	}
}

func TestCheckArtistDirMtimes_OlderFile(t *testing.T) {
	dir := t.TempDir()

	// Write an image file.
	imgPath := filepath.Join(dir, "folder.jpg")
	if err := os.WriteFile(imgPath, []byte("fake-jpg"), 0o644); err != nil {
		t.Fatalf("writing test file: %v", err)
	}

	// Set lastWrittenAt to the future so the file's mtime is older.
	lastWrittenAt := time.Now().Add(10 * time.Second)

	found, err := CheckArtistDirMtimes(dir, lastWrittenAt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Error("expected no mtime mismatch when file is older than lastWrittenAt")
	}
}

func TestCheckArtistDirMtimes_NoImageFiles(t *testing.T) {
	dir := t.TempDir()

	// Write a non-image file.
	txtPath := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(txtPath, []byte("not an image"), 0o644); err != nil {
		t.Fatalf("writing test file: %v", err)
	}

	lastWrittenAt := time.Now().Add(-10 * time.Second)

	found, err := CheckArtistDirMtimes(dir, lastWrittenAt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Error("expected no mtime mismatch when directory has no image files")
	}
}

func TestCheckArtistDirMtimes_NonexistentDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")

	lastWrittenAt := time.Now().Add(-10 * time.Second)

	found, err := CheckArtistDirMtimes(dir, lastWrittenAt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Error("expected no mtime mismatch for nonexistent directory")
	}
}

func TestCheckArtistDirMtimes_ZeroLastWrittenAt(t *testing.T) {
	dir := t.TempDir()

	// Write an image file.
	imgPath := filepath.Join(dir, "fanart.png")
	if err := os.WriteFile(imgPath, []byte("fake-png"), 0o644); err != nil {
		t.Fatalf("writing test file: %v", err)
	}

	// Zero lastWrittenAt means Stillwater has never written to this artist,
	// so there is nothing to compare against.
	found, err := CheckArtistDirMtimes(dir, time.Time{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Error("expected no mtime mismatch when lastWrittenAt is zero")
	}
}

func TestCheckArtistDirMtimes_WithinTolerance(t *testing.T) {
	dir := t.TempDir()

	// Write an image file.
	imgPath := filepath.Join(dir, "folder.jpg")
	if err := os.WriteFile(imgPath, []byte("fake-jpg"), 0o644); err != nil {
		t.Fatalf("writing test file: %v", err)
	}

	// Set lastWrittenAt to 1 second ago -- within the 2-second tolerance.
	// The file was just created so its mtime is ~now; the difference is only
	// ~1 second which is below the FAT32 tolerance threshold.
	lastWrittenAt := time.Now().Add(-1 * time.Second)

	found, err := CheckArtistDirMtimes(dir, lastWrittenAt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Error("expected no mtime mismatch when file is within 2-second tolerance")
	}
}

func TestCheckArtistDirMtimes_MultipleExtensions(t *testing.T) {
	dir := t.TempDir()

	// Write image files with different extensions.
	for _, name := range []string{"thumb.png", "fanart.jpeg", "logo.webp"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	lastWrittenAt := time.Now().Add(-10 * time.Second)

	found, err := CheckArtistDirMtimes(dir, lastWrittenAt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Error("expected mtime mismatch with multiple image extensions")
	}
}

func TestCollectMtimeEvidence_MixedDirs(t *testing.T) {
	// This test covers the interleaved-writes scenario that motivated the
	// per-artist mtime baseline change (#598). With a global MAX approach,
	// Artist B's future timestamp would be used for both directories, causing
	// Artist A's external modification to be missed. With per-artist baselines,
	// each directory uses its own lastWrittenAt and only Artist A is flagged.

	// Create two artist directories.
	dirA := t.TempDir()
	dirB := t.TempDir()

	// dirA: image file newer than lastWrittenAt
	imgA := filepath.Join(dirA, "folder.jpg")
	if err := os.WriteFile(imgA, []byte("data"), 0o644); err != nil {
		t.Fatalf("writing: %v", err)
	}

	// dirB: image file older than lastWrittenAt
	imgB := filepath.Join(dirB, "folder.jpg")
	if err := os.WriteFile(imgB, []byte("data"), 0o644); err != nil {
		t.Fatalf("writing: %v", err)
	}

	artistDirs := map[string]string{
		"Artist A": dirA,
		"Artist B": dirB,
	}
	lastWrittenAts := map[string]time.Time{
		dirA: time.Now().Add(-10 * time.Second), // file is newer
		dirB: time.Now().Add(10 * time.Second),  // file is older
	}

	evidence := CollectMtimeEvidence(artistDirs, lastWrittenAts, nil)
	if len(evidence) != 1 {
		t.Fatalf("expected 1 evidence item, got %d", len(evidence))
	}
	if evidence[0].Path != imgA {
		t.Errorf("expected evidence path %q, got %q", imgA, evidence[0].Path)
	}
}

func TestCollectMtimeEvidence_NoEvidence(t *testing.T) {
	dir := t.TempDir()

	artistDirs := map[string]string{
		"Artist": dir,
	}
	// No lastWrittenAt entry for the directory.
	lastWrittenAts := map[string]time.Time{}

	evidence := CollectMtimeEvidence(artistDirs, lastWrittenAts, nil)
	if len(evidence) != 0 {
		t.Fatalf("expected 0 evidence items, got %d", len(evidence))
	}
}
