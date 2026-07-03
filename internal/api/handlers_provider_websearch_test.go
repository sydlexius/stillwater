package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
)

// isWebSearchImageFieldForTest mirrors the provider package's unexported
// image-field predicate so these api-package tests can filter priority rows.
func isWebSearchImageFieldForTest(field string) bool {
	switch field {
	case "thumb", "fanart", "logo", "banner":
		return true
	default:
		return false
	}
}

// TestHandleSetWebSearchEnabled_InvalidProvider covers the 400 branch when
// the path provider name is not a known web search provider.
func TestHandleSetWebSearchEnabled_InvalidProvider(t *testing.T) {
	t.Parallel()
	r := testRouterWithMirror(t)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/providers/websearch/notreal/toggle",
		strings.NewReader(`{"enabled":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", "notreal")
	w := httptest.NewRecorder()
	r.handleSetWebSearchEnabled(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

// TestHandleSetWebSearchEnabled_InvalidJSON covers the 400 branch when the
// JSON request body fails to decode.
func TestHandleSetWebSearchEnabled_InvalidJSON(t *testing.T) {
	t.Parallel()
	r := testRouterWithMirror(t)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/providers/websearch/duckduckgo/toggle",
		strings.NewReader(`{not json`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", "duckduckgo")
	w := httptest.NewRecorder()
	r.handleSetWebSearchEnabled(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

// TestHandleSetWebSearchEnabled_InvalidForm covers the 400 branch when a
// non-JSON request has a malformed form body.
func TestHandleSetWebSearchEnabled_InvalidForm(t *testing.T) {
	t.Parallel()
	r := testRouterWithMirror(t)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/providers/websearch/duckduckgo/toggle",
		strings.NewReader("%zz"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("name", "duckduckgo")
	w := httptest.NewRecorder()
	r.handleSetWebSearchEnabled(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

// TestHandleSetWebSearchEnabled_EnableJSON covers the enable path: the
// setting is persisted, the provider is added to every image-field
// priority list at the lowest position (append), and a plain JSON status
// response is returned for a non-HTMX caller.
func TestHandleSetWebSearchEnabled_EnableJSON(t *testing.T) {
	t.Parallel()
	r := testRouterWithMirror(t)
	ctx := context.Background()

	req := httptest.NewRequest(http.MethodPut, "/api/v1/providers/websearch/duckduckgo/toggle",
		strings.NewReader(`{"enabled":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", "duckduckgo")
	w := httptest.NewRecorder()
	r.handleSetWebSearchEnabled(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["status"] != "ok" {
		t.Errorf("status = %q, want %q", resp["status"], "ok")
	}

	statuses, err := r.providerSettings.ListWebSearchStatuses(ctx)
	if err != nil {
		t.Fatalf("ListWebSearchStatuses: %v", err)
	}
	found := false
	for _, s := range statuses {
		if s.Name == provider.NameDuckDuckGo {
			found = true
			if !s.Enabled {
				t.Error("expected duckduckgo to be enabled after toggle")
			}
		}
	}
	if !found {
		t.Fatal("duckduckgo not present in ListWebSearchStatuses")
	}

	priorities, err := r.providerSettings.GetPriorities(ctx)
	if err != nil {
		t.Fatalf("GetPriorities: %v", err)
	}
	for _, pri := range priorities {
		if !isWebSearchImageFieldForTest(pri.Field) {
			continue
		}
		if !pri.Contains(provider.NameDuckDuckGo) {
			t.Errorf("field %q: expected duckduckgo to be added to priority list, got %v", pri.Field, pri.Providers)
		}
		if pri.Providers[len(pri.Providers)-1] != provider.NameDuckDuckGo {
			t.Errorf("field %q: expected duckduckgo appended at lowest position, got %v", pri.Field, pri.Providers)
		}
	}
}

// TestHandleSetWebSearchEnabled_DisableForm covers the disable path via a
// form-encoded body: the provider is removed from every image-field
// priority list it was previously added to.
func TestHandleSetWebSearchEnabled_DisableForm(t *testing.T) {
	t.Parallel()
	r := testRouterWithMirror(t)
	ctx := context.Background()

	// Seed: enable first so there is something to remove.
	if err := r.providerSettings.SetWebSearchEnabledAndSyncPriorities(ctx, provider.NameDuckDuckGo, true); err != nil {
		t.Fatalf("seeding SetWebSearchEnabledAndSyncPriorities: %v", err)
	}

	req := httptest.NewRequest(http.MethodPut, "/api/v1/providers/websearch/duckduckgo/toggle",
		strings.NewReader("enabled=false"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("name", "duckduckgo")
	w := httptest.NewRecorder()
	r.handleSetWebSearchEnabled(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	statuses, err := r.providerSettings.ListWebSearchStatuses(ctx)
	if err != nil {
		t.Fatalf("ListWebSearchStatuses: %v", err)
	}
	for _, s := range statuses {
		if s.Name == provider.NameDuckDuckGo && s.Enabled {
			t.Error("expected duckduckgo to be disabled after toggle")
		}
	}

	priorities, err := r.providerSettings.GetPriorities(ctx)
	if err != nil {
		t.Fatalf("GetPriorities: %v", err)
	}
	for _, pri := range priorities {
		if !isWebSearchImageFieldForTest(pri.Field) {
			continue
		}
		if pri.Contains(provider.NameDuckDuckGo) {
			t.Errorf("field %q: expected duckduckgo removed from priority list, got %v", pri.Field, pri.Providers)
		}
	}
}

// TestHandleSetWebSearchEnabled_HTMXRefresh covers the HTMX response branch
// outside the onboarding wizard: an HX-Refresh header, no body assertions
// beyond status.
func TestHandleSetWebSearchEnabled_HTMXRefresh(t *testing.T) {
	t.Parallel()
	r := testRouterWithMirror(t)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/providers/websearch/duckduckgo/toggle",
		strings.NewReader(`{"enabled":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("HX-Request", "true")
	req.SetPathValue("name", "duckduckgo")
	w := httptest.NewRecorder()
	r.handleSetWebSearchEnabled(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if w.Header().Get("HX-Refresh") != "true" {
		t.Errorf("HX-Refresh header = %q, want %q", w.Header().Get("HX-Refresh"), "true")
	}
}

// TestHandleSetWebSearchEnabled_HTMXOnboardingWizard covers the onboarding
// wizard HTMX branch, which renders an OOB card fragment for the toggled
// provider instead of setting HX-Refresh.
func TestHandleSetWebSearchEnabled_HTMXOnboardingWizard(t *testing.T) {
	t.Parallel()
	r := testRouterWithMirror(t)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/providers/websearch/duckduckgo/toggle",
		strings.NewReader(`{"enabled":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-Current-URL", "http://localhost/setup/wizard")
	req.SetPathValue("name", "duckduckgo")
	w := httptest.NewRecorder()
	r.handleSetWebSearchEnabled(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if w.Header().Get("HX-Refresh") == "true" {
		t.Error("expected no HX-Refresh header on the onboarding wizard branch")
	}
	if !strings.Contains(w.Body.String(), "duckduckgo") {
		t.Errorf("expected rendered onboarding fragment to reference duckduckgo; body: %s", w.Body.String())
	}
}

// TestHandleSetWebSearchEnabled_PersistError covers the 500 branch when
// SetWebSearchEnabled fails to persist (DB closed).
func TestHandleSetWebSearchEnabled_PersistError(t *testing.T) {
	t.Parallel()
	r := testRouterWithMirror(t)

	if err := r.db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}

	req := httptest.NewRequest(http.MethodPut, "/api/v1/providers/websearch/duckduckgo/toggle",
		strings.NewReader(`{"enabled":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", "duckduckgo")
	w := httptest.NewRecorder()
	r.handleSetWebSearchEnabled(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusInternalServerError, w.Body.String())
	}
}

// TestRespondWebSearchToggle_OnboardingListError covers the onboarding-wizard
// branch when ListWebSearchStatuses fails: the error is logged and the response
// falls through to the generic HX-Refresh rather than an error page.
func TestRespondWebSearchToggle_OnboardingListError(t *testing.T) {
	t.Parallel()
	r := testRouterWithMirror(t)

	// Close the DB so ListWebSearchStatuses returns an error.
	if err := r.db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}

	req := httptest.NewRequest(http.MethodPut, "/api/v1/providers/websearch/duckduckgo/toggle", nil)
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-Current-URL", "https://example.test/setup/wizard")
	w := httptest.NewRecorder()
	r.respondWebSearchToggle(w, req, provider.NameDuckDuckGo)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if w.Header().Get("HX-Refresh") != "true" {
		t.Errorf("HX-Refresh header = %q, want %q (expected fallback after list error)", w.Header().Get("HX-Refresh"), "true")
	}
}

// TestIsValidWebSearchProviderName exercises the small validity helper
// directly for both the known-good and unknown cases.
func TestIsValidWebSearchProviderName(t *testing.T) {
	t.Parallel()
	if !isValidWebSearchProviderName(provider.NameDuckDuckGo) {
		t.Error("expected duckduckgo to be a valid web search provider")
	}
	if isValidWebSearchProviderName(provider.NameWikipedia) {
		t.Error("expected wikipedia to not be a valid web search provider")
	}
}
