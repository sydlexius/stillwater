package rule

import (
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// TestArtistWorkersRoundTrip pins the exported accessor pair the settings UI
// wires (#1746): SetArtistWorkers stores the value and ArtistWorkers reports it,
// normalized so a non-positive value collapses to 1 (sequential). The settings
// handler reads ArtistWorkers to render the value actually in effect, so a
// regression here would misreport the live concurrency in the UI.
func TestArtistWorkersRoundTrip(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	engine := NewEngine(ruleSvc, db, nil, nil, testLogger())
	p := NewPipeline(engine, artistSvc, ruleSvc, nil, nil, testLogger())

	// A fresh pipeline defaults to sequential (>= 1).
	if got := p.ArtistWorkers(); got != 1 {
		t.Errorf("default ArtistWorkers = %d, want 1", got)
	}

	p.SetArtistWorkers(6)
	if got := p.ArtistWorkers(); got != 6 {
		t.Errorf("after SetArtistWorkers(6), ArtistWorkers = %d, want 6", got)
	}

	// Non-positive values normalize to 1 (the sequential floor).
	for _, n := range []int{0, -3} {
		p.SetArtistWorkers(n)
		if got := p.ArtistWorkers(); got != 1 {
			t.Errorf("after SetArtistWorkers(%d), ArtistWorkers = %d, want 1", n, got)
		}
	}
}
