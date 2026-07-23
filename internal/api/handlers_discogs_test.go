package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/conflict"
	"github.com/sydlexius/stillwater/internal/provider"
)

// installDiscogsOrchestrator wires a Registry+Orchestrator around a Discogs stub
// provider so the Discogs search/link handlers run end-to-end. The stub reuses
// identifyStubProvider (which implements ReleaseGroupFetcher) so the
// album-comparison branch of enrichDiscogsCandidates can be exercised. Mirrors
// installDeezerOrchestrator.
func installDiscogsOrchestrator(t *testing.T, r *Router,
	search func(ctx context.Context, name string) ([]provider.ArtistSearchResult, error),
	releaseGroups func(ctx context.Context, artistID string) ([]provider.ReleaseGroupInfo, error),
) {
	t.Helper()
	stub := &identifyStubProvider{
		name:             provider.NameDiscogs,
		searchFn:         search,
		getReleaseGrpsFn: releaseGroups,
	}
	registry := provider.NewRegistry()
	registry.Register(stub)
	r.providerRegistry = registry
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	r.orchestrator = provider.NewOrchestrator(registry, nil, logger, nil)
}

func TestToDiscogsTemplateCandidates(t *testing.T) {
	t.Parallel()
	cmp := artist.AlbumComparison{MatchPercent: 80}
	in := []ScoredCandidate{
		{
			ArtistSearchResult: provider.ArtistSearchResult{Name: "A", ProviderID: "1"},
			AlbumComparison:    &cmp,
			Confidence:         0.8,
		},
		{ArtistSearchResult: provider.ArtistSearchResult{Name: "B", ProviderID: "2"}},
	}
	got := toDiscogsTemplateCandidates(in)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Result.Name != "A" || got[0].Confidence != 0.8 || got[0].AlbumComparison == nil {
		t.Errorf("candidate 0 not mapped: %+v", got[0])
	}
	if got[1].Result.ProviderID != "2" || got[1].AlbumComparison != nil {
		t.Errorf("candidate 1 not mapped: %+v", got[1])
	}
}

func TestEnrichDiscogsCandidates(t *testing.T) {
	t.Parallel()

	t.Run("nil registry falls back to scored conversion", func(t *testing.T) {
		t.Parallel()
		r, _ := testRouter(t)
		r.providerRegistry = nil
		results := []provider.ArtistSearchResult{{Name: "A", ProviderID: "1"}}
		got := r.enrichDiscogsCandidates(context.Background(), results, []string{"Album One"})
		if len(got) != 1 || got[0].Reason != "no album data available" {
			t.Errorf("got = %+v, want single fallback candidate", got)
		}
	})

	t.Run("no local albums falls back to scored conversion", func(t *testing.T) {
		t.Parallel()
		r, _ := testRouter(t)
		installDiscogsOrchestrator(t, r, nil, func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
			t.Fatal("GetReleaseGroups must not be called when there are no local albums")
			return nil, nil
		})
		results := []provider.ArtistSearchResult{{Name: "A", ProviderID: "1"}}
		got := r.enrichDiscogsCandidates(context.Background(), results, nil)
		if len(got) != 1 || got[0].Reason != "no album data available" {
			t.Errorf("got = %+v, want single fallback candidate", got)
		}
	})

	t.Run("provider not registered falls back", func(t *testing.T) {
		t.Parallel()
		r, _ := testRouter(t)
		r.providerRegistry = provider.NewRegistry() // empty
		results := []provider.ArtistSearchResult{{Name: "A", ProviderID: "1"}}
		got := r.enrichDiscogsCandidates(context.Background(), results, []string{"Album One"})
		if len(got) != 1 || got[0].Reason != "no album data available" {
			t.Errorf("got = %+v, want single fallback candidate", got)
		}
	})

	t.Run("happy path scores by ProviderID", func(t *testing.T) {
		t.Parallel()
		r, _ := testRouter(t)
		installDiscogsOrchestrator(t, r, nil, func(_ context.Context, discogsID string) ([]provider.ReleaseGroupInfo, error) {
			// Keyed on res.ProviderID, NOT MusicBrainzID.
			if discogsID == "111" {
				return []provider.ReleaseGroupInfo{{Title: "Album One"}, {Title: "Album Two"}}, nil
			}
			if discogsID == "222" {
				return []provider.ReleaseGroupInfo{{Title: "Album One"}, {Title: "Unrelated"}}, nil
			}
			return nil, nil
		})
		results := []provider.ArtistSearchResult{
			{Name: "Perfect", ProviderID: "111", Score: 100},
			{Name: "Partial", ProviderID: "222", Score: 80},
		}
		got := r.enrichDiscogsCandidates(context.Background(), results, []string{"Album One", "Album Two"})
		if len(got) != 2 {
			t.Fatalf("len = %d, want 2", len(got))
		}
		if got[0].AlbumComparison == nil || got[0].AlbumComparison.MatchPercent != 100 {
			t.Errorf("candidate 0 MatchPercent not 100: %+v", got[0].AlbumComparison)
		}
		if got[0].Confidence != 1.0 {
			t.Errorf("candidate 0 Confidence = %v, want 1.0", got[0].Confidence)
		}
		if got[1].AlbumComparison == nil || got[1].AlbumComparison.MatchPercent != 50 {
			t.Errorf("candidate 1 MatchPercent not 50: %+v", got[1].AlbumComparison)
		}
	})

	t.Run("empty ProviderID skips lookup", func(t *testing.T) {
		t.Parallel()
		r, _ := testRouter(t)
		var calls int
		installDiscogsOrchestrator(t, r, nil, func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
			calls++
			return []provider.ReleaseGroupInfo{{Title: "Album One"}}, nil
		})
		results := []provider.ArtistSearchResult{{Name: "NoID"}}
		got := r.enrichDiscogsCandidates(context.Background(), results, []string{"Album One"})
		if calls != 0 {
			t.Errorf("GetReleaseGroups calls = %d, want 0 for empty ProviderID", calls)
		}
		if len(got) != 1 || got[0].AlbumComparison != nil {
			t.Errorf("got = %+v, want single candidate with nil AlbumComparison", got)
		}
	})

	t.Run("fetch error skips that candidate", func(t *testing.T) {
		t.Parallel()
		r, _ := testRouter(t)
		installDiscogsOrchestrator(t, r, nil, func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
			return nil, errors.New("discogs boom")
		})
		results := []provider.ArtistSearchResult{{Name: "X", ProviderID: "9"}}
		got := r.enrichDiscogsCandidates(context.Background(), results, []string{"a"})
		if len(got) != 1 || got[0].AlbumComparison != nil {
			t.Errorf("got = %+v, want candidate with nil AlbumComparison after fetch error", got)
		}
	})

	t.Run("caps lookups at 3 candidates", func(t *testing.T) {
		t.Parallel()
		r, _ := testRouter(t)
		var calls int
		installDiscogsOrchestrator(t, r, nil, func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
			calls++
			return nil, nil
		})
		results := []provider.ArtistSearchResult{
			{Name: "1", ProviderID: "1"},
			{Name: "2", ProviderID: "2"},
			{Name: "3", ProviderID: "3"},
			{Name: "4", ProviderID: "4"},
			{Name: "5", ProviderID: "5"},
		}
		got := r.enrichDiscogsCandidates(context.Background(), results, []string{"a"})
		if len(got) != 5 {
			t.Fatalf("len = %d, want 5", len(got))
		}
		if calls != 3 {
			t.Errorf("GetReleaseGroups calls = %d, want 3 (cap)", calls)
		}
	})
}

func TestHandleDiscogsSearch_MissingQuery(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	installDiscogsOrchestrator(t, r, nil, nil)
	a := addTestArtist(t, artistSvc, "No Query")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/discogs/search", strings.NewReader(`{}`))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleDiscogsSearch(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleDiscogsSearch_JSONResults(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	installDiscogsOrchestrator(t, r,
		func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
			return []provider.ArtistSearchResult{
				{Name: "Radiohead", ProviderID: "3840", Source: "discogs", Score: 100},
			}, nil
		}, nil)
	a := addTestArtist(t, artistSvc, "Radiohead DG")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/discogs/search", strings.NewReader(`{"query":"Radiohead"}`))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleDiscogsSearch(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Results       []map[string]any `json:"results"`
		ProviderError string           `json:"provider_error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, w.Body.String())
	}
	if len(resp.Results) != 1 {
		t.Errorf("results len = %d, want 1", len(resp.Results))
	}
	if resp.ProviderError != "" {
		t.Errorf("provider_error = %q, want empty", resp.ProviderError)
	}
}

func TestHandleDiscogsSearch_ProviderErrorJSON(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	installDiscogsOrchestrator(t, r,
		func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
			return nil, errors.New("api.discogs.com: 429 rate limited")
		}, nil)
	a := addTestArtist(t, artistSvc, "Rate Limited DG")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/discogs/search", strings.NewReader(`{"query":"x"}`))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleDiscogsSearch(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Results       []map[string]any `json:"results"`
		ProviderError string           `json:"provider_error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, w.Body.String())
	}
	if resp.ProviderError != provider.NameDiscogs.DisplayName() {
		t.Errorf("provider_error = %q, want %q", resp.ProviderError, provider.NameDiscogs.DisplayName())
	}
}

func TestHandleDiscogsSearch_HTMXFragment(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	installDiscogsOrchestrator(t, r,
		func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
			return []provider.ArtistSearchResult{
				{Name: "Radiohead", ProviderID: "3840", Source: "discogs", Score: 100},
			}, nil
		}, nil)
	a := addTestArtist(t, artistSvc, "Radiohead HTMX DG")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/discogs/search", strings.NewReader(`{"query":"Radiohead"}`))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("HX-Request", "true")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleDiscogsSearch(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// The candidate link button must target the discogs_id ROW (outerHTML
	// swap), not the modal body, and close the modal after the request.
	if !strings.Contains(body, "/discogs/link") {
		t.Errorf("HTMX fragment missing link action; body=%s", body)
	}
	if !strings.Contains(body, `hx-target="#field-discogs_id-`+a.ID+`"`) {
		t.Errorf("HTMX fragment link must target the discogs_id field row; body=%s", body)
	}
	if !strings.Contains(body, `hx-swap="outerHTML"`) {
		t.Errorf("HTMX fragment link must outerHTML-swap the row; body=%s", body)
	}
	if !strings.Contains(body, "hideFieldProviderModal()") {
		t.Errorf("HTMX fragment link must close the modal after request; body=%s", body)
	}
	if !strings.Contains(body, "3840") {
		t.Errorf("HTMX fragment missing candidate ID; body=%s", body)
	}
}

func TestHandleDiscogsSearch_HTMXProviderError(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	installDiscogsOrchestrator(t, r,
		func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
			return nil, errors.New("api.discogs.com: 503 service unavailable")
		}, nil)
	a := addTestArtist(t, artistSvc, "Provider Error HTMX DG")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/discogs/search", strings.NewReader(`{"query":"x"}`))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("HX-Request", "true")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleDiscogsSearch(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "unavailable") {
		t.Errorf("HTMX provider-error fragment missing error banner; body=%s", body)
	}
	if strings.Contains(body, "No Discogs matches found") {
		t.Errorf("HTMX provider-error fragment must not show no-matches copy; body=%s", body)
	}
}

func TestHandleDiscogsSearch_HTMXNoMatches(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	installDiscogsOrchestrator(t, r,
		func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
			return nil, nil
		}, nil)
	a := addTestArtist(t, artistSvc, "No Match DG")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/discogs/search", strings.NewReader(`{"query":"nope"}`))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("HX-Request", "true")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleDiscogsSearch(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "No Discogs matches found") {
		t.Errorf("HTMX no-match fragment missing copy; body=%s", w.Body.String())
	}
}

func TestHandleDiscogsSearch_AlbumComparisonFromDisk(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	installDiscogsOrchestrator(t, r,
		func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
			return []provider.ArtistSearchResult{{Name: "Has Albums", ProviderID: "555", Score: 60}}, nil
		},
		func(_ context.Context, discogsID string) ([]provider.ReleaseGroupInfo, error) {
			if discogsID == "555" {
				return []provider.ReleaseGroupInfo{{Title: "Album One"}, {Title: "Album Two"}}, nil
			}
			return nil, nil
		})

	dir := t.TempDir()
	for _, alb := range []string{"Album One", "Album Two"} {
		if err := os.Mkdir(filepath.Join(dir, alb), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	a := &artist.Artist{Name: "Has Albums DG", SortName: "Has Albums DG", Type: "group", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("create: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/discogs/search", strings.NewReader(`{"query":"Has Albums"}`))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleDiscogsSearch(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Results []struct {
			AlbumComparison *artist.AlbumComparison `json:"album_comparison"`
			Confidence      float64                 `json:"confidence"`
		} `json:"results"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, w.Body.String())
	}
	if len(resp.Results) != 1 {
		t.Fatalf("results len = %d, want 1", len(resp.Results))
	}
	if resp.Results[0].AlbumComparison == nil || resp.Results[0].AlbumComparison.MatchPercent != 100 {
		t.Errorf("expected 100%% album match; got %+v", resp.Results[0].AlbumComparison)
	}
	if resp.Results[0].Confidence != 1.0 {
		t.Errorf("confidence = %v, want 1.0", resp.Results[0].Confidence)
	}
}

func TestHandleDiscogsSearch_NotFound(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	installDiscogsOrchestrator(t, r, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/nope/discogs/search", strings.NewReader(`{"query":"x"}`))
	req.SetPathValue("id", "nope")
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleDiscogsSearch(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleDiscogsSearch_ServiceUnavailable(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	r.orchestrator = nil // no provider orchestrator wired
	a := addTestArtist(t, artistSvc, "No Orchestrator DG")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/discogs/search", strings.NewReader(`{"query":"x"}`))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleDiscogsSearch(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleDiscogsLink_Success(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Link Me DG")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/discogs/link", strings.NewReader(`{"discogs_id":"3840"}`))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleDiscogsLink(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	reloaded, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.DiscogsID != "3840" {
		t.Errorf("DiscogsID = %q, want 3840", reloaded.DiscogsID)
	}
}

func TestHandleDiscogsLink_HTMXRowAndToast(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Link HTMX DG")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/discogs/link", strings.NewReader(`{"discogs_id":"3840"}`))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("HX-Request", "true")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleDiscogsLink(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// The success render IS the discogs_id row's outerHTML replacement (the
	// candidate link button targets #field-discogs_id-{id}, not the modal body).
	if !strings.Contains(body, `id="field-discogs_id-`+a.ID+`"`) {
		t.Errorf("missing in-place discogs_id row render; body=%s", body)
	}
	if !strings.Contains(body, "3840") {
		t.Errorf("linked value not rendered in row; body=%s", body)
	}
	if !strings.Contains(body, "data-discogs-toast") || !strings.Contains(body, "showSuccessToast") {
		t.Errorf("missing success toast; body=%s", body)
	}
	// Regression guards for the stuck-open-blank-modal blocker: the response must
	// not route into the modal body, must not OOB-swap, and must not carry an
	// inline modal-close script (the button's hx-on::after-request closes it).
	for _, banned := range []string{"field-provider-modal-body", "hx-swap-oob", "hideFieldProviderModal"} {
		if strings.Contains(body, banned) {
			t.Errorf("success render must not contain %q; body=%s", banned, body)
		}
	}
}

func TestHandleDiscogsLink_BadID(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Bad ID DG")

	for _, payload := range []string{`{}`, `{"discogs_id":"abc"}`, `{"discogs_id":""}`} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/discogs/link", strings.NewReader(payload))
		req.SetPathValue("id", a.ID)
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(testI18nCtx(t, req.Context()))
		w := httptest.NewRecorder()

		r.handleDiscogsLink(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("payload %s: status = %d, want 400; body=%s", payload, w.Code, w.Body.String())
		}
	}
}

func TestHandleDiscogsLink_NotFound(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/nope/discogs/link", strings.NewReader(`{"discogs_id":"3840"}`))
	req.SetPathValue("id", "nope")
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleDiscogsLink(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleDiscogsLink_ServiceUnavailable(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	r.artistService = nil // no artist service wired

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/x/discogs/link", strings.NewReader(`{"discogs_id":"3840"}`))
	req.SetPathValue("id", "x")
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleDiscogsLink(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleDiscogsLink_FieldLocked409 covers Guard 1: a pinned discogs_id field
// must refuse the identify-flow write with 409 (field_locked) and not overwrite
// the locked value.
func TestHandleDiscogsLink_FieldLocked409(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	a := &artist.Artist{
		Name:         "Locked DG",
		SortName:     "Locked DG",
		Type:         "group",
		DiscogsID:    "111",
		LockedFields: []string{"discogs_id"},
	}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("create: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/discogs/link", strings.NewReader(`{"discogs_id":"3840"}`))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleDiscogsLink(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Error string `json:"error"`
		Field string `json:"field"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, w.Body.String())
	}
	if resp.Error != "field_locked" || resp.Field != "discogs_id" {
		t.Errorf("unexpected 409 payload: %+v", resp)
	}
	// The locked ID must survive untouched.
	reloaded, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.DiscogsID != "111" {
		t.Errorf("DiscogsID = %q, want 111 (locked value preserved)", reloaded.DiscogsID)
	}
}

// TestHandleDiscogsLink_Blocked409 covers Guard 2: with an active, denying
// conflict gate, linking an EXISTING artist must be blocked with 409 (the NFO
// write the refresh may perform is gated on the conflict ledger).
func TestHandleDiscogsLink_Blocked409(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	d := conflict.NewBlockingForTest(testDiscardLogger())
	r.conflictDetector = d
	r.conflictGate = conflict.NewGate(d)
	a := addTestArtist(t, artistSvc, "Gate Blocked DG")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/discogs/link", strings.NewReader(`{"discogs_id":"3840"}`))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleDiscogsLink(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", w.Code, w.Body.String())
	}
	// The blocked link must not persist the Discogs ID.
	reloaded, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.DiscogsID != "" {
		t.Errorf("DiscogsID = %q, want empty after blocked link", reloaded.DiscogsID)
	}
}

// TestHandleDiscogsLink_GateActiveNonexistentArtist404 asserts the 404 check
// runs BEFORE the 409 gate: with the gate ACTIVE and denying, linking a
// NON-EXISTENT artist must return 404 (not 409).
func TestHandleDiscogsLink_GateActiveNonexistentArtist404(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	d := conflict.NewBlockingForTest(testDiscardLogger())
	r.conflictDetector = d
	r.conflictGate = conflict.NewGate(d)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/nope/discogs/link", strings.NewReader(`{"discogs_id":"3840"}`))
	req.SetPathValue("id", "nope")
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleDiscogsLink(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (404 must precede 409); body=%s", w.Code, w.Body.String())
	}
}

// TestHandleDiscogsIdentify_RendersModal asserts the identify endpoint returns
// the modal body: a search form pre-filled with the artist name that POSTs to
// the Discogs search endpoint.
func TestHandleDiscogsIdentify_RendersModal(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Identify Me DG")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/discogs/identify", nil)
	req.SetPathValue("id", a.ID)
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleDiscogsIdentify(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "/api/v1/artists/"+a.ID+"/discogs/search") {
		t.Errorf("identify modal missing search action; body=%s", body)
	}
	if !strings.Contains(body, "Identify Me DG") {
		t.Errorf("identify modal missing pre-filled artist name; body=%s", body)
	}
	if !strings.Contains(body, "provider-identify-results-discogs_id") {
		t.Errorf("identify modal missing results container; body=%s", body)
	}
}

func TestHandleDiscogsIdentify_NotFound(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/nope/discogs/identify", nil)
	req.SetPathValue("id", "nope")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleDiscogsIdentify(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleDiscogsIdentify_ServiceUnavailable(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	r.artistService = nil

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/x/discogs/identify", nil)
	req.SetPathValue("id", "x")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleDiscogsIdentify(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
}

// postDiscogsLink drives handleDiscogsLink with the {id} path value set.
func postDiscogsLink(t *testing.T, r *Router, artistID, discogsID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+artistID+"/discogs/link",
		strings.NewReader(`{"discogs_id":"`+discogsID+`"}`))
	req.SetPathValue("id", artistID)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()
	r.handleDiscogsLink(w, req)
	return w
}

// TestHandleDiscogsLink_LockedArtistSkipsRefresh is the Discogs half of the
// artist-lock split described on TestHandleDeezerLink_LockedArtistSkipsRefresh:
// the provider ID persists, the follow-up refresh does not run (#2754).
//
// As there, the sentinel-biography assertion (not the response flag) is what
// proves the refresh was never invoked; see attachSentinelOrchestrator.
func TestHandleDiscogsLink_LockedArtistSkipsRefresh(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	attachSentinelOrchestrator(t, r)
	a := addTestArtist(t, artistSvc, "Locked Link DG")
	if err := artistSvc.Lock(context.Background(), a.ID, "user"); err != nil {
		t.Fatalf("locking artist: %v", err)
	}
	locked, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !locked.Locked {
		t.Fatal("precondition failed: artist is not locked after Lock")
	}
	if locked.Biography != "" {
		t.Fatalf("precondition failed: artist already has a biography (%q), so its absence would prove nothing", locked.Biography)
	}

	w := postDiscogsLink(t, r, a.ID, "3840")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["refresh_skipped_locked"] != true {
		t.Errorf("refresh_skipped_locked = %v, want true; body=%v", resp["refresh_skipped_locked"], resp)
	}

	reloaded, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.DiscogsID != "3840" {
		t.Errorf("DiscogsID = %q, want 3840; the ID is a manual edit the lock allows", reloaded.DiscogsID)
	}
	assertSentinelBiographyAbsent(t, artistSvc, a.ID)
}

// TestHandleDiscogsLink_UnlockedArtistStillRefreshes is the positive control for
// the test above.
func TestHandleDiscogsLink_UnlockedArtistStillRefreshes(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	attachSentinelOrchestrator(t, r)
	a := addTestArtist(t, artistSvc, "Unlocked Link DG")
	if a.Locked {
		t.Fatal("precondition failed: artist is locked; this test covers the unlocked path")
	}

	w := postDiscogsLink(t, r, a.ID, "3840")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if _, present := resp["refresh_skipped_locked"]; present {
		t.Errorf("refresh_skipped_locked present on an unlocked artist; body=%v", resp)
	}
	assertSentinelBiographyPresent(t, artistSvc, a.ID)
}
