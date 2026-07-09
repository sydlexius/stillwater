package templates

// spotify_thumb_evidence_golden_test.go -- committed rendered-evidence tests
// for issue #2207 (Spotify thumb-only scope). #2212 already dropped genres
// from Spotify's SupportedFields (now just ["name"]); Spotify's only
// remaining surface is thumb images via internal/provider.ProviderCapabilities
// (SupportedImages: []ImageType{ImageThumb}) and the default "thumb" field
// priority (internal/provider/settings.go DefaultPriorities, which already
// lists NameSpotify alongside NameFanartTV/NameAudioDB/NameDeezer).
//
// These tests render the actual production templ funcs (not a hand-rolled
// stand-in) so a future regression that drops the Spotify chip from any of
// these three surfaces trips the golden diff. Do NOT reintroduce genres for
// Spotify anywhere these fixtures touch.
//
// Run with -update to regenerate the golden files after an intentional
// template change.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
)

// TestPriorityChipRow_ThumbWithSpotify_Golden renders the default "thumb"
// field-priority row (which includes Spotify per settings.DefaultPriorities)
// with a configured Spotify key so it clears the availableProviders filter.
// Asserts the Spotify chip is present by name and by its data-provider
// attribute -- a fix that merely renders the row without the Spotify entry
// (e.g. an availableProviders regression that silently drops it) would still
// pass a bare "row renders" check but fails these specific assertions.
func TestPriorityChipRow_ThumbWithSpotify_Golden(t *testing.T) {
	ctx := testCtx(t)
	pri := provider.FieldPriority{
		Field:     "thumb",
		Providers: []provider.ProviderName{provider.NameFanartTV, provider.NameAudioDB, provider.NameDeezer, provider.NameSpotify},
	}
	keys := []provider.ProviderKeyStatus{
		{Name: provider.NameFanartTV, Status: "ok"},
		{Name: provider.NameAudioDB, Status: "not_required"},
		{Name: provider.NameDeezer, Status: "not_required"},
		{Name: provider.NameSpotify, Status: "ok"},
	}
	var buf bytes.Buffer
	if err := PriorityChipRow(pri, keys).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	rendered := buf.String()

	if !strings.Contains(rendered, `data-provider="spotify"`) {
		t.Errorf("thumb priority row missing data-provider=\"spotify\" chip; rendered=%s", rendered)
	}
	if !strings.Contains(rendered, ">Spotify<") {
		t.Errorf("thumb priority row missing visible \"Spotify\" chip label; rendered=%s", rendered)
	}
	if strings.Contains(rendered, "genre") {
		t.Errorf("thumb priority row unexpectedly references genres (out of scope per #2212); rendered=%s", rendered)
	}

	checkOrUpdateGolden(t, "priority_thumb_with_spotify", rendered)
}

// TestProviderKeyCard_SpotifyDualField_Golden renders ProviderKeyCard for the
// Spotify provider and asserts the existing dual-field (client_id +
// client_secret) credential form is present in the config gear panel. Spotify
// stores a {ClientID,ClientSecret} JSON blob in the single api_key slot; the
// dual-field entry already exists (settings.templ ProviderKeyCard, gated on
// pk.Name == provider.NameSpotify) -- this test only proves it renders, it
// does not add new credential UI. A regression that reverted to a
// single-field form (e.g. only client_id, dropping client_secret) would pass
// a bare "the card renders" check but fails the specific field-name
// assertions below.
func TestProviderKeyCard_SpotifyDualField_Golden(t *testing.T) {
	ctx := testCtx(t)
	pk := provider.ProviderKeyStatus{
		Name:        provider.NameSpotify,
		DisplayName: provider.NameSpotify.DisplayName(),
		RequiresKey: true,
		HasKey:      false,
		Status:      "unconfigured",
		AccessTier:  provider.TierPaid,
	}
	var buf bytes.Buffer
	if err := ProviderKeyCard(pk).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	rendered := buf.String()

	if !strings.Contains(rendered, `name="client_id"`) {
		t.Errorf("Spotify provider key card missing client_id field; rendered=%s", rendered)
	}
	if !strings.Contains(rendered, `name="client_secret"`) {
		t.Errorf("Spotify provider key card missing client_secret field (dual-field credential entry); rendered=%s", rendered)
	}
	if !strings.Contains(rendered, ">Spotify<") {
		t.Errorf("Spotify provider key card missing visible \"Spotify\" display name; rendered=%s", rendered)
	}

	checkOrUpdateGolden(t, "provider_key_card_spotify_dual_field", rendered)
}

// TestImageSearchResults_SpotifyThumbSource_Golden renders the same
// templates.ImageSearchResults partial the manual per-field thumb fetch path
// (GET /api/v1/artists/{id}/images/search?type=thumb, handleImageSearch in
// internal/api/handlers_image.go) returns for HTMX requests, with a Spotify
// thumb result among the providers. Asserts the Spotify source chip renders
// with its display name (provider.ProviderName(img.Source).DisplayName(),
// used by components.ImageCard) -- a regression that renders the raw
// "spotify" key, or drops the source badge entirely, would still pass a bare
// "the grid renders" check but fails these assertions.
func TestImageSearchResults_SpotifyThumbSource_Golden(t *testing.T) {
	ctx := testCtx(t)
	images := []provider.ImageResult{
		{URL: "https://i.scdn.co/image/spotify-thumb.jpg", Type: provider.ImageThumb, Source: "spotify", Width: 640, Height: 640},
		{URL: "https://assets.fanart.tv/thumb.jpg", Type: provider.ImageThumb, Source: "fanarttv", Width: 500, Height: 500},
	}
	var buf bytes.Buffer
	if err := ImageSearchResults("artist-1", images, "", false).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	rendered := buf.String()

	if !strings.Contains(rendered, `data-img-source="spotify"`) {
		t.Errorf("image search results missing data-img-source=\"spotify\"; rendered=%s", rendered)
	}
	if !strings.Contains(rendered, ">Spotify<") {
		t.Errorf("image search results missing visible \"Spotify\" source chip (raw key or dropped badge); rendered=%s", rendered)
	}
	if strings.Contains(rendered, ">spotify<") {
		t.Errorf("image search results rendered the raw \"spotify\" key instead of the display name; rendered=%s", rendered)
	}

	checkOrUpdateGolden(t, "image_search_results_spotify_thumb", rendered)
}
