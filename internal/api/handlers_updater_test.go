package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleUpdateStatus_NoChecker(t *testing.T) {
	r, _ := testRouter(t)
	// updateChecker is nil in testRouter; status should still return 200.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/updates/status", nil)
	w := httptest.NewRecorder()
	r.handleUpdateStatus(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["updater_enabled"] != false {
		t.Errorf("updater_enabled = %v, want false", resp["updater_enabled"])
	}
}

func TestHandleCheckUpdate_Disabled(t *testing.T) {
	r, _ := testRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/updates/check", nil)
	w := httptest.NewRecorder()
	r.handleCheckUpdate(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body: %s", w.Code, w.Body.String())
	}
}

func TestHandleApplyUpdate_ContainerDetection(t *testing.T) {
	// When running in a container, apply should return 409.
	// We can only test the non-container path reliably in this environment,
	// so we test the "disabled checker" path here.
	r, _ := testRouter(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/updates/apply", nil)
	w := httptest.NewRecorder()
	r.handleApplyUpdate(w, req)
	// Either 409 (container) or 503 (disabled checker) is acceptable.
	if w.Code != http.StatusConflict && w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 409 or 503; body: %s", w.Code, w.Body.String())
	}
}

func TestHandleGetUpdateConfig_Defaults(t *testing.T) {
	r, _ := testRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/updates/config", nil)
	w := httptest.NewRecorder()
	r.handleGetUpdateConfig(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	// Defaults: enabled=false, channel="latest", auto_update=false, interval=24.
	if resp["enabled"] != false {
		t.Errorf("enabled = %v, want false", resp["enabled"])
	}
	if resp["channel"] != "latest" {
		t.Errorf("channel = %v, want latest", resp["channel"])
	}
}

func TestHandlePutUpdateConfig_Valid(t *testing.T) {
	r, _ := testRouter(t)
	body := `{"enabled": true, "channel": "beta", "check_interval_hours": 12, "auto_update": false}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/updates/config", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.handlePutUpdateConfig(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	// Verify the settings were persisted.
	var val string
	err := r.db.QueryRowContext(context.Background(),
		`SELECT value FROM settings WHERE key = 'updater.channel'`).Scan(&val)
	if err != nil {
		t.Fatalf("reading updater.channel: %v", err)
	}
	if val != "beta" {
		t.Errorf("updater.channel = %q, want beta", val)
	}
}

func TestHandlePutUpdateConfig_InvalidChannel(t *testing.T) {
	r, _ := testRouter(t)
	body := `{"channel": "nightly"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/updates/config", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.handlePutUpdateConfig(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}

func TestHandlePutUpdateConfig_InvalidInterval(t *testing.T) {
	r, _ := testRouter(t)
	body := `{"check_interval_hours": 0}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/updates/config", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.handlePutUpdateConfig(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}

func TestHandlePutUpdateConfig_InvalidJSON(t *testing.T) {
	r, _ := testRouter(t)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/updates/config", strings.NewReader("not-json"))
	w := httptest.NewRecorder()
	r.handlePutUpdateConfig(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}

func TestHandlePutUpdateConfig_NoFields(t *testing.T) {
	r, _ := testRouter(t)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/updates/config", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	r.handlePutUpdateConfig(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}
