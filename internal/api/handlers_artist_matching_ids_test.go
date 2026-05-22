package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// matchingIDsResponse mirrors the JSON shape returned by handleArtistMatchingIDs.
type matchingIDsResponse struct {
	IDs    []string `json:"ids"`
	Total  int      `json:"total"`
	Capped bool     `json:"capped"`
}

// TestHandleArtistMatchingIDs_Empty verifies that the endpoint returns an
// empty ids slice, zero total, and capped=false when no artists exist.
func TestHandleArtistMatchingIDs_Empty(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/matching-ids", nil)
	w := httptest.NewRecorder()
	r.handleArtistMatchingIDs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp matchingIDsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Total != 0 {
		t.Errorf("total = %d, want 0", resp.Total)
	}
	if resp.Capped {
		t.Error("capped = true, want false on empty result")
	}
	if len(resp.IDs) != 0 {
		t.Errorf("len(ids) = %d, want 0", len(resp.IDs))
	}
}

// TestHandleArtistMatchingIDs_ReturnsAllIDs verifies that all matching artist
// IDs are returned when no filters narrow the result.
func TestHandleArtistMatchingIDs_ReturnsAllIDs(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	ctx := context.Background()

	for _, name := range []string{"Radiohead", "Coldplay", "Portishead"} {
		if err := artistSvc.Create(ctx, &artist.Artist{Name: name}); err != nil {
			t.Fatalf("creating artist %q: %v", name, err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/matching-ids", nil)
	w := httptest.NewRecorder()
	r.handleArtistMatchingIDs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp matchingIDsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Total != 3 {
		t.Errorf("total = %d, want 3", resp.Total)
	}
	if resp.Capped {
		t.Error("capped = true, want false for small result set")
	}
	if len(resp.IDs) != 3 {
		t.Errorf("len(ids) = %d, want 3", len(resp.IDs))
	}
}

// TestHandleArtistMatchingIDs_SearchFilter verifies that the search query
// param is forwarded to the repository filter and only matching IDs are
// returned.
func TestHandleArtistMatchingIDs_SearchFilter(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	ctx := context.Background()

	for _, name := range []string{"Radiohead", "Coldplay", "Radio Moscow"} {
		if err := artistSvc.Create(ctx, &artist.Artist{Name: name}); err != nil {
			t.Fatalf("creating artist %q: %v", name, err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/matching-ids?search=Radio", nil)
	w := httptest.NewRecorder()
	r.handleArtistMatchingIDs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp matchingIDsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Total != 2 {
		t.Errorf("total = %d, want 2 (Radiohead + Radio Moscow)", resp.Total)
	}
	if len(resp.IDs) != 2 {
		t.Errorf("len(ids) = %d, want 2", len(resp.IDs))
	}
}

// TestHandleArtistMatchingIDs_CappedResult verifies that when the matching
// count exceeds MaxListIDs the response is capped and capped=true is set.
// The total still reflects the real match count.
func TestHandleArtistMatchingIDs_CappedResult(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	ctx := context.Background()

	// Insert one more than the cap to trigger capping.
	target := artist.MaxListIDs + 3
	for i := 0; i < target; i++ {
		a := &artist.Artist{Name: fmt.Sprintf("Artist%04d", i)}
		if err := artistSvc.Create(ctx, a); err != nil {
			t.Fatalf("creating artist %d: %v", i, err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/matching-ids", nil)
	w := httptest.NewRecorder()
	r.handleArtistMatchingIDs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp matchingIDsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Total != target {
		t.Errorf("total = %d, want %d", resp.Total, target)
	}
	if !resp.Capped {
		t.Error("capped = false, want true when total > MaxListIDs")
	}
	if len(resp.IDs) != artist.MaxListIDs {
		t.Errorf("len(ids) = %d, want MaxListIDs (%d)", len(resp.IDs), artist.MaxListIDs)
	}
}

// TestHandleArtistMatchingIDs_ServiceError verifies that a 500 is returned
// when the artist service returns an error (simulated by closing the DB).
func TestHandleArtistMatchingIDs_ServiceError(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	// Close the DB so the service cannot query.
	if err := r.db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/matching-ids", nil)
	w := httptest.NewRecorder()
	r.handleArtistMatchingIDs(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}
