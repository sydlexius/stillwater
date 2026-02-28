package image

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/filesystem"
)

func bytesReader(data []byte) io.Reader {
	return bytes.NewReader(data)
}

func TestSave_SingleFile(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	jpegData := makeJPEG(t, 100, 100)
	saved, err := Save(dir, "thumb", jpegData, []string{"folder.jpg"}, false, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(saved) != 1 || saved[0] != "folder.jpg" {
		t.Errorf("saved = %v, want [folder.jpg]", saved)
	}

	// Verify file exists
	path := filepath.Join(dir, "folder.jpg")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Error("folder.jpg should exist")
	}
}

func TestSave_MultipleFiles(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	jpegData := makeJPEG(t, 100, 100)
	saved, err := Save(dir, "thumb", jpegData, []string{"folder.jpg", "artist.jpg"}, false, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(saved) != 2 {
		t.Fatalf("saved %d files, want 2", len(saved))
	}

	for _, name := range saved {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("%s should exist", name)
		}
	}
}

func TestSave_LogoForcePNG(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Send a JPEG to be saved as a logo -- should convert to PNG
	jpegData := makeJPEG(t, 100, 100)
	saved, err := Save(dir, "logo", jpegData, []string{"logo.png"}, false, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(saved) != 1 || saved[0] != "logo.png" {
		t.Errorf("saved = %v, want [logo.png]", saved)
	}

	// Verify the saved file is actually PNG
	data, err := os.ReadFile(filepath.Join(dir, "logo.png"))
	if err != nil {
		t.Fatal(err)
	}
	format, _, err := DetectFormat(bytesReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if format != FormatPNG {
		t.Errorf("logo should be PNG, got %s", format)
	}
}

func TestSave_CleansUpConflicts(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Create an existing folder.jpg
	oldPath := filepath.Join(dir, "folder.jpg")
	if err := os.WriteFile(oldPath, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Save a PNG thumb -- should delete the old JPG
	pngData := makePNG(t, 100, 100)
	_, err := Save(dir, "thumb", pngData, []string{"folder.jpg"}, false, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// folder.jpg should have been replaced (not the old content)
	newData, err := os.ReadFile(filepath.Join(dir, "folder.png"))
	if err != nil {
		t.Fatal(err)
	}
	format, _, err := DetectFormat(bytesReader(newData))
	if err != nil {
		t.Fatal(err)
	}
	if format != FormatPNG {
		t.Errorf("expected PNG format, got %s", format)
	}

	// Old .jpg should be cleaned up
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Error("old folder.jpg should have been deleted")
	}
}

func TestSave_NoFileNames_Error(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	jpegData := makeJPEG(t, 10, 10)
	_, err := Save(dir, "thumb", jpegData, nil, false, logger)
	if err == nil {
		t.Error("expected error for empty filenames")
	}
}

func TestSave_PNGThumb_KeepsPNG(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	pngData := makePNG(t, 100, 100)
	saved, err := Save(dir, "thumb", pngData, []string{"folder.jpg"}, false, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// PNG data sent but config says "folder.jpg" -- the extension should change to .png
	if len(saved) != 1 || saved[0] != "folder.png" {
		t.Errorf("saved = %v, want [folder.png]", saved)
	}
}

func TestSave_Symlinks(t *testing.T) {
	if !filesystem.ProbeSymlinkSupport(t.TempDir()) {
		t.Skip("symlinks not supported on this platform/configuration")
	}

	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	jpegData := makeJPEG(t, 100, 100)
	saved, err := Save(dir, "thumb", jpegData, []string{"folder.jpg", "artist.jpg"}, true, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(saved) != 2 {
		t.Fatalf("saved %d files, want 2", len(saved))
	}

	// First file should be a regular file
	primaryPath := filepath.Join(dir, saved[0])
	fi, err := os.Lstat(primaryPath)
	if err != nil {
		t.Fatalf("Lstat primary: %v", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("primary file should not be a symlink")
	}

	// Second file should be a symlink
	secondPath := filepath.Join(dir, saved[1])
	fi, err = os.Lstat(secondPath)
	if err != nil {
		t.Fatalf("Lstat second: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("second file should be a symlink")
	}

	// Symlink target should be relative (just the filename)
	target, err := os.Readlink(secondPath)
	if err != nil {
		t.Fatalf("Readlink: %v", err)
	}
	if target != saved[0] {
		t.Errorf("symlink target = %q, want %q", target, saved[0])
	}

	// Content should be readable through the symlink
	primaryData, err := os.ReadFile(primaryPath)
	if err != nil {
		t.Fatalf("reading primary: %v", err)
	}
	symlinkData, err := os.ReadFile(secondPath)
	if err != nil {
		t.Fatalf("reading symlink: %v", err)
	}
	if !bytes.Equal(primaryData, symlinkData) {
		t.Error("content mismatch between primary and symlink")
	}
}

func TestSave_Symlinks_FanartException(t *testing.T) {
	if !filesystem.ProbeSymlinkSupport(t.TempDir()) {
		t.Skip("symlinks not supported on this platform/configuration")
	}

	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	jpegData := makeJPEG(t, 100, 100)
	saved, err := Save(dir, "fanart", jpegData, []string{"fanart.jpg", "backdrop.jpg"}, true, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(saved) != 2 {
		t.Fatalf("saved %d files, want 2", len(saved))
	}

	// Both files should be regular files (fanart exception)
	for _, name := range saved {
		fi, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("Lstat %s: %v", name, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			t.Errorf("%s should be a regular file (fanart exception), but is a symlink", name)
		}
	}
}

func TestSave_Symlinks_ExtensionCoercionDuplicate(t *testing.T) {
	if !filesystem.ProbeSymlinkSupport(t.TempDir()) {
		t.Skip("symlinks not supported on this platform/configuration")
	}

	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Both "folder.jpg" and "folder.png" resolve to "folder.jpg" when saving
	// JPEG data. Without the guard, the second entry would delete the primary
	// and create a self-referential symlink.
	jpegData := makeJPEG(t, 100, 100)
	saved, err := Save(dir, "thumb", jpegData, []string{"folder.jpg", "folder.png"}, true, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Only one file should be reported (the duplicate is skipped).
	if len(saved) != 1 {
		t.Fatalf("saved %d files, want 1; got %v", len(saved), saved)
	}
	if saved[0] != "folder.jpg" {
		t.Errorf("saved[0] = %q, want %q", saved[0], "folder.jpg")
	}

	// The file should be a regular file, not a symlink.
	fi, err := os.Lstat(filepath.Join(dir, "folder.jpg"))
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("folder.jpg should be a regular file, not a symlink")
	}

	// Verify the file is readable and valid.
	data, err := os.ReadFile(filepath.Join(dir, "folder.jpg"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	format, _, err := DetectFormat(bytesReader(data))
	if err != nil {
		t.Fatalf("DetectFormat: %v", err)
	}
	if format != FormatJPEG {
		t.Errorf("format = %q, want %q", format, FormatJPEG)
	}
}

func TestSave_Symlinks_SingleFile(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	jpegData := makeJPEG(t, 100, 100)
	saved, err := Save(dir, "thumb", jpegData, []string{"folder.jpg"}, true, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(saved) != 1 || saved[0] != "folder.jpg" {
		t.Errorf("saved = %v, want [folder.jpg]", saved)
	}

	// Single file should be a regular file, no symlinks
	fi, err := os.Lstat(filepath.Join(dir, saved[0]))
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("single file should not be a symlink")
	}
}
