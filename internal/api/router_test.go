package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRouterRegistration verifies that all route patterns registered in
// Handler() are compatible with each other. Go 1.22+ panics when two
// patterns overlap ambiguously (e.g. "/{id}/dismiss" vs "/undo/{undoId}").
// This test catches such conflicts at CI time instead of at startup.
func TestRouterRegistration(t *testing.T) {
	t.Parallel()
	r := testRouterForOnboarding(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	defer func() {
		if v := recover(); v != nil {
			t.Fatalf("route registration panicked: %v", v)
		}
	}()

	_ = r.Handler(ctx)
}

// TestNotificationsPageRedirect verifies that GET /notifications returns a 301
// redirect to the root page (notifications are displayed inline on the home
// page, not on a separate route). It also verifies the redirect respects
// base_path and that API routes under /api/v1/notifications are unaffected.
func TestNotificationsPageRedirect(t *testing.T) {
	t.Parallel()
	// Helper: create a user session so requests pass auth middleware.
	setupSession := func(t *testing.T, r *Router) *http.Cookie {
		t.Helper()
		ctx := context.Background()
		user, err := r.authService.CreateLocalUser(ctx, "testuser", "password123", "Test", "administrator", "")
		if err != nil {
			t.Fatalf("CreateLocalUser: %v", err)
		}
		token, err := r.authService.CreateSession(ctx, user.ID)
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		return &http.Cookie{Name: "session", Value: token}
	}

	t.Run("no base path", func(t *testing.T) {
		r := testRouterForOnboarding(t)
		cookie := setupSession(t, r)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mux := r.Handler(ctx)

		req := httptest.NewRequest(http.MethodGet, "/notifications", nil)
		req.AddCookie(cookie)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)

		if w.Code != http.StatusMovedPermanently {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusMovedPermanently)
		}
		loc := w.Header().Get("Location")
		if loc != "/" {
			t.Fatalf("Location = %q, want %q", loc, "/")
		}
	})

	t.Run("with base path", func(t *testing.T) {
		r := testRouterWithBasePath(t, "/stillwater")
		cookie := setupSession(t, r)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mux := r.Handler(ctx)

		req := httptest.NewRequest(http.MethodGet, "/stillwater/notifications", nil)
		req.AddCookie(cookie)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)

		if w.Code != http.StatusMovedPermanently {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusMovedPermanently)
		}
		loc := w.Header().Get("Location")
		if loc != "/stillwater/" {
			t.Fatalf("Location = %q, want %q", loc, "/stillwater/")
		}
	})

	t.Run("API routes unaffected", func(t *testing.T) {
		r, _ := testRouter(t)
		cookie := setupSession(t, r)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mux := r.Handler(ctx)

		req := httptest.NewRequest(http.MethodGet, "/api/v1/notifications/counts", nil)
		req.AddCookie(cookie)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)

		// The API endpoint should NOT return a redirect; it should return a
		// normal response (200 for valid data, not 301).
		if w.Code == http.StatusMovedPermanently {
			t.Fatal("API route /api/v1/notifications/counts should not redirect")
		}
	})
}

// TestForeignFilesSettingsAlias301 verifies that the retired v1 foreign-files
// paths (M55 #1757 PR-6a promoted them to the Reports hub) 301-redirect to
// their canonical /reports/foreign-files counterparts so bookmarks and
// external links still resolve. Mirrors the /settings/artist-duplicates ->
// /reports/duplicates alias. The query string must be preserved.
func TestForeignFilesSettingsAlias301(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		reqPath string
		wantLoc string
	}{
		{"detected files", "/settings/foreign-files", "/reports/foreign-files"},
		{"allowlist", "/settings/foreign-files/allowlist", "/reports/foreign-files/allowlist"},
		{"query preserved", "/settings/foreign-files/allowlist?page=3", "/reports/foreign-files/allowlist?page=3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := testRouterForOnboarding(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			mux := r.Handler(ctx)

			req := httptest.NewRequest(http.MethodGet, tc.reqPath, nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			if w.Code != http.StatusMovedPermanently {
				t.Fatalf("%s: status = %d, want %d", tc.reqPath, w.Code, http.StatusMovedPermanently)
			}
			if loc := w.Header().Get("Location"); loc != tc.wantLoc {
				t.Fatalf("%s: Location = %q, want %q", tc.reqPath, loc, tc.wantLoc)
			}
		})
	}
}
