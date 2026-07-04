package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
)

// TestHandleLogsPage_RendersForAdmin verifies that GET /logs returns 200 for an
// administrator and renders the log-viewer shell (sw-next-logs scope) with the
// key hook ids the inline controller binds to (M55 #1338; promoted to the
// canonical /logs in #1757 PR-5).
func TestHandleLogsPage_RendersForAdmin(t *testing.T) {
	t.Parallel()
	r := newTestRouterFull(t)

	req := httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/logs", nil)
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	r.handleLogsPage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "sw-next-logs") {
		t.Errorf("promoted logs page should render the sw-next-logs scope")
	}
	// Hook ids the controller script binds to; their presence proves the chrome
	// rendered and the SSE wiring has its targets.
	for _, id := range []string{
		`id="sw-logs-root"`,
		`id="log-viewer"`,
		`id="sw-logs-throttle"`,
		`id="sw-logs-filter-flyout"`,
		`id="sw-logs-jump"`,
	} {
		if !strings.Contains(body, id) {
			t.Errorf("preserved hook id missing from logs page: %s", id)
		}
	}

	// #1757 PR-5 fix-round: the promoted page must set SW_IS_NEXT_PAGE so
	// keyboard.js's isNextPage() registers the Global cheat-sheet/g-leader
	// shortcuts here, and it must render before the keyboard.js script tag.
	flagIdx := strings.Index(body, "window.SW_IS_NEXT_PAGE = true;")
	kbdIdx := strings.Index(body, "/static/js/keyboard.js")
	if flagIdx == -1 || kbdIdx == -1 || flagIdx >= kbdIdx {
		t.Errorf("SW_IS_NEXT_PAGE flag (idx %d) must appear before the keyboard.js script tag (idx %d)", flagIdx, kbdIdx)
	}
}

// TestHandleLogsPage_SeedsInitialFilters verifies the URL deep-link is parsed
// into the page root's data-init-* attributes so the controller opens
// pre-filtered (e.g. /logs?level=error&artist_id=1234).
func TestHandleLogsPage_SeedsInitialFilters(t *testing.T) {
	t.Parallel()
	r := newTestRouterFull(t)

	req := httptest.NewRequestWithContext(adminContext(), http.MethodGet,
		"/logs?level=error&component=scanner&q=boom&artist_id=1234&rule=missing_image", nil)
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	r.handleLogsPage(w, req)

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

// TestHandleLogsPage_InvalidLevelDropped verifies a bad level= deep-link is
// dropped (not rejected) so a stale bookmark still opens the page unfiltered on
// that axis.
func TestHandleLogsPage_InvalidLevelDropped(t *testing.T) {
	t.Parallel()
	r := newTestRouterFull(t)

	req := httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/logs?level=bogus", nil)
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	r.handleLogsPage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `data-init-level=""`) {
		t.Errorf("invalid level should be dropped to empty, got body without empty level attr")
	}
}

// TestHandleLogsPage_UnauthRendersLoginPage asserts an unauthenticated GET /logs
// returns 200 with the login page (not 401 JSON): the route uses
// wrapOptionalAuth so the in-handler requireAuth renders the login page.
func TestHandleLogsPage_UnauthRendersLoginPage(t *testing.T) {
	t.Parallel()
	r := newTestRouterFull(t)

	req := withI18nCtx(t, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/logs", nil))
	w := httptest.NewRecorder()
	r.handleLogsPage(w, req)

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

// TestHandleLogsPage_NonAdmin403 asserts the admin gate: an authenticated
// operator gets 403 and never reaches the log surface, matching the
// administrator gate on the underlying /api/v1/logs/stream endpoint.
func TestHandleLogsPage_NonAdmin403(t *testing.T) {
	t.Parallel()
	r := newTestRouterFull(t)

	ctx := middleware.WithTestUserID(context.Background(), "operator-1")
	ctx = middleware.WithTestRole(ctx, "operator")
	req := withI18nCtx(t, httptest.NewRequestWithContext(ctx, http.MethodGet, "/logs", nil))
	w := httptest.NewRecorder()
	r.handleLogsPage(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("operator should get 403, got %d; body: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "sw-next-logs") {
		t.Error("non-administrator must not see the logs surface")
	}
}
