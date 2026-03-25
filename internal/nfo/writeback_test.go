package nfo

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestWriteBackArtistNFO_SetsLockData(t *testing.T) {
	dir := t.TempDir()
	a := &artist.Artist{
		ID:       "art-lock",
		Name:     "Locked Artist",
		SortName: "Locked Artist",
		Path:     dir,
	}

	if err := WriteBackArtistNFO(context.Background(), a, nil, nil); err != nil {
		t.Fatalf("WriteBackArtistNFO: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "artist.nfo"))
	if err != nil {
		t.Fatalf("reading nfo: %v", err)
	}

	parsed, err := Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("parsing nfo: %v", err)
	}

	if !parsed.LockData {
		t.Error("WriteBackArtistNFO should always set LockData=true")
	}

	// Also verify the raw XML contains the element
	output := string(data)
	if !strings.Contains(output, "<lockdata>true</lockdata>") {
		t.Errorf("raw NFO output should contain <lockdata>true</lockdata>, got:\n%s", output)
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

func TestWriteBackArtistNFO_IncludesStillwater(t *testing.T) {
	dir := t.TempDir()
	a := &artist.Artist{
		ID:   "test-id",
		Name: "Test Artist",
		Path: dir,
	}

	if err := WriteBackArtistNFO(context.Background(), a, nil, nil); err != nil {
		t.Fatalf("WriteBackArtistNFO: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "artist.nfo"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "<stillwater") {
		t.Error("NFO does not contain <stillwater element")
	}
	if !strings.Contains(content, `version="1"`) {
		t.Error("NFO does not contain version attribute")
	}
	if !strings.Contains(content, "written=") {
		t.Error("NFO does not contain written attribute")
	}

	// Parse back and validate the written timestamp is valid RFC 3339.
	parsed, err := Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Parse written NFO: %v", err)
	}
	if parsed.Stillwater == nil {
		t.Fatal("Stillwater is nil after round-trip")
	}
	if _, err := time.Parse(time.RFC3339, parsed.Stillwater.Written); err != nil {
		t.Errorf("Written is not valid RFC 3339: %v", err)
	}
}

func TestWriteBackArtistNFOWithFieldMap_MoodsAsStyles(t *testing.T) {
	dir := t.TempDir()
	a := &artist.Artist{
		ID:       "art-fm",
		Name:     "Field Map Artist",
		SortName: "Field Map Artist",
		Path:     dir,
		Genres:   []string{"Rock"},
		Styles:   []string{"Alternative"},
		Moods:    []string{"Energetic", "Uplifting"},
	}

	fm := NFOFieldMap{
		DefaultBehavior: false,
		MoodsAsStyles:   true,
		GenreSources:    []string{"genres"},
	}

	if err := WriteBackArtistNFOWithFieldMap(context.Background(), a, nil, nil, fm); err != nil {
		t.Fatalf("WriteBackArtistNFOWithFieldMap: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "artist.nfo"))
	if err != nil {
		t.Fatalf("reading nfo: %v", err)
	}

	parsed, err := Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("parsing nfo: %v", err)
	}

	// Moods should appear as <style> elements (merged with original styles).
	// With MoodsAsStyles=true, the style output should contain both the
	// original styles and the mood values.
	styleSet := make(map[string]bool, len(parsed.Styles))
	for _, s := range parsed.Styles {
		styleSet[s] = true
	}
	if !styleSet["Alternative"] {
		t.Error("expected original style 'Alternative' in <style> elements")
	}
	if !styleSet["Energetic"] {
		t.Error("expected mood 'Energetic' to appear as <style> element")
	}
	if !styleSet["Uplifting"] {
		t.Error("expected mood 'Uplifting' to appear as <style> element")
	}

	// Moods should still be written as <mood> elements for Kodi.
	moodSet := make(map[string]bool, len(parsed.Moods))
	for _, m := range parsed.Moods {
		moodSet[m] = true
	}
	if !moodSet["Energetic"] {
		t.Error("expected 'Energetic' in <mood> elements")
	}
	if !moodSet["Uplifting"] {
		t.Error("expected 'Uplifting' in <mood> elements")
	}

	// LockData should always be set.
	if !parsed.LockData {
		t.Error("LockData should be true")
	}

	// Stillwater meta should be present.
	if parsed.Stillwater == nil {
		t.Error("Stillwater meta should be set")
	}
}
