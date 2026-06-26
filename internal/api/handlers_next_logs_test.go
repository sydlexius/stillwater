package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
)

// TestHandleNextLogsPage_RendersNextWhenChannelNext verifies that on the "next"
// channel GET /next/logs returns 200 for an administrator and renders the
// next-scoped log-viewer shell (sw-next-logs) with the key hook ids the inline
// controller binds to (M55 #1338).
func TestHandleNextLogsPage_RendersNextWhenChannelNext(t *testing.T) {
	t.Parallel()
	r := newTestRouterFull(t)

	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextLogsPage))
	req := httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/next/logs", nil)
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "sw-next-logs") {
		t.Errorf("next channel should render LogsPage (sw-next-logs scope absent)")
	}
	// Hook ids the controller script binds to; their presence proves the chrome
	// rendered and the SSE wiring has its targets.
	for _, id := range []string{
		`id="sw-logs-root"`,
		`id="log-viewer"`,
		`id="sw-logs-throttle"`,
		`id="sw-logs-flyout"`,
		`id="sw-logs-jump"`,
	} {
		if !strings.Contains(body, id) {
			t.Errorf("preserved hook id missing from next/ logs page: %s", id)
		}
	}
}

// TestHandleNextLogsPage_SeedsInitialFilters verifies the URL deep-link is
// parsed into the page root's data-init-* attributes so the controller opens
// pre-filtered (e.g. /next/logs?level=error&artist_id=1234).
func TestHandleNextLogsPage_SeedsInitialFilters(t *testing.T) {
	t.Parallel()
	r := newTestRouterFull(t)

	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextLogsPage))
	req := httptest.NewRequestWithContext(adminContext(), http.MethodGet,
		"/next/logs?level=error&component=scanner&q=boom&artist_id=1234&rule=missing_image", nil)
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, frag := range []string{
		`data-init-level="error"`,
		`data-init-component="scanner"`,
		`data-init-search="boom"`,
		`data-init-artist-id="1234"`,
		`data-init-rule="missing_image"`,
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("expected seeded filter attribute %q in rendered page", frag)
		}
	}
}

// TestHandleNextLogsPage_InvalidLevelDropped verifies a bad level= deep-link is
// dropped (not rejected) so a stale bookmark still opens the page unfiltered on
// that axis.
func TestHandleNextLogsPage_InvalidLevelDropped(t *testing.T) {
	t.Parallel()
	r := newTestRouterFull(t)

	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextLogsPage))
	req := httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/next/logs?level=bogus", nil)
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `data-init-level=""`) {
		t.Errorf("invalid level should be dropped to empty, got body without empty level attr")
	}
}

// TestHandleNextLogsPage_StableMode404 verifies GET /next/logs returns 404 in
// stable mode (the UX middleware blocks /next/* when the lane is disabled).
func TestHandleNextLogsPage_StableMode404(t *testing.T) {
	t.Parallel()
	r := newTestRouterFull(t)

	h := middleware.UX("stable", "")(http.HandlerFunc(r.handleNextLogsPage))
	req := httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/next/logs", nil)
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("stable mode: status = %d, want 404", w.Code)
	}
}

// TestHandleNextLogsPage_OptOutHeader404 verifies the decision-12 guard: with
// the lane enabled but an X-Stillwater-UX: stable opt-out, checkNextChannel
// returns 404 rather than serving stable content.
func TestHandleNextLogsPage_OptOutHeader404(t *testing.T) {
	t.Parallel()
	r := newTestRouterFull(t)

	ctx := middleware.WithTestUXChannel(adminContext(), middleware.UXStable)
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/logs", nil)
	w := httptest.NewRecorder()
	r.handleNextLogsPage(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("opt-out header: status = %d, want 404 (decision 12)", w.Code)
	}
}

// TestHandleNextLogsPage_UnauthRendersLoginPage asserts an unauthenticated
// GET /next/logs returns 200 with the login page (not 401 JSON): the route uses
// wrapOptionalAuth so the in-handler requireAuth renders the login page.
func TestHandleNextLogsPage_UnauthRendersLoginPage(t *testing.T) {
	t.Parallel()
	r := newTestRouterFull(t)

	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextLogsPage))
	req := withI18nCtx(t, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/next/logs", nil))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unauthenticated request should get login page (200), got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "sw-next-logs") {
		t.Error("unauthenticated visitor must not see the logs surface")
	}
	if !strings.Contains(body, "/api/v1/auth/login") {
		t.Error("response should contain the login form action (/api/v1/auth/login)")
	}
}

// TestHandleNextLogsPage_NonAdmin403 asserts the admin gate: an authenticated
// operator gets 403 and never reaches the log surface, matching the
// administrator gate on the underlying /api/v1/logs/stream endpoint.
func TestHandleNextLogsPage_NonAdmin403(t *testing.T) {
	t.Parallel()
	r := newTestRouterFull(t)

	ctx := middleware.WithTestUserID(context.Background(), "operator-1")
	ctx = middleware.WithTestRole(ctx, "operator")
	ctx = middleware.WithTestUXChannel(ctx, middleware.UXNext)
	req := withI18nCtx(t, httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/logs", nil))
	w := httptest.NewRecorder()
	r.handleNextLogsPage(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("operator should get 403, got %d; body: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "sw-next-logs") {
		t.Error("non-administrator must not see the logs surface")
	}
}
