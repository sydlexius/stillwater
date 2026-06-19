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

// installAudioDBOrchestrator wires a Registry+Orchestrator around an AudioDB
// stub provider (for the by-name search) AND a MusicBrainz stub provider (for
// the cross-MBID album comparison). AudioDB search results carry a MusicBrainz
// ID, so enrichAndScoreTier2 scores them via the MusicBrainz ReleaseGroupFetcher
// rather than an AudioDB-specific fetcher. The releaseGroups hook is attached to
// the MusicBrainz stub and keyed on res.MusicBrainzID.
func installAudioDBOrchestrator(t *testing.T, r *Router,
	search func(ctx context.Context, name string) ([]provider.ArtistSearchResult, error),
	releaseGroups func(ctx context.Context, mbid string) ([]provider.ReleaseGroupInfo, error),
) {
	t.Helper()
	audiodbStub := &identifyStubProvider{
		name:     provider.NameAudioDB,
		searchFn: search,
	}
	mbStub := &identifyStubProvider{
		name:             provider.NameMusicBrainz,
		getReleaseGrpsFn: releaseGroups,
	}
	registry := provider.NewRegistry()
	registry.Register(audiodbStub)
	registry.Register(mbStub)
	r.providerRegistry = registry
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	r.orchestrator = provider.NewOrchestrator(registry, nil, logger)
}

func TestToAudioDBTemplateCandidates(t *testing.T) {
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
	got := toAudioDBTemplateCandidates(in)
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

func TestHandleAudioDBSearch_MissingQuery(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	installAudioDBOrchestrator(t, r, nil, nil)
	a := addTestArtist(t, artistSvc, "No Query")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/audiodb/search", strings.NewReader(`{}`))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleAudioDBSearch(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleAudioDBSearch_JSONResults(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	installAudioDBOrchestrator(t, r,
		func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
			return []provider.ArtistSearchResult{
				{Name: "Radiohead", ProviderID: "111493", Source: "audiodb", Score: 100},
			}, nil
		}, nil)
	a := addTestArtist(t, artistSvc, "Radiohead ADB")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/audiodb/search", strings.NewReader(`{"query":"Radiohead"}`))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleAudioDBSearch(w, req)
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

func TestHandleAudioDBSearch_ProviderErrorJSON(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	installAudioDBOrchestrator(t, r,
		func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
			return nil, errors.New("theaudiodb.com: 429 rate limited")
		}, nil)
	a := addTestArtist(t, artistSvc, "Rate Limited ADB")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/audiodb/search", strings.NewReader(`{"query":"x"}`))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleAudioDBSearch(w, req)
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
	if resp.ProviderError != provider.NameAudioDB.DisplayName() {
		t.Errorf("provider_error = %q, want %q", resp.ProviderError, provider.NameAudioDB.DisplayName())
	}
}

func TestHandleAudioDBSearch_HTMXFragment(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	installAudioDBOrchestrator(t, r,
		func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
			return []provider.ArtistSearchResult{
				{Name: "Radiohead", ProviderID: "111493", Source: "audiodb", Score: 100},
			}, nil
		}, nil)
	a := addTestArtist(t, artistSvc, "Radiohead HTMX ADB")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/audiodb/search", strings.NewReader(`{"query":"Radiohead"}`))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("HX-Request", "true")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleAudioDBSearch(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// The candidate link button must target the audiodb_id ROW (outerHTML swap),
	// not the modal body, and close the modal after the request.
	if !strings.Contains(body, "/audiodb/link") {
		t.Errorf("HTMX fragment missing link action; body=%s", body)
	}
	if !strings.Contains(body, `hx-target="#field-audiodb_id-`+a.ID+`"`) {
		t.Errorf("HTMX fragment link must target the audiodb_id field row; body=%s", body)
	}
	if !strings.Contains(body, `hx-swap="outerHTML"`) {
		t.Errorf("HTMX fragment link must outerHTML-swap the row; body=%s", body)
	}
	if !strings.Contains(body, "hideFieldProviderModal()") {
		t.Errorf("HTMX fragment link must close the modal after request; body=%s", body)
	}
	if !strings.Contains(body, "111493") {
		t.Errorf("HTMX fragment missing candidate ID; body=%s", body)
	}
}

func TestHandleAudioDBSearch_HTMXProviderError(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	installAudioDBOrchestrator(t, r,
		func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
			return nil, errors.New("theaudiodb.com: 503 service unavailable")
		}, nil)
	a := addTestArtist(t, artistSvc, "Provider Error HTMX ADB")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/audiodb/search", strings.NewReader(`{"query":"x"}`))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("HX-Request", "true")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleAudioDBSearch(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "unavailable") {
		t.Errorf("HTMX provider-error fragment missing error banner; body=%s", body)
	}
	if strings.Contains(body, "No TheAudioDB matches found") {
		t.Errorf("HTMX provider-error fragment must not show no-matches copy; body=%s", body)
	}
}

func TestHandleAudioDBSearch_HTMXNoMatches(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	installAudioDBOrchestrator(t, r,
		func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
			return nil, nil
		}, nil)
	a := addTestArtist(t, artistSvc, "No Match ADB")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/audiodb/search", strings.NewReader(`{"query":"nope"}`))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("HX-Request", "true")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleAudioDBSearch(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "No TheAudioDB matches found") {
		t.Errorf("HTMX no-match fragment missing copy; body=%s", w.Body.String())
	}
}

// TestHandleAudioDBSearch_AlbumComparisonFromDisk exercises the cross-MBID
// scoring path: AudioDB search returns a result carrying a MusicBrainz ID, and
// the MusicBrainz release-group fetcher (keyed on that MBID) drives the album
// comparison against on-disk album subdirectories.
func TestHandleAudioDBSearch_AlbumComparisonFromDisk(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	installAudioDBOrchestrator(t, r,
		func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
			return []provider.ArtistSearchResult{
				{Name: "Has Albums", ProviderID: "555", MusicBrainzID: "mbid-555", Score: 60},
			}, nil
		},
		func(_ context.Context, mbid string) ([]provider.ReleaseGroupInfo, error) {
			if mbid == "mbid-555" {
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
	a := &artist.Artist{Name: "Has Albums ADB", SortName: "Has Albums ADB", Type: "group", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("create: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/audiodb/search", strings.NewReader(`{"query":"Has Albums"}`))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleAudioDBSearch(w, req)
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

func TestHandleAudioDBSearch_NotFound(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	installAudioDBOrchestrator(t, r, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/nope/audiodb/search", strings.NewReader(`{"query":"x"}`))
	req.SetPathValue("id", "nope")
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleAudioDBSearch(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleAudioDBSearch_ServiceUnavailable(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	r.orchestrator = nil // no provider orchestrator wired
	a := addTestArtist(t, artistSvc, "No Orchestrator ADB")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/audiodb/search", strings.NewReader(`{"query":"x"}`))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleAudioDBSearch(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleAudioDBLink_Success(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Link Me ADB")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/audiodb/link", strings.NewReader(`{"audiodb_id":"111493"}`))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleAudioDBLink(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	reloaded, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.AudioDBID != "111493" {
		t.Errorf("AudioDBID = %q, want 111493", reloaded.AudioDBID)
	}
}

func TestHandleAudioDBLink_HTMXRowAndToast(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Link HTMX ADB")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/audiodb/link", strings.NewReader(`{"audiodb_id":"111493"}`))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("HX-Request", "true")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleAudioDBLink(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// The success render IS the audiodb_id row's outerHTML replacement (the
	// candidate link button targets #field-audiodb_id-{id}, not the modal body).
	if !strings.Contains(body, `id="field-audiodb_id-`+a.ID+`"`) {
		t.Errorf("missing in-place audiodb_id row render; body=%s", body)
	}
	if !strings.Contains(body, "111493") {
		t.Errorf("linked value not rendered in row; body=%s", body)
	}
	if !strings.Contains(body, "data-audiodb-toast") || !strings.Contains(body, "showSuccessToast") {
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

func TestHandleAudioDBLink_BadID(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Bad ID ADB")

	for _, payload := range []string{`{}`, `{"audiodb_id":"abc"}`, `{"audiodb_id":""}`} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/audiodb/link", strings.NewReader(payload))
		req.SetPathValue("id", a.ID)
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(testI18nCtx(t, req.Context()))
		w := httptest.NewRecorder()

		r.handleAudioDBLink(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("payload %s: status = %d, want 400; body=%s", payload, w.Code, w.Body.String())
		}
	}
}

func TestHandleAudioDBLink_NotFound(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/nope/audiodb/link", strings.NewReader(`{"audiodb_id":"111493"}`))
	req.SetPathValue("id", "nope")
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleAudioDBLink(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleAudioDBLink_ServiceUnavailable(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	r.artistService = nil // no artist service wired

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/x/audiodb/link", strings.NewReader(`{"audiodb_id":"111493"}`))
	req.SetPathValue("id", "x")
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleAudioDBLink(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleAudioDBLink_FieldLocked409 covers Guard 1: a pinned audiodb_id field
// must refuse the identify-flow write with 409 (field_locked) and not overwrite
// the locked value.
func TestHandleAudioDBLink_FieldLocked409(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	a := &artist.Artist{
		Name:         "Locked ADB",
		SortName:     "Locked ADB",
		Type:         "group",
		AudioDBID:    "111",
		LockedFields: []string{"audiodb_id"},
	}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("create: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/audiodb/link", strings.NewReader(`{"audiodb_id":"111493"}`))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleAudioDBLink(w, req)
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
	if resp.Error != "field_locked" || resp.Field != "audiodb_id" {
		t.Errorf("unexpected 409 payload: %+v", resp)
	}
	// The locked ID must survive untouched.
	reloaded, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.AudioDBID != "111" {
		t.Errorf("AudioDBID = %q, want 111 (locked value preserved)", reloaded.AudioDBID)
	}
}

// TestHandleAudioDBLink_Blocked409 covers Guard 2: with an active, denying
// conflict gate, linking an EXISTING artist must be blocked with 409 (the NFO
// write the refresh may perform is gated on the conflict ledger).
func TestHandleAudioDBLink_Blocked409(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	d := conflict.NewBlockingForTest(testDiscardLogger())
	r.conflictDetector = d
	r.conflictGate = conflict.NewGate(d)
	a := addTestArtist(t, artistSvc, "Gate Blocked ADB")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/audiodb/link", strings.NewReader(`{"audiodb_id":"111493"}`))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleAudioDBLink(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", w.Code, w.Body.String())
	}
	// The blocked link must not persist the AudioDB ID.
	reloaded, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.AudioDBID != "" {
		t.Errorf("AudioDBID = %q, want empty after blocked link", reloaded.AudioDBID)
	}
}

// TestHandleAudioDBLink_GateActiveNonexistentArtist404 asserts the 404 check
// runs BEFORE the 409 gate: with the gate ACTIVE and denying, linking a
// NON-EXISTENT artist must return 404 (not 409).
func TestHandleAudioDBLink_GateActiveNonexistentArtist404(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	d := conflict.NewBlockingForTest(testDiscardLogger())
	r.conflictDetector = d
	r.conflictGate = conflict.NewGate(d)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/nope/audiodb/link", strings.NewReader(`{"audiodb_id":"111493"}`))
	req.SetPathValue("id", "nope")
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleAudioDBLink(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (404 must precede 409); body=%s", w.Code, w.Body.String())
	}
}

// TestHandleAudioDBIdentify_RendersModal asserts the identify endpoint returns
// the modal body: a search form pre-filled with the artist name that POSTs to
// the AudioDB search endpoint.
func TestHandleAudioDBIdentify_RendersModal(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Identify Me ADB")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/audiodb/identify", nil)
	req.SetPathValue("id", a.ID)
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleAudioDBIdentify(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "/api/v1/artists/"+a.ID+"/audiodb/search") {
		t.Errorf("identify modal missing search action; body=%s", body)
	}
	if !strings.Contains(body, "Identify Me ADB") {
		t.Errorf("identify modal missing pre-filled artist name; body=%s", body)
	}
	if !strings.Contains(body, "provider-identify-results-audiodb_id") {
		t.Errorf("identify modal missing results container; body=%s", body)
	}
}

func TestHandleAudioDBIdentify_NotFound(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/nope/audiodb/identify", nil)
	req.SetPathValue("id", "nope")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleAudioDBIdentify(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleAudioDBIdentify_ServiceUnavailable(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	r.artistService = nil

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/x/audiodb/identify", nil)
	req.SetPathValue("id", "x")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleAudioDBIdentify(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
}
