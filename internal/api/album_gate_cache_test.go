package api

import (
	"context"
	"errors"
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
)

// countingRGFetcher fails on any non-normalized id, modeling MusicBrainz
// rejecting a padded or uppercase MBID.
type countingRGFetcher struct {
	calls []string
}

func (f *countingRGFetcher) GetReleaseGroups(_ context.Context, id string) ([]provider.ReleaseGroupInfo, error) {
	f.calls = append(f.calls, id)
	if id != normalizeMBID(id) {
		return nil, errors.New("invalid mbid")
	}
	return []provider.ReleaseGroupInfo{{Title: "Album"}}, nil
}

const (
	rgClean  = "5b11f4ce-a62d-471e-81fc-a69a8278c7da"
	rgPadded = " 5B11F4CE-A62D-471E-81FC-A69A8278C7DA "
)

// One MBID spelled two ways must cost ONE fetch, asked with the normalized id
// (#2868), and holds must recognize a spelling other than the fetched one.
func TestReleaseGroupCache_TitlesKeysOnNormalizedMBID(t *testing.T) {
	f := &countingRGFetcher{}
	c := &releaseGroupCache{fetcher: f, entries: make(map[string]releaseGroupEntry)}

	if _, ok := c.titles(context.Background(), rgClean); !ok {
		t.Fatal("first lookup should be known")
	}
	if !c.holds(rgPadded) {
		t.Error("holds must see a different spelling as already cached")
	}
	if titles, ok := c.titles(context.Background(), rgPadded); !ok || len(titles) != 1 {
		t.Fatalf("second spelling = %v, %v; want the cached titles", titles, ok)
	}
	if len(f.calls) != 1 || f.calls[0] != rgClean {
		t.Fatalf("fetch calls = %q, want exactly [%q]", f.calls, rgClean)
	}
}

// A padded spelling fetched FIRST must not poison the shared entry for the
// clean spelling.
func TestReleaseGroupCache_PaddedFirstDoesNotPoisonClean(t *testing.T) {
	f := &countingRGFetcher{}
	c := &releaseGroupCache{fetcher: f, entries: make(map[string]releaseGroupEntry)}

	if _, ok := c.titles(context.Background(), rgPadded); !ok {
		t.Fatal("padded spelling should resolve via the normalized id")
	}
	if titles, ok := c.titles(context.Background(), rgClean); !ok || len(titles) != 1 {
		t.Fatalf("clean spelling = %v, %v; want titles", titles, ok)
	}
}
