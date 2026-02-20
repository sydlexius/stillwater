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
