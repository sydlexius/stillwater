package nfo

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"

	_ "modernc.org/sqlite"
)

func TestWriteBackArtistNFO_Success(t *testing.T) {
	dir := t.TempDir()
	a := &artist.Artist{
		ID:            "art-1",
		Name:          "Test Artist",
		SortName:      "Test Artist",
		MusicBrainzID: "mbid-123",
		Biography:     "A fine musician.",
		Genres:        []string{"Rock", "Jazz"},
		Path:          dir,
	}

	if err := WriteBackArtistNFO(context.Background(), a, nil, nil); err != nil {
		t.Fatalf("WriteBackArtistNFO: %v", err)
	}

	// Read back and verify round-trip
	data, err := os.ReadFile(filepath.Join(dir, "artist.nfo"))
	if err != nil {
		t.Fatalf("reading nfo: %v", err)
	}

	parsed, err := Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("parsing nfo: %v", err)
	}

	if parsed.Name != "Test Artist" {
		t.Errorf("Name = %q, want %q", parsed.Name, "Test Artist")
	}
	if parsed.MusicBrainzArtistID != "mbid-123" {
		t.Errorf("MBID = %q, want %q", parsed.MusicBrainzArtistID, "mbid-123")
	}
	if parsed.Biography != "A fine musician." {
		t.Errorf("Biography = %q, want %q", parsed.Biography, "A fine musician.")
	}
	if len(parsed.Genres) != 2 || parsed.Genres[0] != "Rock" || parsed.Genres[1] != "Jazz" {
		t.Errorf("Genres = %v, want [Rock Jazz]", parsed.Genres)
	}
}

func TestWriteBackArtistNFO_SnapshotsExisting(t *testing.T) {
	db := setupTestDB(t)
	ss := NewSnapshotService(db)
	ctx := context.Background()

	dir := t.TempDir()
	oldContent := "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<artist><name>Old Name</name></artist>\n"
	if err := os.WriteFile(filepath.Join(dir, "artist.nfo"), []byte(oldContent), 0o644); err != nil {
		t.Fatalf("writing seed nfo: %v", err)
	}

	a := &artist.Artist{
		ID:       "art-snap",
		Name:     "New Name",
		SortName: "New Name",
		Path:     dir,
	}

	if err := WriteBackArtistNFO(ctx, a, ss, nil); err != nil {
		t.Fatalf("WriteBackArtistNFO: %v", err)
	}

	// Verify snapshot was saved with old content
	snapshots, err := ss.List(ctx, "art-snap")
	if err != nil {
		t.Fatalf("listing snapshots: %v", err)
	}
	if len(snapshots) != 1 {
		t.Fatalf("expected 1 snapshot, got %d", len(snapshots))
	}
	if snapshots[0].Content != oldContent {
		t.Errorf("snapshot content = %q, want %q", snapshots[0].Content, oldContent)
	}

	// Verify file was overwritten with new content
	data, err := os.ReadFile(filepath.Join(dir, "artist.nfo"))
	if err != nil {
		t.Fatalf("reading updated nfo: %v", err)
	}
	parsed, err := Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("parsing updated nfo: %v", err)
	}
	if parsed.Name != "New Name" {
		t.Errorf("Name = %q, want %q", parsed.Name, "New Name")
	}
}

func TestWriteBackArtistNFO_NilSnapshotService(t *testing.T) {
	dir := t.TempDir()
	a := &artist.Artist{
		ID:       "art-nil",
		Name:     "No Panic",
		SortName: "No Panic",
		Path:     dir,
	}

	// Must not panic with nil SnapshotService
	if err := WriteBackArtistNFO(context.Background(), a, nil, nil); err != nil {
		t.Fatalf("WriteBackArtistNFO with nil ss: %v", err)
	}

	// Verify file was still written
	if _, err := os.Stat(filepath.Join(dir, "artist.nfo")); err != nil {
		t.Errorf("expected artist.nfo to exist: %v", err)
	}
}

func TestWriteBackArtistNFO_NoExistingFile(t *testing.T) {
	db := setupTestDB(t)
	ss := NewSnapshotService(db)
	ctx := context.Background()

	dir := t.TempDir()
	// No pre-existing artist.nfo

	a := &artist.Artist{
		ID:       "art-new",
		Name:     "Fresh Artist",
		SortName: "Fresh Artist",
		Path:     dir,
	}

	if err := WriteBackArtistNFO(ctx, a, ss, nil); err != nil {
		t.Fatalf("WriteBackArtistNFO: %v", err)
	}

	// No snapshot should have been saved (nothing to snapshot)
	snapshots, err := ss.List(ctx, "art-new")
	if err != nil {
		t.Fatalf("listing snapshots: %v", err)
	}
	if len(snapshots) != 0 {
		t.Errorf("expected 0 snapshots, got %d", len(snapshots))
	}

	// File should exist
	data, err := os.ReadFile(filepath.Join(dir, "artist.nfo"))
	if err != nil {
		t.Fatalf("reading nfo: %v", err)
	}
	parsed, err := Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("parsing nfo: %v", err)
	}
	if parsed.Name != "Fresh Artist" {
		t.Errorf("Name = %q, want %q", parsed.Name, "Fresh Artist")
	}
}

func TestWriteBackArtistNFO_NilArtist(t *testing.T) {
	err := WriteBackArtistNFO(context.Background(), nil, nil, nil)
	if err == nil {
		t.Fatal("expected error for nil artist, got nil")
	}
	if got := err.Error(); got != "write artist nfo: artist is nil" {
		t.Errorf("error = %q, want %q", got, "write artist nfo: artist is nil")
	}
}

func TestWriteBackArtistNFO_EmptyPath(t *testing.T) {
	a := &artist.Artist{ID: "art-empty", Name: "Empty Path"}
	err := WriteBackArtistNFO(context.Background(), a, nil, nil)
	if err == nil {
		t.Fatal("expected error for empty path, got nil")
	}
	if got := err.Error(); got != "write artist nfo: artist path is empty" {
		t.Errorf("error = %q, want %q", got, "write artist nfo: artist path is empty")
	}
}
