package templates

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// renderEditor renders ArtworkManageEditor for the given data and returns HTML.
func renderEditor(t *testing.T, data ImageSearchData) string {
	t.Helper()
	var buf bytes.Buffer
	if err := ArtworkManageEditor(data).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render ArtworkManageEditor: %v", err)
	}
	return buf.String()
}

// editorData builds ImageSearchData for a kind with the image present and both
// provider + web search available, so every capability surfaces.
func editorData(selectedType string) ImageSearchData {
	a := artist.Artist{
		ID:            "art-1",
		Name:          "Parity",
		MusicBrainzID: "mbid-1",
		ThumbExists:   true,
		LogoExists:    true,
		BannerExists:  true,
	}
	return ImageSearchData{
		Artist:           a,
		WebSearchEnabled: true,
		SelectedType:     selectedType,
		SelectedIndex:    -1,
		BasePath:         "",
	}
}

// TestArtworkManageEditor_CapabilityParity verifies the reused editor (hosted by
// the next/ Manage-artwork modal) carries every capability of the retired Fetch
// Images page: provider + web search, upload/drag-drop/URL, compare, manual
// crop, auto-crop + sort functions, delete, and the conflict-gate hook. The
// logo auto-trim affordance is asserted separately (logo-only).
func TestArtworkManageEditor_CapabilityParity(t *testing.T) {
	t.Parallel()
	out := renderEditor(t, editorData("logo"))

	for label, want := range map[string]string{
		"provider search":      "/images/search?type=logo",
		"web search":           "/images/websearch?type=logo",
		"drag-drop zone":       `id="image-drop-zone"`,
		"browse file input":    `id="image-file-input"`,
		"fetch-from-URL modal": `id="fetch-url-modal"`,
		"compare panel":        `id="compare-section"`,
		"compare use-this":     `id="compare-save-btn"`,
		"manual crop modal":    `id="crop-modal"`,
		"crop function":        "function openCropModal",
		"sort function":        "function sortImageGrid",
		"delete action":        "/api/v1/artists/art-1/images/logo",
		"conflict-gate hook":   "data-sw-requires-image-write",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("editor (logo) missing capability %s (%q)", label, want)
		}
	}
}

// TestArtworkManageEditor_LogoAutoTrimOnly verifies the auto-trim-padding action
// renders ONLY for the logo kind (a distinct action from crop) and is gated.
func TestArtworkManageEditor_LogoAutoTrimOnly(t *testing.T) {
	t.Parallel()

	logo := renderEditor(t, editorData("logo"))
	if !strings.Contains(logo, "/images/logo/trim") {
		t.Error("logo editor missing the auto-trim-padding action")
	}
	if !strings.Contains(logo, "Auto-trim padding") {
		t.Error("logo editor missing the auto-trim-padding label")
	}

	// Auto-trim must NOT appear for non-logo kinds.
	for _, kind := range []string{"thumb", "banner"} {
		out := renderEditor(t, editorData(kind))
		if strings.Contains(out, "/images/logo/trim") {
			t.Errorf("%s editor must not render the logo-only auto-trim action", kind)
		}
	}
}

// TestArtworkManageEditor_NoCropperAssets verifies the editor fragment does not
// re-include cropper assets; the caller (stable page or modal shell) loads them
// once so kind-switching does not re-declare the library.
func TestArtworkManageEditor_NoCropperAssets(t *testing.T) {
	t.Parallel()
	out := renderEditor(t, editorData("thumb"))
	if strings.Contains(out, "cropper.min.js") || strings.Contains(out, "cropper.min.css") {
		t.Error("editor fragment must not re-include cropper assets")
	}
}
