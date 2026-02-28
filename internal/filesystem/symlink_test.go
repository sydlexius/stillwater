package filesystem

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProbeSymlinkSupport(t *testing.T) {
	if !ProbeSymlinkSupport(t.TempDir()) {
		t.Skip("symlinks not supported on this platform/configuration")
	}

	dir := t.TempDir()
	if !ProbeSymlinkSupport(dir) {
		t.Error("expected symlink support in temp dir")
	}

	// Verify probe cleaned up after itself
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("probe left %d files behind", len(entries))
	}
}

func TestProbeSymlinkSupport_InvalidDir(t *testing.T) {
	if ProbeSymlinkSupport("/nonexistent/path/that/does/not/exist") {
		t.Error("expected false for invalid directory")
	}
}

func TestCreateRelativeSymlink(t *testing.T) {
	if !ProbeSymlinkSupport(t.TempDir()) {
		t.Skip("symlinks not supported on this platform/configuration")
	}

	dir := t.TempDir()
	target := filepath.Join(dir, "real.txt")
	link := filepath.Join(dir, "link.txt")

	data := []byte("hello symlink")
	if err := os.WriteFile(target, data, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := CreateRelativeSymlink(target, link); err != nil {
		t.Fatalf("CreateRelativeSymlink: %v", err)
	}

	// Verify it is a symlink
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("expected link to be a symlink")
	}

	// Verify the symlink target is relative
	dest, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("Readlink: %v", err)
	}
	if dest != "real.txt" {
		t.Errorf("symlink target = %q, want %q", dest, "real.txt")
	}

	// Verify content matches
	got, err := os.ReadFile(link)
	if err != nil {
		t.Fatalf("ReadFile via symlink: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("content via symlink = %q, want %q", got, data)
	}
}

func TestCreateRelativeSymlink_OverwritesExisting(t *testing.T) {
	if !ProbeSymlinkSupport(t.TempDir()) {
		t.Skip("symlinks not supported on this platform/configuration")
	}

	dir := t.TempDir()
	target := filepath.Join(dir, "real.txt")
	link := filepath.Join(dir, "link.txt")

	if err := os.WriteFile(target, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create an existing regular file at the link path
	if err := os.WriteFile(link, []byte("old regular file"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := CreateRelativeSymlink(target, link); err != nil {
		t.Fatalf("CreateRelativeSymlink: %v", err)
	}

	// Verify it is now a symlink
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("expected link to be a symlink after overwrite")
	}

	// Verify content matches the real target
	got, err := os.ReadFile(link)
	if err != nil {
		t.Fatalf("ReadFile via symlink: %v", err)
	}
	if string(got) != "content" {
		t.Errorf("content via symlink = %q, want %q", got, "content")
	}
}
