package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
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

	// Capture the generated IDs keyed by name. SortName is set explicitly so
	// the sort_name-then-id ordering is deterministic and can be asserted.
	created := make(map[string]string) // name -> id
	for _, name := range []string{"Radiohead", "Coldplay", "Portishead"} {
		a := &artist.Artist{Name: name, SortName: name}
		if err := artistSvc.Create(ctx, a); err != nil {
			t.Fatalf("creating artist %q: %v", name, err)
		}
		created[name] = a.ID
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
	// Assert the exact IDs in sort_name order, not just the count.
	want := []string{created["Coldplay"], created["Portishead"], created["Radiohead"]}
	if !slices.Equal(resp.IDs, want) {
		t.Fatalf("ids = %v, want %v (sorted by sort_name)", resp.IDs, want)
	}
}

// TestHandleArtistMatchingIDs_SearchFilter verifies that the search query
// param is forwarded to the repository filter and only matching IDs are
// returned.
func TestHandleArtistMatchingIDs_SearchFilter(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	ctx := context.Background()

	created := make(map[string]string) // name -> id
	for _, name := range []string{"Radiohead", "Coldplay", "Radio Moscow"} {
		a := &artist.Artist{Name: name, SortName: name}
		if err := artistSvc.Create(ctx, a); err != nil {
			t.Fatalf("creating artist %q: %v", name, err)
		}
		created[name] = a.ID
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
	// Assert the exact matching IDs: "Radio Moscow" sorts before "Radiohead"
	// (space < 'h'), and "Coldplay" must be absent from the search result.
	want := []string{created["Radio Moscow"], created["Radiohead"]}
	if !slices.Equal(resp.IDs, want) {
		t.Fatalf("ids = %v, want %v (search=Radio, sorted by sort_name)", resp.IDs, want)
	}
}

// TestHandleArtistMatchingIDs_CappedResult verifies that when the matching
// count exceeds MaxListIDs the response is capped and capped=true is set.
// The total still reflects the real match count.
func TestHandleArtistMatchingIDs_CappedResult(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	ctx := context.Background()

	// Insert more than the cap to trigger capping. Zero-padded names with an
	// explicit SortName make the sort_name ordering match insertion order, so
	// the returned (capped) slice is the first MaxListIDs IDs exactly.
	target := artist.MaxListIDs + 3
	idsByIndex := make([]string, target)
	for i := 0; i < target; i++ {
		name := fmt.Sprintf("Artist%04d", i)
		a := &artist.Artist{Name: name, SortName: name}
		if err := artistSvc.Create(ctx, a); err != nil {
			t.Fatalf("creating artist %d: %v", i, err)
		}
		idsByIndex[i] = a.ID
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
	// The capped slice must be exactly the first MaxListIDs IDs in sort order,
	// and the overflow IDs must not appear.
	if !slices.Equal(resp.IDs, idsByIndex[:artist.MaxListIDs]) {
		t.Fatalf("ids did not match the first %d IDs in sort_name order", artist.MaxListIDs)
	}
	overflow := make(map[string]struct{}, len(idsByIndex)-artist.MaxListIDs)
	for _, id := range idsByIndex[artist.MaxListIDs:] {
		overflow[id] = struct{}{}
	}
	for i, id := range resp.IDs {
		if _, isOverflow := overflow[id]; isOverflow {
			t.Errorf("ids[%d] = %q is an overflow ID that should have been capped out", i, id)
		}
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
