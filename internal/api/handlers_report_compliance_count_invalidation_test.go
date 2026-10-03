package api

// Regression tests for #2395: the sidebar's non-compliant count was cached for
// five minutes and nothing invalidated it, so the badge kept showing the old
// number after a fix, a rule run, a merge, or a library removal.
//
// Shape of every test: prime the cache through the real badge handler, make
// the change through the real production write path, then read the badge
// through the same handler again. Because the first read leaves a fresh cache
// entry behind, the second read can only show the new number if the write
// dropped that entry. Each test also checks the database really changed, so a
// write that silently did nothing cannot pass as "the cache was refreshed".
//
// NOT parallel: complianceCount is module-level and the artists write
// generation is process-wide.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/library"
)

// complianceBadge reads the sidebar badge through the handler the UI polls.
func complianceBadge(t *testing.T, r *Router) string {
	t.Helper()
	rec := httptest.NewRecorder()
	r.handleComplianceCount(rec, withI18nCtx(t, adminComplianceCountReq()))
	if rec.Code != http.StatusOK {
		t.Fatalf("compliance count status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// requireBadgeCount asserts the badge shows exactly want. Zero renders as an
// empty body (the sidebar hides the item), anything else as a count pill.
func requireBadgeCount(t *testing.T, r *Router, want int, when string) {
	t.Helper()
	body := complianceBadge(t, r)
	if want == 0 {
		if body != "" {
			t.Fatalf("%s: badge = %q, want empty (0 non-compliant artists)", when, body)
		}
		return
	}
	if pill := fmt.Sprintf(`sw-sidebar-badge-pill">%d</span>`, want); !strings.Contains(body, pill) {
		t.Fatalf("%s: badge = %q, want a pill showing %d", when, body, want)
	}
}

// requireNonCompliantInDB asserts what the database itself says, bypassing the
// cache, so each test knows its write really landed.
func requireNonCompliantInDB(t *testing.T, r *Router, want int, when string) {
	t.Helper()
	got, err := r.artistService.Count(context.Background(), artist.CountParams{Filter: "non_compliant"})
	if err != nil {
		t.Fatalf("%s: counting non-compliant artists: %v", when, err)
	}
	if got != want {
		t.Fatalf("%s: database has %d non-compliant artists, want %d", when, got, want)
	}
}

// Rule evaluation: the pipeline re-scores an artist and persists health_score.
// With every rule disabled the score becomes 100, so the artist leaves the count.
func TestComplianceCount_RefreshesAfterRuleRun(t *testing.T) {
	complianceCount.invalidate()
	r, artistSvc := testRouterWithPipeline(t)
	a := addTestArtist(t, artistSvc, "Rule Run Artist") // created at health_score 0
	requireBadgeCount(t, r, 1, "before the rule run")

	if _, err := r.db.ExecContext(context.Background(), `UPDATE rules SET enabled = 0`); err != nil {
		t.Fatalf("disabling rules: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/run-rules", nil)
	req.SetPathValue("id", a.ID)
	rec := httptest.NewRecorder()
	r.handleRunArtistRules(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("run-rules status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	requireNonCompliantInDB(t, r, 0, "after the rule run")
	requireBadgeCount(t, r, 0, "after the rule run")
}

// Artist creation: the entry point the scanner and the platform import use.
func TestComplianceCount_RefreshesAfterArtistCreate(t *testing.T) {
	complianceCount.invalidate()
	r, artistSvc := testRouter(t)
	requireBadgeCount(t, r, 0, "empty library")

	addTestArtist(t, artistSvc, "Newly Scanned Artist")

	requireNonCompliantInDB(t, r, 1, "after the create")
	requireBadgeCount(t, r, 1, "after the create")
}

// Library removal: the library service deletes orphaned artists with its own
// SQL inside a transaction, never touching the artist service.
func TestComplianceCount_RefreshesAfterLibraryDelete(t *testing.T) {
	complianceCount.invalidate()
	r, libSvc, artistSvc := testRouterWithLibrary(t)
	ctx := context.Background()

	dir := t.TempDir()
	lib := &library.Library{Name: "Music", Path: dir, Type: "regular"}
	if err := libSvc.Create(ctx, lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}
	a := &artist.Artist{Name: "Orphan", Path: filepath.Join(dir, "orphan"), LibraryID: lib.ID}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	requireBadgeCount(t, r, 1, "before the library delete")

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/libraries/"+lib.ID, nil)
	req.SetPathValue("id", lib.ID)
	rec := httptest.NewRecorder()
	r.handleDeleteLibrary(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete library status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	requireNonCompliantInDB(t, r, 0, "after the library delete")
	requireBadgeCount(t, r, 0, "after the library delete")
}

// Merge: the loser's row is deleted inside the merge transaction.
func TestComplianceCount_RefreshesAfterMerge(t *testing.T) {
	complianceCount.invalidate()
	r, svc, db := mergeTestRouter(t)
	survivorID, loserID, _ := seedMergeFixture(t, svc, db)
	requireBadgeCount(t, r, 2, "before the merge")

	rec := httptest.NewRecorder()
	r.handleArtistsMerge(rec, withI18nCtx(t, adminReq(t, map[string]any{
		"survivor_id": survivorID,
		"loser_ids":   []string{loserID},
		"dry_run":     false,
	})))
	if rec.Code != http.StatusOK {
		t.Fatalf("merge status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	requireNonCompliantInDB(t, r, 1, "after the merge")
	requireBadgeCount(t, r, 1, "after the merge")
}

// The stale-repopulate race: a refresh reads the old count, a write lands
// before the refresh stores it, and the refresh then caches the old count.
// The cache must not serve that count afterwards.
//
// Deterministic, no sleeps: the refresh runs the real count query, then stops
// on a channel while the test performs the real write, then is released to
// store what it read.
func TestComplianceCount_WriteDuringRefreshIsNotCachedAsFresh(t *testing.T) {
	complianceCount.invalidate()
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Raced Artist")
	ctx := context.Background()

	queried := make(chan struct{})
	release := make(chan struct{})
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := complianceCount.get(ctx, func(ctx context.Context) (int, error) {
			n, err := artistSvc.Count(ctx, artist.CountParams{Filter: "non_compliant"})
			close(queried) // the pre-write count is in hand
			<-release      // hold it until the write below has landed
			return n, err
		})
		done <- result{n, err}
	}()

	<-queried
	if err := artistSvc.UpdateHealthScore(ctx, a.ID, 100); err != nil {
		t.Fatalf("writing health score mid-refresh: %v", err)
	}
	close(release)
	got := <-done
	// Precondition: the refresh really did read the PRE-write count. Without
	// this the test would also pass if the write had simply happened first.
	if got.err != nil || got.n != 1 {
		t.Fatalf("in-flight refresh returned (%d, %v), want (1, nil): it must have read the pre-write count", got.n, got.err)
	}

	requireNonCompliantInDB(t, r, 0, "after the mid-refresh write")
	requireBadgeCount(t, r, 0, "first read after the raced refresh")
}
