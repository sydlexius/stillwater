package main

import (
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
)

func TestRenderMatrix_HappyPath(t *testing.T) {
	got, err := renderMatrix(provider.AllProviderNames(), provider.ProviderCapabilities())
	if err != nil {
		t.Fatalf("renderMatrix: %v", err)
	}

	// Header is fixed.
	wantHeader := "| Provider | Tier | Sign-up | Rate limit | Mirror | Metadata fields | Image types |\n|---|---|---|---|---|---|---|\n"
	if !strings.HasPrefix(got, wantHeader) {
		t.Fatalf("missing or wrong header.\ngot:\n%s", got)
	}

	// Every in-use provider gets a row keyed by its DisplayName.
	for _, name := range provider.AllProviderNames() {
		if name == provider.NameAllMusic {
			continue
		}
		needle := "| " + name.DisplayName() + " |"
		if !strings.Contains(got, needle) {
			t.Errorf("expected row for %s; output was:\n%s", name.DisplayName(), got)
		}
	}

	// AllMusic must never appear (skip-entirely policy from the spec).
	if strings.Contains(got, "AllMusic") {
		t.Errorf("AllMusic should be excluded from generated matrix; got:\n%s", got)
	}

	// Spot-check a few representative renderings.
	if !strings.Contains(got, "MusicBrainz | Free | Not required | 1/sec | Yes |") {
		t.Errorf("MusicBrainz row not rendered as expected; got:\n%s", got)
	}
	if !strings.Contains(got, "Fanart.tv | Free key |") || !strings.Contains(got, "Image only") {
		t.Errorf("Fanart.tv row missing expected fragments; got:\n%s", got)
	}
	if !strings.Contains(got, "TheAudioDB | Freemium |") || !strings.Contains(got, "30/min") {
		t.Errorf("TheAudioDB row should render Freemium with 30/min rate limit; got:\n%s", got)
	}
	if !strings.Contains(got, "Discogs |") || !strings.Contains(got, "1/sec, 1000/day") {
		t.Errorf("Discogs row should render combined rate limits; got:\n%s", got)
	}
}

func TestReplaceBetweenMarkers(t *testing.T) {
	src := []byte("prefix\n" + beginMarker + "\nstale body\n" + endMarker + "\nsuffix\n")
	out, err := replaceBetweenMarkers(src, beginMarker, endMarker, "fresh body")
	if err != nil {
		t.Fatal(err)
	}
	want := "prefix\n" + beginMarker + "\nfresh body\n" + endMarker + "\nsuffix\n"
	if string(out) != want {
		t.Fatalf("unexpected output\nwant:\n%s\n\ngot:\n%s", want, string(out))
	}
}

func TestReplaceBetweenMarkers_MissingBegin(t *testing.T) {
	_, err := replaceBetweenMarkers([]byte("no markers here"), beginMarker, endMarker, "body")
	if err == nil {
		t.Fatal("expected error when begin marker is missing")
	}
}

func TestReplaceBetweenMarkers_MissingEnd(t *testing.T) {
	src := []byte("prefix " + beginMarker + " no end")
	_, err := replaceBetweenMarkers(src, beginMarker, endMarker, "body")
	if err == nil {
		t.Fatal("expected error when end marker is missing")
	}
}

func TestRenderTier(t *testing.T) {
	cases := map[provider.AccessTier]string{
		provider.TierFree:     "Free",
		provider.TierFreeKey:  "Free key",
		provider.TierFreemium: "Freemium",
		provider.TierPaid:     "Paid",
	}
	for tier, want := range cases {
		if got := renderTier(tier); got != want {
			t.Errorf("renderTier(%q) = %q, want %q", tier, got, want)
		}
	}
}

func TestFormatPerSecond(t *testing.T) {
	cases := []struct {
		rps  float64
		want string
	}{
		{1, "1/sec"},
		{3, "3/sec"},
		{5, "5/sec"},
		{0.5, "30/min"},
		{2.5, "2.5/sec"},
	}
	for _, c := range cases {
		if got := formatPerSecond(c.rps); got != c.want {
			t.Errorf("formatPerSecond(%v) = %q, want %q", c.rps, got, c.want)
		}
	}
}

func TestRenderRateLimit_Nil(t *testing.T) {
	if got := renderRateLimit(nil); got != "Unknown" {
		t.Errorf("renderRateLimit(nil) = %q, want %q", got, "Unknown")
	}
}

func TestRenderSignup(t *testing.T) {
	if got := renderSignup(""); got != "Not required" {
		t.Errorf("empty helpURL should render as Not required; got %q", got)
	}
	if got := renderSignup("https://example.com/key"); got != "[Sign up](https://example.com/key)" {
		t.Errorf("unexpected sign-up rendering: %q", got)
	}
}

func TestRenderMatrix_MissingCapabilityIsError(t *testing.T) {
	// Drift between AllProviderNames() and ProviderCapabilities() must fail
	// generation loudly rather than silently dropping the provider's row.
	names := []provider.ProviderName{provider.NameMusicBrainz, "ghost-provider"}
	caps := map[provider.ProviderName]provider.ProviderCapability{
		provider.NameMusicBrainz: provider.ProviderCapabilities()[provider.NameMusicBrainz],
	}
	_, err := renderMatrix(names, caps)
	if err == nil {
		t.Fatal("expected error when a name has no capability declaration")
	}
	if !strings.Contains(err.Error(), "ghost-provider") {
		t.Errorf("error should name the missing provider; got %v", err)
	}
}
