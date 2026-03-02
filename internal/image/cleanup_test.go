package image

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestCleanupConflictingFormats_DeletesOldFormat(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Create an existing folder.jpg
	oldPath := filepath.Join(dir, "folder.jpg")
	if err := os.WriteFile(oldPath, []byte("old jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Saving folder.png should delete folder.jpg
	if err := CleanupConflictingFormats(dir, "folder.png", logger); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Error("folder.jpg should have been deleted")
	}
}

func TestCleanupConflictingFormats_DeletesJpegVariant(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Create both .jpg and .jpeg variants
	for _, ext := range []string{".jpg", ".jpeg"} {
		path := filepath.Join(dir, "fanart"+ext)
		if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Saving fanart.png should delete both
	if err := CleanupConflictingFormats(dir, "fanart.png", logger); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, ext := range []string{".jpg", ".jpeg"} {
		path := filepath.Join(dir, "fanart"+ext)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("fanart%s should have been deleted", ext)
		}
	}
}

func TestCleanupConflictingFormats_PreservesOwnFormat(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Create folder.jpg
	path := filepath.Join(dir, "folder.jpg")
	if err := os.WriteFile(path, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Saving folder.jpg should NOT delete folder.jpg
	if err := CleanupConflictingFormats(dir, "folder.jpg", logger); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Error("folder.jpg should NOT have been deleted when saving same format")
	}
}

func TestCleanupConflictingFormats_NoConflicts(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// No files exist, should succeed without error
	if err := CleanupConflictingFormats(dir, "folder.png", logger); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCleanupConflictingFormats_UnknownExtension(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Unknown extension should be a no-op
	if err := CleanupConflictingFormats(dir, "file.bmp", logger); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCleanupConflictingFormats_CaseMismatchSameFormat(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Create a differently-cased file of the same format
	if err := os.WriteFile(filepath.Join(dir, "Folder.JPG"), []byte("old jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Cleaning up for "folder.jpg" should rename "Folder.JPG" to "folder.jpg"
	// (preserving content in case the subsequent write fails)
	if err := CleanupConflictingFormats(dir, "folder.jpg", logger); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The canonical name should now exist with the original content
	data, err := os.ReadFile(filepath.Join(dir, "folder.jpg"))
	if err != nil {
		t.Fatalf("folder.jpg should exist after rename: %v", err)
	}
	if string(data) != "old jpeg" {
		t.Errorf("content = %q, want %q", string(data), "old jpeg")
	}

	// Verify only one file in the directory (no duplicate)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("expected 1 file, got %d: %v", len(entries), names)
	}
}

func TestCleanupConflictingFormats_CaseMismatchConflictFormat(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Create a differently-cased file of a conflicting format
	oldPath := filepath.Join(dir, "Folder.PNG")
	if err := os.WriteFile(oldPath, []byte("old png"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Cleaning up for "folder.jpg" should remove "Folder.PNG"
	if err := CleanupConflictingFormats(dir, "folder.jpg", logger); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Error("Folder.PNG should have been deleted")
	}
}
