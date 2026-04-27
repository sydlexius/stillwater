package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
)

// TestArtistDetail_StaleImageRow_RendersPlaceholderNot500 reproduces issue
// #1101: an artist with a stale artist_images row claiming a thumb exists
// while the artist has an empty path (no servable file) renders the detail
// page successfully (HTTP 200) and the thumb <img> tag includes an onerror
// handler so the browser falls back to a placeholder rather than displaying
// the broken-image icon. The async lazy-clear flow already corrects the
// registry row on first failed fetch; this test pins the rendering invariant
// so the FIRST page load degrades gracefully.
func TestArtistDetail_StaleImageRow_RendersPlaceholderNot500(t *testing.T) {
	r, artistSvc := testRouter(t)

	// Insert an artist with empty path so image lookups cannot find files
	// on disk. Path is the canonical signal that this is an API-imported
	// artist with no filesystem footprint.
	a := &artist.Artist{
		Name:     "Stale Image Test",
		SortName: "Stale Image Test",
		Path:     "",
	}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Plant a stale artist_images row claiming the thumb exists. This
	// simulates the registry-vs-disk drift described in #1101: the DB
	// believes thumb_exists=1 but no serveable file exists.
	_, err := r.db.ExecContext(context.Background(),
		`INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag) VALUES (?, ?, 'thumb', 0, 1)`,
		"stale-thumb-row", a.ID)
	if err != nil {
		t.Fatalf("inserting stale image row: %v", err)
	}

	// Render the artist detail page.
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/artists/"+a.ID, nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handleArtistDetailPage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (page must not 500 on stale image rows)", w.Code)
	}

	body := w.Body.String()

	// The thumb <img> tag MUST be guarded with onerror so the browser hides
	// the broken-image icon when the file 404s. Without this, the user
	// sees a broken image on first render before the lazy-clear self-heal.
	if !strings.Contains(body, "/images/thumb/file") {
		t.Fatal("expected thumb img src in body when ThumbExists=true")
	}
	// Match the onerror handler tolerantly: any quoting style and any
	// whitespace around `=` should pass, so cosmetic templ output changes
	// (single vs double quotes, attribute reordering) don't break the test.
	onerrorRE := regexp.MustCompile(`onerror=["'][^"']*display\s*=\s*['"]none`)
	if !onerrorRE.MatchString(body) {
		t.Error("thumb <img> must include onerror handler hiding the element so a stale registry row degrades to a placeholder rather than showing the broken-image icon")
	}
}
