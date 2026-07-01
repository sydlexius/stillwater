package filesystem

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// --- copyFile tests ---

func TestCopyFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source.txt")
	dst := filepath.Join(dir, "dest.txt")

	data := []byte("copy file test content")
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatalf("writing source: %v", err)
	}

	if err := copyFile(src, dst, 0o644); err != nil {
		t.Fatalf("copyFile: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("reading dest: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("dest content = %q, want %q", got, data)
	}
}

func TestCopyFile_LargeFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "large.bin")
	dst := filepath.Join(dir, "large_copy.bin")

	// 512KB of data
	data := make([]byte, 512*1024)
	for i := range data {
		data[i] = byte(i % 251) // prime modulus for varied pattern
	}
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatalf("writing source: %v", err)
	}

	if err := copyFile(src, dst, 0o644); err != nil {
		t.Fatalf("copyFile: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("reading dest: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Error("large file content mismatch after copy")
	}
}

func TestCopyFile_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "empty.txt")
	dst := filepath.Join(dir, "empty_copy.txt")

	if err := os.WriteFile(src, []byte{}, 0o644); err != nil {
		t.Fatalf("writing source: %v", err)
	}

	if err := copyFile(src, dst, 0o644); err != nil {
		t.Fatalf("copyFile: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("reading dest: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty file, got %d bytes", len(got))
	}
}

func TestCopyFile_SourceNotExist(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "nonexistent.txt")
	dst := filepath.Join(dir, "dest.txt")

	err := copyFile(src, dst, 0o644)
	if err == nil {
		t.Fatal("expected error for nonexistent source")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected ErrNotExist, got: %v", err)
	}
}

func TestCopyFile_DestDirNotExist(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source.txt")
	dst := filepath.Join(dir, "nonexistent", "dest.txt")

	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatalf("writing source: %v", err)
	}

	err := copyFile(src, dst, 0o644)
	if err == nil {
		t.Fatal("expected error when dest directory does not exist")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected ErrNotExist, got: %v", err)
	}
}

// TestCopyFile_SourceIsDirectory covers the io.Copy error branch in copyFile
// (lines 146-148 in atomic.go). On POSIX, os.Open on a directory succeeds
// (returns a valid *os.File), but the first Read call on that fd returns
// EISDIR, causing io.Copy to fail before any bytes are written. This is a
// clean, deterministic way to exercise the branch without needing a failing
// device or disk-full condition.
func TestCopyFile_SourceIsDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory read semantics differ on Windows")
	}

	dir := t.TempDir()
	// Use an existing directory (the temp dir itself) as the source path.
	srcDir := filepath.Join(dir, "srcdir")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst.txt")

	err := copyFile(srcDir, dst, 0o644)
	if err == nil {
		t.Fatal("expected error when source is a directory (EISDIR on read)")
	}
}

// --- renameSafe tests ---

func TestRenameSafe_SameFilesystem(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "old.txt")
	new := filepath.Join(dir, "new.txt")

	data := []byte("rename safe content")
	if err := os.WriteFile(old, data, 0o644); err != nil {
		t.Fatalf("writing old: %v", err)
	}

	if err := renameSafe(old, new, 0o644); err != nil {
		t.Fatalf("renameSafe: %v", err)
	}

	// Old path should be gone.
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("old file should not exist after rename")
	}

	// New path should have the content.
	got, err := os.ReadFile(new)
	if err != nil {
		t.Fatalf("reading new: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("content = %q, want %q", got, data)
	}
}

// TestRenameSafe_EXDEVCopyFallbackFails covers the error branch inside
// renameSafe where the copy fallback itself fails (line 83-84 in atomic.go).
// The test injects an EXDEV error via osRename so the copy path is taken,
// then points the destination at a read-only directory so os.OpenFile fails
// inside copyFile, causing renameSafe to return a "copy fallback" error.
func TestRenameSafe_EXDEVCopyFallbackFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod is not effective on Windows NTFS")
	}
	if os.Getuid() == 0 {
		t.Skip("chmod restrictions do not apply to root")
	}

	// Mutates package-level osRename; must not run in parallel.
	orig := osRename
	t.Cleanup(func() { osRename = orig })
	osRename = func(old, new string) error { return exdevError(old, new) }

	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Destination is inside a directory the process cannot write to. copyFile
	// will fail at os.OpenFile(dst, O_WRONLY|O_CREATE|...) with EACCES.
	roDir := filepath.Join(dir, "readonly")
	if err := os.MkdirAll(roDir, 0o555); err != nil {
		t.Fatal(err)
	}
	// Restore write permission so TempDir cleanup can remove the directory.
	t.Cleanup(func() { os.Chmod(roDir, 0o755) })

	dst := filepath.Join(roDir, "dst.txt")
	err := renameSafe(src, dst, 0o644)
	if err == nil {
		t.Fatal("expected error from renameSafe when copy fallback destination is unwritable")
	}
	if !strings.Contains(err.Error(), "copy fallback") {
		t.Errorf("error = %q, want it to contain 'copy fallback'", err.Error())
	}
}

// --- WriteFileAtomic error path tests ---

func TestWriteFileAtomic_EmptyContent(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "empty.txt")

	if err := WriteFileAtomic(target, []byte{}, 0o644); err != nil {
		t.Fatalf("WriteFileAtomic with empty content: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty file, got %d bytes", len(got))
	}
}

func TestWriteFileAtomic_DeepNestedDir(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "a", "b", "c", "d", "file.txt")

	if err := WriteFileAtomic(target, []byte("deep"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "deep" {
		t.Errorf("content = %q, want %q", got, "deep")
	}
}

func TestWriteFileAtomic_OverwritePreservesNewContent(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "overwrite.txt")

	// Write initial content.
	if err := os.WriteFile(target, []byte("old content here"), 0o644); err != nil {
		t.Fatalf("writing initial: %v", err)
	}

	// Overwrite with different-sized content.
	newData := []byte("new")
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
}

func TestWriteFileAtomic_CleansUpTmpOnOverwriteSuccess(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "cleanup.txt")

	// Pre-create the file so the backup path is exercised.
	if err := os.WriteFile(target, []byte("existing"), 0o644); err != nil {
		t.Fatalf("writing existing: %v", err)
	}

	if err := WriteFileAtomic(target, []byte("replacement"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	// Neither .tmp nor .bak should remain.
	for _, suffix := range []string{".tmp", ".bak"} {
		if _, err := os.Stat(target + suffix); !os.IsNotExist(err) {
			t.Errorf("unexpected %s file remains", suffix)
		}
	}
}

// --- WriteReaderAtomic tests ---

func TestWriteReaderAtomic_EmptyReader(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "empty_reader.txt")

	if err := WriteReaderAtomic(target, bytes.NewReader(nil), 0o644); err != nil {
		t.Fatalf("WriteReaderAtomic with empty reader: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty file, got %d bytes", len(got))
	}
}

func TestWriteReaderAtomic_ErrorReader(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "error_reader.txt")

	// Create a reader that always returns an error.
	readErr := errors.New("read failure")
	errReader := &failingReader{err: readErr}

	err := WriteReaderAtomic(target, errReader, 0o644)
	if err == nil {
		t.Fatal("expected error from failing reader")
	}
	if !errors.Is(err, readErr) {
		t.Errorf("expected wrapped read error, got: %v", err)
	}
	if !bytes.Contains([]byte(err.Error()), []byte("reading source data")) {
		t.Errorf("error = %q, want to contain 'reading source data'", err.Error())
	}
}

// failingReader is an io.Reader that always returns an error.
type failingReader struct {
	err error
}

func (r *failingReader) Read(p []byte) (int, error) {
	return 0, r.err
}

func TestWriteReaderAtomic_Overwrite(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "reader_overwrite.txt")

	// Write initial content.
	if err := os.WriteFile(target, []byte("initial"), 0o644); err != nil {
		t.Fatalf("writing initial: %v", err)
	}

	newData := []byte("from reader update")
	if err := WriteReaderAtomic(target, bytes.NewReader(newData), 0o644); err != nil {
		t.Fatalf("WriteReaderAtomic: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, newData) {
		t.Errorf("content = %q, want %q", got, newData)
	}
}

func TestWriteReaderAtomic_LargeData(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "large_reader.bin")

	data := make([]byte, 256*1024)
	for i := range data {
		data[i] = byte(i % 199)
	}

	if err := WriteReaderAtomic(target, bytes.NewReader(data), 0o644); err != nil {
		t.Fatalf("WriteReaderAtomic: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Error("large file content mismatch")
	}
}

// --- copyDirRecursive tests ---

func TestCopyDirRecursive(t *testing.T) {
	tmp := t.TempDir()

	src := filepath.Join(tmp, "src")
	if err := os.MkdirAll(filepath.Join(src, "sub1", "sub2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "root.txt"), []byte("root file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub1", "mid.txt"), []byte("mid file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub1", "sub2", "deep.txt"), []byte("deep file"), 0o644); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(tmp, "dst")

	if err := copyDirRecursive(src, dst); err != nil {
		t.Fatalf("copyDirRecursive: %v", err)
	}

	// Verify all files were copied correctly.
	tests := []struct {
		path    string
		content string
	}{
		{filepath.Join(dst, "root.txt"), "root file"},
		{filepath.Join(dst, "sub1", "mid.txt"), "mid file"},
		{filepath.Join(dst, "sub1", "sub2", "deep.txt"), "deep file"},
	}

	for _, tt := range tests {
		got, err := os.ReadFile(tt.path)
		if err != nil {
			t.Errorf("reading %s: %v", tt.path, err)
			continue
		}
		if string(got) != tt.content {
			t.Errorf("%s content = %q, want %q", tt.path, got, tt.content)
		}
	}
}

func TestCopyDirRecursive_EmptyDir(t *testing.T) {
	tmp := t.TempDir()

	src := filepath.Join(tmp, "empty_src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(tmp, "empty_dst")

	if err := copyDirRecursive(src, dst); err != nil {
		t.Fatalf("copyDirRecursive empty: %v", err)
	}

	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if !info.IsDir() {
		t.Error("dst should be a directory")
	}
}

func TestCopyDirRecursive_SourceNotExist(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "nonexistent")
	dst := filepath.Join(tmp, "dst")

	err := copyDirRecursive(src, dst)
	if err == nil {
		t.Fatal("expected error for nonexistent source")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected ErrNotExist, got: %v", err)
	}
}

// --- RenameDirAtomic additional tests ---

func TestRenameDirAtomic_SourceNotExist(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "nonexistent")
	dst := filepath.Join(tmp, "dst")

	err := RenameDirAtomic(src, dst)
	if err == nil {
		t.Fatal("expected error for nonexistent source")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected ErrNotExist, got: %v", err)
	}
}

func TestRenameDirAtomic_WithFiles(t *testing.T) {
	tmp := t.TempDir()

	src := filepath.Join(tmp, "src")
	if err := os.MkdirAll(filepath.Join(src, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(src, "top.txt"):         "top level",
		filepath.Join(src, "a", "mid.txt"):    "mid level",
		filepath.Join(src, "a", "b", "d.txt"): "deep level",
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
	}

	dst := filepath.Join(tmp, "dst")

	if err := RenameDirAtomic(src, dst); err != nil {
		t.Fatalf("RenameDirAtomic: %v", err)
	}

	// Source should be gone.
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("source should not exist after rename")
	}

	// Check all files in dst.
	dstFiles := map[string]string{
		filepath.Join(dst, "top.txt"):         "top level",
		filepath.Join(dst, "a", "mid.txt"):    "mid level",
		filepath.Join(dst, "a", "b", "d.txt"): "deep level",
	}
	for path, want := range dstFiles {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("reading %s: %v", path, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
}

// --- Symlink additional tests ---

func TestCreateRelativeSymlink_TargetNotExist(t *testing.T) {
	if !ProbeSymlinkSupport(t.TempDir()) {
		t.Skip("symlinks not supported on this platform/configuration")
	}

	dir := t.TempDir()
	target := filepath.Join(dir, "nonexistent.txt")
	link := filepath.Join(dir, "link.txt")

	// Creating a symlink to a nonexistent target is valid (dangling symlink).
	if err := CreateRelativeSymlink(target, link); err != nil {
		t.Fatalf("CreateRelativeSymlink to nonexistent target: %v", err)
	}

	// The symlink itself should exist (as a symlink).
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("expected link to be a symlink")
	}

	dest, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("Readlink: %v", err)
	}
	if dest != "nonexistent.txt" {
		t.Errorf("symlink target = %q, want %q", dest, "nonexistent.txt")
	}
}

func TestCreateRelativeSymlink_OverwriteExistingSymlink(t *testing.T) {
	if !ProbeSymlinkSupport(t.TempDir()) {
		t.Skip("symlinks not supported on this platform/configuration")
	}

	dir := t.TempDir()
	target1 := filepath.Join(dir, "first.txt")
	target2 := filepath.Join(dir, "second.txt")
	link := filepath.Join(dir, "link.txt")

	if err := os.WriteFile(target1, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target2, []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create initial symlink.
	if err := CreateRelativeSymlink(target1, link); err != nil {
		t.Fatalf("first CreateRelativeSymlink: %v", err)
	}

	// Overwrite with symlink to a different target.
	if err := CreateRelativeSymlink(target2, link); err != nil {
		t.Fatalf("second CreateRelativeSymlink: %v", err)
	}

	dest, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("Readlink: %v", err)
	}
	if dest != "second.txt" {
		t.Errorf("symlink target = %q, want %q", dest, "second.txt")
	}

	got, err := os.ReadFile(link)
	if err != nil {
		t.Fatalf("ReadFile via symlink: %v", err)
	}
	if string(got) != "second" {
		t.Errorf("content = %q, want %q", got, "second")
	}
}

func TestProbeSymlinkSupport_CleanupAfterProbe(t *testing.T) {
	dir := t.TempDir()

	// Run probe twice to ensure no leftover files cause issues.
	for i := 0; i < 3; i++ {
		_ = ProbeSymlinkSupport(dir)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("probe left %d files behind after multiple runs", len(entries))
	}
}

// nonSeekReader wraps an io.Reader to hide any io.Seeker interface,
// ensuring tests exercise the non-seekable code path.
type nonSeekReader struct {
	r io.Reader
}

func (n *nonSeekReader) Read(p []byte) (int, error) {
	return n.r.Read(p)
}

// Verify that WriteReaderAtomic works with an io.Reader that does not
// implement io.Seeker (ensuring it does not require seekability).
func TestWriteReaderAtomic_NonSeekableReader(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "pipe.txt")

	data := []byte("piped data content")
	r := &nonSeekReader{r: bytes.NewReader(data)}

	if err := WriteReaderAtomic(target, r, 0o644); err != nil {
		t.Fatalf("WriteReaderAtomic with non-seekable reader: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("content = %q, want %q", got, data)
	}
}

// --- Security hardening tests (#2172) ---

// TestWriteFileAtomic_TempFilePermissionsRestricted verifies that the temp
// file used during the write is never world/group readable, even before the
// final chmod to the caller's requested perm -- os.CreateTemp always creates
// with 0o600, so a caller-requested wider mode is only granted at the very
// end via tmpFile.Chmod, never during the write itself.
func TestWriteFileAtomic_TempFilePermissionsRestricted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}

	dir := t.TempDir()
	target := filepath.Join(dir, "secret.db")

	if err := WriteFileAtomic(target, []byte("sensitive"), 0o600); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("final mode = %o, want 0600", perm)
	}
}

// TestWriteFileAtomic_CreateTempFails covers the error branch where
// os.CreateTemp cannot create the staging file (e.g. the parent directory is
// unwritable). Uses a real read-only directory rather than an injected
// error, since CreateTemp itself has no package-level hook.
func TestWriteFileAtomic_CreateTempFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod is not effective on Windows NTFS")
	}
	if os.Getuid() == 0 {
		t.Skip("chmod restrictions do not apply to root")
	}

	roDir := t.TempDir()
	if err := os.Chmod(roDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(roDir, 0o755) })

	target := filepath.Join(roDir, "test.txt")
	err := WriteFileAtomic(target, []byte("data"), 0o644)
	if err == nil {
		t.Fatal("expected error when temp directory is unwritable")
	}
	if !strings.Contains(err.Error(), "creating temp file") {
		t.Errorf("error = %q, want it to contain 'creating temp file'", err.Error())
	}
}

// TestWriteFileAtomic_WriteTempFileFails covers the error branch where
// writing/chmoding/closing the temp file fails, injected via the
// writeTempFile hook (same pattern as osRename). Verifies the temp file is
// cleaned up and the target is never created.
func TestWriteFileAtomic_WriteTempFileFails(t *testing.T) {
	orig := writeTempFile
	t.Cleanup(func() { writeTempFile = orig })
	writeTempFile = func(f *os.File, data []byte, perm os.FileMode) error {
		return errors.New("simulated write failure")
	}

	dir := t.TempDir()
	target := filepath.Join(dir, "test.txt")

	err := WriteFileAtomic(target, []byte("data"), 0o644)
	if err == nil {
		t.Fatal("expected error from simulated write failure")
	}
	if !strings.Contains(err.Error(), "writing temp file") {
		t.Errorf("error = %q, want it to contain 'writing temp file'", err.Error())
	}

	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Error("target should not have been created")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("expected temp file to be cleaned up, dir has %d entries", len(entries))
	}
}

// TestWriteFileAtomic_ConcurrentWritesGetUniqueTemps runs many concurrent
// WriteFileAtomic calls, each against its own target file but all sharing the
// same parent directory, and verifies every write lands its complete payload
// with no leftover or cross-contaminated temp files. This exercises the
// os.CreateTemp-based unique naming (replacing the old predictable
// "<target>.tmp" name): under the old scheme, temp names were derived only
// from the target path, so this is the scenario -- many writers racing in one
// directory -- where a fixed suffix risked collisions (e.g. if two targets
// ever normalized to the same tmp path, or under future refactors reusing a
// shared staging name). Writes to the *same* target are intentionally out of
// scope here; WriteFileAtomic never guaranteed same-target concurrent-write
// safety (the backup-then-rename sequence isn't mutex-protected), and this
// change doesn't alter that pre-existing contract.
func TestWriteFileAtomic_ConcurrentWritesGetUniqueTemps(t *testing.T) {
	dir := t.TempDir()
	const n = 50

	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			target := filepath.Join(dir, fmt.Sprintf("target-%d.txt", i))
			data := []byte(strings.Repeat(string(rune('a'+i%26)), 10))
			if err := WriteFileAtomic(target, data, 0o644); err != nil {
				errCh <- err
			}
		}(i)
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrent WriteFileAtomic: %v", err)
	}

	// Every target must exist with its own complete, uncorrupted payload.
	for i := 0; i < n; i++ {
		target := filepath.Join(dir, fmt.Sprintf("target-%d.txt", i))
		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("ReadFile target-%d: %v", i, err)
		}
		want := strings.Repeat(string(rune('a'+i%26)), 10)
		if string(got) != want {
			t.Errorf("target-%d content = %q, want %q", i, got, want)
		}
	}

	// No leftover temp/bak files should remain in the directory.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != n {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("expected exactly %d target files, got %d: %v", n, len(entries), names)
	}
}

// TestWriteFileAtomic_SymlinkTargetNotFollowed verifies that when target is a
// symlink, WriteFileAtomic replaces the symlink itself rather than writing
// through it to the file it points at. os.Rename operates on the directory
// entry (not the link's referent), so the backup-then-rename sequence in
// WriteFileAtomic never dereferences the symlink -- this test locks that
// behavior in so a future refactor cannot silently start following links.
func TestWriteFileAtomic_SymlinkTargetNotFollowed(t *testing.T) {
	if !ProbeSymlinkSupport(t.TempDir()) {
		t.Skip("symlinks not supported on this platform/configuration")
	}

	dir := t.TempDir()
	victim := filepath.Join(dir, "victim.txt")
	link := filepath.Join(dir, "link.txt")

	victimData := []byte("do not touch")
	if err := os.WriteFile(victim, victimData, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CreateRelativeSymlink(victim, link); err != nil {
		t.Fatalf("CreateRelativeSymlink: %v", err)
	}

	newData := []byte("new content via link")
	if err := WriteFileAtomic(link, newData, 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	// The victim file must be untouched.
	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("ReadFile victim: %v", err)
	}
	if !bytes.Equal(got, victimData) {
		t.Errorf("victim content = %q, want untouched %q", got, victimData)
	}

	// The link path must now be a regular file with the new content, since
	// WriteFileAtomic replaces the directory entry rather than the referent.
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("Lstat link: %v", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("expected link path to become a regular file, still a symlink")
	}
	got, err = os.ReadFile(link)
	if err != nil {
		t.Fatalf("ReadFile link: %v", err)
	}
	if !bytes.Equal(got, newData) {
		t.Errorf("link content = %q, want %q", got, newData)
	}
}
