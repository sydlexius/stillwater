package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/provider/musicbrainz"
)

func testRouterWithMirror(t *testing.T) *Router {
	t.Helper()

	db := newTestDB(t)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}

	rateLimiters := provider.NewRateLimiterMap()
	providerSettings := provider.NewSettingsService(db, enc)
	registry := provider.NewRegistry()
	// The musicbrainz adapter's default HTTP client is httpsafe.SafeClient,
	// which blocks loopback addresses (127.0.0.1) -- exactly what
	// httptest.NewServer binds to. Tests inject a plain *http.Client so the
	// mirror auto-test path can reach the loopback fixture. Production
	// wiring is unaffected.
	mbAdapter := musicbrainz.New(rateLimiters, logger)
	mbAdapter.SetHTTPClient(&http.Client{Timeout: 10 * time.Second})
	registry.Register(mbAdapter)

	return NewRouter(RouterDeps{
		SessionSecret:    testSessionSecret,
		ProviderSettings: providerSettings,
		ProviderRegistry: registry,
		RateLimiters:     rateLimiters,
		DB:               db,
		Logger:           logger,
		StaticFS:         os.DirFS("../../web/static"),
	})
}

func TestHandleSetMirror(t *testing.T) {
	t.Parallel()
	// Start a local mirror server so the auto-test in the JSON path succeeds.
	mirrorSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"created":"2024-01-01T00:00:00.000Z","count":0,"offset":0,"artists":[]}`))
	}))
	defer mirrorSrv.Close()

	r := testRouterWithMirror(t)

	mirrorURL := mirrorSrv.URL + "/ws/2"
	body := fmt.Sprintf(`{"base_url":%q,"rate_limit":10}`, mirrorURL)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/providers/musicbrainz/mirror", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/v1/providers/{name}/mirror", r.handleSetMirror)
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["status"] != "saved" {
		t.Errorf("expected status 'saved', got %q", resp["status"])
	}
	if resp["test"] != "ok" {
		t.Errorf("expected test 'ok', got %q", resp["test"])
	}
	if resp["test_error"] != "" {
		t.Errorf("expected empty test_error, got %q", resp["test_error"])
	}

	// Verify settings were persisted.
	ctx := context.Background()
	url, err := r.providerSettings.GetBaseURL(ctx, provider.NameMusicBrainz)
	if err != nil {
		t.Fatalf("GetBaseURL: %v", err)
	}
	if url != mirrorURL {
		t.Errorf("expected persisted base URL %q, got %q", mirrorURL, url)
	}

	limit, err := r.providerSettings.GetRateLimit(ctx, provider.NameMusicBrainz)
	if err != nil {
		t.Fatalf("GetRateLimit: %v", err)
	}
	if limit != 10 {
		t.Errorf("expected persisted rate limit 10, got %v", limit)
	}

	// Verify the adapter was updated.
	p := r.providerRegistry.Get(provider.NameMusicBrainz)
	mirrorable := p.(provider.MirrorableProvider)
	if mirrorable.BaseURL() != mirrorURL {
		t.Errorf("expected adapter base URL updated, got %q", mirrorable.BaseURL())
	}
}

func TestHandleSetMirrorValidation(t *testing.T) {
	t.Parallel()
	r := testRouterWithMirror(t)

	tests := []struct {
		name string
		body string
		want int
	}{
		{"empty body", `{}`, http.StatusBadRequest},
		{"missing base_url", `{"rate_limit":10}`, http.StatusBadRequest},
		{"invalid URL", `{"base_url":"not-a-url"}`, http.StatusBadRequest},
		{"ftp scheme", `{"base_url":"ftp://mirror/ws/2"}`, http.StatusBadRequest},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/v1/providers/{name}/mirror", r.handleSetMirror)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, "/api/v1/providers/musicbrainz/mirror", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != tt.want {
				t.Errorf("status = %d, want %d; body: %s", w.Code, tt.want, w.Body.String())
			}
		})
	}
}

func TestHandleSetMirrorTrailingSlash(t *testing.T) {
	t.Parallel()
	mirrorSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"created":"2024-01-01T00:00:00.000Z","count":0,"offset":0,"artists":[]}`))
	}))
	defer mirrorSrv.Close()

	r := testRouterWithMirror(t)

	// Submit URL with trailing slash; should be normalized.
	mirrorURL := mirrorSrv.URL + "/ws/2/"
	body := fmt.Sprintf(`{"base_url":%q}`, mirrorURL)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/providers/musicbrainz/mirror", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/v1/providers/{name}/mirror", r.handleSetMirror)
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// Verify persisted URL has trailing slash stripped.
	got, err := r.providerSettings.GetBaseURL(context.Background(), provider.NameMusicBrainz)
	if err != nil {
		t.Fatalf("GetBaseURL: %v", err)
	}
	want := strings.TrimRight(mirrorURL, "/")
	if got != want {
		t.Errorf("expected persisted URL %q, got %q", want, got)
	}

	// Verify adapter also has the normalized URL.
	p := r.providerRegistry.Get(provider.NameMusicBrainz)
	if p.(provider.MirrorableProvider).BaseURL() != want {
		t.Errorf("expected adapter URL %q, got %q", want, p.(provider.MirrorableProvider).BaseURL())
	}
}

func TestHandleSetMirrorDefaultRateLimit(t *testing.T) {
	t.Parallel()
	mirrorSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"created":"2024-01-01T00:00:00.000Z","count":0,"offset":0,"artists":[]}`))
	}))
	defer mirrorSrv.Close()

	r := testRouterWithMirror(t)

	// Omit rate_limit; should default to 10.
	body := fmt.Sprintf(`{"base_url":%q}`, mirrorSrv.URL+"/ws/2")
	req := httptest.NewRequest(http.MethodPut, "/api/v1/providers/musicbrainz/mirror", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/v1/providers/{name}/mirror", r.handleSetMirror)
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	limit, err := r.providerSettings.GetRateLimit(context.Background(), provider.NameMusicBrainz)
	if err != nil {
		t.Fatalf("GetRateLimit: %v", err)
	}
	if limit != 10 {
		t.Errorf("expected default rate limit 10, got %v", limit)
	}
}

func TestHandleSetMirrorRateLimitCap(t *testing.T) {
	t.Parallel()
	mirrorSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"created":"2024-01-01T00:00:00.000Z","count":0,"offset":0,"artists":[]}`))
	}))
	defer mirrorSrv.Close()

	r := testRouterWithMirror(t)

	// Rate limit above 100 should be capped.
	body := fmt.Sprintf(`{"base_url":%q,"rate_limit":999}`, mirrorSrv.URL+"/ws/2")
	req := httptest.NewRequest(http.MethodPut, "/api/v1/providers/musicbrainz/mirror", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/v1/providers/{name}/mirror", r.handleSetMirror)
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	limit, err := r.providerSettings.GetRateLimit(context.Background(), provider.NameMusicBrainz)
	if err != nil {
		t.Fatalf("GetRateLimit: %v", err)
	}
	if limit != 100 {
		t.Errorf("expected capped rate limit 100, got %v", limit)
	}
}

func TestHandleDeleteMirror(t *testing.T) {
	t.Parallel()
	r := testRouterWithMirror(t)
	ctx := context.Background()

	// Set mirror first.
	if err := r.providerSettings.SetBaseURL(ctx, provider.NameMusicBrainz, "http://mirror:5000/ws/2"); err != nil {
		t.Fatalf("SetBaseURL: %v", err)
	}
	if err := r.providerSettings.SetRateLimit(ctx, provider.NameMusicBrainz, 20); err != nil {
		t.Fatalf("SetRateLimit: %v", err)
	}
	p := r.providerRegistry.Get(provider.NameMusicBrainz)
	p.(provider.MirrorableProvider).SetBaseURL("http://mirror:5000/ws/2")

	// Delete mirror.
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/providers/musicbrainz/mirror", nil)
	w := httptest.NewRecorder()

	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /api/v1/providers/{name}/mirror", r.handleDeleteMirror)
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// Verify cleared.
	url, err := r.providerSettings.GetBaseURL(ctx, provider.NameMusicBrainz)
	if err != nil {
		t.Fatalf("GetBaseURL: %v", err)
	}
	if url != "" {
		t.Errorf("expected empty base URL after delete, got %q", url)
	}

	// Verify adapter reverted to default.
	mirrorable := p.(provider.MirrorableProvider)
	if mirrorable.BaseURL() != mirrorable.DefaultBaseURL() {
		t.Errorf("expected adapter reverted to default %q, got %q",
			mirrorable.DefaultBaseURL(), mirrorable.BaseURL())
	}
}

func TestHandleSetMirrorUnsupportedProvider(t *testing.T) {
	t.Parallel()
	r := testRouterWithMirror(t)

	body := `{"base_url":"http://mirror:5000/ws/2"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/providers/fanarttv/mirror", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/v1/providers/{name}/mirror", r.handleSetMirror)
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

// TestHandleResetPriorities seeds custom priority + disabled rows, calls the
// reset handler, and asserts GetPriorities matches DefaultPriorities (i.e. no
// stored overrides remain).
func TestHandleResetPriorities(t *testing.T) {
	t.Parallel()
	r := testRouterWithMirror(t)
	ctx := context.Background()

	// Seed: reorder "biography" providers and disable one of them. Both rows
	// land under the `provider.priority.%` key prefix that the handler clears.
	customBio := []provider.ProviderName{provider.NameLastFM, provider.NameWikipedia}
	if err := r.providerSettings.SetPriority(ctx, "biography", customBio); err != nil {
		t.Fatalf("seeding SetPriority: %v", err)
	}
	if err := r.providerSettings.SetDisabledProviders(ctx, "biography", []provider.ProviderName{provider.NameLastFM}); err != nil {
		t.Fatalf("seeding SetDisabledProviders: %v", err)
	}

	// Sanity: the seed actually changed the stored "biography" row.
	before, err := r.providerSettings.GetPriorities(ctx)
	if err != nil {
		t.Fatalf("GetPriorities pre-reset: %v", err)
	}
	var beforeBio provider.FieldPriority
	for _, p := range before {
		if p.Field == "biography" {
			beforeBio = p
			break
		}
	}
	if len(beforeBio.Disabled) != 1 || beforeBio.Disabled[0] != provider.NameLastFM {
		t.Fatalf("seed did not persist disabled list, got %+v", beforeBio.Disabled)
	}

	// Hit the reset endpoint.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/providers/priorities/reset", nil)
	w := httptest.NewRecorder()
	r.handleResetPriorities(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// JSON path: status echoed and priorities fall back to defaults.
	var resp struct {
		Status     string                   `json:"status"`
		Priorities []provider.FieldPriority `json:"priorities"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Status != "reset" {
		t.Errorf("status = %q, want %q", resp.Status, "reset")
	}

	// Validate the handler response payload directly so a serialization
	// regression in resp.Priorities cannot slip past the persisted-state check.
	want := provider.DefaultPriorities()
	if len(resp.Priorities) != len(want) {
		t.Fatalf("response priorities count mismatch: got %d, want %d", len(resp.Priorities), len(want))
	}
	for i := range want {
		if resp.Priorities[i].Field != want[i].Field {
			t.Errorf("response field[%d] = %q, want %q", i, resp.Priorities[i].Field, want[i].Field)
		}
		if !providersEqual(resp.Priorities[i].Providers, want[i].Providers) {
			t.Errorf("response providers[%s] = %v, want %v", want[i].Field, resp.Priorities[i].Providers, want[i].Providers)
		}
		if len(resp.Priorities[i].Disabled) != 0 {
			t.Errorf("response disabled[%s] = %v, want empty", want[i].Field, resp.Priorities[i].Disabled)
		}
	}

	got, err := r.providerSettings.GetPriorities(ctx)
	if err != nil {
		t.Fatalf("GetPriorities post-reset: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("priority count mismatch: got %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Field != want[i].Field {
			t.Errorf("field[%d] = %q, want %q", i, got[i].Field, want[i].Field)
		}
		if !providersEqual(got[i].Providers, want[i].Providers) {
			t.Errorf("providers[%s] = %v, want %v", want[i].Field, got[i].Providers, want[i].Providers)
		}
		if len(got[i].Disabled) != 0 {
			t.Errorf("disabled[%s] = %v, want empty", want[i].Field, got[i].Disabled)
		}
	}

	// HTMX path: response is the rendered chip-rows fragment with the wrapper id.
	htmxReq := httptest.NewRequest(http.MethodPost, "/api/v1/providers/priorities/reset", nil)
	htmxReq.Header.Set("HX-Request", "true")
	htmxW := httptest.NewRecorder()
	r.handleResetPriorities(htmxW, htmxReq)
	if htmxW.Code != http.StatusOK {
		t.Fatalf("htmx status = %d, want %d; body: %s", htmxW.Code, http.StatusOK, htmxW.Body.String())
	}
	if !strings.Contains(htmxW.Body.String(), `id="priority-rows"`) {
		t.Errorf("htmx response missing priority-rows wrapper; body: %s", htmxW.Body.String())
	}
}

// TestHandleResetPriorities_DBError covers the DELETE error branch by closing
// the underlying database before calling the handler.
func TestHandleResetPriorities_DBError(t *testing.T) {
	t.Parallel()
	r := testRouterWithMirror(t)

	// Close the DB to force the DELETE to fail with a 500.
	if err := r.db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/providers/priorities/reset", nil)
	w := httptest.NewRecorder()
	r.handleResetPriorities(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusInternalServerError, w.Body.String())
	}
}

// TestHandleGetProviderConfig verifies that GET /providers/{name}/config
// returns the current verbosity values for a provider that supports them,
// and returns a sensible default when nothing has been stored.
func TestHandleGetProviderConfig(t *testing.T) {
	t.Parallel()
	r := testRouterWithMirror(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/providers/wikipedia/config", nil)
	req.SetPathValue("name", "wikipedia")
	w := httptest.NewRecorder()
	r.handleGetProviderConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp struct {
		Provider  string            `json:"provider"`
		Verbosity map[string]string `json:"verbosity"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Provider != "wikipedia" {
		t.Errorf("provider = %q, want %q", resp.Provider, "wikipedia")
	}
	// Default must be intro (conservative).
	if resp.Verbosity["biography"] != provider.VerbosityIntro {
		t.Errorf("biography verbosity = %q, want %q", resp.Verbosity["biography"], provider.VerbosityIntro)
	}
}

// TestHandleGetProviderConfig_InvalidProvider verifies that an unknown provider
// name returns 400.
func TestHandleGetProviderConfig_InvalidProvider(t *testing.T) {
	t.Parallel()
	r := testRouterWithMirror(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/providers/notreal/config", nil)
	req.SetPathValue("name", "notreal")
	w := httptest.NewRecorder()
	r.handleGetProviderConfig(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// TestHandleSetProviderConfig_HappyPath verifies that
// PUT /providers/{name}/config with a valid JSON body persists the verbosity
// setting and returns 200.
func TestHandleSetProviderConfig_HappyPath(t *testing.T) {
	t.Parallel()
	r := testRouterWithMirror(t)

	body := `{"verbosity_by_field":{"biography":"full"}}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/providers/wikipedia/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", "wikipedia")
	w := httptest.NewRecorder()
	r.handleSetProviderConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// Verify persistence.
	val, err := r.providerSettings.GetFieldVerbosity(context.Background(), provider.NameWikipedia, "biography")
	if err != nil {
		t.Fatalf("GetFieldVerbosity: %v", err)
	}
	if val != provider.VerbosityFull {
		t.Errorf("persisted verbosity = %q, want %q", val, provider.VerbosityFull)
	}
}

// TestHandleSetProviderConfig_FormEncoded verifies that form-encoded data
// (the HTMX path) is also accepted.
func TestHandleSetProviderConfig_FormEncoded(t *testing.T) {
	t.Parallel()
	r := testRouterWithMirror(t)

	body := "verbosity_biography=intro"
	req := httptest.NewRequest(http.MethodPut, "/api/v1/providers/wikipedia/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("name", "wikipedia")
	w := httptest.NewRecorder()
	r.handleSetProviderConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
}

// TestHandleSetProviderConfig_InvalidProvider verifies that an unknown provider
// returns 400.
func TestHandleSetProviderConfig_InvalidProvider(t *testing.T) {
	t.Parallel()
	r := testRouterWithMirror(t)

	body := `{"verbosity_by_field":{"biography":"full"}}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/providers/notreal/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", "notreal")
	w := httptest.NewRecorder()
	r.handleSetProviderConfig(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// TestHandleSetProviderConfig_InvalidField verifies that an unknown field name
// in the request body returns 400.
func TestHandleSetProviderConfig_InvalidField(t *testing.T) {
	t.Parallel()
	r := testRouterWithMirror(t)

	// "nosuchfield" is not a valid verbosity field for Wikipedia.
	body := `{"verbosity_by_field":{"nosuchfield":"full"}}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/providers/wikipedia/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", "wikipedia")
	w := httptest.NewRecorder()
	r.handleSetProviderConfig(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// TestHandleSetProviderConfig_InvalidValue verifies that an unknown verbosity
// value for a valid field returns 400.
func TestHandleSetProviderConfig_InvalidValue(t *testing.T) {
	t.Parallel()
	r := testRouterWithMirror(t)

	body := `{"verbosity_by_field":{"biography":"medium"}}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/providers/wikipedia/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", "wikipedia")
	w := httptest.NewRecorder()
	r.handleSetProviderConfig(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// TestHandleSetProviderConfig_NoVerbosityProvider verifies that providers
// without verbosity options return 400.
func TestHandleSetProviderConfig_NoVerbosityProvider(t *testing.T) {
	t.Parallel()
	r := testRouterWithMirror(t)

	// MusicBrainz has no verbosity options in v1.
	body := `{"verbosity_by_field":{"biography":"full"}}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/providers/musicbrainz/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", "musicbrainz")
	w := httptest.NewRecorder()
	r.handleSetProviderConfig(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func providersEqual(a, b []provider.ProviderName) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestHandleSetProviderConfig_EmptyBody verifies a request carrying no verbosity
// values is rejected with 400, not silently accepted as a 200 no-op.
func TestHandleSetProviderConfig_EmptyBody(t *testing.T) {
	t.Parallel()
	r := testRouterWithMirror(t)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/providers/wikipedia/config", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", "wikipedia")
	w := httptest.NewRecorder()
	r.handleSetProviderConfig(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d (an empty body must be rejected); body: %s",
			w.Code, http.StatusBadRequest, w.Body.String())
	}
}

// TestHandleSetProviderConfig_RoundTrip verifies a PUT through the handler is
// reflected by a subsequent GET through the handler, covering the GET response
// shape (not just direct SettingsService access).
func TestHandleSetProviderConfig_RoundTrip(t *testing.T) {
	t.Parallel()
	r := testRouterWithMirror(t)

	putReq := httptest.NewRequest(http.MethodPut, "/api/v1/providers/wikipedia/config",
		strings.NewReader(`{"verbosity_by_field":{"biography":"full"}}`))
	putReq.Header.Set("Content-Type", "application/json")
	putReq.SetPathValue("name", "wikipedia")
	putW := httptest.NewRecorder()
	r.handleSetProviderConfig(putW, putReq)
	if putW.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body: %s", putW.Code, putW.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/providers/wikipedia/config", nil)
	getReq.SetPathValue("name", "wikipedia")
	getW := httptest.NewRecorder()
	r.handleGetProviderConfig(getW, getReq)
	if getW.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200; body: %s", getW.Code, getW.Body.String())
	}
	var resp struct {
		Provider  string            `json:"provider"`
		Verbosity map[string]string `json:"verbosity"`
	}
	if err := json.Unmarshal(getW.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding GET response: %v", err)
	}
	if resp.Verbosity["biography"] != provider.VerbosityFull {
		t.Errorf("GET verbosity[biography] = %q, want %q", resp.Verbosity["biography"], provider.VerbosityFull)
	}
}

// TestHandleSetProviderConfig_HTMXSuccessFragment verifies an HTMX request gets
// an HTML success fragment, not a raw JSON body swapped into the page.
func TestHandleSetProviderConfig_HTMXSuccessFragment(t *testing.T) {
	t.Parallel()
	r := testRouterWithMirror(t)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/providers/wikipedia/config",
		strings.NewReader("verbosity_biography=full"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	req.SetPathValue("name", "wikipedia")
	w := httptest.NewRecorder()
	r.handleSetProviderConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "<div") {
		t.Errorf("an HTMX request should get an HTML fragment, got: %s", body)
	}
	if strings.Contains(body, `"status"`) {
		t.Errorf("an HTMX response should not be the raw JSON body, got: %s", body)
	}
}
