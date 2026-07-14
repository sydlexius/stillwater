package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/provider"
)

// stubImageProvider is a provider.Provider that only ever returns images, used
// to give handleImageSearch a real (if fake) provider to skip or query.
type stubImageProvider struct {
	name   provider.ProviderName
	images []provider.ImageResult
}

func (s *stubImageProvider) Name() provider.ProviderName { return s.name }
func (s *stubImageProvider) RequiresAuth() bool          { return true }
func (s *stubImageProvider) SearchArtist(context.Context, string) ([]provider.ArtistSearchResult, error) {
	return nil, nil
}

func (s *stubImageProvider) GetArtist(context.Context, string) (*provider.ArtistMetadata, error) {
	return nil, nil
}

func (s *stubImageProvider) GetImages(context.Context, string) ([]provider.ImageResult, error) {
	return s.images, nil
}

// newProviderStatusRouter rebuilds the image test router's orchestrator around a
// registry containing AudioDB (which can use an MBID) and Discogs (which cannot),
// both with a stored API key so AvailableProviderNames admits them.
func newProviderStatusRouter(t *testing.T) (*Router, *artist.Service) {
	t.Helper()
	r, svc := newImageHandlerTestServer(t)

	registry := provider.NewRegistry()
	registry.Register(&stubImageProvider{
		name:   provider.NameAudioDB,
		images: []provider.ImageResult{{Type: provider.ImageThumb, URL: "http://example.com/thumb.jpg"}},
	})
	registry.Register(&stubImageProvider{name: provider.NameDiscogs})

	// A nil encryptor panics on SetAPIKey; the settings service needs a real one.
	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}
	settings := provider.NewSettingsService(r.db, enc)
	for _, name := range []provider.ProviderName{provider.NameAudioDB, provider.NameDiscogs} {
		if err := settings.SetAPIKey(context.Background(), name, "test-key"); err != nil {
			t.Fatalf("SetAPIKey %s: %v", name, err)
		}
	}
	r.orchestrator = provider.NewOrchestrator(registry, settings, r.logger, nil)
	r.providerSettings = settings
	return r, svc
}

// TestHandleImageSearch_AudioDBQueriedWithoutItsOwnID drives the AudioDB fix
// through the real handler: an artist with no AudioDB ID (the common case) must
// still get AudioDB's artwork, because AudioDB accepts the MBID. Before #2457
// this returned zero images and a 200.
func TestHandleImageSearch_AudioDBQueriedWithoutItsOwnID(t *testing.T) {
	r, svc := newProviderStatusRouter(t)

	a := &artist.Artist{Name: "NoIDs", SortName: "NoIDs", Path: t.TempDir(), MusicBrainzID: "mbid-1"}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	// Precondition: without this the test would pass even if the skip branch
	// were untouched, because AudioDB would be queried by its own ID.
	if a.DiscogsID != "" || a.AudioDBID != "" {
		t.Fatalf("precondition: artist must have no Discogs/AudioDB ID, got %q/%q", a.DiscogsID, a.AudioDBID)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/images/search?type=thumb", nil)
	req.SetPathValue("id", a.ID)

	w := serveValidated(t, http.HandlerFunc(r.handleImageSearch), req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Images []provider.ImageResult `json:"images"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(resp.Images) != 1 {
		t.Errorf("images = %d, want the 1 image AudioDB returned for an artist with no AudioDB ID", len(resp.Images))
	}
}

// TestHandleImageSearch_ProviderStatusesHTMX asserts the HTMX branch (the one the
// artwork-search UI actually uses) renders the skip banner. Before #2457 that
// branch passed neither skips nor errors to the template.
func TestHandleImageSearch_ProviderStatusesHTMX(t *testing.T) {
	r, svc := newProviderStatusRouter(t)

	a := &artist.Artist{Name: "HTMXNoIDs", SortName: "HTMXNoIDs", Path: t.TempDir(), MusicBrainzID: "mbid-2"}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/images/search?type=thumb", nil)
	req.Header.Set("HX-Request", "true")
	req.SetPathValue("id", a.ID)

	w := serveValidated(t, http.HandlerFunc(r.handleImageSearch), req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	// The marker proves the statuses reached the HTMX branch's template, which
	// is what regressed: that branch passed neither skips nor errors. The banner
	// COPY (which providers, what reason) is asserted in the templates package,
	// where the test context carries the full i18n bundle this one does not.
	body := w.Body.String()
	if !strings.Contains(body, "data-sw-providers-skipped") {
		t.Errorf("HTMX results carry no skipped-provider banner; body: %s", body)
	}
}

// TestHandleImageSearch_ProviderStatusesJSON asserts the JSON branch reports the
// same facts the UI banner shows.
//
// "errors" alone cannot carry them: it lists only failures, so a provider that
// was never queried (no artist ID stored for it) is indistinguishable from one
// that was queried and found nothing. An API client reading a 200 with a thin
// images array has no way to know the search was incomplete -- the same
// invisibility this issue is about, on the machine-readable surface.
//
// Mutant this kills: dropping the provider_statuses key from the JSON response.
func TestHandleImageSearch_ProviderStatusesJSON(t *testing.T) {
	r, svc := newProviderStatusRouter(t)

	a := &artist.Artist{Name: "JSONNoIDs", SortName: "JSONNoIDs", Path: t.TempDir(), MusicBrainzID: "mbid-3"}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// No HX-Request header: this is the JSON branch.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/images/search?type=thumb", nil)
	req.SetPathValue("id", a.ID)

	w := serveValidated(t, http.HandlerFunc(r.handleImageSearch), req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var got struct {
		Images           []map[string]any `json:"images"`
		ProviderStatuses []struct {
			Provider string `json:"provider"`
			Outcome  string `json:"outcome"`
			Reason   string `json:"reason"`
		} `json:"provider_statuses"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v; body: %s", err, w.Body.String())
	}

	// PRECONDITION: without statuses there is nothing to assert about, and a
	// "no skips reported" check would pass vacuously against the old behavior.
	if len(got.ProviderStatuses) == 0 {
		t.Fatalf("provider_statuses is empty; the JSON branch reports nothing about which "+
			"providers ran, so a caller cannot tell an incomplete search from an empty one. "+
			"body: %s", w.Body.String())
	}

	var skipped []string
	for _, s := range got.ProviderStatuses {
		if s.Outcome == "skipped" {
			if s.Reason == "" {
				t.Errorf("provider %q is reported skipped with no reason; the caller learns "+
					"that it did not run but not why, which it cannot act on", s.Provider)
			}
			skipped = append(skipped, s.Provider)
		}
	}
	if len(skipped) == 0 {
		t.Errorf("this artist has no provider IDs stored, so the providers that require one "+
			"must be reported skipped; got statuses %+v", got.ProviderStatuses)
	}
}
