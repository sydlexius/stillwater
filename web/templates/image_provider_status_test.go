package templates

import (
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/i18n"
	"github.com/sydlexius/stillwater/internal/provider"
)

// #2457. A thin or empty image-search grid used to look identical whether every
// provider had been queried or most had been silently skipped. These tests pin
// the rendered difference, on BOTH results templates the search UI can land on.

func renderImageResults(t *testing.T, images []provider.ImageResult, statuses []provider.ProviderImageStatus) string {
	t.Helper()
	var sb strings.Builder
	if err := ImageSearchResults("a-1", images, "", false, statuses).Render(testCtx(t), &sb); err != nil {
		t.Fatalf("render ImageSearchResults: %v", err)
	}
	return sb.String()
}

func renderFanartResults(t *testing.T, images []provider.ImageResult, statuses []provider.ProviderImageStatus) string {
	t.Helper()
	var sb strings.Builder
	if err := FanartSearchResults("a-1", images, "", statuses).Render(testCtx(t), &sb); err != nil {
		t.Fatalf("render FanartSearchResults: %v", err)
	}
	return sb.String()
}

func skippedStatuses() []provider.ProviderImageStatus {
	return []provider.ProviderImageStatus{
		{Provider: provider.NameAudioDB, Outcome: provider.ImageOutcomeQueried},
		{Provider: provider.NameDiscogs, Outcome: provider.ImageOutcomeSkipped, Reason: provider.SkipReasonNoProviderID},
		{Provider: provider.NameSpotify, Outcome: provider.ImageOutcomeSkipped, Reason: provider.SkipReasonNoProviderID},
	}
}

// TestImageResultsBannerNamesSkippedProviders proves the results grid warns that
// it is incomplete, and names the providers that were never searched.
func TestImageResultsBannerNamesSkippedProviders(t *testing.T) {
	images := []provider.ImageResult{{Type: provider.ImageThumb, URL: "http://example.com/t.jpg", Source: "audiodb"}}

	for _, tc := range []struct {
		name   string
		render func(*testing.T, []provider.ImageResult, []provider.ProviderImageStatus) string
	}{
		{"ImageSearchResults", renderImageResults},
		{"FanartSearchResults", renderFanartResults},
	} {
		t.Run(tc.name, func(t *testing.T) {
			html := tc.render(t, images, skippedStatuses())
			if !strings.Contains(html, "data-sw-providers-skipped") {
				t.Fatalf("no skipped-provider banner rendered; html: %s", html)
			}
			for _, want := range []string{provider.NameDiscogs.DisplayName(), provider.NameSpotify.DisplayName()} {
				if !strings.Contains(html, want) {
					t.Errorf("banner does not name %q; html: %s", want, html)
				}
			}
			// The provider that WAS queried must not be named as skipped.
			if strings.Contains(html, "Deezer") {
				t.Errorf("banner names a provider that was not skipped; html: %s", html)
			}
		})
	}
}

// TestImageResultsNoBannerWhenAllQueried is the positive control: a fully
// successful search must not cry wolf.
func TestImageResultsNoBannerWhenAllQueried(t *testing.T) {
	statuses := []provider.ProviderImageStatus{
		{Provider: provider.NameAudioDB, Outcome: provider.ImageOutcomeQueried},
		{Provider: provider.NameDiscogs, Outcome: provider.ImageOutcomeQueried},
	}
	html := renderImageResults(t, nil, statuses)
	if strings.Contains(html, "data-sw-providers-skipped") || strings.Contains(html, "data-sw-provider-errored") {
		t.Errorf("banner rendered for a fully successful search; html: %s", html)
	}
	// An empty grid after a real search is still the ordinary empty state.
	if strings.Contains(html, enText(t, "image.no_providers_searched")) {
		t.Errorf("all-skipped empty state shown though every provider was queried; html: %s", html)
	}
}

// TestImageResultsAllSkippedEmptyState proves the acceptance criterion: a search
// where nothing could be queried does NOT render as "found nothing".
func TestImageResultsAllSkippedEmptyState(t *testing.T) {
	statuses := []provider.ProviderImageStatus{
		{Provider: provider.NameDiscogs, Outcome: provider.ImageOutcomeSkipped, Reason: provider.SkipReasonNoProviderID},
		{Provider: provider.NameSpotify, Outcome: provider.ImageOutcomeSkipped, Reason: provider.SkipReasonNoProviderID},
	}
	html := renderImageResults(t, nil, statuses)
	if !strings.Contains(html, enText(t, "image.no_providers_searched")) {
		t.Errorf("all-skipped search rendered without its distinct empty state; html: %s", html)
	}
	if strings.Contains(html, enText(t, "image.no_images_from_providers")) {
		t.Errorf("all-skipped search still claims 'no images found from providers'; html: %s", html)
	}
}

// TestImageResultsErrorBannerIsScrubbed proves a provider failure is surfaced
// and that the surfaced text is the scrubbed one the orchestrator produced. The
// template must never render a raw provider error: request URLs embed API keys.
func TestImageResultsErrorBannerIsScrubbed(t *testing.T) {
	statuses := []provider.ProviderImageStatus{
		{
			Provider: provider.NameFanartTV,
			Outcome:  provider.ImageOutcomeErrored,
			Reason:   provider.ScrubError(errFanartWithKey),
		},
	}
	html := renderImageResults(t, nil, statuses)
	if !strings.Contains(html, "data-sw-provider-errored") {
		t.Fatalf("no error banner rendered for a failed provider; html: %s", html)
	}
	if !strings.Contains(html, provider.NameFanartTV.DisplayName()) {
		t.Errorf("error banner does not name the failed provider; html: %s", html)
	}
	if strings.Contains(html, "s3cr3t") {
		t.Errorf("error banner leaks the API key; html: %s", html)
	}
}

// errFanartWithKey is the shape of a real Fanart.tv transport failure: a
// *url.Error whose message carries the request URL, api_key and all.
var errFanartWithKey = errString(`Get "https://webservice.fanart.tv/v3/music/mbid?api_key=s3cr3t": connection refused`)

type errString string

func (e errString) Error() string { return string(e) }

// enText returns the English copy for key, so an assertion pins the rendered
// message rather than a duplicated literal that can drift from en.json.
func enText(t *testing.T, key string) string {
	t.Helper()
	v := i18n.TFromCtx(testCtx(t)).T(key)
	if v == key {
		t.Fatalf("i18n key %q is missing from en.json", key)
	}
	return v
}

// TestAllProvidersSkippedEmptyStatuses pins the len==0 guard in
// allProvidersSkipped. Vacuously "every provider was skipped" is true of an
// empty list, but no statuses means NO INFORMATION, not "nothing was searched":
// a caller that renders results without threading statuses would otherwise get
// the "no provider could be searched" empty state on a search that queried every
// provider and simply found little. Both current nil-statuses call sites pass
// non-empty images, so they never reach the branch -- which is exactly why
// deleting the guard left the whole suite green. This constructs the state they
// do not.
func TestAllProvidersSkippedEmptyStatuses(t *testing.T) {
	if allProvidersSkipped(nil) {
		t.Error("allProvidersSkipped(nil) = true, want false: no statuses is no information, not all-skipped")
	}
	if allProvidersSkipped([]provider.ProviderImageStatus{}) {
		t.Error("allProvidersSkipped(empty) = true, want false: no statuses is no information, not all-skipped")
	}
}

// TestImageSearchResultsNilStatusesNoEmptyState is the render half of the guard
// above: with no statuses and no images, the template must fall through to the
// ordinary no-results state, never claim the providers went unsearched.
func TestImageSearchResultsNilStatusesNoEmptyState(t *testing.T) {
	unsearched := enText(t, "image.no_providers_searched")

	html := renderImageResults(t, nil, nil)
	if strings.Contains(html, unsearched) {
		t.Errorf("ImageSearchResults with nil statuses rendered the no-providers-searched empty state; html: %s", html)
	}

	fanart := renderFanartResults(t, nil, nil)
	if strings.Contains(fanart, unsearched) {
		t.Errorf("FanartSearchResults with nil statuses rendered the no-providers-searched empty state; html: %s", fanart)
	}
}
