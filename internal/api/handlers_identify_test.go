package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/library"
)

// testRouterWithIdentify creates a router with library and artist services
// suitable for bulk-identify handler tests. It does not include an orchestrator,
// so Tier 2/3 searches will be skipped (tests that need them wire it up separately).
func testRouterWithIdentify(t *testing.T) (*Router, *library.Service, *artist.Service) {
	t.Helper()
	r, libSvc, artistSvc := testRouterWithLibrary(t)
	return r, libSvc, artistSvc
}

func TestBulkIdentify_ConcurrentReject(t *testing.T) {
	r, _, _ := testRouterWithIdentify(t)

	// Simulate a running identify job by setting progress directly.
	progress := &IdentifyProgress{Status: "running", Total: 10}
	r.identifyMu.Lock()
	r.identifyProgress = progress
	r.identifyMu.Unlock()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-identify", nil)
	w := httptest.NewRecorder()

	r.handleBulkIdentify(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusConflict, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if resp["status"] != "running" {
		t.Errorf("status = %v, want running", resp["status"])
	}
}

func TestBulkIdentifyProgress_Idle(t *testing.T) {
	r, _, _ := testRouterWithIdentify(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/bulk-identify", nil)
	w := httptest.NewRecorder()

	r.handleBulkIdentifyProgress(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if resp["status"] != "idle" {
		t.Errorf("status = %v, want idle", resp["status"])
	}
}

func TestBulkIdentifyProgress_WithRunningJob(t *testing.T) {
	r, _, _ := testRouterWithIdentify(t)

	// Simulate a running job with progress.
	progress := &IdentifyProgress{
		Status:      "running",
		Total:       50,
		Processed:   20,
		AutoLinked:  10,
		Queued:      5,
		Unmatched:   3,
		Failed:      2,
		CurrentName: "Pink Floyd",
	}
	r.identifyMu.Lock()
	r.identifyProgress = progress
	r.identifyMu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/bulk-identify", nil)
	w := httptest.NewRecorder()

	r.handleBulkIdentifyProgress(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if resp["status"] != "running" {
		t.Errorf("status = %v, want running", resp["status"])
	}
	if resp["total"] != float64(50) {
		t.Errorf("total = %v, want 50", resp["total"])
	}
	if resp["auto_linked"] != float64(10) {
		t.Errorf("auto_linked = %v, want 10", resp["auto_linked"])
	}
	if resp["current_name"] != "Pink Floyd" {
		t.Errorf("current_name = %v, want Pink Floyd", resp["current_name"])
	}
}

func TestBulkIdentifyCancel_NoJob(t *testing.T) {
	r, _, _ := testRouterWithIdentify(t)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/bulk-identify", nil)
	w := httptest.NewRecorder()

	r.handleBulkIdentifyCancel(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if resp["status"] != "idle" {
		t.Errorf("status = %v, want idle", resp["status"])
	}
}

func TestBulkIdentifyCancel_RunningJob(t *testing.T) {
	r, _, _ := testRouterWithIdentify(t)

	_, cancel := context.WithCancel(context.Background())
	progress := &IdentifyProgress{
		Status:   "running",
		cancelFn: cancel,
	}
	r.identifyMu.Lock()
	r.identifyProgress = progress
	r.identifyMu.Unlock()

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/bulk-identify", nil)
	w := httptest.NewRecorder()

	r.handleBulkIdentifyCancel(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if resp["status"] != "canceling" {
		t.Errorf("status = %v, want canceling", resp["status"])
	}
}

func TestBulkIdentify_NoUnidentified(t *testing.T) {
	r, _, artistSvc := testRouterWithIdentify(t)

	// Create an artist with an MBID (should not appear in missing_mbid filter).
	a := &artist.Artist{
		Name:          "Identified Artist",
		SortName:      "Identified Artist",
		Type:          "group",
		Path:          "/music/Identified Artist",
		MusicBrainzID: "12345678-1234-1234-1234-123456789abc",
	}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-identify", nil)
	w := httptest.NewRecorder()

	r.handleBulkIdentify(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if resp["status"] != "completed" {
		t.Errorf("status = %v, want completed", resp["status"])
	}
	if resp["total"] != float64(0) {
		t.Errorf("total = %v, want 0", resp["total"])
	}
}

func TestBulkIdentify_ExcludedSkipped(t *testing.T) {
	r, _, artistSvc := testRouterWithIdentify(t)

	// Create an excluded artist without MBID.
	a := &artist.Artist{
		Name:       "Excluded Artist",
		SortName:   "Excluded Artist",
		Type:       "group",
		Path:       "/music/Excluded Artist",
		IsExcluded: true,
	}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-identify", nil)
	w := httptest.NewRecorder()

	r.handleBulkIdentify(w, req)

	// Excluded artist filtered out, so no unidentified artists found.
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if resp["total"] != float64(0) {
		t.Errorf("total = %v, want 0 (excluded artist should be filtered)", resp["total"])
	}
}

func TestBulkIdentify_Tier1_ConnectionMatch(t *testing.T) {
	r, libSvc, artistSvc := testRouterWithIdentify(t)
	ctx := context.Background()

	// Create a connection library (non-manual source) with an identified artist.
	connLib := &library.Library{
		Name:   "Emby Music",
		Type:   library.TypeRegular,
		Source: library.SourceEmby,
	}
	if err := libSvc.Create(ctx, connLib); err != nil {
		t.Fatalf("creating connection library: %v", err)
	}

	connArtist := &artist.Artist{
		Name:          "Pink Floyd",
		SortName:      "Pink Floyd",
		Type:          "group",
		Path:          "/emby/Pink Floyd",
		LibraryID:     connLib.ID,
		MusicBrainzID: "83d91898-7763-47d7-b03b-b92132375c47",
		DiscogsID:     "123456",
	}
	if err := artistSvc.Create(ctx, connArtist); err != nil {
		t.Fatalf("creating connection artist: %v", err)
	}

	// Create a manual library with an unidentified artist of the same name.
	manualLib := &library.Library{
		Name:   "Local Music",
		Type:   library.TypeRegular,
		Source: library.SourceManual,
		Path:   t.TempDir(),
	}
	if err := libSvc.Create(ctx, manualLib); err != nil {
		t.Fatalf("creating manual library: %v", err)
	}

	unidentified := &artist.Artist{
		Name:      "Pink Floyd",
		SortName:  "Pink Floyd",
		Type:      "group",
		Path:      manualLib.Path + "/Pink Floyd",
		LibraryID: manualLib.ID,
	}
	if err := artistSvc.Create(ctx, unidentified); err != nil {
		t.Fatalf("creating unidentified artist: %v", err)
	}

	// Run bulk identify (no orchestrator, so only Tier 1 will work).
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-identify", nil)
	w := httptest.NewRecorder()

	r.handleBulkIdentify(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}

	// Poll until completed (max 10s).
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r.identifyMu.RLock()
		p := r.identifyProgress
		r.identifyMu.RUnlock()
		if p != nil {
			p.mu.RLock()
			done := p.Status == "completed" || p.Status == "canceled"
			p.mu.RUnlock()
			if done {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Verify the artist was auto-linked via Tier 1.
	updated, err := artistSvc.GetByID(ctx, unidentified.ID)
	if err != nil {
		t.Fatalf("getting updated artist: %v", err)
	}
	if updated.MusicBrainzID != "83d91898-7763-47d7-b03b-b92132375c47" {
		t.Errorf("MusicBrainzID = %q, want %q", updated.MusicBrainzID, "83d91898-7763-47d7-b03b-b92132375c47")
	}

	// Verify progress counters.
	r.identifyMu.RLock()
	p := r.identifyProgress
	r.identifyMu.RUnlock()
	if p == nil {
		t.Fatal("expected non-nil progress after completion")
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.AutoLinked < 1 {
		t.Errorf("auto_linked = %d, want >= 1", p.AutoLinked)
	}
}

func TestBulkIdentify_LockedArtistSkipped(t *testing.T) {
	r, _, artistSvc := testRouterWithIdentify(t)
	ctx := context.Background()

	// Create an artist without MBID, then lock it via the service (the DB
	// has a CHECK constraint requiring locked_at when locked = 1).
	a := &artist.Artist{
		Name:     "Locked Artist",
		SortName: "Locked Artist",
		Type:     "group",
		Path:     "/music/Locked Artist",
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	if err := artistSvc.Lock(ctx, a.ID, "user"); err != nil {
		t.Fatalf("locking artist: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-identify", nil)
	w := httptest.NewRecorder()

	r.handleBulkIdentify(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}

	// Poll until completed.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r.identifyMu.RLock()
		p := r.identifyProgress
		r.identifyMu.RUnlock()
		if p != nil {
			p.mu.RLock()
			done := p.Status == "completed" || p.Status == "canceled"
			p.mu.RUnlock()
			if done {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Verify the locked artist was NOT modified.
	updated, err := artistSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("getting updated artist: %v", err)
	}
	if updated.MusicBrainzID != "" {
		t.Errorf("MusicBrainzID = %q, want empty (locked artist should be skipped)", updated.MusicBrainzID)
	}
}

func TestBulkIdentifyLink(t *testing.T) {
	r, _, artistSvc := testRouterWithIdentify(t)
	ctx := context.Background()

	// Create an artist without MBID.
	a := &artist.Artist{
		Name:     "Unlinked Artist",
		SortName: "Unlinked Artist",
		Type:     "group",
		Path:     "/music/Unlinked Artist",
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Populate the review queue.
	r.identifyMu.Lock()
	r.identifyProgress = &IdentifyProgress{
		Status: "completed",
		ReviewQueue: []IdentifyCandidate{
			{
				ArtistID:   a.ID,
				ArtistName: a.Name,
				Tier:       "name",
			},
		},
	}
	r.identifyMu.Unlock()

	// Call the link endpoint.
	body := strings.NewReader(`{"artist_id":"` + a.ID + `","mbid":"test-mbid-1234"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-identify/link", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleBulkIdentifyLink(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// Verify MBID was set.
	updated, err := artistSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("getting updated artist: %v", err)
	}
	if updated.MusicBrainzID != "test-mbid-1234" {
		t.Errorf("MusicBrainzID = %q, want %q", updated.MusicBrainzID, "test-mbid-1234")
	}

	// Verify artist was removed from review queue.
	r.identifyMu.RLock()
	p := r.identifyProgress
	r.identifyMu.RUnlock()
	if p != nil {
		p.mu.RLock()
		qLen := len(p.ReviewQueue)
		p.mu.RUnlock()
		if qLen != 0 {
			t.Errorf("review queue length = %d, want 0", qLen)
		}
	}
}

func TestBulkIdentifyLink_MissingFields(t *testing.T) {
	r, _, _ := testRouterWithIdentify(t)

	body := strings.NewReader(`{"artist_id":"some-id"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-identify/link", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleBulkIdentifyLink(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestBulkIdentifyLink_ArtistNotFound(t *testing.T) {
	r, _, _ := testRouterWithIdentify(t)

	body := strings.NewReader(`{"artist_id":"nonexistent","mbid":"test-mbid"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-identify/link", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleBulkIdentifyLink(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusNotFound, w.Body.String())
	}
}

func TestBulkIdentify_CompletedJobAllowsRestart(t *testing.T) {
	r, _, _ := testRouterWithIdentify(t)

	// Simulate a completed identify job.
	r.identifyMu.Lock()
	r.identifyProgress = &IdentifyProgress{Status: "completed", Total: 5}
	r.identifyMu.Unlock()

	// Attempting to start a new job should succeed (slot is not "running").
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-identify", nil)
	w := httptest.NewRecorder()

	r.handleBulkIdentify(w, req)

	// No unidentified artists, so it returns 200 "completed" with 0 total.
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if resp["status"] != "completed" {
		t.Errorf("status = %v, want completed (restart should be allowed)", resp["status"])
	}
}

func TestBulkIdentify_WithLibraryFilter(t *testing.T) {
	r, libSvc, artistSvc := testRouterWithIdentify(t)
	ctx := context.Background()

	// Create two manual libraries.
	lib1 := &library.Library{
		Name:   "Library 1",
		Type:   library.TypeRegular,
		Source: library.SourceManual,
		Path:   t.TempDir(),
	}
	lib2 := &library.Library{
		Name:   "Library 2",
		Type:   library.TypeRegular,
		Source: library.SourceManual,
		Path:   t.TempDir(),
	}
	if err := libSvc.Create(ctx, lib1); err != nil {
		t.Fatalf("creating lib1: %v", err)
	}
	if err := libSvc.Create(ctx, lib2); err != nil {
		t.Fatalf("creating lib2: %v", err)
	}

	// Create unidentified artists in both libraries.
	a1 := &artist.Artist{
		Name:      "Artist In Lib1",
		SortName:  "Artist In Lib1",
		Type:      "group",
		Path:      lib1.Path + "/Artist In Lib1",
		LibraryID: lib1.ID,
	}
	a2 := &artist.Artist{
		Name:      "Artist In Lib2",
		SortName:  "Artist In Lib2",
		Type:      "group",
		Path:      lib2.Path + "/Artist In Lib2",
		LibraryID: lib2.ID,
	}
	for _, a := range []*artist.Artist{a1, a2} {
		if err := artistSvc.Create(ctx, a); err != nil {
			t.Fatalf("creating artist %s: %v", a.Name, err)
		}
	}

	// Run bulk identify filtered to lib1 only.
	body := strings.NewReader(`{"library_id":"` + lib1.ID + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-identify", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleBulkIdentify(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	// Should only include 1 artist (from lib1).
	if resp["total"] != float64(1) {
		t.Errorf("total = %v, want 1 (only lib1 artists)", resp["total"])
	}
}
