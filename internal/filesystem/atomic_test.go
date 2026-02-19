package filesystem

import (
	"bytes"
	"os"
	"path/filepath"
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
