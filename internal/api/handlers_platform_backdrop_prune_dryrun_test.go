package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/publish"
)

// TestPlatformBackdropDuplicatesPrune_DryRunKeepsTheCachedReport. A rehearsal
// changed nothing on any platform, so invalidating the cached report would
// cost the operator a fresh 62-second sweep AND hide the very rows they are
// rehearsing against.
//
// The precondition matters: the cache must be POPULATED first, or "still
// populated afterwards" is true of a cache that was never there.
func TestPlatformBackdropDuplicatesPrune_DryRunKeepsTheCachedReport(t *testing.T) {
	t.Parallel()
	r := testRouterWithPlatformPublisher(t)
	r.storePlatformDupReport(publish.PlatformBackdropDupReport{
		ArtistsAffected: 1, RedundantBackdrops: 3,
	}, time.Now())
	if _, _, ok := r.platformDupReportSnapshot(); !ok {
		t.Fatal("precondition: the cached report must be populated before the dry run")
	}

	req := httptest.NewRequestWithContext(adminContext(), http.MethodPost,
		"/api/v1/reports/platform-backdrop-duplicates/prune",
		strings.NewReader(`{"all_artists": true, "dry_run": true}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.handlePlatformBackdropDuplicatesPrune(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if _, _, ok := r.platformDupReportSnapshot(); !ok {
		t.Error("a DRY RUN invalidated the cached report; a rehearsal that costs a full re-sweep will not get used")
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding body: %v", err)
	}
	if dry, _ := got["dry_run"].(bool); !dry {
		t.Error("dry_run not echoed in the response body; a rehearsal must be distinguishable from a run that deleted")
	}
	if _, ok := got["plan"]; !ok {
		t.Error("no plan in the dry-run body; the plan is the entire deliverable of a rehearsal")
	}
}

// TestPlatformBackdropDuplicatesPrune_LiveRunInvalidatesTheCachedReport is the
// counterweight to the dry-run test above. A handler that never invalidated
// would pass that one and would leave the report listing artwork the prune
// just deleted.
func TestPlatformBackdropDuplicatesPrune_LiveRunInvalidatesTheCachedReport(t *testing.T) {
	t.Parallel()
	r := testRouterWithPlatformPublisher(t)
	r.storePlatformDupReport(publish.PlatformBackdropDupReport{
		ArtistsAffected: 1, RedundantBackdrops: 3,
	}, time.Now())
	if _, _, ok := r.platformDupReportSnapshot(); !ok {
		t.Fatal("precondition: the cached report must be populated before the live run")
	}

	req := httptest.NewRequestWithContext(adminContext(), http.MethodPost,
		"/api/v1/reports/platform-backdrop-duplicates/prune", allArtistsPruneBody())
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.handlePlatformBackdropDuplicatesPrune(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if _, _, ok := r.platformDupReportSnapshot(); ok {
		t.Error("a LIVE run left the cached report standing; the page would keep listing rows the prune deleted")
	}
}

// pagedFailAfterFirstLister answers page 1 with a fixed slice and errors on
// every later page, which is the shape of a real paging failure: the
// publisher's library-wide walk has already processed page 1 (and recorded
// whatever per-artist failures that produced) before the List call that
// aborts the whole run.
type pagedFailAfterFirstLister struct {
	page1 []artist.Artist
}

func (l *pagedFailAfterFirstLister) List(_ context.Context, params artist.ListParams) ([]artist.Artist, int, error) {
	if params.Page == 1 {
		return l.page1, len(l.page1), nil
	}
	return nil, 0, fmt.Errorf("test: artist listing unavailable at page %d", params.Page)
}

// TestPlatformBackdropDuplicatesPrune_DryRunPlusErrorKeepsTheCacheAndReportsHonestly
// is the reproduction for hostile-review Finding 1 (#3157). A DRY RUN can
// still take the handler's error path: pruneOneArtist can append a Failure
// (a GetPlatformIDs error, a connection-load error, or a detection error) for
// EVERY entry it processes, dry run or not, all of it above the `if
// scope.DryRun { continue }` inside the delete loop -- so a dry run whose
// per-artist detection fails, combined with a paging failure that aborts the
// whole sweep, reaches PrunePlatformBackdropDuplicates' error return with
// Failures populated. Before the fix, the handler's dry-run early return sat
// BELOW the error block, so this exact shape fell through to the 500 branch,
// invalidated the cached report (destroying it for a run that deleted
// NOTHING), and reported "partial": true next to "dry_run": true -- a
// self-contradictory body, since partial is documented as "the failed run
// may have changed the platform" and a dry run cannot have.
//
// The fixture forces a real page-2 List failure: page 1 returns exactly
// scanBackdropPageSize (200, mirrored here since the publish package's
// constant is unexported) artists so the paging loop does not stop after
// page 1, one of which is a real artist mapped to a connection ID that does
// not exist (so pruneOneArtist's connection-load lookup fails and appends a
// Failure); the other 199 are harmless placeholders with no platform mapping
// at all, so GetPlatformIDs finds nothing for them and neither dups nor
// failures nor plan entries: they exist only to pad page 1 to the page size
// that makes the loop continue to page 2.
func TestPlatformBackdropDuplicatesPrune_DryRunPlusErrorKeepsTheCacheAndReportsHonestly(t *testing.T) {
	t.Parallel()

	lister := &pagedFailAfterFirstLister{}
	r := testRouterWithPlatformLister(t, lister)

	ctx := context.Background()
	broken := &artist.Artist{Name: "Broken Mapping Artist"}
	if err := r.artistService.Create(ctx, broken); err != nil {
		t.Fatalf("creating broken-mapping artist: %v", err)
	}
	if err := r.artistService.SetPlatformID(ctx, broken.ID, "missing-conn", "p1"); err != nil {
		t.Fatalf("mapping broken artist to a nonexistent connection: %v", err)
	}

	const scanBackdropPageSize = 200 // mirrors publish.scanBackdropPageSize, unexported
	page1 := make([]artist.Artist, 0, scanBackdropPageSize)
	page1 = append(page1, *broken)
	for i := 1; i < scanBackdropPageSize; i++ {
		page1 = append(page1, artist.Artist{ID: fmt.Sprintf("dummy-%d", i), Name: "Filler"})
	}
	lister.page1 = page1

	// PRECONDITION: a real, populated report is cached before the run, so
	// "still populated afterwards" proves something.
	r.storePlatformDupReport(publish.PlatformBackdropDupReport{
		ArtistsAffected: 1, RedundantBackdrops: 3,
	}, time.Now())
	if _, _, ok := r.platformDupReportSnapshot(); !ok {
		t.Fatal("precondition: the cached report must be populated before the dry run")
	}

	req := httptest.NewRequestWithContext(adminContext(), http.MethodPost,
		"/api/v1/reports/platform-backdrop-duplicates/prune",
		strings.NewReader(`{"all_artists": true, "dry_run": true}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.handlePlatformBackdropDuplicatesPrune(w, req)

	// PRECONDITION: the run really did take the error path -- otherwise every
	// assertion below is unfalsifiable.
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (the paging failure must reach the handler's error path); body: %s", w.Code, w.Body.String())
	}

	// THE FIX, half 1: a dry run that failed changed NOTHING on any platform,
	// so the cached report must survive exactly like a dry run that succeeded.
	if _, _, ok := r.platformDupReportSnapshot(); !ok {
		t.Error("a FAILED DRY RUN invalidated the cached report; it deleted nothing, so this costs the operator a full re-sweep for no reason")
	}

	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding body: %v (body: %s)", err, w.Body.String())
	}
	// PRECONDITION for the "honest partial" assertion: the run really did
	// record a failure, or "partial: false" would be true only because
	// nothing was attempted at all.
	if failures, _ := got["failures"].(float64); failures != 1 {
		t.Fatalf("precondition: failures = %v, want 1 (the broken-mapping artist's connection load must have failed)", got["failures"])
	}
	if dry, _ := got["dry_run"].(bool); !dry {
		t.Error("dry_run not echoed as true on the error body; a rehearsal's failure must still say it was a rehearsal")
	}
	// THE FIX, half 2: partial must be false. A dry run never reaches a
	// delete, so "the failed run may have changed the platform" is false
	// regardless of how many failures it recorded while planning.
	if partial, _ := got["partial"].(bool); partial {
		t.Error(`partial = true on a failed DRY RUN; a rehearsal cannot have changed the platform, so this contradicts dry_run: true in the same body`)
	}
	if removed, _ := got["backdrops_removed"].(float64); removed != 0 {
		t.Errorf("backdrops_removed = %v, want 0: a dry run never deletes", got["backdrops_removed"])
	}
}
