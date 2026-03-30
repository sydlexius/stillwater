package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetPreferences_ReturnsDefaults(t *testing.T) {
	r, _, userID := testRouterWithAuth(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/preferences", nil)
	req = withUserCtx(req, userID)
	w := httptest.NewRecorder()

	r.handleGetPreferences(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var prefs map[string]string
	if err := json.NewDecoder(w.Body).Decode(&prefs); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	// Verify all default keys are present with correct values.
	for key, def := range preferenceDefaults {
		got, ok := prefs[key]
		if !ok {
			t.Errorf("missing default key %q", key)
			continue
		}
		if got != def.defaultValue {
			t.Errorf("key %q: expected default %q, got %q", key, def.defaultValue, got)
		}
	}

	// Verify no extra keys beyond the defined defaults.
	if len(prefs) != len(preferenceDefaults) {
		t.Errorf("expected %d keys, got %d", len(preferenceDefaults), len(prefs))
	}
}

func TestUpdatePreference_ThenGet(t *testing.T) {
	r, _, userID := testRouterWithAuth(t)

	// PUT a preference.
	body := `{"value":"light"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/preferences/theme", strings.NewReader(body))
	req.SetPathValue("key", "theme")
	req = withUserCtx(req, userID)
	w := httptest.NewRecorder()

	r.handleUpdatePreference(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("PUT expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// GET and verify the updated value.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/preferences", nil)
	req = withUserCtx(req, userID)
	w = httptest.NewRecorder()

	r.handleGetPreferences(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var prefs map[string]string
	if err := json.NewDecoder(w.Body).Decode(&prefs); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if prefs["theme"] != "light" {
		t.Errorf("expected theme=light, got %q", prefs["theme"])
	}

	// Other defaults should still be present.
	if prefs["glass_intensity"] != "medium" {
		t.Errorf("expected glass_intensity=medium, got %q", prefs["glass_intensity"])
	}
}

func TestUpdatePreference_RejectsInvalidKey(t *testing.T) {
	r, _, userID := testRouterWithAuth(t)

	body := `{"value":"anything"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/preferences/nonexistent_key", strings.NewReader(body))
	req.SetPathValue("key", "nonexistent_key")
	req = withUserCtx(req, userID)
	w := httptest.NewRecorder()

	r.handleUpdatePreference(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["error"] != "unknown preference key" {
		t.Errorf("unexpected error message: %q", resp["error"])
	}
}

func TestUpdatePreference_UpsertOverwrites(t *testing.T) {
	r, _, userID := testRouterWithAuth(t)

	// First PUT: set theme to light.
	body := `{"value":"light"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/preferences/theme", strings.NewReader(body))
	req.SetPathValue("key", "theme")
	req = withUserCtx(req, userID)
	w := httptest.NewRecorder()
	r.handleUpdatePreference(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("first PUT expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Second PUT: overwrite to system.
	body = `{"value":"system"}`
	req = httptest.NewRequest(http.MethodPut, "/api/v1/preferences/theme", strings.NewReader(body))
	req.SetPathValue("key", "theme")
	req = withUserCtx(req, userID)
	w = httptest.NewRecorder()
	r.handleUpdatePreference(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("second PUT expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// GET and verify the latest value wins.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/preferences", nil)
	req = withUserCtx(req, userID)
	w = httptest.NewRecorder()
	r.handleGetPreferences(w, req)

	var prefs map[string]string
	if err := json.NewDecoder(w.Body).Decode(&prefs); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if prefs["theme"] != "system" {
		t.Errorf("expected theme=system after upsert, got %q", prefs["theme"])
	}
}

func TestGetPreferences_Unauthenticated(t *testing.T) {
	r, _, _ := testRouterWithAuth(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/preferences", nil)
	// No withUserCtx -- unauthenticated.
	w := httptest.NewRecorder()
	r.handleGetPreferences(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdatePreference_Unauthenticated(t *testing.T) {
	r, _, _ := testRouterWithAuth(t)

	body := `{"value":"light"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/preferences/theme", strings.NewReader(body))
	req.SetPathValue("key", "theme")
	// No withUserCtx -- unauthenticated.
	w := httptest.NewRecorder()
	r.handleUpdatePreference(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdatePreference_RejectsInvalidValue(t *testing.T) {
	r, _, userID := testRouterWithAuth(t)

	body := `{"value":"neon_pink"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/preferences/theme", strings.NewReader(body))
	req.SetPathValue("key", "theme")
	req = withUserCtx(req, userID)
	w := httptest.NewRecorder()

	r.handleUpdatePreference(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["error"] != "invalid value for preference theme" {
		t.Errorf("unexpected error message: %q", resp["error"])
	}
}
