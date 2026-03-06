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

func TestArtistsPageSortParams(t *testing.T) {
	r, _, artistSvc := testRouterWithLibrary(t)

	a1 := &artist.Artist{Name: "Zydeco Band"}
	_ = artistSvc.Create(context.Background(), a1)
	a2 := &artist.Artist{Name: "Alpha Artist"}
	_ = artistSvc.Create(context.Background(), a2)

	cases := []struct {
		sort  string
		order string
		want  string // substring expected in response HTML
	}{
		{"name", "asc", `value="name"`},
		{"health_score", "desc", `value="health_score"`},
		{"updated_at", "asc", `value="updated_at"`},
	}

	for _, tc := range cases {
		t.Run(tc.sort+"_"+tc.order, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/artists?sort="+tc.sort+"&order="+tc.order, nil)
			req = req.WithContext(middleware.WithTestUserID(req.Context(), "test-user"))
			w := httptest.NewRecorder()
			r.handleArtistsPage(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status %d, want 200", w.Code)
			}
			body := w.Body.String()
			if !strings.Contains(body, tc.want) {
				t.Errorf("response does not contain %q", tc.want)
			}
		})
	}
}
