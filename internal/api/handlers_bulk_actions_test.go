package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestBulkAction_InvalidAction rejects unknown action values with 400.
func TestBulkAction_InvalidAction(t *testing.T) {
	r, _, _ := testRouterWithIdentify(t)
	body := strings.NewReader(`{"action":"delete_everything","ids":["abc"]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-actions", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleBulkAction(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}

// TestBulkAction_EmptyIDs rejects empty id lists with 400.
func TestBulkAction_EmptyIDs(t *testing.T) {
	r, _, _ := testRouterWithIdentify(t)
	body := strings.NewReader(`{"action":"run_rules","ids":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-actions", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleBulkAction(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}

// TestBulkAction_InvalidIDFormat rejects IDs that fail the format regex.
func TestBulkAction_InvalidIDFormat(t *testing.T) {
	r, _, _ := testRouterWithIdentify(t)
	body := strings.NewReader(`{"action":"run_rules","ids":["../../etc/passwd"]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-actions", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleBulkAction(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}

// TestBulkAction_ConcurrentReject ensures a second bulk action while one is
// already running returns 409 Conflict, matching the fix-all pattern.
func TestBulkAction_ConcurrentReject(t *testing.T) {
	r, _, _ := testRouterWithIdentify(t)

	// Simulate an in-flight run by claiming the progress slot directly.
	r.bulkActionMu.Lock()
	r.bulkActionProgress = &BulkActionProgress{Status: "running", Action: "run_rules", Total: 5}
	r.bulkActionMu.Unlock()

	body := strings.NewReader(`{"action":"run_rules","ids":["abc123"]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-actions", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleBulkAction(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if resp["status"] != "running" {
		t.Errorf("status = %v, want running", resp["status"])
	}
}

// TestBulkActionStatus_Idle returns idle when no progress is set.
func TestBulkActionStatus_Idle(t *testing.T) {
	r, _, _ := testRouterWithIdentify(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/bulk-actions/status", nil)
	w := httptest.NewRecorder()

	r.handleBulkActionStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if resp["status"] != "idle" {
		t.Errorf("status = %v, want idle", resp["status"])
	}
}

// TestBulkAction_TooManyIDs bounds the request size so a single call cannot
// monopolize the singleton slot indefinitely.
func TestBulkAction_TooManyIDs(t *testing.T) {
	r, _, _ := testRouterWithIdentify(t)

	var b strings.Builder
	b.WriteString(`{"action":"run_rules","ids":[`)
	for i := 0; i < MaxBulkActionIDs+1; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`"abc"`)
	}
	b.WriteString(`]}`)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-actions", strings.NewReader(b.String()))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleBulkAction(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}
