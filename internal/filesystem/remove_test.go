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
	if err := os.WriteFile(target, []byte("hello"), 0o644); err != nil {
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

// TestRemoveFileSafe_RenameFailureFallsBackToDirectRemove forces the rename
// step to fail by pre-creating a non-empty tomb directory at the destination
// path. macOS rename(2) refuses to replace a non-empty directory, so the
// helper falls through to os.Remove(target). The target file must still end
// up gone; the tomb is left in place, which is acceptable.
func TestRemoveFileSafe_RenameFailureFallsBackToDirectRemove(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "x.jpg")
	if err := os.WriteFile(target, []byte("hello"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	tomb := target + ".removing"
	if err := os.Mkdir(tomb, 0o755); err != nil {
		t.Fatalf("mkdir tomb: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tomb, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed tomb: %v", err)
	}

	if err := RemoveFileSafe(target); err != nil {
		t.Fatalf("RemoveFileSafe: %v", err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("target should be gone after fallback direct remove, stat err = %v", err)
	}
}

// TestRemoveFileSafe_RejectsDirectory pins the IsDir guard so a directory
// target cannot be renamed to "<dir>.removing".
func TestRemoveFileSafe_RejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	subdir := filepath.Join(dir, "sub")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := RemoveFileSafe(subdir); err == nil {
		t.Fatal("expected error for directory target")
	}
	if _, err := os.Stat(subdir); err != nil {
		t.Errorf("dir should still exist; stat err = %v", err)
	}
}
