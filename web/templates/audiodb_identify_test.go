package templates

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// TestAudioDBCandidates_RendersAlbumConfidence pins that the TheAudioDB
// candidate rows surface album-comparison confidence and that the link button
// targets the audiodb_id ROW (outerHTML), not the modal body, closing the modal
// via hx-on::after-request. Mirrors TestDiscogsCandidates_RendersAlbumConfidence.
func TestAudioDBCandidates_RendersAlbumConfidence(t *testing.T) {
	t.Parallel()

	data := AudioDBCandidatesData{
		ArtistID: "artist-1",
		Candidates: []AudioDBCandidate{{
			Result: provider.ArtistSearchResult{Name: "Radiohead", ProviderID: "111493", Score: 100},
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
	if err := AudioDBCandidates(data).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()

	for _, want := range []string{
		`hx-post="/api/v1/artists/artist-1/audiodb/link"`,
		`hx-target="#field-audiodb_id-artist-1"`,
		`hx-swap="outerHTML"`,
		`hx-on::after-request="hideFieldProviderModal()"`,
		"111493",
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

// TestAudioDBCandidates_NoMatchesAndProviderError pins the two non-result states
// for the TheAudioDB candidate renderer.
func TestAudioDBCandidates_NoMatchesAndProviderError(t *testing.T) {
	t.Parallel()

	var empty bytes.Buffer
	if err := AudioDBCandidates(AudioDBCandidatesData{ArtistID: "a"}).Render(testCtx(t), &empty); err != nil {
		t.Fatalf("render empty: %v", err)
	}
	if !strings.Contains(empty.String(), "No TheAudioDB matches found") {
		t.Errorf("empty state missing no-matches copy; body:\n%s", empty.String())
	}

	var errored bytes.Buffer
	data := AudioDBCandidatesData{ArtistID: "a", ProviderError: "TheAudioDB"}
	if err := AudioDBCandidates(data).Render(testCtx(t), &errored); err != nil {
		t.Fatalf("render errored: %v", err)
	}
	if !strings.Contains(errored.String(), "TheAudioDB") || !strings.Contains(errored.String(), "unavailable") {
		t.Errorf("provider-error state missing banner copy; body:\n%s", errored.String())
	}
	// When a provider error is surfaced, the empty list must NOT also claim
	// "no matches found" -- the amber "unavailable" banner alone wins.
	if strings.Contains(errored.String(), "No TheAudioDB matches found") {
		t.Errorf("provider-error state must not also show no-matches copy; body:\n%s", errored.String())
	}
}

// TestAudioDBLinkSuccess_RendersRowAndToast pins the success-render contract for
// TheAudioDB: the render IS the audiodb_id row's outerHTML replacement carrying
// the persisted value plus the success toast, and must not route into the modal
// body, OOB-swap, or carry an inline modal-close script.
func TestAudioDBLinkSuccess_RendersRowAndToast(t *testing.T) {
	t.Parallel()

	a := artist.Artist{ID: "artist-1", Name: "Radiohead", AudioDBID: "111493"}

	var buf bytes.Buffer
	if err := AudioDBLinkSuccess(a, nil).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()

	for _, want := range []string{
		`id="field-audiodb_id-artist-1"`,
		"111493",
		"data-audiodb-toast",
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
