package filesystem

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

func TestWriteFileAtomic_NewFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "test.txt")
	data := []byte("hello world")

	if err := WriteFileAtomic(target, data, 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("content = %q, want %q", got, data)
	}

	// No .tmp or .bak should remain
	if _, err := os.Stat(target + ".tmp"); !os.IsNotExist(err) {
		t.Error("unexpected .tmp file remains")
	}
	if _, err := os.Stat(target + ".bak"); !os.IsNotExist(err) {
		t.Error("unexpected .bak file remains")
	}
}

func TestWriteFileAtomic_OverwriteExisting(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "test.txt")

	// Write initial content
	if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
		t.Fatalf("writing original: %v", err)
	}

	// Overwrite with atomic write
	newData := []byte("updated content")
	if err := WriteFileAtomic(target, newData, 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, newData) {
		t.Errorf("content = %q, want %q", got, newData)
	}

	// No .bak should remain
	if _, err := os.Stat(target + ".bak"); !os.IsNotExist(err) {
		t.Error("unexpected .bak file remains")
	}
}

func TestWriteFileAtomic_CreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "sub", "dir", "test.txt")

	if err := WriteFileAtomic(target, []byte("nested"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "nested" {
		t.Errorf("content = %q, want %q", got, "nested")
	}
}

func TestWriteReaderAtomic(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "reader.txt")
	data := []byte("from reader")

	if err := WriteReaderAtomic(target, bytes.NewReader(data), 0o644); err != nil {
		t.Fatalf("WriteReaderAtomic: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("content = %q, want %q", got, data)
	}
}

func TestWriteFileAtomic_LargeFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "large.bin")

	// 1MB of data
	data := make([]byte, 1024*1024)
	for i := range data {
		data[i] = byte(i % 256)
	}

	if err := WriteFileAtomic(target, data, 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Error("large file content mismatch")
	}
}

// TestWriteFileAtomic_EXDEVFallbackPreservesMode verifies that when the rename
// step fails with EXDEV (cross-device move), the copy fallback preserves the
// requested file mode on the destination. This is the regression test for the
// bug where copyFile used os.Create (mode 0666) instead of os.OpenFile with
// the caller-specified perm, causing sensitive files (e.g. encryption.key
// written with 0600) to end up world-readable on cross-device setups.
//
// Notes:
//
//	(1) This tests the EXDEV copy fallback path by injecting an error via the
//	    package-level osRename hook.
//	(2) The mode == perm assertion is POSIX-only: on Windows, Go synthesizes
//	    FileMode values from FILE_ATTRIBUTE_READONLY and does not enforce Unix
//	    permission bits, so stat.Mode()&0o777 does not round-trip.
//	(3) EXDEV (cross-device link) is a Unix/Linux kernel concept; Windows uses
//	    a different mechanism for cross-volume moves and os.Rename does not
//	    return syscall.EXDEV.
func TestWriteFileAtomic_EXDEVFallbackPreservesMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("EXDEV and Unix permission semantics are POSIX-only")
	}

	// This test mutates the package-level osRename hook; must not run in parallel.
	orig := osRename
	t.Cleanup(func() { osRename = orig })

	osRename = func(oldPath, newPath string) error {
		return &os.LinkError{
			Op:  "rename",
			Old: oldPath,
			New: newPath,
			Err: syscall.EXDEV,
		}
	}

	dir := t.TempDir()
	target := filepath.Join(dir, "secret.key")
	data := []byte("sensitive data")

	// Request a restrictive 0600 mode (owner read/write only).
	const wantPerm os.FileMode = 0o600
	if err := WriteFileAtomic(target, data, wantPerm); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	// Confirm the file mode survived the cross-device copy fallback.
	if gotPerm := info.Mode().Perm(); gotPerm != wantPerm {
		t.Errorf("file mode = %04o, want %04o (perm lost on cross-device copy fallback)", gotPerm, wantPerm)
	}
}

func TestWriteFileAtomic_MultipleOverwrites(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "multi.txt")

	for i := 0; i < 10; i++ {
		data := []byte("iteration " + string(rune('0'+i)))
		if err := WriteFileAtomic(target, data, 0o644); err != nil {
			t.Fatalf("WriteFileAtomic iteration %d: %v", i, err)
		}
	}

	// Only the final content should remain
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "iteration 9" {
		t.Errorf("content = %q, want %q", got, "iteration 9")
	}

	// No leftover files
	if _, err := os.Stat(target + ".tmp"); !os.IsNotExist(err) {
		t.Error("unexpected .tmp file remains")
	}
	if _, err := os.Stat(target + ".bak"); !os.IsNotExist(err) {
		t.Error("unexpected .bak file remains")
	}
}
