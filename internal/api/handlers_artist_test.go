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
	"github.com/sydlexius/stillwater/internal/connection"
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

// TestArtistDetailPage_TabDebugFallback verifies that tab=debug falls back to
// overview when the setting is disabled, when no connections exist, or when
// only non-debug-capable (Lidarr) connections exist.
func TestArtistDetailPage_TabDebugFallback(t *testing.T) {
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Debug Tab Artist")

	// Helper to enable the show_platform_debug setting.
	enableDebug := func() {
		_, err := r.db.ExecContext(context.Background(),
			`INSERT OR REPLACE INTO settings (key, value) VALUES ('show_platform_debug', 'true')`)
		if err != nil {
			t.Fatalf("setting show_platform_debug: %v", err)
		}
	}

	// Helper to add a connection and link it to the artist.
	addConn := func(id, connType string) {
		c := &connection.Connection{
			ID:      id,
			Name:    id,
			Type:    connType,
			URL:     "http://localhost:8096",
			APIKey:  "test-key",
			Enabled: true,
			Status:  "ok",
		}
		if err := r.connectionService.Create(context.Background(), c); err != nil {
			t.Fatalf("creating connection %s: %v", id, err)
		}
		if err := artistSvc.SetPlatformID(context.Background(), a.ID, id, "platform-"+id); err != nil {
			t.Fatalf("setting platform ID for %s: %v", id, err)
		}
	}

	// Helper to make a request with tab=debug and return the response body.
	doRequest := func() string {
		ctx := middleware.WithTestUserID(context.Background(), "test-user")
		req := httptest.NewRequestWithContext(ctx, http.MethodGet,
			"/artists/"+a.ID+"?tab=debug", nil)
		req.SetPathValue("id", a.ID)
		w := httptest.NewRecorder()
		r.handleArtistDetailPage(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		return w.Body.String()
	}

	// Case 1: setting disabled, no connections -- debug panel should be hidden.
	t.Run("setting_disabled", func(t *testing.T) {
		body := doRequest()
		if strings.Contains(body, `id="tab-debug"`) {
			t.Error("debug tab should not appear when setting is disabled")
		}
	})

	// Case 2: setting enabled but only Lidarr connection -- no debug-capable connection.
	t.Run("lidarr_only", func(t *testing.T) {
		enableDebug()
		addConn("conn-lidarr", connection.TypeLidarr)
		body := doRequest()
		if strings.Contains(body, `id="tab-debug"`) {
			t.Error("debug tab should not appear with only Lidarr connections")
		}
	})

	// Case 3: setting enabled with Emby connection -- debug tab should appear.
	t.Run("emby_connection", func(t *testing.T) {
		addConn("conn-emby", connection.TypeEmby)
		body := doRequest()
		if !strings.Contains(body, `id="tab-debug"`) {
			t.Error("debug tab should appear when setting is enabled and Emby connection exists")
		}
		// The debug panel should be present since tab=debug is active with a valid connection.
		if !strings.Contains(body, `data-tab-panel="debug"`) {
			t.Error("debug panel should be rendered when tab=debug is active with Emby connection")
		}
	})
}
