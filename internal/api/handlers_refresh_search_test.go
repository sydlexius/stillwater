package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
)

// minimalSearchProvider is a stripped-down provider.Provider implementation
// used only by handleRefreshSearch tests. It supports the SearchArtist hook
// that SearchForLinking calls; every other method returns zero values.
type minimalSearchProvider struct {
	name     provider.ProviderName
	searchFn func(ctx context.Context, name string) ([]provider.ArtistSearchResult, error)
}

func (p *minimalSearchProvider) Name() provider.ProviderName { return p.name }
func (p *minimalSearchProvider) RequiresAuth() bool          { return false }
func (p *minimalSearchProvider) SearchArtist(ctx context.Context, name string) ([]provider.ArtistSearchResult, error) {
	if p.searchFn != nil {
		return p.searchFn(ctx, name)
	}
	return nil, nil
}
func (p *minimalSearchProvider) GetArtist(_ context.Context, _ string) (*provider.ArtistMetadata, error) {
	return nil, nil
}
func (p *minimalSearchProvider) GetImages(_ context.Context, _ string) ([]provider.ImageResult, error) {
	return nil, nil
}

// installSearchOrchestrator wires a Registry+Orchestrator into the router so
// handleRefreshSearch runs against the supplied stub providers. Mirrors the
// pattern in newIdentifyTestServer but stays local to this test file so the
// refresh-search assertions are self-contained.
func installSearchOrchestrator(t *testing.T, r *Router, providers ...*minimalSearchProvider) {
	t.Helper()
	registry := provider.NewRegistry()
	for _, p := range providers {
		registry.Register(p)
	}
	r.providerRegistry = registry
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := provider.NewOrchestrator(registry, nil, logger)
	r.orchestrator = orch
}

// TestHandleRefreshSearch_FailedProvidersJSON pins the JSON contract on the
// disambiguation search endpoint when at least one provider errored. The
// response must carry a failed_providers list of the human-readable display
// names so JSON consumers (smoke tests, future API clients) can distinguish
// "providers errored, results may be incomplete" from "providers all succeeded,
// no matches exist". Issue #1663.
func TestHandleRefreshSearch_FailedProvidersJSON(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Hiromi Disambig JSON")

	// MB succeeds with one result; Discogs errors. The handler should
	// return that one result AND a failed_providers list naming Discogs.
	installSearchOrchestrator(t, r,
		&minimalSearchProvider{
			name: provider.NameMusicBrainz,
			searchFn: func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
				return []provider.ArtistSearchResult{
					{Name: "Hiromi", MusicBrainzID: "mb-1", Source: "musicbrainz", Score: 100},
				}, nil
			},
		},
		&minimalSearchProvider{
			name: provider.NameDiscogs,
			searchFn: func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
				return nil, errors.New("api.discogs.com: 401 unauthorized")
			},
		},
	)

	body := strings.NewReader(`{"query":"Hiromi"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/refresh/search", body)
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleRefreshSearch(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", w.Code, w.Body.String())
	}

	var resp struct {
		Results         []map[string]any `json:"results"`
		FailedProviders []string         `json:"failed_providers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v, body=%s", err, w.Body.String())
	}
	if len(resp.Results) != 1 {
		t.Errorf("expected 1 result (MB succeeded), got %d", len(resp.Results))
	}
	if len(resp.FailedProviders) != 1 {
		t.Fatalf("expected 1 failed provider, got %d (%v)", len(resp.FailedProviders), resp.FailedProviders)
	}
	// Use the canonical DisplayName so a future rename in provider.go
	// flows through here without an extra patch.
	want := provider.NameDiscogs.DisplayName()
	if resp.FailedProviders[0] != want {
		t.Errorf("failed_providers[0] = %q, want %q", resp.FailedProviders[0], want)
	}
}

// TestHandleRefreshSearch_FailedProvidersHTMXBanner pins the HTML render
// path. When at least one provider errored and the request is HTMX, the
// rendered fragment must include the providers-unreachable banner so the
// disambiguation panel surfaces the warning above the (possibly empty)
// candidate list. Issue #1663.
func TestHandleRefreshSearch_FailedProvidersHTMXBanner(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Hiromi Disambig HTMX")

	// Both providers error -- the harder case: zero results AND a banner.
	// Without the fix this renders identically to "no matches found".
	installSearchOrchestrator(t, r,
		&minimalSearchProvider{
			name: provider.NameMusicBrainz,
			searchFn: func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
				return nil, errors.New("musicbrainz.org: tls handshake failed")
			},
		},
		&minimalSearchProvider{
			name: provider.NameDiscogs,
			searchFn: func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
				return nil, errors.New("api.discogs.com: connection refused")
			},
		},
	)

	body := strings.NewReader(`{"query":"Hiromi"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/refresh/search", body)
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("HX-Request", "true")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleRefreshSearch(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", w.Code, w.Body.String())
	}
	html := w.Body.String()
	if !strings.Contains(html, "providers-unreachable-banner") {
		t.Errorf("HTMX response missing providers-unreachable-banner; body:\n%s", html)
	}
	// Both provider display names must appear in the banner copy.
	for _, name := range []string{provider.NameMusicBrainz.DisplayName(), provider.NameDiscogs.DisplayName()} {
		if !strings.Contains(html, name) {
			t.Errorf("HTMX response missing provider name %q; body:\n%s", name, html)
		}
	}
}

// TestHandleRefreshSearch_NoBannerWhenAllSucceed pins the negative case at
// the handler level: with no provider errors the JSON payload must omit
// failed_providers and the HTMX render must not include the banner. This
// guarantees the legitimate "no matches found" empty state stays distinct
// from the "providers unreachable" warning -- the whole point of #1663.
func TestHandleRefreshSearch_NoBannerWhenAllSucceed(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Hiromi No Match Clean")

	installSearchOrchestrator(t, r,
		&minimalSearchProvider{
			name: provider.NameMusicBrainz,
			searchFn: func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
				return nil, nil // success, zero matches
			},
		},
	)

	body := strings.NewReader(`{"query":"Hiromi"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/refresh/search", body)
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("HX-Request", "true")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleRefreshSearch(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", w.Code, w.Body.String())
	}
	html := w.Body.String()
	if strings.Contains(html, "providers-unreachable-banner") {
		t.Errorf("banner should NOT render when all providers succeed; body:\n%s", html)
	}
}

// TestHandleRefreshSearch_JSONOmitsFailedProvidersWhenAllSucceed pins the
// JSON-path twin of the HTMX banner-omission test above: when no providers
// errored the failed_providers field must be omitted (not just empty) so
// JSON consumers can use field presence as the "providers errored" signal
// without parsing an array length. This is the JSON-side contract for #1663.
func TestHandleRefreshSearch_JSONOmitsFailedProvidersWhenAllSucceed(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Hiromi JSON No Failure")

	installSearchOrchestrator(t, r,
		&minimalSearchProvider{
			name: provider.NameMusicBrainz,
			searchFn: func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
				return nil, nil
			},
		},
	)

	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/artists/"+a.ID+"/refresh/search",
		strings.NewReader(`{"query":"Hiromi"}`),
	)
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleRefreshSearch(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v, body=%s", err, w.Body.String())
	}
	if _, ok := resp["failed_providers"]; ok {
		t.Fatalf("expected failed_providers to be omitted when all providers succeed, body=%s", w.Body.String())
	}
}
