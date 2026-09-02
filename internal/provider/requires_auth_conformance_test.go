package provider_test

import (
	"io"
	"log/slog"
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/provider/audiodb"
	"github.com/sydlexius/stillwater/internal/provider/deezer"
	"github.com/sydlexius/stillwater/internal/provider/discogs"
	"github.com/sydlexius/stillwater/internal/provider/fanarttv"
	"github.com/sydlexius/stillwater/internal/provider/genius"
	"github.com/sydlexius/stillwater/internal/provider/lastfm"
	"github.com/sydlexius/stillwater/internal/provider/musicbrainz"
	"github.com/sydlexius/stillwater/internal/provider/spotify"
	"github.com/sydlexius/stillwater/internal/provider/wikidata"
	"github.com/sydlexius/stillwater/internal/provider/wikipedia"
)

// TestAdapterRequiresAuthMatchesProviderRequiresKey is the #2897 guard for the
// RequiresAuth axis: it constructs every real adapter and asserts its
// RequiresAuth() method agrees with provider.ProviderRequiresKey(), the
// single declaration internal/scraper's ProviderCapabilities() reads (see
// providerRequiresAuth in internal/scraper/config.go). Both the scraper-side
// table and this test are pinned to provider.ProviderRequiresKey rather than
// each restating the adapter facts by hand, so a change to any adapter's
// RequiresAuth() has exactly one place left to also change -- and this test
// fails, naming the provider, if that place is missed.
//
// None of the constructed adapters is used to make a network call: only
// RequiresAuth() is invoked, which every adapter implements as a literal
// return with no field access, so a nil settings/logger dependency is safe
// here.
func TestAdapterRequiresAuthMatchesProviderRequiresKey(t *testing.T) {
	silent := slog.New(slog.NewTextHandler(io.Discard, nil))
	limiter := provider.NewRateLimiterMap()

	adapters := map[provider.ProviderName]provider.Provider{
		provider.NameMusicBrainz: musicbrainz.New(limiter, silent),
		provider.NameWikipedia:   wikipedia.New(limiter, nil, silent),
		provider.NameFanartTV:    fanarttv.New(limiter, nil, silent),
		provider.NameAudioDB:     audiodb.New(limiter, nil, silent),
		provider.NameDiscogs:     discogs.New(limiter, nil, silent),
		provider.NameLastFM:      lastfm.New(limiter, nil, silent),
		provider.NameWikidata:    wikidata.New(limiter, silent),
		provider.NameDeezer:      deezer.New(limiter, silent),
		provider.NameGenius:      genius.New(limiter, nil, silent),
		provider.NameSpotify:     spotify.New(limiter, nil, silent),
	}

	all := provider.AllProviderNames()
	if len(all) == 0 {
		t.Fatal("AllProviderNames() is empty -- every assertion below would pass vacuously")
	}
	if len(adapters) != len(all) {
		t.Fatalf("this test constructs %d adapters but AllProviderNames() has %d; a provider would go unpinned", len(adapters), len(all))
	}

	var checked int
	for _, name := range all {
		adapter, ok := adapters[name]
		if !ok {
			t.Errorf("%s has no constructed adapter in this test", name)
			continue
		}
		checked++
		got := adapter.RequiresAuth()
		want := provider.ProviderRequiresKey(name)
		if got != want {
			t.Errorf("%s: adapter.RequiresAuth() = %v, but provider.ProviderRequiresKey(%s) = %v -- these must agree, or GET /api/v1/scraper/providers reports a stale requires_auth value",
				name, got, name, want)
		}
	}
	if checked != len(all) {
		t.Fatalf("checked %d of %d providers -- the rest passed vacuously", checked, len(all))
	}
}
