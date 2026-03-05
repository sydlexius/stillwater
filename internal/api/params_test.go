package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequirePathParam_Present(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/artists/abc123", nil)
	req.SetPathValue("id", "abc123")
	w := httptest.NewRecorder()

	val, ok := RequirePathParam(w, req, "id")
	if !ok {
		t.Fatal("expected ok=true for present path param")
	}
	if val != "abc123" {
		t.Errorf("val = %q, want abc123", val)
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (no write expected)", w.Code)
	}
}

func TestRequirePathParam_Missing(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/artists/", nil)
	w := httptest.NewRecorder()

	val, ok := RequirePathParam(w, req, "id")
	if ok {
		t.Fatal("expected ok=false for missing path param")
	}
	if val != "" {
		t.Errorf("val = %q, want empty", val)
	}
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "missing id") {
		t.Errorf("body %q does not contain 'missing id'", body)
	}
}

func TestDecodeJSON_Valid(t *testing.T) {
	type payload struct {
		Name string `json:"name"`
	}
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{"name":"test"}`))
	w := httptest.NewRecorder()

	var p payload
	if !DecodeJSON(w, req, &p) {
		t.Fatal("expected true for valid JSON")
	}
	if p.Name != "test" {
		t.Errorf("name = %q, want test", p.Name)
	}
}

func TestDecodeJSON_Invalid(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`not-json`))
	w := httptest.NewRecorder()

	var target struct{ Name string }
	if DecodeJSON(w, req, &target) {
		t.Fatal("expected false for invalid JSON")
	}
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "invalid request body") {
		t.Errorf("body %q does not contain expected error message", w.Body.String())
	}
}
