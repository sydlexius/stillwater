package api

// handlers_artist_duplicates_count_test.go -- coverage for the sidebar count
// fragment endpoint (#1665). The endpoint is admin-only and returns either
// an empty body (no duplicates detected) or an <a> link populated with the
// group count. A short-TTL module-level cache memoizes the result so the
// sidebar's per-tab polling collapses to at most one detector run per TTL
// window; merge invalidates the cache.

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

// countTestRouter wires the minimum Router surface the count handler needs.
// Reuses the merge-test fixture pattern so the in-memory SQLite seeding paths
// stay identical across the duplicates handler suites. duplicatesCount is a
// module-level cache, so each test invalidates it up front to avoid bleed.
func countTestRouter(t *testing.T) (*Router, *sql.DB) {
	t.Helper()
	duplicatesCount.invalidate()

	db := newTestDB(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	r := &Router{
		logger:        logger,
		artistService: artist.NewService(db),
		db:            db,
	}
	return r, db
}

// seedTwoDuplicates inserts a single near-duplicate pair (apostrophe variant)
// so DetectDuplicates surfaces exactly one group. The test only cares that
// a group exists, not the artist IDs, so this helper returns nothing.
func seedTwoDuplicates(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	curly := string([]rune{0x2019})
	mustInsert := func(name, path string) {
		t.Helper()
		if _, err := db.ExecContext(ctx,
			`INSERT INTO artists (id, name, sort_name, path, created_at, updated_at)
			 VALUES (lower(hex(randomblob(16))), ?, ?, ?, datetime('now'), datetime('now'))`,
			name, name, path,
		); err != nil {
			t.Fatalf("seeding artist %q: %v", name, err)
		}
	}
	mustInsert("Caedmon's Call", "/music/Caedmon's Call")
	mustInsert("Caedmon"+curly+"s Call", "/music/Caedmon2")
}

// adminCountReq builds a GET request with an admin auth context attached.
// Mirrors adminReq from the merge tests but for the count endpoint's path.
func adminCountReq() *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/duplicates/count", nil)
	ctx := middleware.WithTestUserID(req.Context(), "admin-1")
	ctx = middleware.WithTestRole(ctx, "administrator")
	return req.WithContext(ctx)
}

// TestHandleArtistDuplicatesCount_NoDuplicates asserts the empty-body branch
// so the sidebar's hx-swap=innerHTML clears the placeholder. A 200 with empty
// body is the contract; HTMX treats it as "remove the child".
func TestHandleArtistDuplicatesCount_NoDuplicates(t *testing.T) {
	r, _ := countTestRouter(t)

	rec := httptest.NewRecorder()
	r.handleArtistDuplicatesCount(rec, adminCountReq())

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "" {
		t.Errorf("body = %q, want empty", got)
	}
}

// TestHandleArtistDuplicatesCount_WithDuplicates asserts the populated-link
// branch carries the right href, data-path (for sidebar.js active highlight),
// and the numeric group count.
func TestHandleArtistDuplicatesCount_WithDuplicates(t *testing.T) {
	r, db := countTestRouter(t)
	seedTwoDuplicates(t, db)

	rec := httptest.NewRecorder()
	r.handleArtistDuplicatesCount(rec, adminCountReq())

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`href="/reports/duplicates"`,
		`data-path="/reports/duplicates"`,
		`sw-sidebar-subnav-link`,
		`>1<`, // single group; count rendered inside the badge pill
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\nfull body: %s", want, body)
		}
	}
}

// TestHandleArtistDuplicatesCount_NonAdmin asserts the role gate: an
// authenticated operator hits 403, never reaching the detector.
func TestHandleArtistDuplicatesCount_NonAdmin(t *testing.T) {
	r, db := countTestRouter(t)
	seedTwoDuplicates(t, db)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/duplicates/count", nil)
	ctx := middleware.WithTestUserID(req.Context(), "user-1")
	ctx = middleware.WithTestRole(ctx, "operator")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	r.handleArtistDuplicatesCount(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%q", rec.Code, rec.Body.String())
	}
}

// TestHandleArtistDuplicatesCount_DetectorError pins the fail-safe branch:
// when DetectDuplicates returns an error (here forced by closing the DB
// before the handler call) the handler must log and emit an empty 200
// body so the sidebar simply doesn't render the Duplicates child --
// surfacing the failure inline would clutter every sidebar refresh.
func TestHandleArtistDuplicatesCount_DetectorError(t *testing.T) {
	r, db := countTestRouter(t)
	// Force the underlying DetectDuplicates call to fail with "database is
	// closed" without disturbing the t.Cleanup-registered Close. The cache
	// callback returns the error verbatim; the handler's warn-then-empty
	// branch is what we want to exercise.
	if err := db.Close(); err != nil {
		t.Fatalf("closing db for error injection: %v", err)
	}

	rec := httptest.NewRecorder()
	r.handleArtistDuplicatesCount(rec, adminCountReq())

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fail-safe empty body); body=%q", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "" {
		t.Errorf("body = %q, want empty (detector error must not surface inline)", got)
	}
}

// TestHandleArtistDuplicatesCount_CacheTTL exercises the cache by calling
// the handler twice without invalidating between calls and verifying the
// detector ran only once. Drives this via a synthetic countFn through the
// cache directly because the handler holds no test seam; the production
// counter (countDuplicateGroups) is small and tested by the integration
// branches above. This test focuses on the memoization contract.
func TestHandleArtistDuplicatesCount_CacheTTL(t *testing.T) {
	duplicatesCount.invalidate()

	calls := 0
	fn := func(ctx context.Context) (int, error) {
		calls++
		return 7, nil
	}
	ctx := context.Background()

	if n, err := duplicatesCount.get(ctx, fn); err != nil || n != 7 {
		t.Fatalf("first get: n=%d err=%v, want 7 nil", n, err)
	}
	if n, err := duplicatesCount.get(ctx, fn); err != nil || n != 7 {
		t.Fatalf("second get: n=%d err=%v, want 7 nil", n, err)
	}
	if calls != 1 {
		t.Errorf("refresh fn calls = %d, want 1 (second call should hit cache)", calls)
	}

	duplicatesCount.invalidate()
	if n, err := duplicatesCount.get(ctx, fn); err != nil || n != 7 {
		t.Fatalf("post-invalidate get: n=%d err=%v, want 7 nil", n, err)
	}
	if calls != 2 {
		t.Errorf("refresh fn calls = %d after invalidate, want 2", calls)
	}
}
