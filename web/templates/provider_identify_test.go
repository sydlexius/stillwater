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
		`hx-target="#field-provider-modal-body"`,
		"4050205",
		"OK Computer", // matched album name surfaced
		"Radiohead",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("candidate row missing %q; body:\n%s", want, body)
		}
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
}

// TestDeezerLinkSuccess_OOBSwapAndClose pins the success response: an OOB swap
// of the deezer_id row so the linked value lands in place, a toast hook, and
// the modal auto-close.
func TestDeezerLinkSuccess_OOBSwapAndClose(t *testing.T) {
	t.Parallel()

	a := artist.Artist{ID: "artist-1", Name: "Radiohead", DeezerID: "4050205"}

	var buf bytes.Buffer
	if err := DeezerLinkSuccess(a, nil).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()

	for _, want := range []string{
		"outerHTML:#field-deezer_id-artist-1",
		"data-deezer-toast",
		"hideFieldProviderModal",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("link success missing %q; body:\n%s", want, body)
		}
	}
}
