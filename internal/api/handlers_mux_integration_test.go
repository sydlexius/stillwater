package api

// Mux-level integration tests for wrapOptionalAuth page routes.
//
// These tests exist to catch a specific class of regression: any change that
// reverts a page route from wrapOptionalAuth to wrapAuth would cause
// unauthenticated GET requests to return 401 JSON instead of the login page
// (HTTP 200 with the login form rendered inline). The handler-level tests in
// handlers_foreign_files_test.go and related files call handlers directly,
// bypassing the router and wrapOptionalAuth entirely, so that class of bug
// would not be caught there. See PR #2017 for the original gap description.
//
// Each test drives requests through r.Handler(ctx).ServeHTTP so that
// wrapOptionalAuth, the session middleware, and the full mux routing stack
// all execute exactly as they do in production.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// loginPageMarker is present in every login page response. The rendered login
// form posts to /api/v1/auth/login, which is distinct from the 401 JSON
// responses that wrapAuth returns and distinct from actual page content.
const loginPageMarker = `hx-post="/api/v1/auth/login"`

// TestWrapOptionalAuthRoutes_Unauthenticated verifies that unauthenticated
// GET requests to wrapOptionalAuth page routes receive HTTP 200 with the
// login page rendered inline -- not a 401 JSON error. If any of these routes
// were changed from wrapOptionalAuth to wrapAuth in router.go, the response
// would be 401 JSON and these tests would catch the regression.
func TestWrapOptionalAuthRoutes_Unauthenticated(t *testing.T) {
	t.Parallel()

	routes := []struct {
		name string
		path string
	}{
		{name: "foreign-files page", path: "/settings/foreign-files"},
		{name: "foreign-files allowlist page", path: "/settings/foreign-files/allowlist"},
		{name: "reports/duplicates page", path: "/reports/duplicates"},
		{name: "re-identify wizard step", path: "/artists/re-identify/wizard/fake-sid/step/0"},
		{name: "artists list page", path: "/artists"},
		{name: "artist detail page", path: "/artists/nonexistent-id"},
		{name: "artist images page", path: "/artists/nonexistent-id/images"},
		// /reports/compliance itself 302s full-page requests to the workspace
		// (#1757 PR-4), so the optional-auth page route to exercise is /reports.
		{name: "reports workspace page", path: "/reports"},
	}

	for _, tc := range routes {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// testRouter includes all services (artist, rule, etc.) that the
			// handlers reference after the auth check.  For the unauthenticated
			// flow the handlers return early at requireAuth, so a minimal router
			// would also work, but using testRouter keeps setup consistent with
			// the authenticated subtest below.
			r, _ := testRouter(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			mux := r.Handler(ctx)

			// No session cookie: unauthenticated request.
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			// Must return 200: the login page is rendered inline, not a 401.
			if w.Code != http.StatusOK {
				t.Fatalf("unauthenticated %s: status = %d, want 200 (login page); body: %.200s",
					tc.path, w.Code, w.Body.String())
			}

			body := w.Body.String()

			// Must contain the login form action so we know the login page rendered.
			if !strings.Contains(body, loginPageMarker) {
				t.Errorf("unauthenticated %s: response does not contain login form marker %q; got %.200s",
					tc.path, loginPageMarker, body)
			}

			// Must contain a password field -- a further signal that the full
			// login form rendered, not some other 200 page.
			if !strings.Contains(body, `type="password"`) {
				t.Errorf("unauthenticated %s: response does not contain password field; got %.200s",
					tc.path, body)
			}
		})
	}
}

// TestWrapOptionalAuthRoutes_AuthenticatedAdmin verifies that requests with a
// valid admin session cookie pass through wrapOptionalAuth and reach the real
// handler -- the response must not be the login page. For routes that require
// an admin role (foreign-files, duplicates), the admin session grants access;
// for routes that need a real resource ID (artist detail, images), a 404 from
// the handler is acceptable and proves the middleware layer did not reject the
// request.
func TestWrapOptionalAuthRoutes_AuthenticatedAdmin(t *testing.T) {
	t.Parallel()

	// adminSession creates an admin user and returns its session cookie.
	// Reuses the pattern from TestNotificationsPageRedirect.
	adminSession := func(t *testing.T, r *Router) *http.Cookie {
		t.Helper()
		ctx := context.Background()
		user, err := r.authService.CreateLocalUser(ctx, "testadmin", "Password123!", "Admin", "administrator", "")
		if err != nil {
			t.Fatalf("CreateLocalUser: %v", err)
		}
		token, err := r.authService.CreateSession(ctx, user.ID)
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		return &http.Cookie{Name: "session", Value: token}
	}

	routes := []struct {
		name           string
		path           string
		wantBody       string // non-empty: substring that must appear in a 200 body
		acceptStatuses []int  // allowed non-login response codes
	}{
		{
			name:           "foreign-files page",
			path:           "/settings/foreign-files",
			wantBody:       "foreign-files-table",
			acceptStatuses: []int{http.StatusOK},
		},
		{
			name:           "foreign-files allowlist page",
			path:           "/settings/foreign-files/allowlist",
			wantBody:       "foreign-allowlist-table",
			acceptStatuses: []int{http.StatusOK},
		},
		{
			name:           "reports/duplicates page",
			path:           "/reports/duplicates",
			wantBody:       "artist-duplicates-table",
			acceptStatuses: []int{http.StatusOK},
		},
		{
			// Wizard session "fake-sid" does not exist, so the handler returns
			// 404. That is intentional: it proves wrapOptionalAuth passed the
			// request through and the handler's auth check was satisfied. A
			// regression to wrapAuth would return 401 JSON instead.
			name:           "re-identify wizard step (invalid sid)",
			path:           "/artists/re-identify/wizard/fake-sid/step/0",
			wantBody:       "",
			acceptStatuses: []int{http.StatusNotFound},
		},
		{
			name:           "artists list page",
			path:           "/artists",
			wantBody:       "artist-content",
			acceptStatuses: []int{http.StatusOK},
		},
		{
			// No artist exists in the test DB so the handler returns 404.
			name:           "artist detail page (nonexistent id)",
			path:           "/artists/nonexistent-id",
			wantBody:       "",
			acceptStatuses: []int{http.StatusNotFound},
		},
		{
			// No artist exists in the test DB so the handler returns 404.
			name:           "artist images page (nonexistent id)",
			path:           "/artists/nonexistent-id/images",
			wantBody:       "",
			acceptStatuses: []int{http.StatusNotFound},
		},
		{
			// The promoted reports workspace, compliance tab active (#1757
			// PR-4); the compliance table renders inside the right pane.
			name:           "reports workspace page",
			path:           "/reports",
			wantBody:       "compliance-table",
			acceptStatuses: []int{http.StatusOK},
		},
		{
			// Full-page /reports/compliance 302s to the workspace with the
			// compliance tab active (#1757 PR-4). A regression to wrapAuth
			// would return 401 JSON instead of the redirect.
			name:           "reports/compliance full-page redirect",
			path:           "/reports/compliance",
			wantBody:       "",
			acceptStatuses: []int{http.StatusFound},
		},
	}

	for _, tc := range routes {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// testRouter wires the artist service and rule service, which the
			// artist list, detail, images, and compliance handlers need after
			// the auth check passes.
			r, _ := testRouter(t)
			cookie := adminSession(t, r)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			mux := r.Handler(ctx)

			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.AddCookie(cookie)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			// Must not return the login page: the session was valid so the
			// handler should have been reached, not the auth redirect.
			body := w.Body.String()
			if strings.Contains(body, loginPageMarker) {
				t.Fatalf("authenticated %s: response contains login page marker -- middleware rejected the session; status=%d",
					tc.path, w.Code)
			}

			// Must be one of the expected status codes.
			found := false
			for _, s := range tc.acceptStatuses {
				if w.Code == s {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("authenticated %s: status = %d, want one of %v; body: %.200s",
					tc.path, w.Code, tc.acceptStatuses, body)
			}

			// For routes with a known page-content marker, verify it is present.
			if tc.wantBody != "" && !strings.Contains(body, tc.wantBody) {
				t.Errorf("authenticated %s: response does not contain %q; got %.200s",
					tc.path, tc.wantBody, body)
			}
		})
	}
}
