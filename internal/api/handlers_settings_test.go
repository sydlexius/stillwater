package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleUpdateSettings_CacheMaxSize_Invalid(t *testing.T) {
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
	r, _ := testRouter(t)
	body := `{"cache.image.max_size_mb": "0"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.handleUpdateSettings(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for zero (unlimited), got %d: %s", w.Code, w.Body.String())
	}
}
