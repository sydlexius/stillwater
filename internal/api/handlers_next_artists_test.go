package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
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
