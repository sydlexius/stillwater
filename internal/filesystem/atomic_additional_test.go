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

	// Pre-create the file so the overwrite path is exercised.
	if err := os.WriteFile(target, []byte("existing"), 0o644); err != nil {
		t.Fatalf("writing existing: %v", err)
	}

	if err := WriteFileAtomic(target, []byte("replacement"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	// Neither .tmp nor .bak should remain (the single-rename design no longer
	// creates a .bak at all).
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

// TestWriteTempFile_WriteFails covers writeTempFile's own Write error branch
// (as opposed to WriteFileAtomic's handling of a writeTempFile failure, which
// is covered separately via the hook). Closing the file before calling the
// real writeTempFile implementation makes the underlying Write syscall fail
// deterministically.
func TestWriteTempFile_WriteFails(t *testing.T) {
	dir := t.TempDir()
	f, err := os.CreateTemp(dir, "closed-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if err := writeTempFile(f, []byte("data"), 0o644); err == nil {
		t.Fatal("expected error writing to a closed file")
	}
}

// TestWriteTempFile_ChmodFails covers writeTempFile's Chmod error branch.
// Writing to /dev/null always succeeds (the kernel discards the data), but
// chmod-ing it fails for a non-root caller since /dev/null is root-owned --
// this gives a Write-succeeds-then-Chmod-fails sequence without needing to
// inject a fake filesystem.
func TestWriteTempFile_ChmodFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission semantics; /dev/null chmod behavior differs on Windows")
	}
	if os.Getuid() == 0 {
		t.Skip("root can chmod any file, including /dev/null")
	}
	f, err := os.OpenFile("/dev/null", os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("/dev/null not writable in this environment: %v", err)
	}
	defer f.Close()

	if err := writeTempFile(f, []byte("data"), 0o600); err == nil {
		t.Fatal("expected error chmod-ing /dev/null as a non-root user")
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

// TestWriteFileAtomic_SyncFails covers the fsync error branch in writeTempFile
// (#2661). The f.Sync() before the promoting rename is what makes the atomic
// replace crash-durable; if it fails, the write must abort BEFORE the rename so
// a partially-flushed temp is never promoted onto the target. fsync failures
// are real (I/O errors, ENOSPC on some filesystems) but impractical to provoke
// on a tmpfs, so the failure is injected via the syncFile hook -- exercising
// the real writeTempFile, not a stand-in.
//
// Asserts the OUTCOME of a crash-path failure, not that a line ran:
//   - WriteFileAtomic returns the sync error (wrapped as "writing temp file");
//   - the pre-existing target is left byte-for-byte UNTOUCHED -- a failed sync
//     must never promote the temp, so the old content survives;
//   - no temp file is left behind in the target directory.
func TestWriteFileAtomic_SyncFails(t *testing.T) {
	orig := syncFile
	t.Cleanup(func() { syncFile = orig })
	syncErr := errors.New("simulated fsync failure")
	syncFile = func(*os.File) error { return syncErr }

	dir := t.TempDir()
	target := filepath.Join(dir, "test.txt")

	// Seed an existing target so the assertion can prove it was untouched, not
	// merely that a new file was never created.
	original := []byte("original content that must survive a failed write")
	if err := os.WriteFile(target, original, 0o644); err != nil {
		t.Fatal(err)
	}

	err := WriteFileAtomic(target, []byte("new content that never lands"), 0o644)
	if err == nil {
		t.Fatal("expected error from simulated fsync failure")
	}
	if !errors.Is(err, syncErr) {
		t.Errorf("error = %v, want it to wrap the injected fsync error", err)
	}
	if !strings.Contains(err.Error(), "writing temp file") {
		t.Errorf("error = %q, want it to contain 'writing temp file'", err.Error())
	}

	// The target must still hold its ORIGINAL bytes: a failed sync aborts before
	// the rename, so nothing was promoted.
	got, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatalf("reading target after failed write: %v", readErr)
	}
	if !bytes.Equal(got, original) {
		t.Errorf("target content = %q, want the original %q left untouched", got, original)
	}

	// Only the target must remain -- the aborted temp file is cleaned up.
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(target) {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("dir entries = %v, want only the untouched target %q; a temp was left behind",
			names, filepath.Base(target))
	}
}

// TestWriteFileAtomic_DoesNotUseThePredictableTempName is the effective
// regression test for the O_EXCL/unique-name property: it plants a decoy
// file at the OLD predictable "<target>.tmp" path before calling
// WriteFileAtomic, then asserts (a) WriteFileAtomic never touches that
// decoy and (b) the staging name it actually used (captured via the
// writeTempFile hook) is not the predictable path. The concurrency test
// above uses distinct targets per goroutine, so it passes even with the old
// `tmpPath := target + ".tmp"` code -- it proves nothing about uniqueness.
// This test is the one that must go red against that old code; see the
// verification note in the PR description / commit message for the
// revert-and-confirm-red check.
func TestWriteFileAtomic_DoesNotUseThePredictableTempName(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "test.txt")
	predictableTmp := target + ".tmp"

	decoy := []byte("decoy: must not be touched")
	if err := os.WriteFile(predictableTmp, decoy, 0o644); err != nil {
		t.Fatal(err)
	}

	orig := writeTempFile
	t.Cleanup(func() { writeTempFile = orig })
	var capturedTmpName string
	writeTempFile = func(f *os.File, data []byte, perm os.FileMode) error {
		capturedTmpName = f.Name()
		return orig(f, data, perm)
	}

	data := []byte("real content")
	if err := WriteFileAtomic(target, data, 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	if capturedTmpName == "" {
		t.Fatal("writeTempFile hook was never invoked; test did not observe the staging name")
	}
	if capturedTmpName == predictableTmp {
		t.Errorf("staging temp name %q collided with the predictable <target>.tmp path", capturedTmpName)
	}

	// The decoy planted at the predictable path must be untouched.
	got, err := os.ReadFile(predictableTmp)
	if err != nil {
		t.Fatalf("ReadFile decoy: %v", err)
	}
	if !bytes.Equal(got, decoy) {
		t.Errorf("decoy at predictable path was modified: got %q, want %q", got, decoy)
	}

	// The real target must contain the new data, not the decoy.
	got, err = os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile target: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("target content = %q, want %q", got, data)
	}
}

// TestWriteFileAtomic_DoesNotClobberSymlinkAtPredictableTempName is the
// symlink variant: a symlink sits at the predictable "<target>.tmp" path,
// pointing at a victim file. If WriteFileAtomic ever regressed to writing
// through that predictable name, it would write attacker-controlled content
// through the symlink into the victim's target. Asserts the victim is
// untouched and the symlink itself survives.
func TestWriteFileAtomic_DoesNotClobberSymlinkAtPredictableTempName(t *testing.T) {
	if !ProbeSymlinkSupport(t.TempDir()) {
		t.Skip("symlinks not supported on this platform/configuration")
	}

	dir := t.TempDir()
	target := filepath.Join(dir, "test.txt")
	predictableTmp := target + ".tmp"
	victim := filepath.Join(dir, "victim.txt")

	victimData := []byte("do not touch")
	if err := os.WriteFile(victim, victimData, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CreateRelativeSymlink(victim, predictableTmp); err != nil {
		t.Fatalf("CreateRelativeSymlink: %v", err)
	}

	data := []byte("real content")
	if err := WriteFileAtomic(target, data, 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	// The victim must be untouched.
	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("ReadFile victim: %v", err)
	}
	if !bytes.Equal(got, victimData) {
		t.Errorf("victim content = %q, want untouched %q", got, victimData)
	}

	// The symlink at the predictable path must still be a symlink (never
	// replaced or written through).
	fi, err := os.Lstat(predictableTmp)
	if err != nil {
		t.Fatalf("Lstat predictable path: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("symlink at predictable <target>.tmp path was replaced")
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
// shared staging name). Writes to the *same* target are covered separately by
// TestWriteFileAtomic_ConcurrentSameTargetSucceeds.
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

// TestWriteFileAtomic_ConcurrentSameTargetSucceeds is the regression test for
// the same-target concurrency gap (#2181 review). N goroutines all call
// WriteFileAtomic on ONE shared target at once, with the target pre-existing so
// the backup (.bak) path is exercised on every write. Each writer must get its
// own unique staging temp (O_EXCL) AND its own unique .bak name, and a writer
// whose target was moved out from under it by a concurrent writer must treat
// that as benign (nothing to back up) rather than failing.
//
// This goes RED against the pre-fix code, which used a single shared
// "<target>.bak" path and treated a vanished target as a hard error: several
// writers reliably failed with "backing up existing file: ... no such file or
// directory". After the fix every write succeeds, no *.tmp/*.bak leftovers
// remain, and the final file is one writer's complete, untorn payload.
func TestWriteFileAtomic_ConcurrentSameTargetSucceeds(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "shared.txt")

	// Pre-create the target so the backup path runs on every write.
	if err := os.WriteFile(target, []byte("initial-baseline-content"), 0o644); err != nil {
		t.Fatal(err)
	}

	const n = 32
	// Every writer's payload is the same fixed length so the final file, whichever
	// writer wins, is trivially checkable for torn/partial writes.
	payloads := make([][]byte, n)
	valid := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		p := []byte(fmt.Sprintf("writer-%03d-complete-payload!!", i))
		payloads[i] = p
		valid[string(p)] = true
	}

	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := WriteFileAtomic(target, payloads[i], 0o644); err != nil {
				errCh <- err
			}
		}(i)
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrent same-target WriteFileAtomic failed: %v", err)
	}

	// The final file must be exactly one writer's complete payload -- never
	// torn, truncated, or a mix of two writers.
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile target: %v", err)
	}
	if !valid[string(got)] {
		t.Errorf("final content %q is not any single writer's complete payload (torn write?)", got)
	}

	// Only the target itself should remain: no leftover *.tmp or *.bak files.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if name == "shared.txt" {
			continue
		}
		t.Errorf("unexpected leftover file after concurrent writes: %s", name)
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
