package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
)

func TestHandleGuidePage_Authenticated(t *testing.T) {
	r := testRouterForOnboarding(t)

	req := httptest.NewRequest(http.MethodGet, "/guide", nil)
	ctx := testI18nCtx(t, req.Context())
	ctx = middleware.WithTestUserID(ctx, "test-user-id")
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	r.handleGuidePage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	body := w.Body.String()
	if !strings.Contains(body, "User Guide") {
		t.Error("expected body to contain \"User Guide\"")
	}
}

func TestHandleGuidePage_Unauthenticated(t *testing.T) {
	r := testRouterForOnboarding(t)

	req := httptest.NewRequest(http.MethodGet, "/guide", nil)
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()

	r.handleGuidePage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	body := w.Body.String()
	if !strings.Contains(body, "Sign in") {
		t.Error("expected body to contain \"Sign in\" (login page)")
	}
}

// testRouterWithBasePath creates a minimal router with a non-empty base path.
// It reuses testRouterForOnboarding for setup and overrides basePath.
func testRouterWithBasePath(t *testing.T, bp string) *Router {
	t.Helper()
	r := testRouterForOnboarding(t)
	r.basePath = bp
	return r
}

// TestLayout_NavLinksUseBasePath verifies that navigation links in the layout are
// prefixed with server.base_path when it is set to a non-empty value. This is the
// primary regression check for sub-path deployments (e.g. /stillwater).
func TestLayout_NavLinksUseBasePath(t *testing.T) {
	const bp = "/stillwater"
	r := testRouterWithBasePath(t, bp)

	req := httptest.NewRequest(http.MethodGet, bp+"/guide", nil)
	ctx := testI18nCtx(t, req.Context())
	ctx = middleware.WithTestUserID(ctx, "test-user-id")
	ctx = middleware.WithTestRole(ctx, "administrator")
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	r.handleGuidePage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	body := w.Body.String()

	// Verify that nav anchor hrefs carry the base path prefix.
	// Note: the guide link is not in the sidebar nav (issue #836) but the page
	// itself is still accessible at /guide.
	navLinks := []string{
		`href="/stillwater/"`,
		`href="/stillwater/artists"`,
		`href="/stillwater/reports/compliance"`,
		`href="/stillwater/settings"`,
	}
	for _, link := range navLinks {
		if !strings.Contains(body, link) {
			t.Errorf("expected nav link %q in rendered body", link)
		}
	}

	// Verify the meta tag carries the base path so the JS hook can use it.
	if !strings.Contains(body, `content="/stillwater"`) {
		t.Error("expected htmx-base-path meta tag with content=\"/stillwater\"")
	}

	// Verify that static asset URLs (CSS, JS) are prefixed with the base path.
	if !strings.Contains(body, `href="/stillwater/static/`) {
		t.Error("expected stylesheet href to start with base path /stillwater/static/")
	}
	if !strings.Contains(body, `src="/stillwater/static/`) {
		t.Error("expected script src to start with base path /stillwater/static/")
	}

	// Verify that root-relative nav links (without the prefix) are NOT present
	// to catch any missed hrefs. The guide link is excluded from this check
	// because the guide page content itself may reference /guide.
	rootLinks := []string{
		`href="/"`,
		`href="/artists"`,
		`href="/settings"`,
	}
	for _, link := range rootLinks {
		if strings.Contains(body, link) {
			t.Errorf("found unprefixed nav link %q; expected all nav links to use base path %q", link, bp)
		}
	}
}

// TestLayout_NavLinksNoBasePath verifies that when base_path is empty the layout
// renders standard root-relative links so the default deployment is unaffected.
func TestLayout_NavLinksNoBasePath(t *testing.T) {
	r := testRouterWithBasePath(t, "")

	req := httptest.NewRequest(http.MethodGet, "/guide", nil)
	ctx := testI18nCtx(t, req.Context())
	ctx = middleware.WithTestUserID(ctx, "test-user-id")
	ctx = middleware.WithTestRole(ctx, "administrator")
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	r.handleGuidePage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	body := w.Body.String()

	// With an empty base path the nav links should still resolve as root-relative.
	// Note: the guide link is not in the sidebar nav (issue #836) but the page
	// itself is still accessible at /guide.
	navLinks := []string{
		`href="/"`,
		`href="/artists"`,
		`href="/reports/compliance"`,
		`href="/settings"`,
	}
	for _, link := range navLinks {
		if !strings.Contains(body, link) {
			t.Errorf("expected nav link %q in rendered body", link)
		}
	}

	// Verify the meta tag has an empty base path.
	if !strings.Contains(body, `content=""`) {
		t.Error("expected htmx-base-path meta tag with empty content")
	}
}
