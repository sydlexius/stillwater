package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
)

func TestHandleArtistsBadge_ZeroCount(t *testing.T) {
	r, _ := testRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/badge", nil)
	w := httptest.NewRecorder()
	r.handleArtistsBadge(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if body := w.Body.String(); body != "" {
		t.Errorf("body = %q, want empty for zero count", body)
	}
}

func TestHandleArtistsBadge_NonZeroCount(t *testing.T) {
	r, artistSvc := testRouter(t)
	if err := artistSvc.Create(context.Background(), &artist.Artist{Name: "Badge Artist"}); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/badge", nil)
	w := httptest.NewRecorder()
	r.handleArtistsBadge(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if body := w.Body.String(); !strings.Contains(body, "1") {
		t.Errorf("body = %q, want span containing count", body)
	}
}

func TestHandleArtistsBadge_ServiceError(t *testing.T) {
	r, _ := testRouter(t)
	// Close the DB to force a service error. testRouter's t.Cleanup will
	// attempt a second close; that error is intentionally ignored there.
	if err := r.db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/badge", nil)
	w := httptest.NewRecorder()
	r.handleArtistsBadge(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}

func TestArtistsPageSortParams(t *testing.T) {
	r, _, artistSvc := testRouterWithLibrary(t)

	a1 := &artist.Artist{Name: "Zydeco Band"}
	if err := artistSvc.Create(context.Background(), a1); err != nil {
		t.Fatalf("creating artist a1: %v", err)
	}
	a2 := &artist.Artist{Name: "Alpha Artist"}
	if err := artistSvc.Create(context.Background(), a2); err != nil {
		t.Fatalf("creating artist a2: %v", err)
	}

	cases := []struct {
		sort  string
		order string
	}{
		{"name", "asc"},
		{"name", "desc"},
		{"health_score", "desc"},
		{"updated_at", "asc"},
	}

	for _, tc := range cases {
		t.Run(tc.sort+"_"+tc.order, func(t *testing.T) {
			ctx := middleware.WithTestUserID(context.Background(), "test-user")
			req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/artists?sort="+tc.sort+"&order="+tc.order, nil)
			w := httptest.NewRecorder()
			r.handleArtistsPage(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status %d, want 200", w.Code)
			}
			body := w.Body.String()
			// Check that sort and order values are reflected in the specific hidden inputs.
			wantSort := fmt.Sprintf(`id="artist-sort-input" value=%q`, tc.sort)
			wantOrder := fmt.Sprintf(`id="artist-order-input" value=%q`, tc.order)
			if !strings.Contains(body, wantSort) {
				t.Errorf("response missing sort input: want %s", wantSort)
			}
			if !strings.Contains(body, wantOrder) {
				t.Errorf("response missing order input: want %s", wantOrder)
			}
		})
	}
}
