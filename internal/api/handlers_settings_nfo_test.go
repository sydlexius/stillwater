package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/nfo"
)

func TestHandleGetNFOOutput_Defaults(t *testing.T) {
	r, _ := testRouter(t)
	// Attach NFO settings service since testRouter does not set it up
	r.nfoSettingsService = nfo.NewNFOSettingsService(r.db, r.logger)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings/nfo-output", nil)
	w := httptest.NewRecorder()
	r.handleGetNFOOutput(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var fm nfo.NFOFieldMap
	if err := json.NewDecoder(w.Body).Decode(&fm); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if !fm.DefaultBehavior {
		t.Error("expected DefaultBehavior=true")
	}
	if fm.MoodsAsStyles {
		t.Error("expected MoodsAsStyles=false")
	}
}

func TestHandleUpdateNFOOutput_MoodsAsStyles(t *testing.T) {
	r, _ := testRouter(t)
	r.nfoSettingsService = nfo.NewNFOSettingsService(r.db, r.logger)

	body := `{"default_behavior":false,"moods_as_styles":true,"genre_sources":["genres"]}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings/nfo-output", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.handleUpdateNFOOutput(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Read back and verify
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/settings/nfo-output", nil)
	w2 := httptest.NewRecorder()
	r.handleGetNFOOutput(w2, req2)

	var fm nfo.NFOFieldMap
	if err := json.NewDecoder(w2.Body).Decode(&fm); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if fm.DefaultBehavior {
		t.Error("DefaultBehavior should be false")
	}
	if !fm.MoodsAsStyles {
		t.Error("MoodsAsStyles should be true")
	}
}

func TestHandleUpdateNFOOutput_AdvancedRemap(t *testing.T) {
	r, _ := testRouter(t)
	r.nfoSettingsService = nfo.NewNFOSettingsService(r.db, r.logger)

	body := `{
		"default_behavior": false,
		"moods_as_styles": false,
		"genre_sources": ["genres"],
		"advanced_remap": {
			"genre": ["styles"],
			"style": ["genres", "moods"],
			"mood": []
		}
	}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings/nfo-output", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.handleUpdateNFOOutput(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Read back
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/settings/nfo-output", nil)
	w2 := httptest.NewRecorder()
	r.handleGetNFOOutput(w2, req2)

	var fm nfo.NFOFieldMap
	if err := json.NewDecoder(w2.Body).Decode(&fm); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if fm.AdvancedRemap == nil {
		t.Fatal("AdvancedRemap should not be nil")
	}
	if len(fm.AdvancedRemap["genre"]) != 1 || fm.AdvancedRemap["genre"][0] != "styles" {
		t.Errorf("genre remap = %v, want [styles]", fm.AdvancedRemap["genre"])
	}
}

func TestHandleUpdateNFOOutput_InvalidGenreSource(t *testing.T) {
	r, _ := testRouter(t)
	r.nfoSettingsService = nfo.NewNFOSettingsService(r.db, r.logger)

	body := `{"default_behavior":false,"genre_sources":["invalid"]}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings/nfo-output", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.handleUpdateNFOOutput(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleUpdateNFOOutput_InvalidRemapKey(t *testing.T) {
	r, _ := testRouter(t)
	r.nfoSettingsService = nfo.NewNFOSettingsService(r.db, r.logger)

	body := `{"advanced_remap":{"invalid_element":["genres"]}}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings/nfo-output", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.handleUpdateNFOOutput(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleUpdateNFOOutput_InvalidRemapSource(t *testing.T) {
	r, _ := testRouter(t)
	r.nfoSettingsService = nfo.NewNFOSettingsService(r.db, r.logger)

	body := `{"advanced_remap":{"genre":["invalid_source"]}}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings/nfo-output", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.handleUpdateNFOOutput(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleUpdateNFOOutput_InvalidJSON(t *testing.T) {
	r, _ := testRouter(t)
	r.nfoSettingsService = nfo.NewNFOSettingsService(r.db, r.logger)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings/nfo-output", strings.NewReader("not json"))
	w := httptest.NewRecorder()
	r.handleUpdateNFOOutput(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleGetNFOOutput_NilService(t *testing.T) {
	r, _ := testRouter(t)
	// Explicitly leave nfoSettingsService nil
	r.nfoSettingsService = nil

	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings/nfo-output", nil)
	w := httptest.NewRecorder()
	r.handleGetNFOOutput(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleUpdateNFOOutput_NilService(t *testing.T) {
	r, _ := testRouter(t)
	// Explicitly leave nfoSettingsService nil
	r.nfoSettingsService = nil

	body := `{"default_behavior":true,"genre_sources":["genres"]}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings/nfo-output", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.handleUpdateNFOOutput(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
}
