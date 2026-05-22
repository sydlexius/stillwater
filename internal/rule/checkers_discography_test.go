package rule

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/provider"
)

// newDiscographyTestEngine builds a minimal Engine carrying only the fields the
// discography checker reads (logger and release-group fetcher). Constructing
// the struct directly avoids standing up a full database for a checker that
// touches neither the DB nor any other engine subsystem.
func newDiscographyTestEngine(fetcher ReleaseGroupFetcher) *Engine {
	return &Engine{
		logger:              testLogger(),
		releaseGroupFetcher: fetcher,
	}
}

// TestDiscographyChecker_NoMBID does not flag an artist that has no MusicBrainz
// ID: its discography cannot be fetched, so there is nothing actionable.
func TestDiscographyChecker_NoMBID(t *testing.T) {
	dir := t.TempDir()
	writeTestNFO(t, dir, &nfo.ArtistNFO{Name: "Test Artist"})

	e := newDiscographyTestEngine(nil)
	checker := e.makeDiscographyChecker()

	a := &artist.Artist{Name: "Test Artist", Path: dir} // no MBID
	if v := checker(context.Background(), a, RuleConfig{}); v != nil {
		t.Errorf("checker flagged an artist with no MBID: %+v", v)
	}
}

// TestDiscographyChecker_EmptyDiscography flags an artist whose NFO has zero
// album entries. No provider round-trip is needed for this signal.
func TestDiscographyChecker_EmptyDiscography(t *testing.T) {
	dir := t.TempDir()
	writeTestNFO(t, dir, &nfo.ArtistNFO{Name: "Test Artist"})

	// A fetcher that would panic if called: the empty-discography signal must
	// not make a MusicBrainz request.
	fetcher := &stubReleaseGroupFetcher{}
	e := newDiscographyTestEngine(fetcher)
	checker := e.makeDiscographyChecker()

	a := &artist.Artist{Name: "Test Artist", Path: dir, MusicBrainzID: "mbid-abc"}
	v := checker(context.Background(), a, RuleConfig{})
	if v == nil {
		t.Fatal("checker did not flag an artist with an empty discography")
	}
	if v.RuleID != RuleDiscographyPopulated {
		t.Errorf("RuleID = %q, want %q", v.RuleID, RuleDiscographyPopulated)
	}
	if !v.Fixable {
		t.Error("empty-discography violation should be fixable")
	}
	if fetcher.calls != 0 {
		t.Errorf("fetcher called %d times for empty discography, want 0 (no MB call needed)", fetcher.calls)
	}
}

// TestDiscographyChecker_NoNFO stays silent when no artist.nfo exists: the
// nfo_exists rule owns that violation.
func TestDiscographyChecker_NoNFO(t *testing.T) {
	dir := t.TempDir() // empty dir, no artist.nfo

	e := newDiscographyTestEngine(&stubReleaseGroupFetcher{})
	checker := e.makeDiscographyChecker()

	a := &artist.Artist{Name: "Test Artist", Path: dir, MusicBrainzID: "mbid-abc"}
	if v := checker(context.Background(), a, RuleConfig{}); v != nil {
		t.Errorf("checker flagged an artist with no NFO file: %+v", v)
	}
}

// TestDiscographyChecker_CoverageBelowThreshold flags an artist whose NFO
// covers materially fewer release groups than MusicBrainz lists.
func TestDiscographyChecker_CoverageBelowThreshold(t *testing.T) {
	dir := t.TempDir()
	// NFO has 1 album; MusicBrainz will report 4 Album release groups -> 25%
	// coverage, below the 50% default threshold.
	writeTestNFO(t, dir, &nfo.ArtistNFO{
		Name: "Test Artist",
		Albums: []nfo.DiscographyAlbum{
			{Title: "Debut", Year: "2000", MusicBrainzReleaseGroupID: "rg-1"},
		},
	})

	fetcher := &stubReleaseGroupFetcher{
		groups: []provider.ReleaseGroupInfo{
			{ID: "rg-1", Title: "Debut", PrimaryType: "Album"},
			{ID: "rg-2", Title: "Second", PrimaryType: "Album"},
			{ID: "rg-3", Title: "Third", PrimaryType: "Album"},
			{ID: "rg-4", Title: "Fourth", PrimaryType: "Album"},
		},
	}
	e := newDiscographyTestEngine(fetcher)
	checker := e.makeDiscographyChecker()

	a := &artist.Artist{Name: "Test Artist", Path: dir, MusicBrainzID: "mbid-abc"}
	v := checker(context.Background(), a, RuleConfig{CoverageThreshold: 50})
	if v == nil {
		t.Fatal("checker did not flag an under-covered discography")
	}
	if !v.Fixable {
		t.Error("coverage violation should be fixable")
	}
}

// TestDiscographyChecker_CoverageMeetsThreshold does not flag an artist whose
// NFO covers enough of the MusicBrainz release groups.
func TestDiscographyChecker_CoverageMeetsThreshold(t *testing.T) {
	dir := t.TempDir()
	// NFO has 3 of 4 release groups -> 75% coverage, above the 50% threshold.
	writeTestNFO(t, dir, &nfo.ArtistNFO{
		Name: "Test Artist",
		Albums: []nfo.DiscographyAlbum{
			{Title: "Debut", MusicBrainzReleaseGroupID: "rg-1"},
			{Title: "Second", MusicBrainzReleaseGroupID: "rg-2"},
			{Title: "Third", MusicBrainzReleaseGroupID: "rg-3"},
		},
	})

	fetcher := &stubReleaseGroupFetcher{
		groups: []provider.ReleaseGroupInfo{
			{ID: "rg-1", Title: "Debut", PrimaryType: "Album"},
			{ID: "rg-2", Title: "Second", PrimaryType: "Album"},
			{ID: "rg-3", Title: "Third", PrimaryType: "Album"},
			{ID: "rg-4", Title: "Fourth", PrimaryType: "Album"},
		},
	}
	e := newDiscographyTestEngine(fetcher)
	checker := e.makeDiscographyChecker()

	a := &artist.Artist{Name: "Test Artist", Path: dir, MusicBrainzID: "mbid-abc"}
	if v := checker(context.Background(), a, RuleConfig{CoverageThreshold: 50}); v != nil {
		t.Errorf("checker flagged an adequately-covered discography: %+v", v)
	}
}

// TestDiscographyChecker_CoverageRespectsReleaseTypeFilter verifies coverage is
// measured against the configured release types only: Singles MusicBrainz
// reports are excluded from the denominator when the filter is Album,EP.
func TestDiscographyChecker_CoverageRespectsReleaseTypeFilter(t *testing.T) {
	dir := t.TempDir()
	// NFO has both Album release groups. MusicBrainz reports those 2 Albums
	// plus 6 Singles. With an Album,EP filter the denominator is 2, so coverage
	// is 100% and the artist must not be flagged.
	writeTestNFO(t, dir, &nfo.ArtistNFO{
		Name: "Test Artist",
		Albums: []nfo.DiscographyAlbum{
			{Title: "Debut", MusicBrainzReleaseGroupID: "rg-1"},
			{Title: "Second", MusicBrainzReleaseGroupID: "rg-2"},
		},
	})

	groups := []provider.ReleaseGroupInfo{
		{ID: "rg-1", Title: "Debut", PrimaryType: "Album"},
		{ID: "rg-2", Title: "Second", PrimaryType: "Album"},
	}
	for i := 0; i < 6; i++ {
		groups = append(groups, provider.ReleaseGroupInfo{
			ID: "single-" + string(rune('a'+i)), Title: "A Single", PrimaryType: "Single",
		})
	}
	fetcher := &stubReleaseGroupFetcher{groups: groups}
	e := newDiscographyTestEngine(fetcher)
	checker := e.makeDiscographyChecker()

	a := &artist.Artist{Name: "Test Artist", Path: dir, MusicBrainzID: "mbid-abc"}
	cfg := RuleConfig{CoverageThreshold: 50, ReleaseTypes: "Album,EP"}
	if v := checker(context.Background(), a, cfg); v != nil {
		t.Errorf("checker flagged an artist with full Album coverage (Singles must be excluded): %+v", v)
	}
}

// TestDiscographyChecker_NoFetcherSkipsCoverage accepts a non-empty discography
// when no release-group fetcher is wired: the coverage comparison is skipped.
func TestDiscographyChecker_NoFetcherSkipsCoverage(t *testing.T) {
	dir := t.TempDir()
	writeTestNFO(t, dir, &nfo.ArtistNFO{
		Name:   "Test Artist",
		Albums: []nfo.DiscographyAlbum{{Title: "Debut", MusicBrainzReleaseGroupID: "rg-1"}},
	})

	e := newDiscographyTestEngine(nil) // no fetcher
	checker := e.makeDiscographyChecker()

	a := &artist.Artist{Name: "Test Artist", Path: dir, MusicBrainzID: "mbid-abc"}
	if v := checker(context.Background(), a, RuleConfig{CoverageThreshold: 99}); v != nil {
		t.Errorf("checker flagged a non-empty discography with no fetcher wired: %+v", v)
	}
}

// TestDiscographyChecker_CorruptNFO does not flag (and does not panic) when the
// artist.nfo on disk cannot be parsed. The content below is not well-formed
// XML (mismatched closing tag), which the NFO parser rejects.
func TestDiscographyChecker_CorruptNFO(t *testing.T) {
	dir := t.TempDir()
	corrupt := "<artist><name>Test</wrong></artist>"
	if err := os.WriteFile(filepath.Join(dir, "artist.nfo"), []byte(corrupt), 0o644); err != nil {
		t.Fatalf("writing corrupt nfo: %v", err)
	}

	e := newDiscographyTestEngine(&stubReleaseGroupFetcher{})
	checker := e.makeDiscographyChecker()

	a := &artist.Artist{Name: "Test Artist", Path: dir, MusicBrainzID: "mbid-abc"}
	if v := checker(context.Background(), a, RuleConfig{}); v != nil {
		t.Errorf("checker flagged an artist with a corrupt NFO: %+v", v)
	}
}
