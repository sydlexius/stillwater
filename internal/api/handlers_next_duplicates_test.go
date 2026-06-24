package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
)

// TestHandleNextArtistDuplicatesPage_RendersNextWhenChannelNext verifies that on
// the "next" channel GET /next/reports/duplicates returns 200 and renders the
// next-scoped shell (sw-next-duplicates) with the shared merge modal. An empty
// test DB yields no groups, so the none-detected empty state is expected.
func TestHandleNextArtistDuplicatesPage_RendersNextWhenChannelNext(t *testing.T) {
	t.Parallel()
	r := newTestRouterFull(t)

	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextArtistDuplicatesPage))
	req := httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/next/reports/duplicates", nil)
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "sw-next-duplicates") {
		t.Errorf("next channel should render ArtistDuplicatesNextPage (sw-next-duplicates absent)")
	}
	if !strings.Contains(body, `id="merge-modal"`) {
		t.Errorf("shared ArtistMergeModal (#merge-modal) absent")
	}
	if !strings.Contains(body, `id="duplicates-empty-none"`) {
		t.Errorf("empty test DB should render the none-detected empty state")
	}
}

// TestHandleNextArtistDuplicatesPage_StableMode404 verifies that GET
// /next/reports/duplicates returns 404 in stable mode. The UX middleware blocks
// /next/* paths before the handler runs when the lane is disabled (decision 12
// in architecture-decisions.md).
func TestHandleNextArtistDuplicatesPage_StableMode404(t *testing.T) {
	t.Parallel()
	r := newTestRouterFull(t)

	h := middleware.UX("stable", "")(http.HandlerFunc(r.handleNextArtistDuplicatesPage))
	req := httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/next/reports/duplicates", nil)
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("stable mode: status = %d, want 404 (/next/* must 404 when lane is disabled)", w.Code)
	}
}

// TestHandleNextArtistDuplicatesPage_NonAdminForbidden verifies the admin gate
// (requireForeignAdmin): the duplicates page is admin-only, so a non-admin must
// get 403 rather than the rendered next/ page.
func TestHandleNextArtistDuplicatesPage_NonAdminForbidden(t *testing.T) {
	t.Parallel()
	r := newTestRouterFull(t)

	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextArtistDuplicatesPage))
	ctx := middleware.WithTestUserID(context.Background(), "u1")
	ctx = middleware.WithTestRole(ctx, "operator")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/reports/duplicates", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("non-admin should get 403; got %d", w.Code)
	}
}

// TestHandleNextArtistDuplicatesPage_NilDB pins the partially-constructed-Router
// guard: when r.db is nil the handler renders an empty next/ page (200) rather
// than panicking on the detection call. Mirrors the stable handler's nil-db
// guard.
func TestHandleNextArtistDuplicatesPage_NilDB(t *testing.T) {
	t.Parallel()
	r := newTestRouterFull(t)
	r.db = nil

	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextArtistDuplicatesPage))
	req := httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/next/reports/duplicates", nil)
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("nil db: status = %d, want 200 (empty page)", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "sw-next-duplicates") {
		t.Errorf("nil db should still render the next/ shell (sw-next-duplicates)")
	}
	if !strings.Contains(body, `id="duplicates-empty-none"`) {
		t.Errorf("nil db should render the none-detected empty state")
	}
}

// TestHandleNextArtistDuplicatesPage_DetectorError pins the fail-loud branch:
// when DetectDuplicates errors (forced here by closing the DB) the handler must
// return 500 rather than rendering a misleading empty page. sql.DB.Close is
// idempotent, so the t.Cleanup-registered Close stays safe.
func TestHandleNextArtistDuplicatesPage_DetectorError(t *testing.T) {
	t.Parallel()
	r := newTestRouterFull(t)
	if err := r.db.Close(); err != nil {
		t.Fatalf("closing db for error injection: %v", err)
	}

	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextArtistDuplicatesPage))
	req := httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/next/reports/duplicates", nil)
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("detector error: status = %d, want 500", w.Code)
	}
}
