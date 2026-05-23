package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSortAllowlist_RejectsUnknown asserts every list/search endpoint that
// reads ?sort returns 400 (Bad Request) for an unknown key instead of
// silently falling back to a default. This is the boundary protection
// added by #1372; downstream allowlist checks remain as defense-in-depth.
func TestSortAllowlist_RejectsUnknown(t *testing.T) {
	t.Parallel()

	// Each entry exercises one handler that reads the ?sort query
	// parameter. The chosen unknown keys deliberately include shapes a
	// naive caller might use to probe injection (SQL fragments, mixed
	// case, unrelated terms) so the test doubles as a small fuzz table.
	cases := []struct {
		name    string
		method  string
		url     string
		handler func(r *Router, w http.ResponseWriter, req *http.Request)
	}{
		{
			name:    "list artists JSON",
			method:  http.MethodGet,
			url:     "/api/v1/artists?sort=DROP+TABLE",
			handler: (*Router).handleListArtists,
		},
		{
			name:    "list artists JSON unknown key",
			method:  http.MethodGet,
			url:     "/api/v1/artists?sort=evil_key",
			handler: (*Router).handleListArtists,
		},
		{
			name:    "list locked artists",
			method:  http.MethodGet,
			url:     "/api/v1/artists/locked?sort=evil_key",
			handler: (*Router).handleListLockedArtists,
		},
		{
			name:    "compliance report",
			method:  http.MethodGet,
			url:     "/api/v1/reports/compliance?sort=evil_key",
			handler: (*Router).handleReportCompliance,
		},
		{
			name:    "compliance export",
			method:  http.MethodGet,
			url:     "/api/v1/reports/compliance/export?sort=evil_key",
			handler: (*Router).handleReportComplianceExport,
		},
		{
			name:    "list notifications",
			method:  http.MethodGet,
			url:     "/api/v1/notifications?sort=evil_key",
			handler: (*Router).handleListNotifications,
		},
		{
			name:    "notifications export",
			method:  http.MethodGet,
			url:     "/api/v1/notifications/export?sort=evil_key",
			handler: (*Router).handleNotificationsExport,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, _ := testRouter(t)
			req := httptest.NewRequest(tc.method, tc.url, nil)
			w := httptest.NewRecorder()
			tc.handler(r, w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body=%q)", w.Code, w.Body.String())
			}
			// API responses use the structured {"error": "..."} JSON envelope.
			var body map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decoding response body %q: %v", w.Body.String(), err)
			}
			if !strings.Contains(body["error"], "invalid sort key") {
				t.Errorf("error message %q does not mention invalid sort key", body["error"])
			}
		})
	}
}

// TestSortAllowlist_AcceptsKnown asserts a known sort key still produces 200,
// guarding against accidental over-restriction during the allowlist sweep.
func TestSortAllowlist_AcceptsKnown(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		url     string
		handler func(r *Router, w http.ResponseWriter, req *http.Request)
	}{
		{
			name:    "list artists with sort_name",
			url:     "/api/v1/artists?sort=sort_name&order=asc",
			handler: (*Router).handleListArtists,
		},
		{
			name:    "list artists with type",
			url:     "/api/v1/artists?sort=type&order=asc",
			handler: (*Router).handleListArtists,
		},
		{
			name:    "list artists with origin",
			url:     "/api/v1/artists?sort=origin&order=desc",
			handler: (*Router).handleListArtists,
		},
		{
			name:    "list artists empty sort uses default",
			url:     "/api/v1/artists",
			handler: (*Router).handleListArtists,
		},
		{
			name:    "list notifications with severity",
			url:     "/api/v1/notifications?sort=severity&order=desc",
			handler: (*Router).handleListNotifications,
		},
		{
			name:    "list notifications empty sort uses default",
			url:     "/api/v1/notifications",
			handler: (*Router).handleListNotifications,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, _ := testRouter(t)
			req := httptest.NewRequest(http.MethodGet, tc.url, nil)
			w := httptest.NewRecorder()
			tc.handler(r, w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body=%q)", w.Code, w.Body.String())
			}
		})
	}
}

// TestSortAllowlist_RejectsUnknownOrder asserts the order parameter is also
// allowlisted; only "", "asc", "desc" are accepted. Mirrors the sort test by
// asserting both the 400 status and the structured error envelope content.
func TestSortAllowlist_RejectsUnknownOrder(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists?order=sideways", nil)
	w := httptest.NewRecorder()
	r.handleListArtists(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%q)", w.Code, w.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding response body %q: %v", w.Body.String(), err)
	}
	if !strings.Contains(body["error"], "invalid order") {
		t.Errorf("error message %q does not mention invalid order", body["error"])
	}
}

// Note: the image search endpoints (handleImageSearch, handleWebImageSearch)
// also gate sort through validateSortParam against allowedImageSearchSort,
// but exercising them end-to-end requires a fully wired artist and live
// provider responses. The shared helper is covered by the table-driven
// cases above plus the dbutil package tests; the image handlers route
// through the same boundary helper with the same semantics.
