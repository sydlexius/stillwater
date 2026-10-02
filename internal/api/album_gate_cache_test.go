package api

import (
	"context"
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
)

type countingRGFetcher struct {
	calls []string
}

func (f *countingRGFetcher) GetReleaseGroups(_ context.Context, id string) ([]provider.ReleaseGroupInfo, error) {
	f.calls = append(f.calls, id)
	return []provider.ReleaseGroupInfo{{Title: "Album"}}, nil
}

// One MBID spelled two ways (case, surrounding whitespace) must cost ONE fetch
// (#2868), and the provider must be asked with the RAW spelling of the first.
func TestReleaseGroupCache_TitlesKeysOnNormalizedMBID(t *testing.T) {
	f := &countingRGFetcher{}
	c := &releaseGroupCache{fetcher: f, entries: make(map[string]releaseGroupEntry)}
	const raw = " 5B11F4CE-A62D-471E-81FC-A69A8278C7DA "
	const alt = "5b11f4ce-a62d-471e-81fc-a69a8278c7da"

	if _, ok := c.titles(context.Background(), raw); !ok {
		t.Fatal("first lookup should be known")
	}
	if !c.holds(alt) {
		t.Error("holds must see the other spelling as already cached")
	}
	titles, ok := c.titles(context.Background(), alt)
	if !ok || len(titles) != 1 {
		t.Fatalf("second spelling = %v, %v; want the cached titles", titles, ok)
	}
	if len(f.calls) != 1 {
		t.Fatalf("fetch calls = %d (%q), want 1", len(f.calls), f.calls)
	}
	if f.calls[0] != raw {
		t.Errorf("provider asked with %q, want the raw %q", f.calls[0], raw)
	}
}
