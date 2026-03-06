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
