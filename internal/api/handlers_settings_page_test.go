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

// settingsPageTestRouter builds a Router wired with the full service suite that
// buildSettingsData touches. testRouter already wires provider settings,
// connections, rules, and auth; the settings page additionally reads platforms,
// webhooks, and backup config, so those are added here.
func settingsPageTestRouter(t *testing.T) *Router {
	t.Helper()
	r, _ := testRouter(t)
	r.platformService = platform.NewService(r.db)
	r.webhookService = webhook.NewService(r.db)
	r.backupService = backup.NewService(r.db, t.TempDir(), 5, r.logger)
	r.imageCacheDir = t.TempDir()
	return r
}

// adminSettingsRequest returns a GET /settings request whose context carries an
// authenticated administrator -- the state the auth middleware produces in
// production for an admin hitting the settings page.
func adminSettingsRequest() *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	ctx := middleware.WithTestUserID(req.Context(), "test-admin")
	ctx = middleware.WithTestRole(ctx, "administrator")
	return req.WithContext(ctx)
}

// TestHandleSettingsPage_HappyPath: an administrator gets the rendered promoted
// settings shell (M55 #1339, promoted to the canonical /settings in #1757 PR-5)
// -- the keyword filter, the four rail groups, and the deep-link section anchors.
func TestHandleSettingsPage_HappyPath(t *testing.T) {
	t.Parallel()
	r := settingsPageTestRouter(t)

	w := httptest.NewRecorder()
	r.handleSettingsPage(w, adminSettingsRequest())

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, marker := range []string{
		`class="sw-next-settings"`,       // scope class (chrome shell)
		`id="settings-search-input"`,     // keyword filter
		`data-rail-group="essentials"`,   // a rail group
		`data-rail-group="system"`,       // last rail group
		`id="section-platform"`,          // a deep-link section anchor (Platform profile)
		`href="#section-platform"`,       // its rail link
		`id="group-essentials"`,          // a pane <h2> group divider (all-scrollable)
		`data-rail-toggle`,               // the mobile section-nav hamburger
		`id="section-updates"`,           // Updates section (SettingsUpdatesTab)
		`window.SW_IS_NEXT_PAGE = true;`, // #1757 PR-5 fix-round: global cheat-sheet/g-leader flag
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("expected settings marker %q in body", marker)
		}
	}

	// The SW_IS_NEXT_PAGE flag must render BEFORE the keyboard.js <script> tag:
	// keyboard.js's isNextPage() reads the flag at script-execution time, and
	// browsers execute inline/external scripts in document order (#1757 PR-5).
	flagIdx := strings.Index(body, "window.SW_IS_NEXT_PAGE = true;")
	kbdIdx := strings.Index(body, "/static/js/keyboard.js")
	if flagIdx == -1 || kbdIdx == -1 || flagIdx >= kbdIdx {
		t.Errorf("SW_IS_NEXT_PAGE flag (idx %d) must appear before the keyboard.js script tag (idx %d)", flagIdx, kbdIdx)
	}
}

// TestHandleSettingsPage_NonAdminForbidden: settings is administrator-only, so
// an operator gets 403.
func TestHandleSettingsPage_NonAdminForbidden(t *testing.T) {
	t.Parallel()
	r := settingsPageTestRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	ctx := middleware.WithTestUserID(req.Context(), "operator-1")
	ctx = middleware.WithTestRole(ctx, "operator")
	w := httptest.NewRecorder()
	r.handleSettingsPage(w, req.WithContext(ctx))

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body: %s", w.Code, w.Body.String())
	}
}

// TestHandleSettingsPage_Unauthenticated: with no user in context the handler
// renders the login page (wrapOptionalAuth lets anonymous visitors through)
// rather than the settings rail.
func TestHandleSettingsPage_Unauthenticated(t *testing.T) {
	t.Parallel()
	r := settingsPageTestRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	w := httptest.NewRecorder()
	r.handleSettingsPage(w, req)

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

// TestHandleSettingsPage_BuildDataError covers the failure path: when the
// platform list (buildSettingsData's one hard dependency) cannot be read, the
// handler returns 500 rather than rendering a half-built page. Closing the DB
// makes platformService.List error.
func TestHandleSettingsPage_BuildDataError(t *testing.T) {
	r := settingsPageTestRouter(t)
	if err := r.db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}
	w := httptest.NewRecorder()
	r.handleSettingsPage(w, adminSettingsRequest())
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}
