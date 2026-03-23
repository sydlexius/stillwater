package filesystem

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
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

func TestRenameDirAtomic_EmptyDir(t *testing.T) {
	tmp := t.TempDir()

	src := filepath.Join(tmp, "src")
	dst := filepath.Join(tmp, "dst")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := RenameDirAtomic(src, dst); err != nil {
		t.Fatalf("RenameDirAtomic: %v", err)
	}

	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if !info.IsDir() {
		t.Error("dst should be a directory")
	}
}

// Tests below override the package-level renameFunc and must NOT use t.Parallel().

// exdevError returns a *os.LinkError wrapping syscall.EXDEV, mimicking the
// error os.Rename returns for cross-device moves.
func exdevError(src, dst string) error {
	return &os.LinkError{
		Op:  "rename",
		Old: src,
		New: dst,
		Err: syscall.EXDEV,
	}
}

// TestRenameDirAtomic_EXDEVFallback verifies that when the initial rename
// fails with EXDEV, the function falls back to copyDirRecursive + RemoveAll.
func TestRenameDirAtomic_EXDEVFallback(t *testing.T) {
	orig := renameFunc
	t.Cleanup(func() { renameFunc = orig })

	renameFunc = func(src, dst string) error {
		return exdevError(src, dst)
	}

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
		t.Fatalf("RenameDirAtomic with EXDEV fallback: %v", err)
	}

	// Source should have been removed after a successful copy.
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("source directory should not exist after fallback rename")
	}

	// Destination should contain all expected files with correct content.
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

// TestRenameDirAtomic_EXDEVFallback_EmptyDir verifies the EXDEV fallback
// path works for an empty directory.
func TestRenameDirAtomic_EXDEVFallback_EmptyDir(t *testing.T) {
	orig := renameFunc
	t.Cleanup(func() { renameFunc = orig })

	renameFunc = func(src, dst string) error {
		return exdevError(src, dst)
	}

	tmp := t.TempDir()

	src := filepath.Join(tmp, "src")
	dst := filepath.Join(tmp, "dst")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := RenameDirAtomic(src, dst); err != nil {
		t.Fatalf("RenameDirAtomic with EXDEV fallback: %v", err)
	}

	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("source directory should not exist after fallback rename")
	}

	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if !info.IsDir() {
		t.Error("dst should be a directory")
	}
}

// TestRenameDirAtomic_FallbackCopyFailure verifies that when the EXDEV
// fallback copy itself fails, partial destination content is cleaned up
// (when dst did not exist before the call). A broken symlink in the source
// tree causes copyFile to fail, which works even when running as root
// (unlike chmod-based approaches).
func TestRenameDirAtomic_FallbackCopyFailure(t *testing.T) {
	if !ProbeSymlinkSupport(t.TempDir()) {
		t.Skip("symlinks not supported")
	}

	orig := renameFunc
	t.Cleanup(func() { renameFunc = orig })

	renameFunc = func(src, dst string) error {
		return exdevError(src, dst)
	}

	tmp := t.TempDir()

	// Create source with a regular file and a broken symlink. The regular
	// file will be copied first (alphabetical Walk order: "aaa.txt" before
	// "zzz-broken"), then the broken symlink causes copyFile to fail with
	// ENOENT, exercising the partial-copy cleanup path.
	src := filepath.Join(tmp, "source-dir")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "aaa.txt"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Create a symlink that points to a non-existent target. filepath.Walk
	// reports the symlink entry via os.Lstat; copyFile then follows it via
	// os.Open and fails trying to read the dangling target.
	if err := os.Symlink("/nonexistent-target-for-test", filepath.Join(src, "zzz-broken")); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(tmp, "dest-dir")

	err := RenameDirAtomic(src, dst)
	if err == nil {
		t.Fatal("expected error from RenameDirAtomic when copy fails, got nil")
	}

	// The error should mention the copy fallback failure.
	if got := err.Error(); !strings.Contains(got, "copy fallback failed") {
		t.Errorf("error = %q, want it to contain %q", got, "copy fallback failed")
	}

	// Because dst did not exist before the call, the partial destination
	// should have been cleaned up.
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Error("partial destination should have been cleaned up after copy failure")
	}

	// Source should still exist (it was not removed because the copy failed).
	if _, statErr := os.Lstat(src); statErr != nil {
		t.Error("source directory should still exist after copy failure")
	}
}

// TestRenameDirAtomic_FallbackCopyFailure_ExistingDst verifies that when
// dst already exists and the copy fails, the function does NOT remove dst
// (to avoid destroying pre-existing data).
func TestRenameDirAtomic_FallbackCopyFailure_ExistingDst(t *testing.T) {
	if !ProbeSymlinkSupport(t.TempDir()) {
		t.Skip("symlinks not supported")
	}

	orig := renameFunc
	t.Cleanup(func() { renameFunc = orig })

	renameFunc = func(src, dst string) error {
		return exdevError(src, dst)
	}

	tmp := t.TempDir()

	// Create a pre-existing destination with content we want to preserve.
	dst := filepath.Join(tmp, "dest-dir")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "existing.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create source with a broken symlink to force copy failure.
	src := filepath.Join(tmp, "source-dir")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nonexistent-target-for-test", filepath.Join(src, "broken-link")); err != nil {
		t.Fatal(err)
	}

	err := RenameDirAtomic(src, dst)
	if err == nil {
		t.Fatal("expected error from RenameDirAtomic when copy fails, got nil")
	}

	// Because dst existed before the call, it should NOT be removed.
	data, readErr := os.ReadFile(filepath.Join(dst, "existing.txt"))
	if readErr != nil {
		t.Fatalf("pre-existing dst file should still be readable: %v", readErr)
	}
	if string(data) != "keep" {
		t.Errorf("existing.txt = %q, want %q", data, "keep")
	}
}
