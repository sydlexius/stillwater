package rule

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// stubProviderAvailability is a ProviderAvailability whose result (and optional
// error) are fixed for the test. calls counts invocations so a test can assert
// the checker made exactly one availability lookup.
type stubProviderAvailability struct {
	available map[provider.ProviderName]bool
	err       error
	calls     int
}

func (s *stubProviderAvailability) AvailableProviderNames(_ context.Context) (map[provider.ProviderName]bool, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.available, nil
}

// allThreeAvailable reports every in-scope provider as configured.
func allThreeAvailable() map[provider.ProviderName]bool {
	return map[provider.ProviderName]bool{
		provider.NameDiscogs: true,
		provider.NameDeezer:  true,
		provider.NameSpotify: true,
	}
}

// newProviderIDTestEngine builds a minimal Engine carrying only the fields the
// provider_id_missing checker reads.
func newProviderIDTestEngine(pa ProviderAvailability) *Engine {
	return &Engine{
		logger:               testLogger(),
		providerAvailability: pa,
	}
}

// TestProviderIDChecker_MissingAllFlagsAllSorted flags an artist missing every
// in-scope provider ID and lists them in a deterministic (sorted) order.
func TestProviderIDChecker_MissingAllFlagsAllSorted(t *testing.T) {
	e := newProviderIDTestEngine(&stubProviderAvailability{available: allThreeAvailable()})
	checker := e.makeProviderIDMissingChecker()

	a := &artist.Artist{Name: "Test Artist"} // no provider IDs at all
	v := checker(context.Background(), a, RuleConfig{})
	if v == nil {
		t.Fatal("checker did not flag an artist missing every provider ID")
	}
	if v.RuleID != RuleProviderIDMissing {
		t.Errorf("RuleID = %q, want %q", v.RuleID, RuleProviderIDMissing)
	}
	if !v.Fixable {
		t.Error("provider_id_missing violation should be fixable")
	}
	// Sorted order: deezer, discogs, spotify.
	for _, want := range []string{"deezer", "discogs", "spotify"} {
		if !strings.Contains(v.Message, want) {
			t.Errorf("message %q does not mention missing provider %q", v.Message, want)
		}
	}
	if di, si := strings.Index(v.Message, "deezer"), strings.Index(v.Message, "spotify"); di > si {
		t.Errorf("message not in sorted order (deezer must precede spotify): %q", v.Message)
	}
}

// TestProviderIDChecker_AllPresentPasses does not flag an artist that carries
// every required provider ID.
func TestProviderIDChecker_AllPresentPasses(t *testing.T) {
	e := newProviderIDTestEngine(&stubProviderAvailability{available: allThreeAvailable()})
	checker := e.makeProviderIDMissingChecker()

	a := &artist.Artist{
		Name:      "Test Artist",
		DiscogsID: "24941",
		DeezerID:  "3106",
		SpotifyID: "7dGJo4pcD2V6oG8kP0tJRR",
	}
	if v := checker(context.Background(), a, RuleConfig{}); v != nil {
		t.Errorf("checker flagged an artist with all provider IDs present: %+v", v)
	}
}

// TestProviderIDChecker_OnlyMissingListed flags only the providers actually
// missing, leaving present ones out of the message.
func TestProviderIDChecker_OnlyMissingListed(t *testing.T) {
	e := newProviderIDTestEngine(&stubProviderAvailability{available: allThreeAvailable()})
	checker := e.makeProviderIDMissingChecker()

	a := &artist.Artist{Name: "Test Artist", DiscogsID: "24941"} // deezer + spotify missing
	v := checker(context.Background(), a, RuleConfig{})
	if v == nil {
		t.Fatal("checker did not flag an artist missing deezer and spotify IDs")
	}
	if strings.Contains(v.Message, "discogs") {
		t.Errorf("message should not mention discogs (present): %q", v.Message)
	}
	for _, want := range []string{"deezer", "spotify"} {
		if !strings.Contains(v.Message, want) {
			t.Errorf("message %q missing provider %q", v.Message, want)
		}
	}
}

// TestProviderIDChecker_DynamicDefaultSkipsUnconfigured does not require a
// provider ID for a provider that is not configured, so an artist missing only
// the unconfigured provider's ID passes.
func TestProviderIDChecker_DynamicDefaultSkipsUnconfigured(t *testing.T) {
	// Only Discogs is configured; Deezer and Spotify are not.
	e := newProviderIDTestEngine(&stubProviderAvailability{
		available: map[provider.ProviderName]bool{provider.NameDiscogs: true},
	})
	checker := e.makeProviderIDMissingChecker()

	// Artist has the only configured provider's ID; the two unconfigured
	// providers are blank but must not be required.
	a := &artist.Artist{Name: "Test Artist", DiscogsID: "24941"}
	if v := checker(context.Background(), a, RuleConfig{}); v != nil {
		t.Errorf("checker required an unconfigured provider's ID: %+v", v)
	}

	// Same availability, but now the configured provider's ID is missing -> flag.
	b := &artist.Artist{Name: "Test Artist", SpotifyID: "x"} // discogs missing, spotify set but unconfigured
	v := checker(context.Background(), b, RuleConfig{})
	if v == nil {
		t.Fatal("checker did not flag a missing configured (Discogs) ID")
	}
	if !strings.Contains(v.Message, "discogs") || strings.Contains(v.Message, "deezer") || strings.Contains(v.Message, "spotify") {
		t.Errorf("message should require only the configured Discogs ID: %q", v.Message)
	}
}

// TestProviderIDChecker_OverrideNarrows honors an operator override that
// narrows the required set to a subset of the configured providers.
func TestProviderIDChecker_OverrideNarrows(t *testing.T) {
	e := newProviderIDTestEngine(&stubProviderAvailability{available: allThreeAvailable()})
	checker := e.makeProviderIDMissingChecker()

	// All three configured, but the operator requires only Spotify. An artist
	// missing Discogs and Deezer (but having Spotify) must pass.
	a := &artist.Artist{Name: "Test Artist", SpotifyID: "7dGJo4pcD2V6oG8kP0tJRR"}
	if v := checker(context.Background(), a, RuleConfig{RequiredProviderIDs: "spotify"}); v != nil {
		t.Errorf("override to spotify-only still flagged non-spotify gaps: %+v", v)
	}

	// The same artist missing Spotify must be flagged under the spotify-only override.
	b := &artist.Artist{Name: "Test Artist", DiscogsID: "24941", DeezerID: "3106"}
	v := checker(context.Background(), b, RuleConfig{RequiredProviderIDs: "spotify"})
	if v == nil {
		t.Fatal("spotify-only override did not flag a missing Spotify ID")
	}
	if strings.Contains(v.Message, "discogs") || strings.Contains(v.Message, "deezer") {
		t.Errorf("override message leaked non-required providers: %q", v.Message)
	}
}

// TestProviderIDChecker_AvailabilityErrorNoOps degrades to a no-op (no
// violation) when the availability lookup errors, rather than guessing.
func TestProviderIDChecker_AvailabilityErrorNoOps(t *testing.T) {
	e := newProviderIDTestEngine(&stubProviderAvailability{err: errors.New("db down")})
	checker := e.makeProviderIDMissingChecker()

	a := &artist.Artist{Name: "Test Artist"} // missing all, but availability errors
	if v := checker(context.Background(), a, RuleConfig{}); v != nil {
		t.Errorf("checker flagged despite an availability error (should no-op): %+v", v)
	}
}

// TestProviderIDChecker_NilDependencyNoOps degrades to a no-op when the
// provider-availability dependency is unwired.
func TestProviderIDChecker_NilDependencyNoOps(t *testing.T) {
	e := newProviderIDTestEngine(nil)
	checker := e.makeProviderIDMissingChecker()

	a := &artist.Artist{Name: "Test Artist"}
	if v := checker(context.Background(), a, RuleConfig{}); v != nil {
		t.Errorf("checker flagged with no availability dependency (should no-op): %+v", v)
	}
}

// TestEngine_SetProviderAvailability_WiresChecker proves SetProviderAvailability
// actually attaches its argument to the field the provider_id_missing checker
// reads: a checker built after the call sees the availability source (flags a
// missing ID), and the same call with nil detaches it (checker no-ops). A
// setter that silently dropped its argument would leave both assertions false.
func TestEngine_SetProviderAvailability_WiresChecker(t *testing.T) {
	e := &Engine{logger: testLogger()}

	e.SetProviderAvailability(&stubProviderAvailability{available: allThreeAvailable()})
	checker := e.makeProviderIDMissingChecker()
	a := &artist.Artist{Name: "Test Artist"}
	if v := checker(context.Background(), a, RuleConfig{}); v == nil {
		t.Fatal("SetProviderAvailability did not wire the availability source: checker no-opped")
	}

	e.SetProviderAvailability(nil)
	checker = e.makeProviderIDMissingChecker()
	if v := checker(context.Background(), a, RuleConfig{}); v != nil {
		t.Errorf("SetProviderAvailability(nil) did not detach the source: checker still flagged %+v", v)
	}
}

// TestEngine_ReleaseGroupFetcher_GetterReturnsWhatWasSet proves the getter
// returns the exact fetcher passed to SetReleaseGroupFetcher (identity, not
// just non-nil), and nil before anything is set.
func TestEngine_ReleaseGroupFetcher_GetterReturnsWhatWasSet(t *testing.T) {
	e := &Engine{logger: testLogger()}
	if got := e.ReleaseGroupFetcher(); got != nil {
		t.Errorf("ReleaseGroupFetcher() on a fresh engine = %v, want nil", got)
	}

	rg := &stubReleaseGroupFetcher{}
	e.SetReleaseGroupFetcher(rg)
	if got := e.ReleaseGroupFetcher(); got != rg {
		t.Errorf("ReleaseGroupFetcher() = %v, want the exact fetcher set (%v)", got, rg)
	}
}
