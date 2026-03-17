package filesystem

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRenameDirAtomic(t *testing.T) {
	tmp := t.TempDir()

	src := filepath.Join(tmp, "source-dir")
	if err := os.MkdirAll(filepath.Join(src, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "file.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "subdir", "nested.txt"), []byte("world"), 0o644); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(tmp, "dest-dir")

	if err := RenameDirAtomic(src, dst); err != nil {
		t.Fatalf("RenameDirAtomic: %v", err)
	}

	// Source should no longer exist.
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("source directory should not exist after rename")
	}

	// Destination should contain both files.
	data, err := os.ReadFile(filepath.Join(dst, "file.txt"))
	if err != nil {
		t.Fatalf("reading file.txt: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("file.txt = %q, want %q", data, "hello")
	}

	data, err = os.ReadFile(filepath.Join(dst, "subdir", "nested.txt"))
	if err != nil {
		t.Fatalf("reading nested.txt: %v", err)
	}
	if string(data) != "world" {
		t.Errorf("nested.txt = %q, want %q", data, "world")
	}
}

func TestRenameDirAtomic_DestExists(t *testing.T) {
	tmp := t.TempDir()

	src := filepath.Join(tmp, "src")
	dst := filepath.Join(tmp, "dst")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create destination with different content.
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}

	// os.Rename on same device with existing dst replaces it.
	if err := RenameDirAtomic(src, dst); err != nil {
		t.Fatalf("RenameDirAtomic: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dst, "a.txt"))
	if err != nil {
		t.Fatalf("reading a.txt: %v", err)
	}
	if string(data) != "a" {
		t.Errorf("a.txt = %q, want %q", data, "a")
	}
}
