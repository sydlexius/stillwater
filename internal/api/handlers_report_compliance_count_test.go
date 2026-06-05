package api

// handlers_report_compliance_count_test.go -- coverage for the sidebar count
// fragment endpoint GET /api/v1/reports/compliance/count (#1715). The endpoint
// is admin-only and returns either an empty body (count=0 or all artists
// compliant) or an <a> link populated with the non-compliant-artist count.
// A short-TTL module-level cache memoizes the result so per-tab polling
// collapses to at most one DB query per TTL window.

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
)

// complianceCountTestRouter wires the minimum Router surface the compliance
// count handler needs: an artistService backed by a real SQLite (migrations
// applied via newTestDB). complianceCount is a module-level cache, so each
// test invalidates it up front to prevent bleed between test runs.
func complianceCountTestRouter(t *testing.T) (*Router, *sql.DB) {
	t.Helper()
	complianceCount.invalidate()

	db := newTestDB(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	r := &Router{
		logger:        logger,
		artistService: artist.NewService(db),
	}
	return r, db
}

// seedNonCompliantArtist inserts one artist whose health_score is below 100,
// making it show up in the "non_compliant" filter used by the count handler.
func seedNonCompliantArtist(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO artists (id, name, sort_name, path, health_score, created_at, updated_at)
		 VALUES (lower(hex(randomblob(16))), 'Test Artist', 'Test Artist', '/music/test', 50, datetime('now'), datetime('now'))`,
	); err != nil {
		t.Fatalf("seeding non-compliant artist: %v", err)
	}
}

// adminComplianceCountReq builds a GET request for the compliance count
// endpoint with an admin auth context attached.
func adminComplianceCountReq() *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/compliance/count", nil)
	ctx := middleware.WithTestUserID(req.Context(), "admin-1")
	ctx = middleware.WithTestRole(ctx, "administrator")
	return req.WithContext(ctx)
}

// TestHandleComplianceCount_EmptyWhenZero asserts the empty-body branch when
// all artists are compliant (or no artists exist). The sidebar's hx-swap=innerHTML
// clears the placeholder; a 200 with empty body is the contract.
//
// Note: these tests are NOT run in parallel because complianceCount is a
// module-level cache and parallel tests would race on the shared state.
func TestHandleComplianceCount_EmptyWhenZero(t *testing.T) {
	r, _ := complianceCountTestRouter(t)

	rec := httptest.NewRecorder()
	r.handleComplianceCount(rec, adminComplianceCountReq())

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "" {
		t.Errorf("body = %q, want empty (no non-compliant artists)", got)
	}
}

// TestHandleComplianceCount_NonAdmin asserts the 403 JSON envelope when the
// caller is not an administrator. HTMX does not swap on non-2xx, so the JSON
// body is never shown; the contract exists for API consumers.
func TestHandleComplianceCount_NonAdmin(t *testing.T) {

	r, _ := complianceCountTestRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/compliance/count", nil)
	ctx := middleware.WithTestRole(req.Context(), "operator")
	rec := httptest.NewRecorder()
	r.handleComplianceCount(rec, req.WithContext(ctx))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "forbidden") {
		t.Errorf("body = %q, want JSON with 'forbidden'", rec.Body.String())
	}
}

// TestHandleComplianceCount_StableChannel asserts the stable-sidebar fragment
// (no ?ch=next): href uses the stable /reports/compliance path, no SVG glyph,
// uses sw-sidebar-badge-pill (not sw-sidebar-count-pill).
func TestHandleComplianceCount_StableChannel(t *testing.T) {

	r, db := complianceCountTestRouter(t)
	seedNonCompliantArtist(t, db)

	rec := httptest.NewRecorder()
	r.handleComplianceCount(rec, withI18nCtx(t, adminComplianceCountReq()))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `href="/reports/compliance"`) {
		t.Errorf("body %q missing stable compliance href", body)
	}
	if strings.Contains(body, "/next/") {
		t.Errorf("stable channel should not contain /next/ href; got %q", body)
	}
	if !strings.Contains(body, "sw-sidebar-badge-pill") {
		t.Errorf("stable channel should use sw-sidebar-badge-pill; got %q", body)
	}
}

// TestHandleComplianceCount_NextChannel asserts the next/-sidebar fragment
// (?ch=next): href uses /next/reports/compliance, SVG chart-bar glyph is
// present, pill uses sw-sidebar-count-pill.
func TestHandleComplianceCount_NextChannel(t *testing.T) {

	r, db := complianceCountTestRouter(t)
	seedNonCompliantArtist(t, db)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/compliance/count?ch=next", nil)
	ctx := middleware.WithTestUserID(req.Context(), "admin-1")
	ctx = middleware.WithTestRole(ctx, "administrator")
	req = withI18nCtx(t, req.WithContext(ctx))

	rec := httptest.NewRecorder()
	r.handleComplianceCount(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `href="/next/reports/compliance"`) {
		t.Errorf("body %q missing /next/reports/compliance href", body)
	}
	if !strings.Contains(body, "<svg") {
		t.Errorf("next/ channel should include chart-bar SVG glyph; got %q", body)
	}
	if !strings.Contains(body, "sw-sidebar-count-pill") {
		t.Errorf("next/ channel should use sw-sidebar-count-pill; got %q", body)
	}
	if !strings.Contains(body, "1") {
		t.Errorf("body %q should contain count 1", body)
	}
}

// TestHandleComplianceCount_CountError asserts the fail-safe branch when the
// DB is closed (causing Count to return an error). The handler must return 200
// with an empty body rather than surface the error in the sidebar.
func TestHandleComplianceCount_CountError(t *testing.T) {
	complianceCount.invalidate()

	// Open and immediately close the DB so queries fail with a real error.
	db := newTestDB(t)
	_ = db.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	r := &Router{logger: logger, artistService: artist.NewService(db)}

	rec := httptest.NewRecorder()
	r.handleComplianceCount(rec, adminComplianceCountReq())

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fail-safe on DB error)", rec.Code)
	}
	if got := rec.Body.String(); got != "" {
		t.Errorf("body = %q, want empty (fail-safe)", got)
	}
}

// TestHandleComplianceCount_CachedResult exercises the TTL-fresh branch of
// complianceCountState.get: a second request within the TTL window returns the
// cached count without re-querying the DB.
func TestHandleComplianceCount_CachedResult(t *testing.T) {
	r, db := complianceCountTestRouter(t)
	seedNonCompliantArtist(t, db)

	// First call: populates the cache with count=1.
	rec1 := httptest.NewRecorder()
	r.handleComplianceCount(rec1, withI18nCtx(t, adminComplianceCountReq()))
	if rec1.Code != http.StatusOK {
		t.Fatalf("first call: status = %d", rec1.Code)
	}
	if rec1.Body.String() == "" {
		t.Fatal("first call: expected non-empty body (count pill) after seeding non-compliant artist")
	}

	// Mutate the DB so all artists become compliant. If the second handler call
	// re-queries the DB it will see count=0 and return an empty body; if it
	// uses the in-memory cache it returns the previously stored count (same
	// non-empty body as the first call).
	if _, err := db.ExecContext(context.Background(),
		`UPDATE artists SET health_score = 100`,
	); err != nil {
		t.Fatalf("DB mutation: %v", err)
	}

	// Second call: must hit the TTL-fresh cache path, not re-query the DB.
	rec2 := httptest.NewRecorder()
	r.handleComplianceCount(rec2, withI18nCtx(t, adminComplianceCountReq()))
	if rec2.Code != http.StatusOK {
		t.Fatalf("second call: status = %d", rec2.Code)
	}
	// The cached (pre-mutation) body must be served unchanged.
	if rec1.Body.String() != rec2.Body.String() {
		t.Errorf("cache not served: first=%q second=%q (second call re-queried the DB instead of using the cache)",
			rec1.Body.String(), rec2.Body.String())
	}
}
