package mbidcheck

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// causeMB is a MusicBrainz client that records the cause each call arrived
// with, so a test can see what the sweep's context actually carried to the
// provider boundary.
type causeMB struct {
	mu          sync.Mutex
	artistCause []string
	groupCause  []string
	metaErr     error
}

func (c *causeMB) GetArtist(ctx context.Context, _ string) (*provider.ArtistMetadata, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.artistCause = append(c.artistCause, provider.CauseFromContext(ctx).String())
	if c.metaErr != nil {
		return nil, c.metaErr
	}
	return &provider.ArtistMetadata{Name: "Example Band"}, nil
}

func (c *causeMB) GetReleaseGroups(ctx context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.groupCause = append(c.groupCause, provider.CauseFromContext(ctx).String())
	return rgs("One", "Two"), nil
}

// runCauseSweep drives the real Sweep and real Resolver over one artist and
// returns the log records the resolver emitted.
func runCauseSweep(t *testing.T, mb *causeMB) *recordingHandler {
	t.Helper()
	const id = "artist-1"
	h := &recordingHandler{}
	r := New(mb, found("One", "Two"), WithLogger(slog.New(h)))
	pop := &fakePopulation{rows: []artist.MBIDPath{{ArtistID: id, MBID: "m", Path: "/library/Example"}}}
	arts := &fakeArtists{byID: map[string]*artist.Artist{id: sweepArtist(id, "/library/Example")}}
	sw := newTestSweep(t, pop, arts, newFakeLedger(), r, Config{MaxPerPass: 10})
	if _, err := sw.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return h
}

// The sweep calls the adapter directly, bypassing the shared fetch points, so
// the cause has to be on the context Run hands the resolver.
func TestCause_SweepAttributesProviderCalls(t *testing.T) {
	t.Parallel()

	mb := &causeMB{}
	runCauseSweep(t, mb)

	// PRECONDITION: both provider calls happened; otherwise the loops below
	// assert over nothing.
	if len(mb.artistCause) == 0 || len(mb.groupCause) == 0 {
		t.Fatalf("precondition: GetArtist calls = %d, GetReleaseGroups calls = %d, want both > 0",
			len(mb.artistCause), len(mb.groupCause))
	}
	const want = "sweep:mbid_revalidate"
	for _, got := range append(append([]string{}, mb.artistCause...), mb.groupCause...) {
		if got != want {
			t.Errorf("provider call cause = %q, want %q", got, want)
		}
	}
}

// The verdict line the resolver already logs carries the cause too.
func TestCause_SweepVerdictLineCarriesCause(t *testing.T) {
	t.Parallel()

	mb := &causeMB{metaErr: &provider.ErrProviderUnavailable{Provider: provider.NameMusicBrainz}}
	h := runCauseSweep(t, mb)

	const msg = "stored musicbrainz id could not be checked: provider unavailable"
	got, ok := h.attr(msg, "cause")
	// PRECONDITION: the existing provider-unavailable Warn was emitted.
	if !ok {
		t.Fatalf("precondition: no %q record with a cause attribute", msg)
	}
	if got != "sweep:mbid_revalidate" {
		t.Errorf("cause = %v, want sweep:mbid_revalidate", got)
	}
}

// A Resolve outside any sweep logs the explicit marker, never a blank.
func TestCause_ResolveWithoutSweepIsUnattributed(t *testing.T) {
	t.Parallel()

	h := &recordingHandler{}
	mb := &causeMB{metaErr: &provider.ErrProviderUnavailable{Provider: provider.NameMusicBrainz}}
	r := New(mb, found("One"), WithLogger(slog.New(h)))
	if _, err := r.Resolve(context.Background(), testArtist()); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	got, ok := h.attr("stored musicbrainz id could not be checked: provider unavailable", "cause")
	if !ok {
		t.Fatal("precondition: the verdict line carries no cause attribute")
	}
	if got != "unattributed" {
		t.Errorf("cause = %v, want unattributed", got)
	}
}
