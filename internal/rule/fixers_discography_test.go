package rule

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/provider"
)

// stubReleaseGroupFetcher is a test-only ReleaseGroupFetcher that returns a
// fixed set of release groups (or an error). It records how many times
// GetReleaseGroups was called so tests can assert the fixer made (or skipped)
// the MusicBrainz round-trip.
type stubReleaseGroupFetcher struct {
	groups []provider.ReleaseGroupInfo
	err    error
	calls  int
}

func (s *stubReleaseGroupFetcher) GetReleaseGroups(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
	s.calls++
	return s.groups, s.err
}

// writeTestNFO serializes an ArtistNFO to artist.nfo inside dir. It is a small
// helper so tests can seed a pre-existing on-disk discography.
func writeTestNFO(t *testing.T, dir string, n *nfo.ArtistNFO) {
	t.Helper()
	if err := nfo.WriteNFOAtomic(filepath.Join(dir, "artist.nfo"), n); err != nil {
		t.Fatalf("seeding artist.nfo: %v", err)
	}
}

// readTestNFO parses the artist.nfo inside dir back into an ArtistNFO.
func readTestNFO(t *testing.T, dir string) *nfo.ArtistNFO {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "artist.nfo"))
	if err != nil {
		t.Fatalf("reading artist.nfo: %v", err)
	}
	parsed, err := nfo.Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("parsing artist.nfo: %v", err)
	}
	return parsed
}

func TestDiscographyFixer_CanFix(t *testing.T) {
	f := &DiscographyFixer{}
	if !f.CanFix(&Violation{RuleID: RuleDiscographyPopulated}) {
		t.Error("DiscographyFixer should handle discography_populated")
	}
	if f.CanFix(&Violation{RuleID: RuleNFOExists}) {
		t.Error("DiscographyFixer should not handle nfo_exists")
	}
}

// TestDiscographyFixer_DoesNotImplementCandidateDiscovery locks the manual-mode
// contract: a fixer that writes a file to disk must NOT advertise candidate
// discovery, or the pipeline would run it speculatively during evaluation.
func TestDiscographyFixer_DoesNotImplementCandidateDiscovery(t *testing.T) {
	var f Fixer = &DiscographyFixer{}
	if _, ok := f.(CandidateDiscoverer); ok {
		t.Error("DiscographyFixer must not implement CandidateDiscoverer (it writes to disk)")
	}
}

// TestDiscographyFixer_Fix_PopulatesEmptyDiscography verifies the core auto-fix
// path: an artist with an empty NFO discography gets MusicBrainz release groups
// merged in and the file rewritten.
func TestDiscographyFixer_Fix_PopulatesEmptyDiscography(t *testing.T) {
	dir := t.TempDir()
	// Seed an NFO with no albums.
	writeTestNFO(t, dir, &nfo.ArtistNFO{Name: "Test Artist"})

	fetcher := &stubReleaseGroupFetcher{
		groups: []provider.ReleaseGroupInfo{
			{ID: "rg-1", Title: "First Album", PrimaryType: "Album", FirstReleaseDate: "2001-05-10"},
			{ID: "rg-2", Title: "An EP", PrimaryType: "EP", FirstReleaseDate: "2003"},
		},
	}
	f := NewDiscographyFixer(fetcher, nonSharedFSCheck(), nil, testLogger())

	a := &artist.Artist{
		Name:          "Test Artist",
		Path:          dir,
		MusicBrainzID: "mbid-abc",
		LibraryID:     "lib-test",
	}
	fr, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleDiscographyPopulated})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if !fr.Fixed {
		t.Fatalf("Fixed = false, want true (message: %s)", fr.Message)
	}
	if fetcher.calls != 1 {
		t.Errorf("fetcher called %d times, want 1", fetcher.calls)
	}

	got := readTestNFO(t, dir)
	if len(got.Albums) != 2 {
		t.Fatalf("merged NFO has %d albums, want 2", len(got.Albums))
	}
	// Provider-to-NFO mapping: the four-digit year is extracted from the
	// MusicBrainz first-release-date and the release-group MBID is carried over.
	if got.Albums[0].Title != "First Album" || got.Albums[0].Year != "2001" {
		t.Errorf("album[0] = %+v, want title=First Album year=2001", got.Albums[0])
	}
	if got.Albums[0].MusicBrainzReleaseGroupID != "rg-1" {
		t.Errorf("album[0] release-group MBID = %q, want rg-1", got.Albums[0].MusicBrainzReleaseGroupID)
	}
	if got.Albums[1].Year != "2003" {
		t.Errorf("album[1] year = %q, want 2003", got.Albums[1].Year)
	}
}

// TestDiscographyFixer_Fix_PreservesUserAddedAlbums is the key acceptance test:
// an album the user hand-added (no release-group MBID) must survive the merge,
// and an existing MBID-keyed entry the user refined must not be overwritten by
// the incoming MusicBrainz data.
func TestDiscographyFixer_Fix_PreservesUserAddedAlbums(t *testing.T) {
	dir := t.TempDir()
	// Seed an NFO with one user-added album (no MBID) and one MBID-keyed album
	// whose title the user refined.
	writeTestNFO(t, dir, &nfo.ArtistNFO{
		Name: "Test Artist",
		Albums: []nfo.DiscographyAlbum{
			{Title: "Bootleg Live Set", Year: "1999"},                                      // user-added, no MBID
			{Title: "Debut (Remastered)", Year: "2000", MusicBrainzReleaseGroupID: "rg-1"}, // user-refined title
		},
	})

	fetcher := &stubReleaseGroupFetcher{
		groups: []provider.ReleaseGroupInfo{
			// Same MBID as the user-refined entry: the user's version must win.
			{ID: "rg-1", Title: "Debut", PrimaryType: "Album", FirstReleaseDate: "2000"},
			// A genuinely new release group: should be appended.
			{ID: "rg-2", Title: "Second Album", PrimaryType: "Album", FirstReleaseDate: "2004"},
		},
	}
	f := NewDiscographyFixer(fetcher, nonSharedFSCheck(), nil, testLogger())

	a := &artist.Artist{
		Name:          "Test Artist",
		Path:          dir,
		MusicBrainzID: "mbid-abc",
		LibraryID:     "lib-test",
	}
	fr, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleDiscographyPopulated})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if !fr.Fixed {
		t.Fatalf("Fixed = false, want true (message: %s)", fr.Message)
	}

	got := readTestNFO(t, dir)
	if len(got.Albums) != 3 {
		t.Fatalf("merged NFO has %d albums, want 3", len(got.Albums))
	}

	// User-added album (no MBID) is preserved and lands first.
	if got.Albums[0].Title != "Bootleg Live Set" {
		t.Errorf("album[0] = %q, want the user-added Bootleg Live Set", got.Albums[0].Title)
	}
	// The MBID-keyed entry keeps the user's refined title, NOT the MB title.
	var refined *nfo.DiscographyAlbum
	for i := range got.Albums {
		if got.Albums[i].MusicBrainzReleaseGroupID == "rg-1" {
			refined = &got.Albums[i]
		}
	}
	if refined == nil {
		t.Fatal("rg-1 entry missing after merge")
	}
	if refined.Title != "Debut (Remastered)" {
		t.Errorf("rg-1 title = %q, want the user-refined Debut (Remastered)", refined.Title)
	}
	// The new release group was appended.
	var added bool
	for _, alb := range got.Albums {
		if alb.MusicBrainzReleaseGroupID == "rg-2" {
			added = true
		}
	}
	if !added {
		t.Error("new release group rg-2 was not appended")
	}
}

// TestDiscographyFixer_Fix_ReleaseTypeFilter verifies the per-rule release-type
// filter is honored: a Single is skipped when the filter is Album,EP.
func TestDiscographyFixer_Fix_ReleaseTypeFilter(t *testing.T) {
	dir := t.TempDir()
	writeTestNFO(t, dir, &nfo.ArtistNFO{Name: "Test Artist"})

	fetcher := &stubReleaseGroupFetcher{
		groups: []provider.ReleaseGroupInfo{
			{ID: "rg-1", Title: "An Album", PrimaryType: "Album"},
			{ID: "rg-2", Title: "A Single", PrimaryType: "Single"},
		},
	}
	f := NewDiscographyFixer(fetcher, nonSharedFSCheck(), nil, testLogger())

	a := &artist.Artist{
		Name:          "Test Artist",
		Path:          dir,
		MusicBrainzID: "mbid-abc",
		LibraryID:     "lib-test",
	}
	v := &Violation{RuleID: RuleDiscographyPopulated, Config: RuleConfig{ReleaseTypes: "Album,EP"}}
	fr, err := f.Fix(context.Background(), a, v)
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if !fr.Fixed {
		t.Fatalf("Fixed = false, want true (message: %s)", fr.Message)
	}

	got := readTestNFO(t, dir)
	if len(got.Albums) != 1 {
		t.Fatalf("merged NFO has %d albums, want 1 (Single should be filtered out)", len(got.Albums))
	}
	if got.Albums[0].Title != "An Album" {
		t.Errorf("merged album = %q, want An Album", got.Albums[0].Title)
	}
}

// TestDiscographyFixer_Fix_NoMBID returns a non-fatal result when the artist
// has no MusicBrainz ID: there is nothing to fetch.
func TestDiscographyFixer_Fix_NoMBID(t *testing.T) {
	f := NewDiscographyFixer(&stubReleaseGroupFetcher{}, nonSharedFSCheck(), nil, testLogger())
	a := &artist.Artist{Name: "Test Artist", Path: t.TempDir(), LibraryID: "lib-test"}
	fr, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleDiscographyPopulated})
	if err != nil {
		t.Fatalf("Fix returned error, want non-fatal result: %v", err)
	}
	if fr.Fixed {
		t.Error("Fixed = true, want false for an artist with no MBID")
	}
}

// TestDiscographyFixer_Fix_NoNewReleaseGroups reports a non-fatal no-op when
// the merge adds nothing (the NFO already covers every incoming release group).
func TestDiscographyFixer_Fix_NoNewReleaseGroups(t *testing.T) {
	dir := t.TempDir()
	writeTestNFO(t, dir, &nfo.ArtistNFO{
		Name: "Test Artist",
		Albums: []nfo.DiscographyAlbum{
			{Title: "Only Album", Year: "2001", MusicBrainzReleaseGroupID: "rg-1"},
		},
	})

	fetcher := &stubReleaseGroupFetcher{
		groups: []provider.ReleaseGroupInfo{
			{ID: "rg-1", Title: "Only Album", PrimaryType: "Album", FirstReleaseDate: "2001"},
		},
	}
	f := NewDiscographyFixer(fetcher, nonSharedFSCheck(), nil, testLogger())

	a := &artist.Artist{
		Name:          "Test Artist",
		Path:          dir,
		MusicBrainzID: "mbid-abc",
		LibraryID:     "lib-test",
	}
	fr, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleDiscographyPopulated})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if fr.Fixed {
		t.Error("Fixed = true, want false when nothing new was merged")
	}
}

// TestDiscographyFixer_Fix_FetchError surfaces a provider failure as an error
// so the pipeline records the fix as failed and keeps the violation open.
func TestDiscographyFixer_Fix_FetchError(t *testing.T) {
	dir := t.TempDir()
	writeTestNFO(t, dir, &nfo.ArtistNFO{Name: "Test Artist"})

	fetcher := &stubReleaseGroupFetcher{err: errors.New("musicbrainz unreachable")}
	f := NewDiscographyFixer(fetcher, nonSharedFSCheck(), nil, testLogger())

	a := &artist.Artist{
		Name:          "Test Artist",
		Path:          dir,
		MusicBrainzID: "mbid-abc",
		LibraryID:     "lib-test",
	}
	if _, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleDiscographyPopulated}); err == nil {
		t.Error("Fix returned nil error, want the provider failure surfaced")
	}
}

// TestDiscographyFixer_Fix_NoFetcher reports a non-fatal "not available" result
// when the release-group fetcher is unwired.
func TestDiscographyFixer_Fix_NoFetcher(t *testing.T) {
	dir := t.TempDir()
	writeTestNFO(t, dir, &nfo.ArtistNFO{Name: "Test Artist"})

	f := NewDiscographyFixer(nil, nonSharedFSCheck(), nil, testLogger())
	a := &artist.Artist{
		Name:          "Test Artist",
		Path:          dir,
		MusicBrainzID: "mbid-abc",
		LibraryID:     "lib-test",
	}
	fr, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleDiscographyPopulated})
	if err != nil {
		t.Fatalf("Fix returned error, want non-fatal result: %v", err)
	}
	if fr.Fixed {
		t.Error("Fixed = true, want false when no fetcher is wired")
	}
}

// TestDiscographyFixer_Fix_SharedFilesystem refuses to write the NFO when the
// artist's library shares its filesystem with a media server: the platform's
// own NFO saver could overwrite the merged file.
func TestDiscographyFixer_Fix_SharedFilesystem(t *testing.T) {
	dir := t.TempDir()
	writeTestNFO(t, dir, &nfo.ArtistNFO{Name: "Test Artist"})

	sharedCheck := NewSharedFSCheck(&stubLibQuerier{
		lib: &library.Library{SharedFSStatus: library.SharedFSConfirmed},
	}, testLogger())
	// A fetcher that would record a call: the shared-FS guard must short-circuit
	// before any MusicBrainz round-trip.
	fetcher := &stubReleaseGroupFetcher{}
	f := NewDiscographyFixer(fetcher, sharedCheck, nil, testLogger())

	a := &artist.Artist{
		Name:          "Test Artist",
		Path:          dir,
		MusicBrainzID: "mbid-abc",
		LibraryID:     "lib-test",
	}
	fr, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleDiscographyPopulated})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if fr.Fixed {
		t.Error("Fixed = true, want false for a shared-filesystem library")
	}
	if fetcher.calls != 0 {
		t.Errorf("fetcher called %d times, want 0 (shared-FS guard must short-circuit)", fetcher.calls)
	}
}

// TestDiscographyFixer_Fix_NoPath returns a non-fatal result for an artist with
// no filesystem path: the NFO cannot be written.
func TestDiscographyFixer_Fix_NoPath(t *testing.T) {
	f := NewDiscographyFixer(&stubReleaseGroupFetcher{}, nil, nil, testLogger())
	a := &artist.Artist{Name: "Test Artist", MusicBrainzID: "mbid-abc"} // no Path
	fr, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleDiscographyPopulated})
	if err != nil {
		t.Fatalf("Fix returned error, want non-fatal result: %v", err)
	}
	if fr.Fixed {
		t.Error("Fixed = true, want false for an artist with no path")
	}
}

// TestDiscographyFixer_Fix_CorruptNFO refuses to overwrite an unparsable NFO
// so hand-edited content is never destroyed.
func TestDiscographyFixer_Fix_CorruptNFO(t *testing.T) {
	dir := t.TempDir()
	corrupt := "<artist><name>Test</wrong></artist>"
	if err := os.WriteFile(filepath.Join(dir, "artist.nfo"), []byte(corrupt), 0o644); err != nil {
		t.Fatalf("writing corrupt nfo: %v", err)
	}

	f := NewDiscographyFixer(&stubReleaseGroupFetcher{}, nonSharedFSCheck(), nil, testLogger())
	a := &artist.Artist{
		Name:          "Test Artist",
		Path:          dir,
		MusicBrainzID: "mbid-abc",
		LibraryID:     "lib-test",
	}
	fr, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleDiscographyPopulated})
	if err != nil {
		t.Fatalf("Fix returned error, want non-fatal result: %v", err)
	}
	if fr.Fixed {
		t.Error("Fixed = true, want false for a corrupt NFO that must not be overwritten")
	}
}

// TestDiscographyFixer_Fix_SeedsMissingNFO writes a fresh NFO when none exists
// on disk yet, seeded from the DB artist.
func TestDiscographyFixer_Fix_SeedsMissingNFO(t *testing.T) {
	dir := t.TempDir() // no artist.nfo on disk

	fetcher := &stubReleaseGroupFetcher{
		groups: []provider.ReleaseGroupInfo{
			{ID: "rg-1", Title: "First Album", PrimaryType: "Album", FirstReleaseDate: "2001"},
		},
	}
	f := NewDiscographyFixer(fetcher, nonSharedFSCheck(), nil, testLogger())

	a := &artist.Artist{
		Name:          "Test Artist",
		Path:          dir,
		MusicBrainzID: "mbid-abc",
		LibraryID:     "lib-test",
	}
	fr, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleDiscographyPopulated})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if !fr.Fixed {
		t.Fatalf("Fixed = false, want true (message: %s)", fr.Message)
	}
	got := readTestNFO(t, dir)
	if len(got.Albums) != 1 {
		t.Fatalf("seeded NFO has %d albums, want 1", len(got.Albums))
	}
}
