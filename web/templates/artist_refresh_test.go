package templates

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
)

// TestDisambiguationResults_RendersProviderUnreachableBanner pins the
// banner-on-failure contract added for issue #1663. When SearchForLinking
// reports a provider error via ProviderSearchStatus, the handler forwards
// the failed-provider display names through DisambiguationResultsData and the
// template renders a visible warning above the candidate list so the user
// can tell "providers unreachable" apart from "no matches found".
func TestDisambiguationResults_RendersProviderUnreachableBanner(t *testing.T) {
	t.Parallel()

	data := DisambiguationResultsData{
		ArtistID: "artist-1",
		Candidates: []DisambiguationCandidate{{
			Result: provider.ArtistSearchResult{
				Name:          "Hiromi",
				MusicBrainzID: "mbid-abc",
				Source:        "musicbrainz",
				Score:         95,
			},
		}},
		FailedProviders: []string{"Discogs"},
	}

	var buf bytes.Buffer
	if err := DisambiguationResults(data).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()

	// Stable hook for any future test or CSS audit that needs to find the
	// banner without relying on translated copy.
	if !strings.Contains(body, `data-testid="providers-unreachable-banner"`) {
		t.Errorf("expected providers-unreachable-banner data-testid in rendered output, body:\n%s", body)
	}

	// Title and body copy must come through the i18n helpers; the en.json
	// strings live at artist.disambiguation.providers_unreachable.*.
	if !strings.Contains(body, "Some providers could not be reached") {
		t.Errorf("expected banner title text in body:\n%s", body)
	}
	if !strings.Contains(body, "Discogs") {
		t.Errorf("expected failed provider name (Discogs) interpolated into banner, body:\n%s", body)
	}

	// The candidate row must still render alongside the banner -- partial
	// failures should not hide the providers that did return results.
	if !strings.Contains(body, "Hiromi") {
		t.Errorf("expected candidate name to render alongside banner, body:\n%s", body)
	}
}

// TestDisambiguationResults_NoBannerWhenAllProvidersSucceed pins the
// negative case: with zero failed providers the banner must not render. This
// keeps the legitimate "no matches found" empty state distinct from the
// new "providers unreachable" warning -- conflating them would defeat the
// purpose of issue #1663.
func TestDisambiguationResults_NoBannerWhenAllProvidersSucceed(t *testing.T) {
	t.Parallel()

	data := DisambiguationResultsData{
		ArtistID:        "artist-1",
		Candidates:      nil, // genuine empty-result state
		FailedProviders: nil,
	}

	var buf bytes.Buffer
	if err := DisambiguationResults(data).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()

	if strings.Contains(body, "providers-unreachable-banner") {
		t.Errorf("banner should not render when no providers failed; body:\n%s", body)
	}
	// The legitimate "no results" message should appear instead.
	if !strings.Contains(body, "No results found") {
		t.Errorf("expected the empty-state no-results message; body:\n%s", body)
	}
}

// TestDisambiguationResults_BannerWhenProvidersFailAndNoCandidates pins the
// exact ambiguous state issue #1663 fixes: providers errored AND zero
// candidates came back. The banner must render so the operator sees a
// reason for the empty list; the legitimate "no results found" copy still
// renders alongside it (the template does not suppress the empty-state when
// the banner is present, and that combined presentation is intentional --
// the banner explains *why* the list might be empty without lying that the
// search itself succeeded).
func TestDisambiguationResults_BannerWhenProvidersFailAndNoCandidates(t *testing.T) {
	t.Parallel()

	data := DisambiguationResultsData{
		ArtistID:        "artist-1",
		Candidates:      nil,
		FailedProviders: []string{"Discogs"},
	}

	var buf bytes.Buffer
	if err := DisambiguationResults(data).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()

	if !strings.Contains(body, `data-testid="providers-unreachable-banner"`) {
		t.Errorf("expected providers-unreachable banner when providers fail with zero candidates, body:\n%s", body)
	}
	if !strings.Contains(body, "Discogs") {
		t.Errorf("expected failed provider name (Discogs) interpolated into banner, body:\n%s", body)
	}
	if !strings.Contains(body, "No results found") {
		t.Errorf("expected no-results text to remain visible alongside the failure banner, body:\n%s", body)
	}
}
