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

	if err := WriteBackArtistNFOWithFieldMap(context.Background(), a, nil, nil, DefaultFieldMap(), false); err != nil {
		t.Fatalf("WriteBackArtistNFOWithFieldMap: %v", err)
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

	if err := WriteBackArtistNFOWithFieldMap(ctx, a, ss, nil, DefaultFieldMap(), false); err != nil {
		t.Fatalf("WriteBackArtistNFOWithFieldMap: %v", err)
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
	if err := WriteBackArtistNFOWithFieldMap(context.Background(), a, nil, nil, DefaultFieldMap(), false); err != nil {
		t.Fatalf("WriteBackArtistNFOWithFieldMap with nil ss: %v", err)
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

	if err := WriteBackArtistNFOWithFieldMap(ctx, a, ss, nil, DefaultFieldMap(), false); err != nil {
		t.Fatalf("WriteBackArtistNFOWithFieldMap: %v", err)
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

// TestWriteBackArtistNFO_LockDataDefaultOff verifies the post-#1264 default:
// WriteBackArtistNFOWithFieldMap with lockNFO=false does NOT stamp <lockdata>true</lockdata>.
// Lock semantics are opt-in per library via WriteBackArtistNFOWithFieldMap.
func TestWriteBackArtistNFO_LockDataDefaultOff(t *testing.T) {
	dir := t.TempDir()
	a := &artist.Artist{
		ID:       "art-lock-default",
		Name:     "Default Lock Artist",
		SortName: "Default Lock Artist",
		Path:     dir,
	}

	if err := WriteBackArtistNFOWithFieldMap(context.Background(), a, nil, nil, DefaultFieldMap(), false); err != nil {
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

	if parsed.LockData {
		t.Error("default WriteBackArtistNFOWithFieldMap must not set LockData (issue #1264)")
	}

	output := string(data)
	if strings.Contains(output, "<lockdata>") {
		t.Errorf("raw NFO output must not contain any <lockdata> element, got:\n%s", output)
	}
}

// TestWriteBackArtistNFOWithFieldMap_LockNFOOptIn verifies the explicit
// per-library opt-in path: when lockNFO=true is passed, the resulting NFO
// carries <lockdata>true</lockdata>; when lockNFO=false, it does not.
func TestWriteBackArtistNFOWithFieldMap_LockNFOOptIn(t *testing.T) {
	cases := []struct {
		name    string
		lockNFO bool
		want    bool
	}{
		{"opt-in stamps lockdata", true, true},
		{"opt-out omits lockdata", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			a := &artist.Artist{
				ID:       "art-lock-" + tc.name,
				Name:     "Lock Opt-In Artist",
				SortName: "Lock Opt-In Artist",
				Path:     dir,
			}

			if err := WriteBackArtistNFOWithFieldMap(
				context.Background(), a, nil, nil, DefaultFieldMap(), tc.lockNFO,
			); err != nil {
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
			if parsed.LockData != tc.want {
				t.Errorf("LockData = %v, want %v", parsed.LockData, tc.want)
			}
			output := string(data)
			// The opt-out contract is "no <lockdata> element at all," not
			// merely "no <lockdata>true</lockdata>." Asserting on the bare
			// opening tag catches a regression that emits
			// <lockdata>false</lockdata>, which would still mislead
			// downstream tooling that treats element presence as the
			// "managed by the writer" signal.
			contains := strings.Contains(output, "<lockdata>")
			if contains != tc.want {
				t.Errorf("raw <lockdata> element presence = %v, want %v; output:\n%s", contains, tc.want, output)
			}
		})
	}
}

func TestWriteBackArtistNFO_NilArtist(t *testing.T) {
	err := WriteBackArtistNFOWithFieldMap(context.Background(), nil, nil, nil, DefaultFieldMap(), false)
	if err == nil {
		t.Fatal("expected error for nil artist, got nil")
	}
	if got := err.Error(); got != "write artist nfo: artist is nil" {
		t.Errorf("error = %q, want %q", got, "write artist nfo: artist is nil")
	}
}

func TestWriteBackArtistNFO_EmptyPath(t *testing.T) {
	a := &artist.Artist{ID: "art-empty", Name: "Empty Path"}
	err := WriteBackArtistNFOWithFieldMap(context.Background(), a, nil, nil, DefaultFieldMap(), false)
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

	if err := WriteBackArtistNFOWithFieldMap(context.Background(), a, nil, nil, DefaultFieldMap(), false); err != nil {
		t.Fatalf("WriteBackArtistNFOWithFieldMap: %v", err)
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

	if err := WriteBackArtistNFOWithFieldMap(context.Background(), a, nil, nil, fm, false); err != nil {
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

	// LockData defaults off (issue #1264) -- this case passed lockNFO=false.
	if parsed.LockData {
		t.Error("LockData should be false when lockNFO=false")
	}

	// Stillwater meta should be present.
	if parsed.Stillwater == nil {
		t.Error("Stillwater meta should be set")
	}
}

// TestWriteBackArtistNFO_Discography covers the data-loss regression where
// <album> entries on an Artist's Discography were dropped during write-back.
// Empty, single, and multi-entry cases all round-trip through WriteBackArtistNFOWithFieldMap
// and reload from disk without losing fields.
func TestWriteBackArtistNFO_Discography(t *testing.T) {
	cases := []struct {
		name   string
		albums []artist.DiscographyAlbum
	}{
		{name: "empty", albums: nil},
		{
			name: "single",
			albums: []artist.DiscographyAlbum{
				{Title: "Nevermind", Year: "1991", MusicBrainzReleaseGroupID: "rg-nevermind"},
			},
		},
		{
			name: "multiple",
			albums: []artist.DiscographyAlbum{
				{Title: "Bleach", Year: "1989"},
				{Title: "Nevermind", Year: "1991", MusicBrainzReleaseGroupID: "rg-nevermind"},
				{Title: "In Utero", Year: "1993"},
			},
		},
		{
			name: "partial fields",
			albums: []artist.DiscographyAlbum{
				{Title: "Title Only"},
				{Title: "Year Missing MBID", MusicBrainzReleaseGroupID: "rg-only"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			a := &artist.Artist{
				ID:             "art-disco",
				Name:           "Nirvana",
				Type:           "group",
				Disambiguation: "Seattle grunge",
				Path:           dir,
				Discography:    tc.albums,
			}
			if err := WriteBackArtistNFOWithFieldMap(context.Background(), a, nil, nil, DefaultFieldMap(), false); err != nil {
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

			if parsed.Disambiguation != "Seattle grunge" {
				t.Errorf("Disambiguation = %q, want %q", parsed.Disambiguation, "Seattle grunge")
			}
			if len(parsed.Albums) != len(tc.albums) {
				t.Fatalf("Albums count = %d, want %d; serialized: %s",
					len(parsed.Albums), len(tc.albums), string(data))
			}
			for i, want := range tc.albums {
				got := parsed.Albums[i]
				if got.Title != want.Title || got.Year != want.Year ||
					got.MusicBrainzReleaseGroupID != want.MusicBrainzReleaseGroupID {
					t.Errorf("album[%d] = %+v, want %+v", i, got, want)
				}
			}
		})
	}
}

// TestWriteBackArtistNFO_PreservesOnDiskDiscography covers the #1616
// data-loss regression. Discography (<album> entries) is NFO-only -- it is
// never persisted to the database -- so an artist.Artist loaded from the DB
// carries an empty Discography slice. Before the fix, a write-back over an
// NFO that already had <album> entries would silently drop every one of
// them. This test seeds an on-disk NFO with discography, writes back a
// DB-style artist (empty Discography), and asserts the entries survive.
func TestWriteBackArtistNFO_PreservesOnDiskDiscography(t *testing.T) {
	dir := t.TempDir()

	// Seed an existing on-disk NFO that already carries a discography.
	seed := &ArtistNFO{
		Name: "Old Name",
		Albums: []DiscographyAlbum{
			{Title: "Bleach", Year: "1989"},
			{Title: "Nevermind", Year: "1991", MusicBrainzReleaseGroupID: "rg-nevermind"},
			{Title: "In Utero", Year: "1993"},
		},
	}
	if err := WriteNFOAtomic(filepath.Join(dir, "artist.nfo"), seed); err != nil {
		t.Fatalf("seeding nfo: %v", err)
	}

	// A DB-loaded artist: metadata is present but Discography is empty,
	// because the database has no discography column.
	a := &artist.Artist{
		ID:       "art-preserve",
		Name:     "Nirvana",
		SortName: "Nirvana",
		Path:     dir,
		// Discography intentionally left nil.
	}

	if err := WriteBackArtistNFOWithFieldMap(context.Background(), a, nil, nil, DefaultFieldMap(), false); err != nil {
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

	// The metadata write-back should have applied.
	if parsed.Name != "Nirvana" {
		t.Errorf("Name = %q, want %q", parsed.Name, "Nirvana")
	}

	// The on-disk discography must survive the write-back.
	if len(parsed.Albums) != len(seed.Albums) {
		t.Fatalf("Albums count = %d, want %d; serialized:\n%s",
			len(parsed.Albums), len(seed.Albums), string(data))
	}
	for i, want := range seed.Albums {
		got := parsed.Albums[i]
		if got.Title != want.Title || got.Year != want.Year ||
			got.MusicBrainzReleaseGroupID != want.MusicBrainzReleaseGroupID {
			t.Errorf("album[%d] = %+v, want %+v", i, got, want)
		}
	}
}

// TestWriteBackArtistNFO_NoDiscographyToPreserve verifies that when the
// existing on-disk NFO has no <album> entries, the write-back still succeeds
// and the output likewise has none -- no crash, no spurious entries.
func TestWriteBackArtistNFO_NoDiscographyToPreserve(t *testing.T) {
	dir := t.TempDir()

	// Seed an existing NFO with metadata but no discography.
	seed := &ArtistNFO{Name: "Old Name"}
	if err := WriteNFOAtomic(filepath.Join(dir, "artist.nfo"), seed); err != nil {
		t.Fatalf("seeding nfo: %v", err)
	}

	a := &artist.Artist{
		ID:       "art-nodisco",
		Name:     "Solo Act",
		SortName: "Solo Act",
		Path:     dir,
	}

	if err := WriteBackArtistNFOWithFieldMap(context.Background(), a, nil, nil, DefaultFieldMap(), false); err != nil {
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
	if len(parsed.Albums) != 0 {
		t.Errorf("Albums = %v, want none", parsed.Albums)
	}
}

// TestWriteBackArtistNFO_UnparsableExistingNFO verifies that a corrupt or
// otherwise unparsable existing NFO does not break the write-back. The fix
// is best-effort: discography preservation is skipped, but the write itself
// still succeeds and produces a valid NFO.
func TestWriteBackArtistNFO_UnparsableExistingNFO(t *testing.T) {
	dir := t.TempDir()

	// Seed a file that is not valid NFO XML.
	garbage := []byte("this is not xml at all <<< &&& >>>")
	if err := os.WriteFile(filepath.Join(dir, "artist.nfo"), garbage, 0o644); err != nil {
		t.Fatalf("seeding garbage nfo: %v", err)
	}

	a := &artist.Artist{
		ID:       "art-corrupt",
		Name:     "Recovered Artist",
		SortName: "Recovered Artist",
		Path:     dir,
	}

	// The write-back must not fail despite the unparsable existing file.
	if err := WriteBackArtistNFOWithFieldMap(context.Background(), a, nil, nil, DefaultFieldMap(), false); err != nil {
		t.Fatalf("WriteBackArtistNFOWithFieldMap over unparsable NFO: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "artist.nfo"))
	if err != nil {
		t.Fatalf("reading nfo: %v", err)
	}
	parsed, err := Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("parsing rewritten nfo: %v", err)
	}
	if parsed.Name != "Recovered Artist" {
		t.Errorf("Name = %q, want %q", parsed.Name, "Recovered Artist")
	}
	// No discography could be recovered from the garbage input.
	if len(parsed.Albums) != 0 {
		t.Errorf("Albums = %v, want none", parsed.Albums)
	}
}

// TestWriteBackArtistNFO_ArtistDiscographyWinsOverOnDisk verifies that when
// the in-memory artist itself carries discography (e.g. freshly scanned from
// the same NFO), that value is used and is not silently replaced by a stale
// on-disk parse.
func TestWriteBackArtistNFO_ArtistDiscographyWinsOverOnDisk(t *testing.T) {
	dir := t.TempDir()

	// On-disk NFO has an outdated single-album discography.
	seed := &ArtistNFO{
		Name:   "Old Name",
		Albums: []DiscographyAlbum{{Title: "Stale Album", Year: "1900"}},
	}
	if err := WriteNFOAtomic(filepath.Join(dir, "artist.nfo"), seed); err != nil {
		t.Fatalf("seeding nfo: %v", err)
	}

	// The in-memory artist carries a fresher, different discography.
	a := &artist.Artist{
		ID:       "art-wins",
		Name:     "Updated Artist",
		SortName: "Updated Artist",
		Path:     dir,
		Discography: []artist.DiscographyAlbum{
			{Title: "Fresh Album One", Year: "2020"},
			{Title: "Fresh Album Two", Year: "2024", MusicBrainzReleaseGroupID: "rg-fresh"},
		},
	}

	if err := WriteBackArtistNFOWithFieldMap(context.Background(), a, nil, nil, DefaultFieldMap(), false); err != nil {
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

	if len(parsed.Albums) != 2 {
		t.Fatalf("Albums count = %d, want 2; serialized:\n%s", len(parsed.Albums), string(data))
	}
	if parsed.Albums[0].Title != "Fresh Album One" || parsed.Albums[1].Title != "Fresh Album Two" {
		t.Errorf("Albums = %+v, want the in-memory discography to win", parsed.Albums)
	}
}

// --- WriteNFOAtomic ---

func TestWriteNFOAtomic_Success(t *testing.T) {
	path := filepath.Join(t.TempDir(), "artist.nfo")
	n := &ArtistNFO{
		Name:                "Pixies",
		MusicBrainzArtistID: "mbid-pixies",
		Albums: []DiscographyAlbum{
			{Title: "Surfer Rosa", Year: "1988", MusicBrainzReleaseGroupID: "rg-surfer"},
			{Title: "Doolittle", Year: "1989"},
		},
	}

	if err := WriteNFOAtomic(path, n); err != nil {
		t.Fatalf("WriteNFOAtomic: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	parsed, err := Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed.Name != "Pixies" {
		t.Errorf("Name = %q, want Pixies", parsed.Name)
	}
	if len(parsed.Albums) != 2 {
		t.Fatalf("Albums count = %d, want 2; serialized: %s", len(parsed.Albums), data)
	}
	if parsed.Albums[0].Title != "Surfer Rosa" ||
		parsed.Albums[0].MusicBrainzReleaseGroupID != "rg-surfer" {
		t.Errorf("Albums[0] = %+v", parsed.Albums[0])
	}
}

func TestWriteNFOAtomic_NilNFO(t *testing.T) {
	err := WriteNFOAtomic(filepath.Join(t.TempDir(), "artist.nfo"), nil)
	if err == nil {
		t.Fatal("expected error for nil nfo, got nil")
	}
	if got := err.Error(); got != "write nfo: nfo is nil" {
		t.Errorf("error = %q, want %q", got, "write nfo: nfo is nil")
	}
}

func TestWriteNFOAtomic_EmptyPath(t *testing.T) {
	err := WriteNFOAtomic("", &ArtistNFO{Name: "Pixies"})
	if err == nil {
		t.Fatal("expected error for empty path, got nil")
	}
	if got := err.Error(); got != "write nfo: path is empty" {
		t.Errorf("error = %q, want %q", got, "write nfo: path is empty")
	}
}

func TestWriteNFOAtomic_UnwritablePath(t *testing.T) {
	// Use a regular file as a parent path component. WriteFileAtomic's
	// MkdirAll cannot create a directory under a file, so WriteNFOAtomic
	// must return a wrapped error.
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("WriteFile blocker: %v", err)
	}
	err := WriteNFOAtomic(filepath.Join(blocker, "artist.nfo"), &ArtistNFO{Name: "Pixies"})
	if err == nil {
		t.Fatal("expected error writing under a non-directory path, got nil")
	}
	if !strings.Contains(err.Error(), "writing nfo file") {
		t.Errorf("error = %q, want it to mention 'writing nfo file'", err.Error())
	}
}

// TestWriteBackArtistNFO_UnreadableExistingNFO verifies the write-back
// distinguishes a genuinely-absent NFO from one that exists but cannot be read.
// Here the artist.nfo path is a directory, so os.ReadFile fails with a
// non-os.ErrNotExist error: the new readErr branch logs a Warn instead of
// treating the failure as a benign "no existing NFO". The write-back stays
// best-effort and still completes (WriteFileAtomic moves the unreadable path
// aside), but it cannot preserve discography it was unable to read.
func TestWriteBackArtistNFO_UnreadableExistingNFO(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "artist.nfo")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("mkdir artist.nfo: %v", err)
	}

	a := &artist.Artist{
		ID:       "art-unreadable",
		Name:     "Nirvana",
		SortName: "Nirvana",
		Path:     dir,
	}

	if err := WriteBackArtistNFOWithFieldMap(context.Background(), a, nil, nil, DefaultFieldMap(), false); err != nil {
		t.Fatalf("WriteBackArtistNFOWithFieldMap over an unreadable NFO path: %v", err)
	}

	// The path is now a regular NFO file carrying the artist metadata.
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("reading nfo after write-back: %v", err)
	}
	parsed, err := Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("parsing nfo: %v", err)
	}
	if parsed.Name != "Nirvana" {
		t.Errorf("Name = %q, want %q", parsed.Name, "Nirvana")
	}
}
