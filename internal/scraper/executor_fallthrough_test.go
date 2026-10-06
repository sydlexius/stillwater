package scraper

import (
	"context"
	"fmt"
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
)

// Real-executor (ScrapeAll) tests replacing legacy-loop tests (#3292).
func scrapeOne(t *testing.T, chain []provider.ProviderName, field FieldName, cat FieldCategory, prio string, regs ...*mockProvider) *provider.FetchResult {
	t.Helper()
	registry, settings, svc, logger := setupExecutorTest(t)
	ctx := context.Background()
	for _, m := range regs {
		registry.Register(m)
	}
	if err := settings.SetPriority(ctx, prio, chain); err != nil {
		t.Fatalf("SetPriority: %v", err)
	}
	saveFieldsConfig(t, svc,
		[]FieldConfig{{Field: field, Primary: chain[0], Enabled: true, Category: cat}},
		[]FallbackChain{{Category: cat, Providers: chain}})
	result, err := NewExecutor(svc, registry, settings, logger, nil).ScrapeAll(ctx, "mbid-1", "X", ScopeGlobal, nil)
	if err != nil {
		t.Fatalf("ScrapeAll: %v", err)
	}
	return result
}

func metaProvider(name provider.ProviderName, meta *provider.ArtistMetadata, err error) *mockProvider {
	return &mockProvider{name: name, getArtFn: func(context.Context, string) (*provider.ArtistMetadata, error) { return meta, err }}
}

// TestScrapeAll_JunkBiographyFallsThroughToNextProvider pins that junk biography from the
// primary provider causes ScrapeAll to fall through to the next provider in the chain.
func TestScrapeAll_JunkBiographyFallsThroughToNextProvider(t *testing.T) {
	cases := []struct{ name, first, second, want string }{
		{"junk then real", "?", longBio, longBio},
		{"too short then real", "A rock band.", longBio, longBio},
		{"all junk leaves field empty", "?", "N/A", ""},
	}
	chain := []provider.ProviderName{provider.NameMusicBrainz, provider.NameAudioDB}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !provider.IsJunkBiography(tc.first) {
				t.Fatalf("precondition: %q must be classified junk", tc.first)
			}
			r := scrapeOne(t, chain, FieldBiography, CategoryMetadata, "biography",
				metaProvider(chain[0], &provider.ArtistMetadata{Name: "X", Biography: tc.first}, nil),
				metaProvider(chain[1], &provider.ArtistMetadata{Name: "X", Biography: tc.second}, nil))
			wantSrc := provider.ProviderName("")
			if tc.want != "" {
				wantSrc = chain[1]
			}
			if r.Metadata.Biography != tc.want || sourceFor(r, "biography") != wantSrc {
				t.Errorf("biography = %q from %q, want %q from %q", r.Metadata.Biography, sourceFor(r, "biography"), tc.want, wantSrc)
			}
		})
	}
}

// TestScrapeAll_WrongImageTypeDoesNotBlockLaterProvider pins that a primary provider
// returning the wrong image type does not block ScrapeAll from attempting the next provider.
func TestScrapeAll_WrongImageTypeDoesNotBlockLaterProvider(t *testing.T) {
	img := func(name provider.ProviderName, typ provider.ImageType) *mockProvider {
		m := metaProvider(name, &provider.ArtistMetadata{Name: "X"}, nil)
		m.getImgFn = func(context.Context, string) ([]provider.ImageResult, error) {
			return []provider.ImageResult{{URL: "http://example.test/" + string(typ) + ".jpg", Type: typ}}, nil
		}
		return m
	}
	chain := []provider.ProviderName{provider.NameMusicBrainz, provider.NameAudioDB}
	r := scrapeOne(t, chain, FieldThumb, CategoryImages, "thumb", img(chain[0], provider.ImageFanart), img(chain[1], provider.ImageThumb))
	if len(r.Images) != 1 || r.Images[0].Type != provider.ImageThumb || sourceFor(r, "thumb") != chain[1] {
		t.Errorf("want exactly the second provider's thumb, got images %v source %q", r.Images, sourceFor(r, "thumb"))
	}
}

// TestScrapeAll_MembersAttemptedBookkeeping pins that ScrapeAll correctly tracks attempted
// fields, the authoritative flag, and member count across various metadata states and errors.
func TestScrapeAll_MembersAttemptedBookkeeping(t *testing.T) {
	cases := []struct {
		name                    string
		meta                    *provider.ArtistMetadata
		err                     error
		wantAttempted, wantAuth bool
		wantMembers             int
	}{
		{"sparse empty is not attempted", &provider.ArtistMetadata{Name: "X"}, nil, false, false, 0},
		{"authoritative empty is attempted", &provider.ArtistMetadata{Name: "X", MembersAuthoritative: true}, nil, true, true, 0},
		{"real members are attempted", &provider.ArtistMetadata{Name: "X", Members: []provider.MemberInfo{{Name: "A"}, {Name: "B"}}}, nil, true, false, 2},
		{"provider error is not attempted", nil, fmt.Errorf("connection refused"), false, false, 0},
	}
	chain := []provider.ProviderName{provider.NameMusicBrainz}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := scrapeOne(t, chain, FieldMembers, CategoryMetadata, "members", metaProvider(chain[0], tc.meta, tc.err))
			attempted := false
			for _, f := range r.AttemptedFields {
				attempted = attempted || f == "members"
			}
			if attempted != tc.wantAttempted || r.MembersAuthoritative != tc.wantAuth || len(r.Metadata.Members) != tc.wantMembers {
				t.Errorf("attempted=%v auth=%v members=%d, want %v/%v/%d", attempted, r.MembersAuthoritative, len(r.Metadata.Members), tc.wantAttempted, tc.wantAuth, tc.wantMembers)
			}
		})
	}
}

// TestScrapeAll_MusicBrainzNameAuthorityWithoutWinningField pins the
// unconditional Name/SortName block in ScrapeAll: MusicBrainz's names apply
// even though MusicBrainz wins no field, and an errored MusicBrainz leaves the
// earlier provider's name alone.
func TestScrapeAll_MusicBrainzNameAuthorityWithoutWinningField(t *testing.T) {
	chain := []provider.ProviderName{provider.NameWikipedia, provider.NameMusicBrainz}
	cases := []struct {
		name         string
		mb           *provider.ArtistMetadata
		mbErr        error
		wantName     string
		wantSortName string
	}{
		{"MB names override without winning a field", &provider.ArtistMetadata{Name: "Promoted", SortName: "Promoted Sort"}, nil, "Promoted", "Promoted Sort"},
		{"errored MB keeps the earlier name", nil, fmt.Errorf("connection refused"), "Canonical", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Biography is won by wikipedia; formed is asked of musicbrainz
			// alone (so it is queried) but has no value, so it wins nothing.
			registry, settings, svc, logger := setupExecutorTest(t)
			ctx := context.Background()
			registry.Register(metaProvider(chain[0], &provider.ArtistMetadata{Name: "Canonical", Biography: longBio}, nil))
			registry.Register(metaProvider(chain[1], tc.mb, tc.mbErr))
			if err := settings.SetPriority(ctx, "biography", chain[:1]); err != nil {
				t.Fatalf("SetPriority: %v", err)
			}
			if err := settings.SetPriority(ctx, "formed", chain[1:]); err != nil {
				t.Fatalf("SetPriority: %v", err)
			}
			saveFieldsConfig(t, svc,
				[]FieldConfig{
					{Field: FieldBiography, Primary: chain[0], Enabled: true, Category: CategoryMetadata},
					{Field: FieldFormed, Primary: chain[1], Enabled: true, Category: CategoryMetadata},
				},
				[]FallbackChain{{Category: CategoryMetadata, Providers: chain}})
			r, err := NewExecutor(svc, registry, settings, logger, nil).ScrapeAll(ctx, "mbid-1", "X", ScopeGlobal, nil)
			if err != nil {
				t.Fatalf("ScrapeAll: %v", err)
			}
			if got := sourceFor(r, "biography"); got != provider.NameWikipedia {
				t.Fatalf("precondition: biography source = %q, want wikipedia", got)
			}
			for _, s := range r.Sources {
				if s.Provider == provider.NameMusicBrainz {
					t.Fatalf("precondition: musicbrainz must win no field, won %q", s.Field)
				}
			}
			if r.Metadata.Name != tc.wantName || r.Metadata.SortName != tc.wantSortName {
				t.Errorf("Name/SortName = %q/%q, want %q/%q", r.Metadata.Name, r.Metadata.SortName, tc.wantName, tc.wantSortName)
			}
		})
	}
}
