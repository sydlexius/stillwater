package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
)

// contextCapturingProvider is a minimal Provider implementation that records
// the context passed to GetArtist so tests can verify that language preferences
// were injected by the handler before the orchestrator reaches the provider.
type contextCapturingProvider struct {
	capturedCtx context.Context
}

func (p *contextCapturingProvider) Name() provider.ProviderName { return provider.NameAudioDB }
func (p *contextCapturingProvider) RequiresAuth() bool          { return false }
func (p *contextCapturingProvider) SearchArtist(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
	return nil, nil
}
func (p *contextCapturingProvider) GetArtist(ctx context.Context, _ string) (*provider.ArtistMetadata, error) {
	p.capturedCtx = ctx
	return &provider.ArtistMetadata{Name: "Test Artist", Biography: "Test biography."}, nil
}
func (p *contextCapturingProvider) GetImages(_ context.Context, _ string) ([]provider.ImageResult, error) {
	return nil, nil
}

// TestHandleFieldProviders_LanguageContextInjected verifies that
// handleFieldProviders injects language preferences into the context before
// calling FetchFieldFromProviders (#1848). Before the fix the handler passed
// req.Context() directly; after the fix it calls r.injectMetadataLanguages
// first so each provider receives the user's locale settings.
func TestHandleFieldProviders_LanguageContextInjected(t *testing.T) {
	t.Parallel()

	r, artistSvc := testRouter(t)

	// Use a synthetic user ID. user_preferences has no FK constraint to users,
	// so we can store preferences for any ID without creating a real user row.
	const userID = "field-providers-lang-test-user"

	// Store a French language preference via the preferences handler. This
	// writes a row that injectMetadataLanguages will read when the user is in
	// context.
	body := `{"value":"[\"fr\"]"}`
	prefReq := httptest.NewRequest(http.MethodPut, "/api/v1/preferences/metadata_languages", strings.NewReader(body))
	prefReq.SetPathValue("key", "metadata_languages")
	prefReq = withUserCtx(prefReq, userID)
	prefW := httptest.NewRecorder()
	r.handleUpdatePreference(prefW, prefReq)
	if prefW.Code != http.StatusOK {
		t.Fatalf("storing language preference: expected 200, got %d: %s", prefW.Code, prefW.Body.String())
	}

	// Wire a context-capturing mock provider as AudioDB. AudioDB does not
	// require an API key and is not excluded for the "biography" field, so
	// FetchFieldFromProviders will call it when it appears in the priority list.
	cap := &contextCapturingProvider{}
	reg := provider.NewRegistry()
	reg.Register(cap)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	providerSettings := provider.NewSettingsService(r.db, nil)
	orch := provider.NewOrchestrator(reg, providerSettings, logger)
	r.orchestrator = orch

	a := addTestArtist(t, artistSvc, "Language Injection Test Artist")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/fields/biography/providers", nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("field", "biography")
	req = withUserCtx(req, userID)
	w := httptest.NewRecorder()

	r.handleFieldProviders(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("handleFieldProviders status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	// The mock must have been called, proving FetchFieldFromProviders proceeded
	// to the provider level (i.e., did not fail before reaching it).
	if cap.capturedCtx == nil {
		t.Fatal("provider GetArtist was not called; FetchFieldFromProviders may have failed before reaching the AudioDB provider")
	}

	// Verify the context carries the stored language preference.
	// Before the fix, req.Context() was passed directly and would have had no
	// preferences. With the fix, r.injectMetadataLanguages enriches it from DB.
	langs := provider.MetadataLanguages(cap.capturedCtx)
	if len(langs) == 0 {
		t.Fatal("no language preferences in provider context; injectMetadataLanguages was not called or failed to inject")
	}
	if langs[0] != "fr" {
		t.Errorf("first language preference = %q, want %q", langs[0], "fr")
	}
}

// TestHandleFieldProviders_InvalidField verifies that handleFieldProviders
// rejects an unknown or non-editable field value with 400 Bad Request.
func TestHandleFieldProviders_InvalidField(t *testing.T) {
	t.Parallel()

	r, _ := testRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/any-id/fields/__invalid__/providers", nil)
	req.SetPathValue("id", "any-id")
	req.SetPathValue("field", "__invalid__")
	req = withUserCtx(req, "test-user")
	w := httptest.NewRecorder()

	r.handleFieldProviders(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("handleFieldProviders with invalid field: status = %d, want %d; body=%s",
			w.Code, http.StatusBadRequest, w.Body.String())
	}
}
