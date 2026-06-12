package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
)

// newTestRouterFull builds a Router with static assets and a foreign repo,
// suitable for tests that call assetsFor (full page renders). Uses
// testRouterWithStubPipeline which passes StaticFS so staticAssets is
// non-nil, and NewRouter automatically sets foreignRepo from deps.DB.
func newTestRouterFull(t *testing.T) *Router {
	t.Helper()
	r, _ := testRouterWithStubPipeline(t, &stubPipeline{})
	return r
}

// adminContext returns a context with a test user ID and administrator role.
func adminContext() context.Context {
	ctx := middleware.WithTestUserID(context.Background(), "test-admin")
	return middleware.WithTestRole(ctx, "administrator")
}

// TestHandleNextForeignFilesPage_RendersNextWhenChannelNext verifies that on
// the "next" channel GET /next/settings/foreign-files returns 200 and renders
// the next-scoped shell (sw-next-foreign-files) with the shared table fragment.
func TestHandleNextForeignFilesPage_RendersNextWhenChannelNext(t *testing.T) {
	t.Parallel()
	r := newTestRouterFull(t)

	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextForeignFilesPage))
	req := httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/next/reports/foreign-files", nil)
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "sw-next-foreign-files") {
		t.Errorf("next channel should render ForeignFilesPageNext (sw-next-foreign-files absent)")
	}
	if !strings.Contains(body, "foreign-files-table") {
		t.Errorf("shared ForeignFilesTable fragment (foreign-files-table) absent")
	}
}

// TestHandleNextForeignFilesPage_StableMode404 verifies that GET
// /next/reports/foreign-files returns 404 in stable mode. The UX middleware
// blocks /next/* paths before the handler runs when the lane is disabled
// (decision 12 in architecture-decisions.md).
func TestHandleNextForeignFilesPage_StableMode404(t *testing.T) {
	t.Parallel()
	r := newTestRouterFull(t)

	h := middleware.UX("stable", "")(http.HandlerFunc(r.handleNextForeignFilesPage))
	req := httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/next/reports/foreign-files", nil)
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("stable mode: status = %d, want 404 (/next/* must 404 when lane is disabled)", w.Code)
	}
}

// TestHandleNextForeignAllowlistPage_RendersNextWhenChannelNext verifies that
// on the "next" channel GET /next/reports/foreign-files/allowlist returns 200
// and renders the next-scoped shell.
func TestHandleNextForeignAllowlistPage_RendersNextWhenChannelNext(t *testing.T) {
	t.Parallel()
	r := newTestRouterFull(t)

	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextForeignAllowlistPage))
	req := httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/next/reports/foreign-files/allowlist", nil)
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "sw-next-foreign-files") {
		t.Errorf("next channel should render ForeignAllowlistPageNext (sw-next-foreign-files absent)")
	}
	if !strings.Contains(body, "foreign-allowlist-table") {
		t.Errorf("shared ForeignAllowlistTable fragment (foreign-allowlist-table) absent")
	}
}

// TestHandleNextForeignAllowlistPage_StableMode404 verifies that GET
// /next/reports/foreign-files/allowlist returns 404 in stable mode. The UX
// middleware blocks /next/* paths before the handler runs when the lane is
// disabled (decision 12 in architecture-decisions.md).
func TestHandleNextForeignAllowlistPage_StableMode404(t *testing.T) {
	t.Parallel()
	r := newTestRouterFull(t)

	h := middleware.UX("stable", "")(http.HandlerFunc(r.handleNextForeignAllowlistPage))
	req := httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/next/reports/foreign-files/allowlist", nil)
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("stable mode: status = %d, want 404 (/next/* must 404 when lane is disabled)", w.Code)
	}
}

// TestHandleNextForeignFilesPage_NonAdminForbidden verifies the admin gate
// on the next/ foreign-files page handler.
func TestHandleNextForeignFilesPage_NonAdminForbidden(t *testing.T) {
	t.Parallel()
	r := newTestRouterFull(t)

	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextForeignFilesPage))
	ctx := middleware.WithTestUserID(context.Background(), "u1")
	ctx = middleware.WithTestRole(ctx, "operator")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/reports/foreign-files", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("non-admin should get 403; got %d", w.Code)
	}
}

// TestHandleNextForeignAllowlistPage_NonAdminForbidden verifies the admin gate
// on the next/ allowlist page handler. The allowlist exposes destructive
// row-removal actions so the 403 gate is equally important here.
func TestHandleNextForeignAllowlistPage_NonAdminForbidden(t *testing.T) {
	t.Parallel()
	r := newTestRouterFull(t)

	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextForeignAllowlistPage))
	ctx := middleware.WithTestUserID(context.Background(), "u1")
	ctx = middleware.WithTestRole(ctx, "operator")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/reports/foreign-files/allowlist", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("non-admin should get 403 on allowlist; got %d", w.Code)
	}
}

// TestHandleNextForeignFilesPage_DBError exercises the 500 branch: closed DB
// causes loadForeignFilesView to return an error on the next/ channel.
func TestHandleNextForeignFilesPage_DBError(t *testing.T) {
	t.Parallel()
	r, db := newTestRouterWithForeign(t)
	// Promote to a router that has staticAssets so assetsFor does not panic,
	// then swap in the foreign repo. We close the DB to force the error path,
	// which is reached BEFORE renderTempl, so static assets are irrelevant here.
	// Use newTestRouterWithForeign's minimal router since the 500 path does not
	// reach renderTempl and therefore does not need staticAssets.
	if err := db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}

	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextForeignFilesPage))
	req := httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/next/reports/foreign-files", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("closed-DB should yield 500; got %d", w.Code)
	}
}

// TestHandleNextForeignAllowlistPage_DBError exercises the 500 branch for the
// allowlist handler when the DB is closed.
func TestHandleNextForeignAllowlistPage_DBError(t *testing.T) {
	t.Parallel()
	r, db := newTestRouterWithForeign(t)
	if err := db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}

	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextForeignAllowlistPage))
	req := httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/next/reports/foreign-files/allowlist", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("closed-DB should yield 500 on allowlist; got %d", w.Code)
	}
}

// TestHandleNextForeignFilesPage_UnauthRendersLoginPage asserts that an
// unauthenticated GET /next/reports/foreign-files returns HTTP 200 with the
// login page rather than a 401 JSON error. The route must use wrapOptionalAuth
// so requireForeignAdmin -> renderLoginPage runs for cookieless visitors.
func TestHandleNextForeignFilesPage_UnauthRendersLoginPage(t *testing.T) {
	t.Parallel()
	r := newTestRouterFull(t)

	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextForeignFilesPage))
	req := withI18nCtx(t, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/next/reports/foreign-files", nil))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unauthenticated request should get login page (200), got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "sw-next-foreign-files") {
		t.Error("unauthenticated visitor must not see the foreign-files table")
	}
	if !strings.Contains(body, "/api/v1/auth/login") {
		t.Error("response should contain the login form action (/api/v1/auth/login)")
	}
}

// TestHandleNextForeignAllowlistPage_UnauthRendersLoginPage is the same check
// for the allowlist route.
func TestHandleNextForeignAllowlistPage_UnauthRendersLoginPage(t *testing.T) {
	t.Parallel()
	r := newTestRouterFull(t)

	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextForeignAllowlistPage))
	req := withI18nCtx(t, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/next/reports/foreign-files/allowlist", nil))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unauthenticated request should get login page (200), got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "sw-next-foreign-files") {
		t.Error("unauthenticated visitor must not see the allowlist table")
	}
	if !strings.Contains(body, "/api/v1/auth/login") {
		t.Error("response should contain the login form action (/api/v1/auth/login)")
	}
}
