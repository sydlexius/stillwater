package image

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func bytesReader(data []byte) io.Reader {
	return bytes.NewReader(data)
}

func TestSave_SingleFile(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	jpegData := makeJPEG(t, 100, 100)
	saved, err := Save(dir, "thumb", jpegData, []string{"folder.jpg"}, logger)
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
	saved, err := Save(dir, "thumb", jpegData, []string{"folder.jpg", "artist.jpg"}, logger)
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
	saved, err := Save(dir, "logo", jpegData, []string{"logo.png"}, logger)
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
	_, err := Save(dir, "thumb", pngData, []string{"folder.jpg"}, logger)
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
	_, err := Save(dir, "thumb", jpegData, nil, logger)
	if err == nil {
		t.Error("expected error for empty filenames")
	}
}

func TestSave_PNGThumb_KeepsPNG(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	pngData := makePNG(t, 100, 100)
	saved, err := Save(dir, "thumb", pngData, []string{"folder.jpg"}, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// PNG data sent but config says "folder.jpg" -- the extension should change to .png
	if len(saved) != 1 || saved[0] != "folder.png" {
		t.Errorf("saved = %v, want [folder.png]", saved)
	}
}
