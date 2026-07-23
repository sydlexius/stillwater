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
	r.orchestrator = provider.NewOrchestrator(registry, nil, logger, nil)
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

func TestHandleDeezerLink_HTMXRowAndToast(t *testing.T) {
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
	// The success render IS the deezer_id row's outerHTML replacement (the
	// candidate link button targets #field-deezer_id-{id}, not the modal body):
	// it must render that row in place, carrying the persisted value, and fire
	// the success toast.
	if !strings.Contains(body, `id="field-deezer_id-`+a.ID+`"`) {
		t.Errorf("missing in-place deezer_id row render; body=%s", body)
	}
	if !strings.Contains(body, "4050205") {
		t.Errorf("linked value not rendered in row; body=%s", body)
	}
	if !strings.Contains(body, "data-deezer-toast") || !strings.Contains(body, "showSuccessToast") {
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

// TestHandleDeezerSearch_NotFound covers the 404 branch: a search against a path
// ID that does not resolve to an artist must 404 (the orchestrator is wired so
// the handler reaches the GetByID lookup rather than short-circuiting on 503).
func TestHandleDeezerSearch_NotFound(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	installDeezerOrchestrator(t, r, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/nope/deezer/search", strings.NewReader(`{"query":"x"}`))
	req.SetPathValue("id", "nope")
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleDeezerSearch(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleDeezerSearch_ServiceUnavailable covers the 503 branch: when the
// orchestrator is not configured the search handler must report the service as
// unavailable rather than attempting a lookup.
func TestHandleDeezerSearch_ServiceUnavailable(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	r.orchestrator = nil // no provider orchestrator wired
	a := addTestArtist(t, artistSvc, "No Orchestrator DZ")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/deezer/search", strings.NewReader(`{"query":"x"}`))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleDeezerSearch(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleDeezerLink_ServiceUnavailable covers the 503 branch: when the artist
// service is not configured the link handler must report 503.
func TestHandleDeezerLink_ServiceUnavailable(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	r.artistService = nil // no artist service wired

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/x/deezer/link", strings.NewReader(`{"deezer_id":"4050205"}`))
	req.SetPathValue("id", "x")
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleDeezerLink(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleDeezerLink_Blocked409 covers the 409 branch: with an active,
// denying conflict gate, linking an EXISTING artist must be blocked with 409
// (the image-write the refresh may perform is gated on the conflict ledger).
func TestHandleDeezerLink_Blocked409(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	d := conflict.NewBlockingForTest(testDiscardLogger())
	r.conflictDetector = d
	r.conflictGate = conflict.NewGate(d)
	a := addTestArtist(t, artistSvc, "Gate Blocked DZ")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/deezer/link", strings.NewReader(`{"deezer_id":"4050205"}`))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleDeezerLink(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", w.Code, w.Body.String())
	}
	// The blocked link must not persist the Deezer ID.
	reloaded, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.DeezerID != "" {
		t.Errorf("DeezerID = %q, want empty after blocked link", reloaded.DeezerID)
	}
}

// TestHandleDeezerLink_GateActiveNonexistentArtist404 asserts finding #1's
// corrected ordering: the artist-existence (404) check must run BEFORE the
// conflict-gate (409) check. With the gate ACTIVE and denying, linking a
// NON-EXISTENT artist must return 404 (not 409) -- an unknown ID is not masked
// by the gate block.
func TestHandleDeezerLink_GateActiveNonexistentArtist404(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	d := conflict.NewBlockingForTest(testDiscardLogger())
	r.conflictDetector = d
	r.conflictGate = conflict.NewGate(d)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/nope/deezer/link", strings.NewReader(`{"deezer_id":"4050205"}`))
	req.SetPathValue("id", "nope")
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleDeezerLink(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (404 must precede 409); body=%s", w.Code, w.Body.String())
	}
}

// TestHandleDeezerIdentify_RendersModal asserts the identify endpoint returns
// the modal body: a search form pre-filled with the artist name that POSTs to
// the Deezer search endpoint.
func TestHandleDeezerIdentify_RendersModal(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Identify Me DZ")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/deezer/identify", nil)
	req.SetPathValue("id", a.ID)
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleDeezerIdentify(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// The form must target the Deezer search endpoint and pre-fill the name.
	if !strings.Contains(body, "/api/v1/artists/"+a.ID+"/deezer/search") {
		t.Errorf("identify modal missing search action; body=%s", body)
	}
	if !strings.Contains(body, "Identify Me DZ") {
		t.Errorf("identify modal missing pre-filled artist name; body=%s", body)
	}
	if !strings.Contains(body, "provider-identify-results-deezer_id") {
		t.Errorf("identify modal missing results container; body=%s", body)
	}
}

// TestHandleDeezerIdentify_NotFound covers the 404 branch: an unknown artist ID
// must 404 rather than render a modal for a non-existent artist.
func TestHandleDeezerIdentify_NotFound(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/nope/deezer/identify", nil)
	req.SetPathValue("id", "nope")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleDeezerIdentify(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleDeezerIdentify_ServiceUnavailable covers the 503 branch: with no
// artist service wired the identify handler must report 503.
func TestHandleDeezerIdentify_ServiceUnavailable(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	r.artistService = nil

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/x/deezer/identify", nil)
	req.SetPathValue("id", "x")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleDeezerIdentify(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
}

// postDeezerLink drives handleDeezerLink with the {id} path value set.
func postDeezerLink(t *testing.T, r *Router, artistID, deezerID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+artistID+"/deezer/link",
		strings.NewReader(`{"deezer_id":"`+deezerID+`"}`))
	req.SetPathValue("id", artistID)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()
	r.handleDeezerLink(w, req)
	return w
}

// TestHandleDeezerLink_LockedArtistSkipsRefresh covers the artist-lock split on
// the Deezer link path (#2754), which autoLinkAndRefresh implements once for
// every link handler: the caller-chosen provider ID is a manual edit the lock
// permits and is still persisted, while the automated refresh that normally
// follows is suppressed and reported as refresh_skipped_locked.
//
// The load-bearing assertion is the ABSENT sentinel biography, not the response
// flag. A router with no orchestrator cannot refresh at all, so a flag-only
// test would stay green against an implementation that set the flag and then
// ran executeRefreshCtx anyway; attachSentinelOrchestrator is what closes that
// hole. Its paired positive control is
// TestHandleDeezerLink_UnlockedArtistStillRefreshes.
func TestHandleDeezerLink_LockedArtistSkipsRefresh(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	attachSentinelOrchestrator(t, r)
	a := addTestArtist(t, artistSvc, "Locked Link DZ")
	if err := artistSvc.Lock(context.Background(), a.ID, "user"); err != nil {
		t.Fatalf("locking artist: %v", err)
	}
	// Precondition: the lock actually stuck. Without it the assertions below
	// would describe the ordinary success path, not the locked one.
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

	w := postDeezerLink(t, r, a.ID, "4050205")
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
	if reloaded.DeezerID != "4050205" {
		t.Errorf("DeezerID = %q, want 4050205; the ID is a manual edit the lock allows", reloaded.DeezerID)
	}
	assertSentinelBiographyAbsent(t, artistSvc, a.ID)
}

// TestHandleDeezerLink_UnlockedArtistStillRefreshes is the positive control for
// the test above: it proves the stubbed orchestrator genuinely lands the
// sentinel biography when no lock intervenes, so the locked test's "biography
// is empty" assertion is a real observation rather than a broken-for-everyone
// no-op.
func TestHandleDeezerLink_UnlockedArtistStillRefreshes(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	attachSentinelOrchestrator(t, r)
	a := addTestArtist(t, artistSvc, "Unlocked Link DZ")
	if a.Locked {
		t.Fatal("precondition failed: artist is locked; this test covers the unlocked path")
	}

	w := postDeezerLink(t, r, a.ID, "4050205")
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
