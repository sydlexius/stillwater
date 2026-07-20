package filesystem

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// TestWriteFileAtomic_PromotionFailureLeavesTargetIntact verifies the
// structural crash/failure-recovery guarantee of the single-rename design: if
// the promoting rename fails, the pre-existing target is left untouched with
// its original content and only the orphaned temp file is cleaned up.
//
// This replaces the former EXDEV copy-fallback test. WriteFileAtomic now
// promotes with a single osRename onto the target (tmp is created in the
// target's own directory, so the rename is always same-filesystem and never
// falls back to a non-atomic copy). We inject an osRename failure to exercise
// the promotion-error branch.
func TestWriteFileAtomic_PromotionFailureLeavesTargetIntact(t *testing.T) {
	// This test mutates the package-level osRename hook; must not run in parallel.
	orig := osRename
	t.Cleanup(func() { osRename = orig })

	osRename = func(oldPath, newPath string) error {
		return &os.LinkError{
			Op:  "rename",
			Old: oldPath,
			New: newPath,
			Err: syscall.EIO,
		}
	}

	dir := t.TempDir()
	target := filepath.Join(dir, "config.toml")
	original := []byte("original content")
	if err := os.WriteFile(target, original, 0o644); err != nil {
		t.Fatalf("seeding target: %v", err)
	}

	if err := WriteFileAtomic(target, []byte("new content"), 0o644); err == nil {
		t.Fatal("WriteFileAtomic: expected an error when the promoting rename fails, got nil")
	}

	// The target must still hold its original content -- the failed write must
	// not have destroyed or truncated it.
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("reading target after failed write: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Errorf("target content = %q, want %q (a failed promotion must not disturb the existing file)", got, original)
	}

	// No orphaned temp file should be left behind in the directory.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("orphaned temp file left behind: %s", e.Name())
		}
	}
}

// TestWriteFileAtomic_OverwriteAppliesNewMode verifies that overwriting an
// existing file installs the caller's requested mode, not the pre-existing one.
// The temp file is chmod'd to perm before the promoting rename, and the rename
// swaps that inode into place, so the new mode wins even when the previous file
// had a different (here, more restrictive) mode.
//
// The mode == perm assertion is POSIX-only: on Windows, Go synthesizes
// FileMode from FILE_ATTRIBUTE_READONLY and does not enforce Unix permission
// bits, so stat.Mode().Perm() does not round-trip.
func TestWriteFileAtomic_OverwriteAppliesNewMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission semantics are POSIX-only")
	}

	dir := t.TempDir()
	target := filepath.Join(dir, "secret.key")

	// Pre-create the target with a deliberately different (restrictive) mode so
	// we can verify the new perm wins, not the pre-existing one.
	if err := os.WriteFile(target, []byte("old content"), 0o400); err != nil {
		t.Fatalf("pre-creating target: %v", err)
	}

	const wantPerm os.FileMode = 0o600
	if err := WriteFileAtomic(target, []byte("new content"), wantPerm); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	// The final file must carry the new perm, not the pre-existing 0o400.
	if gotPerm := info.Mode().Perm(); gotPerm != wantPerm {
		t.Errorf("file mode = %04o, want %04o (new perm not applied on overwrite)", gotPerm, wantPerm)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("reading target: %v", err)
	}
	if string(got) != "new content" {
		t.Errorf("content = %q, want %q", got, "new content")
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
