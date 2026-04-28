package filesystem

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveFileSafe_RemovesFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "x.jpg")
	if err := os.WriteFile(target, []byte("hello"), 0o644); err != nil { //nolint:gosec
		t.Fatalf("write: %v", err)
	}
	if err := RemoveFileSafe(target); err != nil {
		t.Fatalf("RemoveFileSafe: %v", err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("file should be gone, stat err = %v", err)
	}
	// Tomb file must also be removed.
	if _, err := os.Stat(target + ".removing"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("tomb should be gone, stat err = %v", err)
	}
}

func TestRemoveFileSafe_MissingFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "absent.jpg")
	err := RemoveFileSafe(target)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected ErrNotExist, got %v", err)
	}
}
