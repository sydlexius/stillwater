package nfo

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCheckFileConflict_NoFile(t *testing.T) {
	result := CheckFileConflict("/nonexistent/artist.nfo", time.Now())
	if result.HasConflict {
		t.Error("expected no conflict for nonexistent file")
	}
}

func TestCheckFileConflict_NoConflict(t *testing.T) {
	dir := t.TempDir()
	nfoPath := filepath.Join(dir, "artist.nfo")

	if err := os.WriteFile(nfoPath, []byte("<artist/>"), 0644); err != nil {
		t.Fatal(err)
	}

	// Use a reference time after the file was written
	time.Sleep(50 * time.Millisecond)
	since := time.Now()

	result := CheckFileConflict(nfoPath, since)
	if result.HasConflict {
		t.Error("expected no conflict when file is older than reference time")
	}
	if result.LastModified == nil {
		t.Error("expected LastModified to be set")
	}
}

func TestCheckFileConflict_HasConflict(t *testing.T) {
	dir := t.TempDir()
	nfoPath := filepath.Join(dir, "artist.nfo")

	// Set reference time before the file is written
	since := time.Now().Add(-1 * time.Second)
	time.Sleep(50 * time.Millisecond)

	if err := os.WriteFile(nfoPath, []byte("<artist/>"), 0644); err != nil {
		t.Fatal(err)
	}

	result := CheckFileConflict(nfoPath, since)
	if !result.HasConflict {
		t.Error("expected conflict when file is newer than reference time")
	}
	if result.Reason == "" {
		t.Error("expected Reason to be set")
	}
}
