package filesystem

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
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

	if err := copyFile(src, dst); err != nil {
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

	if err := copyFile(src, dst); err != nil {
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

	if err := copyFile(src, dst); err != nil {
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

	err := copyFile(src, dst)
	if err == nil {
		t.Fatal("expected error for nonexistent source")
	}
	if !os.IsNotExist(err) {
		t.Errorf("expected IsNotExist error, got: %v", err)
	}
}

func TestCopyFile_DestDirNotExist(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source.txt")
	dst := filepath.Join(dir, "nonexistent", "dest.txt")

	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatalf("writing source: %v", err)
	}

	err := copyFile(src, dst)
	if err == nil {
		t.Fatal("expected error when dest directory does not exist")
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

	if err := renameSafe(old, new); err != nil {
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
	errReader := &failingReader{err: errors.New("read failure")}

	err := WriteReaderAtomic(target, errReader, 0o644)
	if err == nil {
		t.Fatal("expected error from failing reader")
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

// Verify that WriteReaderAtomic works with an io.Reader that does not
// implement io.Seeker (ensuring it does not require seekability).
func TestWriteReaderAtomic_NonSeekableReader(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "pipe.txt")

	data := []byte("piped data content")
	pr, pw := io.Pipe()

	go func() {
		_, _ = pw.Write(data)
		pw.Close()
	}()

	if err := WriteReaderAtomic(target, pr, 0o644); err != nil {
		t.Fatalf("WriteReaderAtomic with pipe: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("content = %q, want %q", got, data)
	}
}
