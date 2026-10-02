package provider

import (
	"context"
	"testing"
)

// linkingSortOrch registers one fake per provider with canned results.
func linkingSortOrch(t *testing.T, byProvider map[ProviderName][]ArtistSearchResult) *Orchestrator {
	t.Helper()
	var provs []Provider
	for name, res := range byProvider {
		provs = append(provs, &mockProvider{
			name: name,
			searchFn: func(_ context.Context, _ string) ([]ArtistSearchResult, error) {
				return res, nil
			},
		})
	}
	return newTimeoutTestOrchestrator(t, nil, provs...)
}

// TestSearchForLinking_MusicBrainzTierFirst pins the #2811 contract: MB above
// every other provider regardless of score, score descending inside each tier,
// and the same tiering whichever order the caller lists the providers.
func TestSearchForLinking_MusicBrainzTierFirst(t *testing.T) {
	t.Parallel()

	byProvider := map[ProviderName][]ArtistSearchResult{
		NameMusicBrainz: {
			{Name: "mb-low", Source: "musicbrainz", Score: 20},
			{Name: "mb-high", Source: "musicbrainz", Score: 60},
		},
		NameDiscogs: {
			{Name: "dc-low", Source: "discogs", Score: 30},
			{Name: "dc-high", Source: "discogs", Score: 99},
		},
	}
	want := []string{"mb-high", "mb-low", "dc-high", "dc-low"}

	for _, order := range [][]ProviderName{
		{NameMusicBrainz, NameDiscogs},
		{NameDiscogs, NameMusicBrainz},
	} {
		o := linkingSortOrch(t, byProvider)
		results, statuses, err := o.SearchForLinking(context.Background(), "x", order)
		if err != nil {
			t.Fatalf("order %v: err = %v", order, err)
		}
		if len(results) != len(want) {
			t.Fatalf("order %v: got %d results, want %d", order, len(results), len(want))
		}
		for i, w := range want {
			if results[i].Name != w {
				t.Errorf("order %v: results[%d] = %q, want %q", order, i, results[i].Name, w)
			}
		}
		// perStatus must stay in the caller's order for the failed-provider banner.
		if statuses[0].Provider != order[0] || statuses[1].Provider != order[1] {
			t.Errorf("order %v: statuses reordered: %v, %v", order, statuses[0].Provider, statuses[1].Provider)
		}
	}
}
