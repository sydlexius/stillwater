package templates

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// TestProviderIdentifyModal_RendersSearchForm pins the next/ identify modal
// body: a disambiguation search form pre-filled with the artist name that POSTs
// to the provider search endpoint, plus a field-suffixed results container the
// candidate list swaps into. hx-trigger="load, submit" auto-runs the first
// search when the modal opens.
func TestProviderIdentifyModal_RendersSearchForm(t *testing.T) {
	t.Parallel()

	data := ProviderIdentifyModalData{
		ArtistID:   "artist-1",
		Provider:   provider.NameDeezer,
		Field:      "deezer_id",
		ArtistName: "Radiohead",
		SearchURL:  "/api/v1/artists/artist-1/deezer/search",
	}

	var buf bytes.Buffer
	if err := ProviderIdentifyModal(data).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()

	for _, want := range []string{
		`hx-post="/api/v1/artists/artist-1/deezer/search"`,
		`hx-trigger="load, submit"`,
		`value="Radiohead"`,
		`id="provider-identify-results-deezer_id"`,
		"Deezer", // provider display name in the heading
	} {
		if !strings.Contains(body, want) {
			t.Errorf("identify modal missing %q; body:\n%s", want, body)
		}
	}
}

// TestDeezerCandidates_RendersAlbumConfidence pins that the candidate rows
// surface the album-comparison confidence (the same MatchCount/LocalCount pill
// the MusicBrainz disambiguation rows use) and link to the Deezer link
// endpoint targeting the shared modal body.
func TestDeezerCandidates_RendersAlbumConfidence(t *testing.T) {
	t.Parallel()

	data := DeezerCandidatesData{
		ArtistID: "artist-1",
		Candidates: []DeezerCandidate{{
			Result: provider.ArtistSearchResult{Name: "Radiohead", ProviderID: "4050205", Score: 100},
			AlbumComparison: &artist.AlbumComparison{
				MatchCount:   2,
				LocalCount:   2,
				MatchPercent: 100,
				Matches:      []artist.AlbumMatch{{RemoteName: "OK Computer"}},
			},
			Confidence: 1.0,
		}},
	}

	var buf bytes.Buffer
	if err := DeezerCandidates(data).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()

	for _, want := range []string{
		`hx-post="/api/v1/artists/artist-1/deezer/link"`,
		// The link button targets the Deezer ID ROW (not the modal body) and
		// replaces it in place, mirroring the proven "Use this" field-apply
		// pattern. This is the contract that keeps the modal's afterSwap
		// auto-open listener from re-firing on a successful link.
		`hx-target="#field-deezer_id-artist-1"`,
		`hx-swap="outerHTML"`,
		// The modal closes via the button's after-request hook, not an inline
		// script in the success render.
		`hx-on::after-request="hideFieldProviderModal()"`,
		"4050205",
		"OK Computer", // matched album name surfaced
		"Radiohead",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("candidate row missing %q; body:\n%s", want, body)
		}
	}

	// The link button must NOT target the shared modal body: swapping the
	// success fragment into #field-provider-modal-body is what re-triggered the
	// modal's afterSwap auto-open (stuck-open blank modal + removeChild
	// swapError). Guard against a regression to that target.
	if strings.Contains(body, `hx-target="#field-provider-modal-body"`) {
		t.Errorf("link button must not target the modal body; body:\n%s", body)
	}
}

// TestDeezerCandidates_NoMatchesAndProviderError pins the two non-result
// states: a genuine empty list shows the no-matches copy, and a provider error
// surfaces a distinct banner so the empty list is not mistaken for "no such
// artist".
func TestDeezerCandidates_NoMatchesAndProviderError(t *testing.T) {
	t.Parallel()

	var empty bytes.Buffer
	if err := DeezerCandidates(DeezerCandidatesData{ArtistID: "a"}).Render(testCtx(t), &empty); err != nil {
		t.Fatalf("render empty: %v", err)
	}
	if !strings.Contains(empty.String(), "No Deezer matches found") {
		t.Errorf("empty state missing no-matches copy; body:\n%s", empty.String())
	}

	var errored bytes.Buffer
	data := DeezerCandidatesData{ArtistID: "a", ProviderError: "Deezer"}
	if err := DeezerCandidates(data).Render(testCtx(t), &errored); err != nil {
		t.Fatalf("render errored: %v", err)
	}
	if !strings.Contains(errored.String(), "Deezer") || !strings.Contains(errored.String(), "unavailable") {
		t.Errorf("provider-error state missing banner copy; body:\n%s", errored.String())
	}
	// When a provider error is surfaced, the empty list must NOT also claim
	// "no matches found" -- that would be two contradictory messages (the amber
	// "unavailable" banner plus the italic no-matches line). The banner alone wins.
	if strings.Contains(errored.String(), "No Deezer matches found") {
		t.Errorf("provider-error state must not also show no-matches copy; body:\n%s", errored.String())
	}
}

// TestDeezerLinkSuccess_RendersRowAndToast pins the CORRECTED success-render
// contract. The candidate link button targets the Deezer ID row with
// hx-swap="outerHTML", so the success render IS that row's replacement: it must
// render the field row (id="field-deezer_id-{id}") carrying the persisted value,
// plus fire the success toast. It must NOT swap into the modal body, must NOT
// OOB-swap (it lands in place directly), and must NOT carry an inline
// hideFieldProviderModal close script -- the button's hx-on::after-request
// closes the modal. The old test only asserted the substring
// "hideFieldProviderModal" was present and so passed while the runtime close was
// broken (the swap-into-modal-body path re-opened the modal); this asserts the
// real contract instead.
func TestDeezerLinkSuccess_RendersRowAndToast(t *testing.T) {
	t.Parallel()

	a := artist.Artist{ID: "artist-1", Name: "Radiohead", DeezerID: "4050205"}

	var buf bytes.Buffer
	if err := DeezerLinkSuccess(a, nil).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()

	// The render replaces the deezer_id row in place and shows the linked value.
	for _, want := range []string{
		`id="field-deezer_id-artist-1"`, // the row updates in place (outerHTML target)
		"4050205",                       // the persisted Deezer ID is rendered in the row
		"data-deezer-toast",             // toast marker present
		"showSuccessToast",              // toast actually fires
	} {
		if !strings.Contains(body, want) {
			t.Errorf("link success missing %q; body:\n%s", want, body)
		}
	}

	// Regression guards for the close blocker: the success render must not route
	// into the modal body, must not OOB-swap, and must not try to close the
	// modal itself (that is the link button's job via hx-on::after-request).
	for _, banned := range []string{
		"field-provider-modal-body",
		"hx-swap-oob",
		"hideFieldProviderModal",
	} {
		if strings.Contains(body, banned) {
			t.Errorf("link success must not contain %q; body:\n%s", banned, body)
		}
	}
}

// TestDiscogsCandidates_RendersAlbumConfidence pins that the Discogs candidate
// rows surface album-comparison confidence and that the link button targets the
// discogs_id ROW (outerHTML), not the modal body, closing the modal via
// hx-on::after-request. Mirrors TestDeezerCandidates_RendersAlbumConfidence.
func TestDiscogsCandidates_RendersAlbumConfidence(t *testing.T) {
	t.Parallel()

	data := DiscogsCandidatesData{
		ArtistID: "artist-1",
		Candidates: []DiscogsCandidate{{
			Result: provider.ArtistSearchResult{Name: "Radiohead", ProviderID: "3840", Score: 100},
			AlbumComparison: &artist.AlbumComparison{
				MatchCount:   2,
				LocalCount:   2,
				MatchPercent: 100,
				Matches:      []artist.AlbumMatch{{RemoteName: "OK Computer"}},
			},
			Confidence: 1.0,
		}},
	}

	var buf bytes.Buffer
	if err := DiscogsCandidates(data).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()

	for _, want := range []string{
		`hx-post="/api/v1/artists/artist-1/discogs/link"`,
		`hx-target="#field-discogs_id-artist-1"`,
		`hx-swap="outerHTML"`,
		`hx-on::after-request="hideFieldProviderModal()"`,
		"3840",
		"OK Computer",
		"Radiohead",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("candidate row missing %q; body:\n%s", want, body)
		}
	}

	if strings.Contains(body, `hx-target="#field-provider-modal-body"`) {
		t.Errorf("link button must not target the modal body; body:\n%s", body)
	}
}

// TestDiscogsCandidates_NoMatchesAndProviderError pins the two non-result states
// for the Discogs candidate renderer.
func TestDiscogsCandidates_NoMatchesAndProviderError(t *testing.T) {
	t.Parallel()

	var empty bytes.Buffer
	if err := DiscogsCandidates(DiscogsCandidatesData{ArtistID: "a"}).Render(testCtx(t), &empty); err != nil {
		t.Fatalf("render empty: %v", err)
	}
	if !strings.Contains(empty.String(), "No Discogs matches found") {
		t.Errorf("empty state missing no-matches copy; body:\n%s", empty.String())
	}

	var errored bytes.Buffer
	data := DiscogsCandidatesData{ArtistID: "a", ProviderError: "Discogs"}
	if err := DiscogsCandidates(data).Render(testCtx(t), &errored); err != nil {
		t.Fatalf("render errored: %v", err)
	}
	if !strings.Contains(errored.String(), "Discogs") || !strings.Contains(errored.String(), "unavailable") {
		t.Errorf("provider-error state missing banner copy; body:\n%s", errored.String())
	}
	// When a provider error is surfaced, the empty list must NOT also claim
	// "no matches found" -- that would be two contradictory messages (the amber
	// "unavailable" banner plus the italic no-matches line). The banner alone wins.
	if strings.Contains(errored.String(), "No Discogs matches found") {
		t.Errorf("provider-error state must not also show no-matches copy; body:\n%s", errored.String())
	}
}

// TestDiscogsLinkSuccess_RendersRowAndToast pins the success-render contract for
// Discogs: the render IS the discogs_id row's outerHTML replacement carrying the
// persisted value plus the success toast, and must not route into the modal
// body, OOB-swap, or carry an inline modal-close script.
func TestDiscogsLinkSuccess_RendersRowAndToast(t *testing.T) {
	t.Parallel()

	a := artist.Artist{ID: "artist-1", Name: "Radiohead", DiscogsID: "3840"}

	var buf bytes.Buffer
	if err := DiscogsLinkSuccess(a, nil).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()

	for _, want := range []string{
		`id="field-discogs_id-artist-1"`,
		"3840",
		"data-discogs-toast",
		"showSuccessToast",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("link success missing %q; body:\n%s", want, body)
		}
	}

	for _, banned := range []string{
		"field-provider-modal-body",
		"hx-swap-oob",
		"hideFieldProviderModal",
	} {
		if strings.Contains(body, banned) {
			t.Errorf("link success must not contain %q; body:\n%s", banned, body)
		}
	}
}
