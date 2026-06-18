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
	"github.com/sydlexius/stillwater/internal/provider"
)

// installDeezerOrchestrator wires a Registry+Orchestrator around a Deezer stub
// provider so the Deezer search/link handlers run end-to-end. The stub reuses
// identifyStubProvider (which implements ReleaseGroupFetcher) so the
// album-comparison branch of enrichDeezerCandidates can be exercised.
func installDeezerOrchestrator(t *testing.T, r *Router,
	search func(ctx context.Context, name string) ([]provider.ArtistSearchResult, error),
	releaseGroups func(ctx context.Context, artistID string) ([]provider.ReleaseGroupInfo, error),
) {
	t.Helper()
	stub := &identifyStubProvider{
		name:             provider.NameDeezer,
		searchFn:         search,
		getReleaseGrpsFn: releaseGroups,
	}
	registry := provider.NewRegistry()
	registry.Register(stub)
	r.providerRegistry = registry
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	r.orchestrator = provider.NewOrchestrator(registry, nil, logger)
}

func TestIsAllDigits(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want bool
	}{
		{"4050205", true},
		{"0", true},
		{"", false},
		{"12a45", false},
		{"a74b1b7f", false},
		{" 123", false},
		{"123 ", false},
	}
	for _, tc := range cases {
		if got := isAllDigits(tc.in); got != tc.want {
			t.Errorf("isAllDigits(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestToDeezerTemplateCandidates(t *testing.T) {
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
	got := toDeezerTemplateCandidates(in)
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

func TestEnrichDeezerCandidates(t *testing.T) {
	t.Parallel()

	t.Run("nil registry falls back to scored conversion", func(t *testing.T) {
		t.Parallel()
		r, _ := testRouter(t)
		r.providerRegistry = nil
		results := []provider.ArtistSearchResult{{Name: "A", ProviderID: "1"}}
		got := r.enrichDeezerCandidates(context.Background(), results, []string{"Album One"})
		if len(got) != 1 || got[0].Reason != "no album data available" {
			t.Errorf("got = %+v, want single fallback candidate", got)
		}
	})

	t.Run("no local albums falls back to scored conversion", func(t *testing.T) {
		t.Parallel()
		r, _ := testRouter(t)
		installDeezerOrchestrator(t, r, nil, func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
			t.Fatal("GetReleaseGroups must not be called when there are no local albums")
			return nil, nil
		})
		results := []provider.ArtistSearchResult{{Name: "A", ProviderID: "1"}}
		got := r.enrichDeezerCandidates(context.Background(), results, nil)
		if len(got) != 1 || got[0].Reason != "no album data available" {
			t.Errorf("got = %+v, want single fallback candidate", got)
		}
	})

	t.Run("provider not registered falls back", func(t *testing.T) {
		t.Parallel()
		r, _ := testRouter(t)
		r.providerRegistry = provider.NewRegistry() // empty
		results := []provider.ArtistSearchResult{{Name: "A", ProviderID: "1"}}
		got := r.enrichDeezerCandidates(context.Background(), results, []string{"Album One"})
		if len(got) != 1 || got[0].Reason != "no album data available" {
			t.Errorf("got = %+v, want single fallback candidate", got)
		}
	})

	t.Run("happy path scores by ProviderID", func(t *testing.T) {
		t.Parallel()
		r, _ := testRouter(t)
		installDeezerOrchestrator(t, r, nil, func(_ context.Context, deezerID string) ([]provider.ReleaseGroupInfo, error) {
			// Keyed on res.ProviderID, NOT MusicBrainzID.
			if deezerID == "111" {
				return []provider.ReleaseGroupInfo{{Title: "Album One"}, {Title: "Album Two"}}, nil
			}
			if deezerID == "222" {
				return []provider.ReleaseGroupInfo{{Title: "Album One"}, {Title: "Unrelated"}}, nil
			}
			return nil, nil
		})
		results := []provider.ArtistSearchResult{
			{Name: "Perfect", ProviderID: "111", Score: 100},
			{Name: "Partial", ProviderID: "222", Score: 80},
		}
		got := r.enrichDeezerCandidates(context.Background(), results, []string{"Album One", "Album Two"})
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
		installDeezerOrchestrator(t, r, nil, func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
			calls++
			return []provider.ReleaseGroupInfo{{Title: "Album One"}}, nil
		})
		results := []provider.ArtistSearchResult{{Name: "NoID"}}
		got := r.enrichDeezerCandidates(context.Background(), results, []string{"Album One"})
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
		installDeezerOrchestrator(t, r, nil, func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
			return nil, errors.New("deezer boom")
		})
		results := []provider.ArtistSearchResult{{Name: "X", ProviderID: "9"}}
		got := r.enrichDeezerCandidates(context.Background(), results, []string{"a"})
		if len(got) != 1 || got[0].AlbumComparison != nil {
			t.Errorf("got = %+v, want candidate with nil AlbumComparison after fetch error", got)
		}
	})

	t.Run("caps lookups at 3 candidates", func(t *testing.T) {
		t.Parallel()
		r, _ := testRouter(t)
		var calls int
		installDeezerOrchestrator(t, r, nil, func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
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
		got := r.enrichDeezerCandidates(context.Background(), results, []string{"a"})
		if len(got) != 5 {
			t.Fatalf("len = %d, want 5", len(got))
		}
		if calls != 3 {
			t.Errorf("GetReleaseGroups calls = %d, want 3 (cap)", calls)
		}
	})
}

func TestHandleDeezerSearch_MissingQuery(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	installDeezerOrchestrator(t, r, nil, nil)
	a := addTestArtist(t, artistSvc, "No Query")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/deezer/search", strings.NewReader(`{}`))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleDeezerSearch(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleDeezerSearch_JSONResults(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	installDeezerOrchestrator(t, r,
		func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
			return []provider.ArtistSearchResult{
				{Name: "Radiohead", ProviderID: "4050205", Source: "deezer", Score: 100},
			}, nil
		}, nil)
	a := addTestArtist(t, artistSvc, "Radiohead DZ")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/deezer/search", strings.NewReader(`{"query":"Radiohead"}`))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleDeezerSearch(w, req)
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

func TestHandleDeezerSearch_ProviderErrorJSON(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	installDeezerOrchestrator(t, r,
		func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
			return nil, errors.New("api.deezer.com: 429 rate limited")
		}, nil)
	a := addTestArtist(t, artistSvc, "Rate Limited DZ")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/deezer/search", strings.NewReader(`{"query":"x"}`))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleDeezerSearch(w, req)
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
	if resp.ProviderError != provider.NameDeezer.DisplayName() {
		t.Errorf("provider_error = %q, want %q", resp.ProviderError, provider.NameDeezer.DisplayName())
	}
}

func TestHandleDeezerSearch_HTMXFragment(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	installDeezerOrchestrator(t, r,
		func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
			return []provider.ArtistSearchResult{
				{Name: "Radiohead", ProviderID: "4050205", Source: "deezer", Score: 100},
			}, nil
		}, nil)
	a := addTestArtist(t, artistSvc, "Radiohead HTMX")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/deezer/search", strings.NewReader(`{"query":"Radiohead"}`))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("HX-Request", "true")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleDeezerSearch(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "/deezer/link") {
		t.Errorf("HTMX fragment missing link action; body=%s", body)
	}
	if !strings.Contains(body, "4050205") {
		t.Errorf("HTMX fragment missing candidate ID; body=%s", body)
	}
}

func TestHandleDeezerSearch_HTMXNoMatches(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	installDeezerOrchestrator(t, r,
		func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
			return nil, nil
		}, nil)
	a := addTestArtist(t, artistSvc, "No Match DZ")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/deezer/search", strings.NewReader(`{"query":"nope"}`))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("HX-Request", "true")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleDeezerSearch(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	// The cancel control is always present; the no-match copy must appear too.
	if !strings.Contains(w.Body.String(), "No Deezer matches found") {
		t.Errorf("HTMX no-match fragment missing copy; body=%s", w.Body.String())
	}
}

func TestHandleDeezerSearch_AlbumComparisonFromDisk(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	installDeezerOrchestrator(t, r,
		func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
			return []provider.ArtistSearchResult{{Name: "Has Albums", ProviderID: "555", Score: 60}}, nil
		},
		func(_ context.Context, deezerID string) ([]provider.ReleaseGroupInfo, error) {
			if deezerID == "555" {
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
	a := &artist.Artist{Name: "Has Albums", SortName: "Has Albums", Type: "group", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("create: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/deezer/search", strings.NewReader(`{"query":"Has Albums"}`))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleDeezerSearch(w, req)
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

func TestHandleDeezerLink_Success(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Link Me DZ")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/deezer/link", strings.NewReader(`{"deezer_id":"4050205"}`))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleDeezerLink(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	reloaded, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.DeezerID != "4050205" {
		t.Errorf("DeezerID = %q, want 4050205", reloaded.DeezerID)
	}
}

func TestHandleDeezerLink_HTMXOOBSwap(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Link HTMX DZ")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/deezer/link", strings.NewReader(`{"deezer_id":"4050205"}`))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("HX-Request", "true")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleDeezerLink(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// Must OOB-swap the deezer_id row and flip the trigger button.
	if !strings.Contains(body, "outerHTML:#field-deezer_id-"+a.ID) {
		t.Errorf("missing OOB deezer_id row swap; body=%s", body)
	}
	if !strings.Contains(body, "deezer-match-button-"+a.ID) {
		t.Errorf("missing OOB button swap; body=%s", body)
	}
}

func TestHandleDeezerLink_BadID(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Bad ID DZ")

	for _, payload := range []string{`{}`, `{"deezer_id":"abc"}`, `{"deezer_id":""}`} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/deezer/link", strings.NewReader(payload))
		req.SetPathValue("id", a.ID)
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(testI18nCtx(t, req.Context()))
		w := httptest.NewRecorder()

		r.handleDeezerLink(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("payload %s: status = %d, want 400; body=%s", payload, w.Code, w.Body.String())
		}
	}
}

func TestHandleDeezerLink_NotFound(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/nope/deezer/link", strings.NewReader(`{"deezer_id":"4050205"}`))
	req.SetPathValue("id", "nope")
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleDeezerLink(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}
