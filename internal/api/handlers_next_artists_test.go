package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/auth"
)

// TestHandleNextArtistsPage_RendersNextWhenChannelNext verifies that when the
// resolved UI channel is "next", GET /next/artists renders the next/ artists
// shell (M55 #1335). The UX middleware resolves the channel from SW_UX=next, so
// the handler renders next.ArtistsPage rather than falling back to stable.
func TestHandleNextArtistsPage_RendersNextWhenChannelNext(t *testing.T) {
	t.Parallel()
	r, _, artistSvc := testRouterWithLibrary(t)
	if err := artistSvc.Create(context.Background(), &artist.Artist{Name: "Alpha Artist"}); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Wrap the handler in the UX middleware in "next" mode so the request
	// context carries UXNext, exactly as it would in production.
	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextArtistsPage))

	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/artists", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "sw-next-artists") {
		t.Errorf("next channel should render next.ArtistsPage (sw-next-artists absent)")
	}
	// The reused body, flyout, and behavior script must all be present so the
	// page is behaviorally complete, not a bare shell.
	for _, marker := range []string{`id="artist-content"`, `id="artist-filters-flyout"`, "setSortColumn"} {
		if !strings.Contains(body, marker) {
			t.Errorf("next page missing %q", marker)
		}
	}
}

// TestHandleNextArtistsPage_StableMode404 verifies that GET /next/artists in
// stable mode returns 404. The /next/* lane is gated by the UX middleware so it
// is completely unreachable when the lane is disabled.
func TestHandleNextArtistsPage_StableMode404(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithLibrary(t)

	h := middleware.UX("stable", "")(http.HandlerFunc(r.handleNextArtistsPage))

	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/artists", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (stable mode must 404 /next/ routes)", w.Code)
	}
}

// TestHandleNextArtistsPage_OptOutHeader404 verifies the handler-level
// decision-12 guard: when the lane IS enabled (next/dual mode) but the
// per-request X-Stillwater-UX: stable header opts back to the stable channel,
// the handler returns 404. The channel is injected directly via
// WithTestUXChannel, simulating the header opt-out scenario without relying on
// the middleware-level gate (which is tested by TestHandleNextArtistsPage_StableMode404).
func TestHandleNextArtistsPage_OptOutHeader404(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithLibrary(t)

	ctx := middleware.WithTestUXChannel(context.Background(), middleware.UXStable)
	ctx = middleware.WithTestUserID(ctx, "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/artists", nil)
	w := httptest.NewRecorder()
	r.handleNextArtistsPage(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("opt-out header: status = %d, want 404 (decision 12)", w.Code)
	}
}

// TestHandleNextArtistsPage_WiresNextBaseURL verifies the PR's core routing fix
// end to end: when the channel is "next", buildArtistListData stamps the
// /next/artists BaseURL, so the rendered toolbar/pagination hx-get targets
// /next/artists and never the stable /artists. A regression that dropped the
// UXNext BaseURL branch in buildArtistListData would make this fail (the page
// would swap the stable table into the next shell).
func TestHandleNextArtistsPage_WiresNextBaseURL(t *testing.T) {
	t.Parallel()
	r, _, artistSvc := testRouterWithLibrary(t)
	if err := artistSvc.Create(context.Background(), &artist.Artist{Name: "Alpha Artist"}); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextArtistsPage))
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/artists", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `hx-get="/next/artists"`) {
		t.Errorf("next page must wire the /next/artists BaseURL (hx-get=\"/next/artists\" absent)")
	}
	if strings.Contains(body, `hx-get="/artists"`) {
		t.Errorf("next page must not target the stable /artists endpoint (would swap the stable table)")
	}
}

// TestHandleNextArtistsPage_HTMXFragmentDispatch verifies that an HTMX request
// (HX-Request: true) renders only the next/ table or grid FRAGMENT, not the full
// page shell, and that ?view= selects the right fragment. The table fragment
// carries the consolidated Sources/Coverage columns; the grid fragment carries
// the card grid layout. Neither must emit the full-page <html>/sidebar chrome.
func TestHandleNextArtistsPage_HTMXFragmentDispatch(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		view        string
		wantMarkers []string
		denyMarkers []string
	}{
		{
			name:        "table",
			view:        "table",
			wantMarkers: []string{`data-col="sources"`, `data-col="coverage"`, `id="artists-table"`},
			denyMarkers: []string{"grid-cols"},
		},
		{
			name:        "grid",
			view:        "grid",
			wantMarkers: []string{"grid-cols"},
			denyMarkers: []string{`id="artists-table"`},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, _, artistSvc := testRouterWithLibrary(t)
			if err := artistSvc.Create(context.Background(), &artist.Artist{Name: "Alpha Artist"}); err != nil {
				t.Fatalf("creating artist: %v", err)
			}

			h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextArtistsPage))
			ctx := middleware.WithTestUserID(context.Background(), "test-user")
			req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/artists?view="+tc.view, nil)
			req.Header.Set("HX-Request", "true")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", w.Code)
			}
			body := w.Body.String()

			// A fragment must NOT carry the full-page shell: no <html> document
			// element and no sidebar/page scoping chrome.
			for _, deny := range []string{"<html", "sw-next-artists"} {
				if strings.Contains(body, deny) {
					t.Errorf("%s fragment must not render full-page chrome (found %q)", tc.name, deny)
				}
			}
			// The fragment is the #artist-content swap target, not the whole page.
			if !strings.Contains(body, `id="artist-content"`) {
				t.Errorf("%s fragment must render the #artist-content swap target", tc.name)
			}
			for _, want := range tc.wantMarkers {
				if !strings.Contains(body, want) {
					t.Errorf("%s fragment missing marker %q", tc.name, want)
				}
			}
			for _, deny := range tc.denyMarkers {
				if strings.Contains(body, deny) {
					t.Errorf("%s fragment must not contain %q (wrong view selected)", tc.name, deny)
				}
			}
		})
	}
}

// TestBuildArtistListData_LoadsSavedViewsForNextChannel verifies that when the
// UX channel is UXNext and a saved_views preference is stored for the user,
// buildArtistListData loads the views and the rendered page includes the
// saved-views row (M55 #1777). This exercises the `if
// middleware.UXChannelFromContext == UXNext` branch in buildArtistListData and
// the parseSavedViews call on the stored preference string.
func TestBuildArtistListData_LoadsSavedViewsForNextChannel(t *testing.T) {
	t.Parallel()
	r, _, artistSvc := testRouterWithLibrary(t)
	if err := artistSvc.Create(context.Background(), &artist.Artist{Name: "Test Artist"}); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Create a real user to satisfy the FK constraint on user_preferences.
	authSvc := auth.NewService(r.db)
	if _, err := authSvc.Setup(context.Background(), "viewuser", "testpassword"); err != nil {
		t.Fatalf("creating user: %v", err)
	}
	var userID string
	if err := r.db.QueryRowContext(context.Background(),
		`SELECT id FROM users WHERE username = 'viewuser'`).Scan(&userID); err != nil {
		t.Fatalf("looking up user id: %v", err)
	}

	// Seed a valid saved_views preference row directly so the handler's read
	// path has data to load.
	savedViewsJSON := `[{"name":"My Saved View","params":"filter=complete","created_at":"2024-01-01T00:00:00Z"}]`
	if _, err := r.db.ExecContext(context.Background(),
		`INSERT INTO user_preferences (user_id, key, value, updated_at) VALUES (?, ?, ?, datetime('now'))
		 ON CONFLICT(user_id, key) DO UPDATE SET value = excluded.value`,
		userID, PrefSavedViews, savedViewsJSON); err != nil {
		t.Fatalf("seeding saved_views preference: %v", err)
	}

	// Request via UXNext channel.
	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextArtistsPage))
	ctx := middleware.WithTestUserID(context.Background(), userID)
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/artists", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	// The template renders #saved-views-row only when len(data.SavedViews) > 0.
	if !strings.Contains(body, `saved-views-row`) {
		t.Errorf("saved-views-row not rendered; saved_views preference was not loaded for next/ channel")
	}
	if !strings.Contains(body, "My Saved View") {
		t.Errorf("saved view name %q not found in rendered output", "My Saved View")
	}
}

// TestBuildArtistListData_SkipsSavedViewsForStableChannel verifies that the
// stable channel does NOT load saved views, keeping the DB query conditional.
func TestBuildArtistListData_SkipsSavedViewsForStableChannel(t *testing.T) {
	t.Parallel()
	r, _, artistSvc := testRouterWithLibrary(t)
	if err := artistSvc.Create(context.Background(), &artist.Artist{Name: "Test Artist"}); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Create a real user to satisfy the FK constraint on user_preferences.
	authSvc := auth.NewService(r.db)
	if _, err := authSvc.Setup(context.Background(), "stableuser", "testpassword"); err != nil {
		t.Fatalf("creating user: %v", err)
	}
	var userID string
	if err := r.db.QueryRowContext(context.Background(),
		`SELECT id FROM users WHERE username = 'stableuser'`).Scan(&userID); err != nil {
		t.Fatalf("looking up user id: %v", err)
	}

	// Seed a saved_views preference for the user.
	savedViewsJSON := `[{"name":"Stable View","params":"filter=complete","created_at":"2024-01-01T00:00:00Z"}]`
	if _, err := r.db.ExecContext(context.Background(),
		`INSERT INTO user_preferences (user_id, key, value, updated_at) VALUES (?, ?, ?, datetime('now'))
		 ON CONFLICT(user_id, key) DO UPDATE SET value = excluded.value`,
		userID, PrefSavedViews, savedViewsJSON); err != nil {
		t.Fatalf("seeding saved_views preference: %v", err)
	}

	// Request via stable channel: saved views must NOT appear.
	ctx := middleware.WithTestUXChannel(context.Background(), middleware.UXStable)
	ctx = middleware.WithTestUserID(ctx, userID)
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/artists", nil)
	w := httptest.NewRecorder()
	r.handleArtistsPage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "Stable View") {
		t.Errorf("saved view name appeared in stable channel response (should be skipped)")
	}
}

func TestHandleNextArtistsPage_InvalidSortReturnsBadRequest(t *testing.T) {
	t.Parallel()

	r, _, _ := testRouterWithLibrary(t)
	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextArtistsPage))

	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/artists?sort=invalid_sort_key", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}
