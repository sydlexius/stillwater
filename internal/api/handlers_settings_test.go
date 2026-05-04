package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNormalizeSettingsSection verifies that valid section names pass through
// unchanged and that unknown names fall back to "general".
func TestNormalizeSettingsSection(t *testing.T) {
	t.Parallel()
	valid := []string{
		"general", "providers", "connections", "libraries",
		"automation", "rules", "users", "auth_providers", "maintenance", "logs",
	}
	for _, s := range valid {
		if got := normalizeSettingsSection(s); got != s {
			t.Errorf("normalizeSettingsSection(%q) = %q, want %q", s, got, s)
		}
	}
	for _, bad := range []string{"", "admin", "unknown", "../etc", "appearance", "authentication"} {
		if got := normalizeSettingsSection(bad); got != "general" {
			t.Errorf("normalizeSettingsSection(%q) = %q, want \"general\"", bad, got)
		}
	}
}

func TestHandleUpdateSettings_CacheMaxSize_Invalid(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	body := `{"cache.image.max_size_mb": "-5"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.handleUpdateSettings(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for negative value, got %d", w.Code)
	}
}

func TestHandleUpdateSettings_CacheMaxSize_Valid(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	body := `{"cache.image.max_size_mb": "512"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.handleUpdateSettings(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleUpdateSettings_CacheMaxSize_Zero(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	body := `{"cache.image.max_size_mb": "0"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.handleUpdateSettings(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for zero (unlimited), got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleUpdateSettings_Threshold_Invalid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		value string
	}{
		{"non-integer", "abc"},
		{"negative", "-1"},
		{"above 100", "101"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := testRouter(t)
			body := `{"provider.name_similarity_threshold": "` + tt.value + `"}`
			req := httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader(body))
			w := httptest.NewRecorder()
			r.handleUpdateSettings(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 for %s (%s), got %d: %s", tt.name, tt.value, w.Code, w.Body.String())
			}
		})
	}
}

// TestHandleUpdateSettings_LocalAuthCannotBeDisabled verifies that any attempt
// to set auth.providers.local.enabled to a falsy value is rejected with 400,
// including case and whitespace variants.
func TestHandleUpdateSettings_LocalAuthCannotBeDisabled(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		value string
	}{
		{"false string", "false"},
		{"zero string", "0"},
		{"uppercase False", "False"},
		{"padded false", " false "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := testRouter(t)
			body := `{"auth.providers.local.enabled": "` + tt.value + `"}`
			req := httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader(body))
			w := httptest.NewRecorder()
			r.handleUpdateSettings(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 for value %q, got %d: %s", tt.value, w.Code, w.Body.String())
			}
		})
	}
}

// TestHandleUpdateSettings_LocalAuthEnabled verifies that setting
// auth.providers.local.enabled to true is accepted.
func TestHandleUpdateSettings_LocalAuthEnabled(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	body := `{"auth.providers.local.enabled": "true"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.handleUpdateSettings(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// TestHandleUpdateSettings_BasePath_Invalid verifies the validation rules
// for the editable SW_BASE_PATH override (#1005). Each case covers a rule
// the API documents: must start with "/", must not end with "/", must use
// the allowed character set.
func TestHandleUpdateSettings_BasePath_Invalid(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{"missing leading slash", "stillwater"},
		{"trailing slash", "/stillwater/"},
		{"disallowed chars (space)", "/still water"},
		{"disallowed chars (dot)", "/v1.2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := testRouter(t)
			body := `{"server.base_path": "` + tt.value + `"}`
			req := httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader(body))
			w := httptest.NewRecorder()
			r.handleUpdateSettings(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 for %q, got %d: %s", tt.value, w.Code, w.Body.String())
			}
		})
	}
}

// TestHandleUpdateSettings_BasePath_Valid covers the accepted shapes:
// the canonical "/" and a typical sub-path with hyphens/underscores. The
// follow-up GET asserts the canonical persisted form so a regression that
// returns 200 but stores a non-canonical value still fails.
func TestHandleUpdateSettings_BasePath_Valid(t *testing.T) {
	tests := []struct {
		name      string
		value     string
		canonical string // expected persisted value after canonicalization
	}{
		{"root", "/", "/"},
		{"empty (coerced to /)", "", "/"},
		{"simple sub-path", "/stillwater", "/stillwater"},
		{"hyphen sub-path", "/my-app", "/my-app"},
		{"nested", "/apps/stillwater", "/apps/stillwater"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := testRouter(t)
			body := `{"server.base_path": "` + tt.value + `"}`
			req := httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader(body))
			w := httptest.NewRecorder()
			r.handleUpdateSettings(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("expected 200 for %q, got %d: %s", tt.value, w.Code, w.Body.String())
			}

			// Follow-up GET to assert the canonical persisted value.
			getReq := httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
			getW := httptest.NewRecorder()
			r.handleGetSettings(getW, getReq)
			if getW.Code != http.StatusOK {
				t.Fatalf("GET /settings: status %d, body %s", getW.Code, getW.Body.String())
			}
			var settings map[string]any
			if err := json.Unmarshal(getW.Body.Bytes(), &settings); err != nil {
				t.Fatalf("unmarshal settings: %v", err)
			}
			got, _ := settings["server.base_path"].(string)
			if got != tt.canonical {
				t.Errorf("persisted server.base_path = %q, want canonical %q", got, tt.canonical)
			}
		})
	}
}

func TestHandleUpdateSettings_Threshold_Valid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		value string
	}{
		{"mid-range", "75"},
		{"lower bound", "0"},
		{"upper bound", "100"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := testRouter(t)
			body := `{"provider.name_similarity_threshold": "` + tt.value + `"}`
			req := httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader(body))
			w := httptest.NewRecorder()
			r.handleUpdateSettings(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("expected 200 for %s (%s), got %d: %s", tt.name, tt.value, w.Code, w.Body.String())
			}
		})
	}
}
