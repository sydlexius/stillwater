package provider

import (
	"context"
	"errors"
	"fmt"
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
		t.Run(fmt.Sprint(order), func(t *testing.T) {
			o := linkingSortOrch(t, byProvider)
			results, statuses, err := o.SearchForLinking(context.Background(), "x", order)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if len(results) != len(want) {
				t.Fatalf("got %d results, want %d", len(results), len(want))
			}
			for i, w := range want {
				if results[i].Name != w {
					t.Errorf("results[%d] = %q, want %q", i, results[i].Name, w)
				}
			}
			// perStatus must stay in the caller's order for the failed-provider banner.
			if len(statuses) != 2 {
				t.Fatalf("got %d statuses, want 2", len(statuses))
			}
			if statuses[0].Provider != order[0] || statuses[1].Provider != order[1] {
				t.Errorf("statuses reordered: %v, %v", statuses[0].Provider, statuses[1].Provider)
			}
		})
	}
}

// TestSearchForLinking_MusicBrainzTierEdgeCases covers an errored provider,
// ties inside a tier (stability), and a provider that returns nothing.
func TestSearchForLinking_MusicBrainzTierEdgeCases(t *testing.T) {
	t.Parallel()

	prov := func(name ProviderName, res ...ArtistSearchResult) Provider {
		return &mockProvider{name: name, searchFn: func(context.Context, string) ([]ArtistSearchResult, error) { return res, nil }}
	}
	r := func(name, src string, score int) ArtistSearchResult {
		return ArtistSearchResult{Name: name, Source: src, Score: score}
	}
	// 30 tied results, alternating tiers so the sort has to move them: sort.Slice
	// is unstable above 12 elements (and a pre-sorted fixture is a no-op for it).
	var tied []ArtistSearchResult
	var mbWant, dcWant []string
	for i := range 30 {
		n, src := fmt.Sprintf("mb-%02d", i), "musicbrainz"
		if i%2 == 1 {
			n, src = fmt.Sprintf("dc-%02d", i), "discogs"
			dcWant = append(dcWant, n)
		} else {
			mbWant = append(mbWant, n)
		}
		tied = append(tied, r(n, src, 40))
	}
	tiedWant := append(mbWant, dcWant...)
	failing := &mockProvider{name: NameDiscogs, searchFn: func(context.Context, string) ([]ArtistSearchResult, error) {
		return nil, errors.New("boom")
	}}

	cases := []struct {
		name        string
		provs       []Provider
		want        []string
		erroredName ProviderName
	}{
		{"errored provider: survivor still sorted, status errored",
			[]Provider{prov(NameMusicBrainz, r("mb-low", "musicbrainz", 10), r("mb-high", "musicbrainz", 50)), failing},
			[]string{"mb-high", "mb-low"}, NameDiscogs},
		{"ties keep input order", []Provider{prov(NameMusicBrainz, tied...)}, tiedWant, ""},
		{"empty provider changes nothing",
			[]Provider{prov(NameMusicBrainz), prov(NameDiscogs, r("dc-a", "discogs", 70), r("dc-b", "discogs", 80))},
			[]string{"dc-b", "dc-a"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			names := make([]ProviderName, len(tc.provs))
			for i, p := range tc.provs {
				names[i] = p.Name()
			}
			results, statuses, err := newTimeoutTestOrchestrator(t, nil, tc.provs...).
				SearchForLinking(context.Background(), "x", names)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if len(results) != len(tc.want) {
				t.Fatalf("got %d results, want %d", len(results), len(tc.want))
			}
			for i, w := range tc.want {
				if results[i].Name != w {
					t.Errorf("results[%d] = %q, want %q", i, results[i].Name, w)
				}
			}
			for _, st := range statuses {
				if st.Errored != (st.Provider == tc.erroredName) {
					t.Errorf("status %v Errored = %v", st.Provider, st.Errored)
				}
			}
		})
	}
}
