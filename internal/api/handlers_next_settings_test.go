package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/backup"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/webhook"
)

// nextSettingsTestRouter builds a Router wired with the full service suite that
// buildSettingsData touches. testRouter already wires provider settings,
// connections, rules, and auth; the settings page additionally reads platforms,
// webhooks, and backup config, so those are added here.
func nextSettingsTestRouter(t *testing.T) *Router {
	t.Helper()
	r, _ := testRouter(t)
	r.platformService = platform.NewService(r.db)
	r.webhookService = webhook.NewService(r.db)
	r.backupService = backup.NewService(r.db, t.TempDir(), 5, r.logger)
	r.imageCacheDir = t.TempDir()
	return r
}

// adminNextRequest returns a GET /next/settings request whose context carries an
// authenticated administrator on the resolved next channel -- the state the UX
// middleware + auth middleware produce in production for an admin hitting the
// next/ settings page.
func adminNextRequest() *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/next/settings", nil)
	ctx := middleware.WithTestUserID(req.Context(), "test-admin")
	ctx = middleware.WithTestRole(ctx, "administrator")
	ctx = middleware.WithTestUXChannel(ctx, middleware.UXNext)
	return req.WithContext(ctx)
}

// TestHandleNextSettingsPage_HappyPath: an administrator on the next channel
// gets the rendered next/ settings shell -- the keyword filter, the four rail
// groups, and the deep-link section anchors.
func TestHandleNextSettingsPage_HappyPath(t *testing.T) {
	t.Parallel()
	r := nextSettingsTestRouter(t)

	w := httptest.NewRecorder()
	r.handleNextSettingsPage(w, adminNextRequest())

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, marker := range []string{
		`class="sw-next-settings"`,     // scope class (chrome shell)
		`id="settings-search-input"`,   // keyword filter
		`data-rail-group="essentials"`, // a rail group
		`data-rail-group="system"`,     // last rail group
		`id="section-platform"`,        // a deep-link section anchor (Platform profile)
		`href="#section-platform"`,     // its rail link
		`id="group-essentials"`,        // a pane <h2> group divider (all-scrollable)
		`data-rail-toggle`,             // the mobile section-nav hamburger
		`id="section-updates"`,         // Updates section (SettingsUpdatesTab)
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("expected next/ settings marker %q in body", marker)
		}
	}
}

// TestHandleNextSettingsPage_NonAdminForbidden: settings is administrator-only,
// so an operator on the next channel gets 403 (mirrors the stable page).
func TestHandleNextSettingsPage_NonAdminForbidden(t *testing.T) {
	t.Parallel()
	r := nextSettingsTestRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/next/settings", nil)
	ctx := middleware.WithTestUserID(req.Context(), "operator-1")
	ctx = middleware.WithTestRole(ctx, "operator")
	ctx = middleware.WithTestUXChannel(ctx, middleware.UXNext)
	w := httptest.NewRecorder()
	r.handleNextSettingsPage(w, req.WithContext(ctx))

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body: %s", w.Code, w.Body.String())
	}
}

// TestHandleNextSettingsPage_Unauthenticated: with no user in context the
// handler renders the login page (wrapOptionalAuth lets anonymous visitors
// through) rather than the settings rail.
func TestHandleNextSettingsPage_Unauthenticated(t *testing.T) {
	t.Parallel()
	r := nextSettingsTestRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/next/settings", nil)
	ctx := middleware.WithTestUXChannel(req.Context(), middleware.UXNext)
	w := httptest.NewRecorder()
	r.handleNextSettingsPage(w, req.WithContext(ctx))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (login page); body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="login-result"`) {
		t.Errorf("unauthenticated request must render the login page (missing id=\"login-result\")")
	}
	if strings.Contains(body, `id="settings-search-input"`) {
		t.Error("unauthenticated request must not render the settings rail")
	}
}

// TestHandleNextSettingsPage_StableChannel404: on the stable channel an explicit
// /next/ request 404s (decision 12) rather than serving stable content.
func TestHandleNextSettingsPage_StableChannel404(t *testing.T) {
	t.Parallel()
	r := nextSettingsTestRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/next/settings", nil)
	ctx := middleware.WithTestUserID(req.Context(), "test-admin")
	ctx = middleware.WithTestRole(ctx, "administrator")
	ctx = middleware.WithTestUXChannel(ctx, middleware.UXStable)
	w := httptest.NewRecorder()
	r.handleNextSettingsPage(w, req.WithContext(ctx))

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

// TestHandleNextSettingsPage_BuildDataError covers the failure path: when the
// platform list (buildSettingsData's one hard dependency) cannot be read, the
// handler returns 500 rather than rendering a half-built page. Closing the DB
// makes platformService.List error.
func TestHandleNextSettingsPage_BuildDataError(t *testing.T) {
	r := nextSettingsTestRouter(t)
	if err := r.db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}
	w := httptest.NewRecorder()
	r.handleNextSettingsPage(w, adminNextRequest())
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}

// TestHandleSettingsPage_BuildDataError covers the same failure path on the
// stable handler (the shared buildSettingsData bail + the handler's 500 branch).
func TestHandleSettingsPage_BuildDataError(t *testing.T) {
	r := nextSettingsTestRouter(t)
	if err := r.db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	ctx := middleware.WithTestUserID(req.Context(), "test-admin")
	ctx = middleware.WithTestRole(ctx, "administrator")
	w := httptest.NewRecorder()
	r.handleSettingsPage(w, req.WithContext(ctx))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}

// TestHandleSettingsPage_StableHappyPath covers the refactored stable handler:
// an administrator gets the rendered stable settings page via the shared
// buildSettingsData aggregation.
func TestHandleSettingsPage_StableHappyPath(t *testing.T) {
	t.Parallel()
	r := nextSettingsTestRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	ctx := middleware.WithTestUserID(req.Context(), "test-admin")
	ctx = middleware.WithTestRole(ctx, "administrator")
	w := httptest.NewRecorder()
	r.handleSettingsPage(w, req.WithContext(ctx))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	// The stable page renders the tab bar (next/ replaces it with the rail).
	if !strings.Contains(w.Body.String(), `data-tab-panel="general"`) {
		t.Error("expected stable settings tab panel in body")
	}
}
