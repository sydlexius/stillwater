package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"log/slog"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/dupimages"
	"github.com/sydlexius/stillwater/internal/rule"
)

// fanartCapablePipeline embeds stubPipeline (satisfying rule.PipelineRunner)
// and adds the two fanartDuplicateRepairer methods, so it satisfies both the
// interface r.pipeline is declared as and the capability interface the
// backdrop-duplicates handlers narrow to.
type fanartCapablePipeline struct {
	*stubPipeline
	scanFn      func(ctx context.Context) (rule.FanartDupReport, error)
	remediateFn func(ctx context.Context) (rule.FanartRepairResult, error)
}

func (f *fanartCapablePipeline) ScanFanartDuplicates(ctx context.Context) (rule.FanartDupReport, error) {
	if f.scanFn != nil {
		return f.scanFn(ctx)
	}
	return rule.FanartDupReport{}, nil
}

func (f *fanartCapablePipeline) RemediateFanartDuplicates(ctx context.Context) (rule.FanartRepairResult, error) {
	if f.remediateFn != nil {
		return f.remediateFn(ctx)
	}
	return rule.FanartRepairResult{}, nil
}

// testRouterWithFanartPipeline builds a full Router (static assets wired, so
// assetsFor/renderTempl work) backed by an arbitrary rule.PipelineRunner.
// Mirrors testRouterWithStubPipeline but accepts the interface directly so a
// fanartCapablePipeline (or any other PipelineRunner test double) can be
// wired in without widening that helper's signature.
func testRouterWithFanartPipeline(t *testing.T, pipeline rule.PipelineRunner) *Router {
	t.Helper()

	db := newTestDB(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	authSvc := auth.NewService(db)
	artistSvc := artist.NewService(db)
	ruleSvc := rule.NewService(db)
	if err := ruleSvc.SeedDefaults(context.Background()); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}

	return NewRouter(RouterDeps{
		SessionSecret: testSessionSecret,
		AuthService:   authSvc,
		ArtistService: artistSvc,
		RuleService:   ruleSvc,
		Pipeline:      pipeline,
		DB:            db,
		Logger:        logger,
		StaticFS:      os.DirFS("../../web/static"),
	})
}

// TestBackdropDuplicatesPage_UnauthRendersLoginPage mirrors
// TestHandleArtistDuplicatesPage_UnauthRendersLoginPage: an unauthenticated
// GET must render the login page (200), not a bare 401, because the route
// uses wrapOptionalAuth.
func TestBackdropDuplicatesPage_UnauthRendersLoginPage(t *testing.T) {
	t.Parallel()
	r := testRouterWithFanartPipeline(t, &fanartCapablePipeline{stubPipeline: &stubPipeline{}})

	req := withI18nCtx(t, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/reports/backdrop-duplicates", nil))
	w := httptest.NewRecorder()
	r.handleBackdropDuplicatesPage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unauthenticated request should get login page (200), got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "backdrop-duplicates-table") {
		t.Error("unauthenticated visitor must not see the backdrop-duplicates table")
	}
	if !strings.Contains(body, "/api/v1/auth/login") {
		t.Error("login page must have the login form action (/api/v1/auth/login)")
	}
}

// TestBackdropDuplicatesPage_NonAdminForbidden mirrors
// TestHandleArtistDuplicatesPage_NonAdminForbidden: the report page is
// admin-only via requireForeignAdmin, so an authenticated non-admin must get
// 403 rather than the rendered page.
func TestBackdropDuplicatesPage_NonAdminForbidden(t *testing.T) {
	t.Parallel()
	r := testRouterWithFanartPipeline(t, &fanartCapablePipeline{stubPipeline: &stubPipeline{}})

	ctx := middleware.WithTestUserID(context.Background(), "u1")
	ctx = middleware.WithTestRole(ctx, "operator")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/reports/backdrop-duplicates", nil)
	w := httptest.NewRecorder()
	r.handleBackdropDuplicatesPage(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("non-admin should get 403; got %d", w.Code)
	}
}

// TestBackdropDuplicatesPage_WarmCacheRendersWithoutScanning is the
// authenticated-path regression test for #2684's redesign: the handler no
// longer scans on render at all. A pipeline whose ScanFanartDuplicates would
// fail the test if invoked proves the GET path renders purely from the
// pre-populated cache (r.storeBackdropDupReport), the totals and per-artist
// table coming straight from that cached snapshot.
func TestBackdropDuplicatesPage_WarmCacheRendersWithoutScanning(t *testing.T) {
	t.Parallel()
	pipeline := &fanartCapablePipeline{
		stubPipeline: &stubPipeline{},
		scanFn: func(_ context.Context) (rule.FanartDupReport, error) {
			t.Fatal("a warm cache must not invoke ScanFanartDuplicates on the GET request path (#2684)")
			return rule.FanartDupReport{}, nil
		},
	}
	r := testRouterWithFanartPipeline(t, pipeline)
	r.storeBackdropDupReport(rule.FanartDupReport{
		ArtistsAffected:     2,
		ExactRedundantSlots: 3,
		PerArtist: []rule.ArtistFanartDup{
			{ArtistID: "artist-1", Name: "Test Artist One", ExactDrops: 2},
			{ArtistID: "artist-2", Name: "Test Artist Two", ExactDrops: 1},
		},
	}, time.Now())

	req := withI18nCtx(t, httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/reports/backdrop-duplicates", nil))
	w := httptest.NewRecorder()
	r.handleBackdropDuplicatesPage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("authenticated admin request should get 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "backdrop-duplicates-table") {
		t.Error("authenticated admin must see the backdrop-duplicates-table in the response")
	}
	if !strings.Contains(body, "Test Artist One") || !strings.Contains(body, "Test Artist Two") {
		t.Errorf("report table must list every affected artist; body: %s", body)
	}
	if !strings.Contains(body, `id="backdrop-duplicates-remediate-button"`) {
		t.Error("report must expose the Remediate Exact Duplicates action when ExactRedundantSlots > 0")
	}
	if !strings.Contains(body, "/api/v1/reports/backdrop-duplicates/remediate") {
		t.Error("remediate button must post to /api/v1/reports/backdrop-duplicates/remediate")
	}
	if strings.Contains(body, `id="backdrop-duplicates-partial-notice"`) {
		t.Error("a clean scan (ScanErrors=0) must not render the partial-scan notice")
	}
	if !strings.Contains(body, `id="backdrop-duplicates-as-of"`) {
		t.Error("a cached report must show when it was scanned, so an operator can tell the numbers are a snapshot, not live")
	}
}

// TestBackdropDuplicatesPage_PartialScanShowsNotice asserts that a
// nonzero ScanErrors always surfaces the partial-scan notice, so a partial
// report is never mistaken for a clean/complete one. Reads from the cache
// like every other GET path (#2684); ScanFanartDuplicates is never invoked.
func TestBackdropDuplicatesPage_PartialScanShowsNotice(t *testing.T) {
	t.Parallel()
	r := testRouterWithFanartPipeline(t, &fanartCapablePipeline{stubPipeline: &stubPipeline{}})
	r.storeBackdropDupReport(rule.FanartDupReport{ScanErrors: 4}, time.Now())

	req := withI18nCtx(t, httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/reports/backdrop-duplicates", nil))
	w := httptest.NewRecorder()
	r.handleBackdropDuplicatesPage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="backdrop-duplicates-partial-notice"`) {
		t.Error("ScanErrors > 0 must render the partial-scan notice")
	}
	if !strings.Contains(body, "4") {
		t.Errorf("partial-scan notice must surface the skipped-artist count; body: %s", body)
	}
}

// TestBackdropDuplicatesPage_ColdCacheTriggersBackgroundScanAndShowsPendingNotice
// pins the #2684 replacement for the busy/timeout guards: when no report has
// ever been cached, the GET handler must not block on a scan (the root cause
// of the original hang) but must still kick a REAL background scan -- the
// exact TriggerRefresh -> dupImageCache -> libraryDupCount ->
// ScanFanartDuplicates chain the sidebar already relies on (#2608), not a
// seam -- so the cache eventually warms without any further page load.
//
// NOT t.Parallel(): asserts against dupimages.Shared(), process-wide state
// (see handlers_duplicate_images_nav_test.go's file comment).
func TestBackdropDuplicatesPage_ColdCacheTriggersBackgroundScanAndShowsPendingNotice(t *testing.T) {
	dupimages.Shared().Reset()
	t.Cleanup(func() { dupimages.Shared().Reset() })

	scanned := make(chan struct{})
	pipeline := &fanartCapablePipeline{
		stubPipeline: &stubPipeline{},
		scanFn: func(_ context.Context) (rule.FanartDupReport, error) {
			defer close(scanned)
			return rule.FanartDupReport{
				ArtistsAffected:     1,
				ExactRedundantSlots: 1,
				PerArtist:           []rule.ArtistFanartDup{{ArtistID: "a1", Name: "A", ExactDrops: 1}},
			}, nil
		},
	}
	r := testRouterWithFanartPipeline(t, pipeline)

	req := withI18nCtx(t, httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/reports/backdrop-duplicates", nil))
	w := httptest.NewRecorder()
	r.handleBackdropDuplicatesPage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="backdrop-duplicates-unavailable-notice"`) {
		t.Errorf("a never-computed cache must render the pending notice; body: %s", body)
	}
	if strings.Contains(body, `id="backdrop-duplicates-table"`) {
		t.Error("a never-computed cache must not render the report table (there is nothing established to show)")
	}

	select {
	case <-scanned:
	case <-time.After(2 * time.Second):
		t.Fatal("a cold-cache GET must trigger a real background scan (TriggerRefresh -> libraryDupCount -> ScanFanartDuplicates), but ScanFanartDuplicates was never invoked")
	}

	// Poll for the CACHE WRITE, not just the scan. `scanned` closes when
	// ScanFanartDuplicates returns, but storeBackdropDupReport runs after that
	// inside libraryDupCount, so asserting on the snapshot immediately races the
	// write and flakes. Wait for the observable end state instead.
	var report rule.FanartDupReport
	var ok bool
	deadline := time.Now().Add(2 * time.Second)
	for {
		if report, _, ok = r.backdropDupReportSnapshot(); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the background scan must land in the cache via storeBackdropDupReport")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if report.ExactRedundantSlots != 1 {
		t.Errorf("cached report ExactRedundantSlots = %d, want 1 (from the real scan, not a stub)", report.ExactRedundantSlots)
	}
}

// TestBackdropDuplicatesPage_PerceptualNoticeShowsWhenPerceptualPresent
// asserts the #2716 notice renders (with the affected-population counts and
// the pre-computed percentage) whenever PerceptualRedundantSlots > 0, and
// that a perceptual-only artist's row is neither dropped nor rendered as if
// it were clean -- the "false-clean row" defect PR 1 introduced by widening
// the affected-gate without threading PerceptualDrops through the view.
func TestBackdropDuplicatesPage_PerceptualNoticeShowsWhenPerceptualPresent(t *testing.T) {
	t.Parallel()
	r := testRouterWithFanartPipeline(t, &fanartCapablePipeline{stubPipeline: &stubPipeline{}})
	r.storeBackdropDupReport(rule.FanartDupReport{
		ArtistsAffected:          1,
		ExactRedundantSlots:      0,
		PerceptualRedundantSlots: 4,
		TotalFanartSlots:         31,
		PerArtist: []rule.ArtistFanartDup{
			{ArtistID: "a1", Name: "Near Match Artist", ExactDrops: 0, PerceptualDrops: 4, TotalSlots: 31},
		},
	}, time.Now())

	req := withI18nCtx(t, httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/reports/backdrop-duplicates", nil))
	w := httptest.NewRecorder()
	r.handleBackdropDuplicatesPage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="backdrop-duplicates-perceptual-notice"`) {
		t.Errorf("PerceptualRedundantSlots > 0 must render the perceptual-duplicates notice; body: %s", body)
	}
	if !strings.Contains(body, `id="backdrop-duplicates-perceptual-slots"`) {
		t.Error("page must expose the perceptual-redundant-slots stat tile")
	}
	// The row for the perceptual-only artist must carry its PerceptualDrops
	// count (4) and TotalSlots (31), not read as clean because ExactDrops is 0.
	if !strings.Contains(body, "Near Match Artist") {
		t.Fatalf("row for the perceptual-only artist is missing entirely; body: %s", body)
	}
	// ExactRedundantSlots is 0, so the exact-only remediate button correctly
	// does not appear -- but the perceptual-only row must still be visibly
	// non-clean via the badge below, not silently absent from the page.
	if !strings.Contains(body, t2716PerceptualOnlyBadgeSubstring) {
		t.Errorf("perceptual-only row must carry a visible non-clean marker (the perceptual-only badge); body: %s", body)
	}
}

// TestBackdropDuplicatesPage_NoPerceptualNoticeWhenZero is the negative half
// of the notice test above: a report with PerceptualRedundantSlots == 0 must
// NOT render the notice. Asserting only the present-branch (as the sibling
// test above does) is vacuous against a mutant that always renders the
// notice regardless of the count -- this is the test that catches it.
func TestBackdropDuplicatesPage_NoPerceptualNoticeWhenZero(t *testing.T) {
	t.Parallel()
	r := testRouterWithFanartPipeline(t, &fanartCapablePipeline{stubPipeline: &stubPipeline{}})
	r.storeBackdropDupReport(rule.FanartDupReport{
		ArtistsAffected:     1,
		ExactRedundantSlots: 3,
		PerArtist: []rule.ArtistFanartDup{
			{ArtistID: "a1", Name: "Exact Only Artist", ExactDrops: 3, TotalSlots: 3},
		},
	}, time.Now())

	req := withI18nCtx(t, httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/reports/backdrop-duplicates", nil))
	w := httptest.NewRecorder()
	r.handleBackdropDuplicatesPage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, `id="backdrop-duplicates-perceptual-notice"`) {
		t.Error("PerceptualRedundantSlots == 0 must NOT render the perceptual-duplicates notice")
	}
	if strings.Contains(body, t2716PerceptualOnlyBadgeSubstring) {
		t.Error("an exact-only artist row must not carry the perceptual-only badge")
	}
}

// t2716PerceptualOnlyBadgeSubstring targets the badge ELEMENT, not its
// rendered text. An earlier version asserted the text "Perceptual Only" and
// was VACUOUS: the fixture artist was named "Perceptual Only Artist", so the
// substring matched that row's own name whether or not the badge rendered.
// Deleting the badge branch entirely still passed. A marker attribute cannot
// collide with page copy, and the fixture artist is now named so that it
// could not collide anyway.
const t2716PerceptualOnlyBadgeSubstring = "data-sw-perceptual-only-badge"

// TestBackdropDuplicatesRemediate_Error pins the remediate endpoint's error path:
// if remediation fails, it returns 500 rather than a misleading success summary,
// and releases the singleton so the next request is not blocked.
func TestBackdropDuplicatesRemediate_Error(t *testing.T) {
	t.Parallel()
	pipeline := &fanartCapablePipeline{
		stubPipeline: &stubPipeline{},
		remediateFn: func(_ context.Context) (rule.FanartRepairResult, error) {
			return rule.FanartRepairResult{}, errors.New("remediate failed")
		},
	}
	r := testRouterWithFanartPipeline(t, pipeline)

	req := httptest.NewRequestWithContext(adminContext(), http.MethodPost, "/api/v1/reports/backdrop-duplicates/remediate", nil)
	w := httptest.NewRecorder()
	r.handleBackdropDuplicatesRemediate(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 when remediation errors", w.Code)
	}
	// The singleton must be released on the error path so a later repair is not
	// permanently blocked.
	r.bulkActionMu.Lock()
	running := r.backdropRepairRunning
	r.bulkActionMu.Unlock()
	if running {
		t.Error("backdropRepairRunning must be released after a failed remediation")
	}
}

// TestBuildBackdropDuplicatesView pins the report-to-view-model conversion:
// every PerArtist entry becomes a row and the totals -- including the #2716
// perceptual fields -- pass through unchanged, and the percentage is computed
// correctly for a known input.
func TestBuildBackdropDuplicatesView(t *testing.T) {
	report := rule.FanartDupReport{
		ArtistsAffected:          1,
		ExactRedundantSlots:      2,
		PerceptualRedundantSlots: 5,
		TotalFanartSlots:         20,
		ScanErrors:               1,
		PerArtist: []rule.ArtistFanartDup{
			{ArtistID: "a1", Name: "Artist One", ExactDrops: 2, PerceptualDrops: 5, TotalSlots: 20},
		},
	}
	view := buildBackdropDuplicatesView(report)

	if view.ArtistsAffected != 1 || view.ExactRedundantSlots != 2 || view.ScanErrors != 1 {
		t.Fatalf("totals did not pass through: %+v", view)
	}
	if view.PerceptualRedundantSlots != 5 {
		t.Errorf("PerceptualRedundantSlots = %d, want 5", view.PerceptualRedundantSlots)
	}
	if view.TotalFanartSlots != 20 {
		t.Errorf("TotalFanartSlots = %d, want 20", view.TotalFanartSlots)
	}
	// 5 of 20 is 25%.
	if view.PerceptualRedundantPercent != 25 {
		t.Errorf("PerceptualRedundantPercent = %d, want 25 (5/20)", view.PerceptualRedundantPercent)
	}
	if len(view.Rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(view.Rows))
	}
	row := view.Rows[0]
	if row.ArtistID != "a1" || row.Name != "Artist One" || row.ExactDrops != 2 {
		t.Errorf("row mismatch: %+v", row)
	}
	if row.PerceptualDrops != 5 {
		t.Errorf("row.PerceptualDrops = %d, want 5", row.PerceptualDrops)
	}
	if row.TotalSlots != 20 {
		t.Errorf("row.TotalSlots = %d, want 20", row.TotalSlots)
	}
}

// TestBuildBackdropDuplicatesView_ZeroTotalFanartSlots asserts the percentage
// computation never divides by zero: a report with no affected artists (or,
// defensively, a nonzero PerceptualRedundantSlots against a zero denominator)
// must render 0%, not panic or produce NaN/Inf-adjacent garbage.
func TestBuildBackdropDuplicatesView_ZeroTotalFanartSlots(t *testing.T) {
	report := rule.FanartDupReport{
		TotalFanartSlots:         0,
		PerceptualRedundantSlots: 0,
	}
	view := buildBackdropDuplicatesView(report)

	if view.PerceptualRedundantPercent != 0 {
		t.Errorf("PerceptualRedundantPercent = %d, want 0 when TotalFanartSlots is 0", view.PerceptualRedundantPercent)
	}
	if view.TotalFanartSlots != 0 || view.PerceptualRedundantSlots != 0 {
		t.Errorf("expected zero totals to pass through as zero: %+v", view)
	}
}

// TestBackdropDuplicatesRemediate_ConflictWhenRunning asserts a request
// returns 409 while the singleton slot is already claimed, mirroring the
// bulk-action 409 guard.
func TestBackdropDuplicatesRemediate_ConflictWhenRunning(t *testing.T) {
	t.Parallel()
	r := testRouterWithFanartPipeline(t, &fanartCapablePipeline{stubPipeline: &stubPipeline{}})

	r.bulkActionMu.Lock()
	r.backdropRepairRunning = true
	r.bulkActionMu.Unlock()

	req := httptest.NewRequestWithContext(adminContext(), http.MethodPost, "/api/v1/reports/backdrop-duplicates/remediate", nil)
	w := httptest.NewRecorder()
	r.handleBackdropDuplicatesRemediate(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
}

// TestBackdropDuplicatesRemediate_ConflictWhenBulkActionRunning asserts a
// remediate request gets 409 while a bulk action (fetch_images/run_rules) is
// running: both singletons share bulkActionMu (#2540 PR-2 Task 4 follow-up)
// because a bulk action can write/renumber the same artist's fanart rows a
// remediation run would touch, so the two must be mutually exclusive.
func TestBackdropDuplicatesRemediate_ConflictWhenBulkActionRunning(t *testing.T) {
	t.Parallel()
	r := testRouterWithFanartPipeline(t, &fanartCapablePipeline{stubPipeline: &stubPipeline{}})

	r.bulkActionMu.Lock()
	r.bulkActionProgress = &BulkActionProgress{Status: bulkActionRunning, Action: "run_rules", Total: 1}
	r.bulkActionMu.Unlock()

	req := httptest.NewRequestWithContext(adminContext(), http.MethodPost, "/api/v1/reports/backdrop-duplicates/remediate", nil)
	w := httptest.NewRecorder()
	r.handleBackdropDuplicatesRemediate(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body: %s", w.Code, w.Body.String())
	}
}

// TestBackdropDuplicatesRemediate_NonAdminForbidden mirrors
// TestBackdropDuplicatesPage_NonAdminForbidden: the remediate endpoint is
// admin-only via requireForeignAdmin, so an authenticated non-admin must get
// 403 rather than triggering the repair.
func TestBackdropDuplicatesRemediate_NonAdminForbidden(t *testing.T) {
	t.Parallel()
	r := testRouterWithFanartPipeline(t, &fanartCapablePipeline{stubPipeline: &stubPipeline{}})

	ctx := middleware.WithTestUserID(context.Background(), "u1")
	ctx = middleware.WithTestRole(ctx, "operator")
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/reports/backdrop-duplicates/remediate", nil)
	w := httptest.NewRecorder()
	r.handleBackdropDuplicatesRemediate(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("non-admin should get 403; got %d", w.Code)
	}
}

// TestBackdropDuplicatesRemediate_Success asserts the happy path: an admin
// POST against a pipeline that implements fanartDuplicateRepairer reaches
// RemediateFanartDuplicates and the JSON body reports its result.
//
// NOT t.Parallel() (#2908): the remediate handler's post-remediation rescan
// (handlers_backdrop_repair.go, handleBackdropDuplicatesRemediate) reaches
// dupimages.Shared() -- the SAME process-wide singleton
// TestBackdropDuplicatesPage_ColdCacheTriggersBackgroundScanAndShowsPendingNotice
// above documents itself against. Unlike that test, this one does not call
// Reset(); it depends on the shared cache's installed sources still pointing
// at THIS test's router (r.libraryDupCount/r.platformDupCounts) at the moment
// Refresh runs. dupImageCache's sync.Once only installs a router's sources
// ONCE, at construction, and installing them again is a process-wide
// replacement (dupimages.Cache.SetSources), not a per-router claim -- so any
// OTHER parallel test that constructs a router (every dupImageCache()/
// NewRouter() call does, router.go:480) re-points the singleton at ITS OWN
// sources and races this one's rescan. A prior version of this test ran
// t.Parallel() and, when a fanart-reorder test built such a router mid-scan,
// this test's rescan read the WRONG router's (non-fanart-capable) sources,
// which failed to establish a value at all -- so the post-remediation write
// never landed and the assertion below observed the stale pre-remediation
// count. Go's test runner fully drains every non-parallel top-level test
// before resuming any parallel one, so declaring this test serial is a
// deterministic fix, not a reordering of the same race: no other test's
// router construction can execute concurrently with this one's Refresh call.
func TestBackdropDuplicatesRemediate_Success(t *testing.T) {
	pipeline := &fanartCapablePipeline{
		stubPipeline: &stubPipeline{},
		remediateFn: func(_ context.Context) (rule.FanartRepairResult, error) {
			return rule.FanartRepairResult{
				ArtistsProcessed: 2,
				SlotsRemoved:     3,
				Failures: []rule.FanartRepairFailure{
					{ArtistID: "artist-1", Err: "boom"},
				},
			}, nil
		},
		scanFn: func(_ context.Context) (rule.FanartDupReport, error) {
			// The post-remediation rescan (#2684): the cache must end up
			// reflecting THIS report, not whatever (if anything) was cached
			// before the POST -- otherwise the page's HX-Refresh reload would
			// show the stale pre-remediation duplicates. Nonzero
			// PerceptualRedundantSlots here (#2716) so the response's
			// perceptual_duplicates_remain field has something distinct from
			// slots_removed to assert against.
			return rule.FanartDupReport{ExactRedundantSlots: 0, PerceptualRedundantSlots: 4, ArtistsAffected: 1}, nil
		},
	}
	r := testRouterWithFanartPipeline(t, pipeline)
	r.storeBackdropDupReport(rule.FanartDupReport{ExactRedundantSlots: 3, ArtistsAffected: 2}, time.Now())

	req := httptest.NewRequestWithContext(adminContext(), http.MethodPost, "/api/v1/reports/backdrop-duplicates/remediate", nil)
	w := httptest.NewRecorder()
	r.handleBackdropDuplicatesRemediate(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if hx := w.Header().Get("HX-Refresh"); hx != "true" {
		t.Errorf("HX-Refresh = %q, want true", hx)
	}
	body := w.Body.String()
	for _, want := range []string{`"artists_processed":2`, `"slots_removed":3`, `"failures":1`, `"perceptual_duplicates_remain":4`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; body: %s", want, body)
		}
	}

	report, _, ok := r.backdropDupReportSnapshot()
	if !ok {
		t.Fatal("remediation must leave a cached report behind")
	}
	if report.ExactRedundantSlots != 0 {
		t.Errorf("cached ExactRedundantSlots = %d, want 0 (the post-remediation rescan result, not the stale pre-remediation value of 3)", report.ExactRedundantSlots)
	}
}

// TestBackdropDuplicatesRemediate_RescanFailureLeavesPriorCache asserts that
// a failed post-remediation rescan (#2684) is best-effort: it must not wipe
// out or corrupt the last known-good cached report, and the remediation
// response itself must still report success (the collapse itself succeeded;
// only the opportunistic refresh afterward failed).
//
// NOT t.Parallel() (#2908): this handler's rescan reaches dupimages.Shared()
// through r.dupImageCache().Refresh(ctx) -- the SAME process-wide singleton
// TestBackdropDuplicatesRemediate_Success's file comment (above) documents
// itself against. Every NewRouter() call (testRouterWithFanartPipeline ->
// NewRouter, router.go:493) eagerly re-points that singleton's installed
// scan sources at ITS OWN router via dupimages.Cache.SetSources -- a
// process-wide replacement, not a per-router claim. A parallel sibling test
// that constructs a router (e.g. TestBackdropDuplicatesPage_
// WarmCacheRendersWithoutScanning, whose scanFn calls t.Fatal if ever
// invoked) can therefore re-point the shared sources mid-Refresh here and
// this test's rescan ends up invoking the WRONG router's scan function --
// measured by a hostile reviewer as a first-run race+shuffle panic
// (WarmCache's t.Fatal firing inside this test's Refresh call). Declaring
// this test serial, exactly like Success above it, is what makes that
// impossible rather than merely unlikely: Go's test runner fully drains
// every non-parallel top-level test before resuming any parallel one, so no
// other test's router construction can execute concurrently with this
// test's Refresh call.
func TestBackdropDuplicatesRemediate_RescanFailureLeavesPriorCache(t *testing.T) {
	pipeline := &fanartCapablePipeline{
		stubPipeline: &stubPipeline{},
		remediateFn: func(_ context.Context) (rule.FanartRepairResult, error) {
			return rule.FanartRepairResult{ArtistsProcessed: 1, SlotsRemoved: 1}, nil
		},
		scanFn: func(_ context.Context) (rule.FanartDupReport, error) {
			return rule.FanartDupReport{}, errors.New("rescan failed")
		},
	}
	r := testRouterWithFanartPipeline(t, pipeline)
	r.storeBackdropDupReport(rule.FanartDupReport{ExactRedundantSlots: 5, ArtistsAffected: 3}, time.Now())

	req := httptest.NewRequestWithContext(adminContext(), http.MethodPost, "/api/v1/reports/backdrop-duplicates/remediate", nil)
	w := httptest.NewRecorder()
	r.handleBackdropDuplicatesRemediate(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a failed opportunistic rescan must not fail the remediation response); body: %s", w.Code, w.Body.String())
	}

	report, _, ok := r.backdropDupReportSnapshot()
	if !ok {
		t.Fatal("a failed rescan must not clear the previously cached report")
	}
	if report.ExactRedundantSlots != 5 {
		t.Errorf("cached ExactRedundantSlots = %d, want unchanged 5 (a failed rescan must carry the prior value forward)", report.ExactRedundantSlots)
	}
}

// TestStoreBackdropDupReport_OlderScanCannotClobberNewer pins the ordering
// guard on the cached report.
//
// Scans overlap in practice and finish in whatever order the filesystem
// allows. Without ordering, a periodic scan that STARTED before an operator's
// remediation can land AFTER it and restore the pre-remediation duplicate
// counts, so the operator sees their own remediation appear to have done
// nothing. A reviewer reproduced exactly that against the unguarded version.
//
// Ordering is keyed on when each scan BEGAN, not when it completed, because
// that is what actually determines whose data is older.
func TestStoreBackdropDupReport_OlderScanCannotClobberNewer(t *testing.T) {
	t.Parallel()
	r := testRouterWithFanartPipeline(t, &fanartCapablePipeline{stubPipeline: &stubPipeline{}})

	older := time.Now()
	newer := older.Add(time.Second)

	// The newer scan (the post-remediation rescan) lands first: 0 duplicates.
	r.storeBackdropDupReport(rule.FanartDupReport{ExactRedundantSlots: 0}, newer)
	// The older scan, still walking the library when remediation ran, finishes
	// afterwards and reports the pre-remediation state.
	r.storeBackdropDupReport(rule.FanartDupReport{ExactRedundantSlots: 7}, older)

	got, _, ok := r.backdropDupReportSnapshot()
	if !ok {
		t.Fatal("expected a cached report")
	}
	if got.ExactRedundantSlots != 0 {
		t.Errorf("ExactRedundantSlots = %d, want 0: a scan that STARTED earlier "+
			"overwrote a newer result, so a completed remediation reads as having "+
			"done nothing", got.ExactRedundantSlots)
	}
}

// TestStoreBackdropDupReport_NewerScanWins is the other half: the guard must
// not freeze the cache. Without this, a guard that rejected everything would
// satisfy the test above while never updating again.
func TestStoreBackdropDupReport_NewerScanWins(t *testing.T) {
	t.Parallel()
	r := testRouterWithFanartPipeline(t, &fanartCapablePipeline{stubPipeline: &stubPipeline{}})

	first := time.Now()
	r.storeBackdropDupReport(rule.FanartDupReport{ExactRedundantSlots: 7}, first)
	r.storeBackdropDupReport(rule.FanartDupReport{ExactRedundantSlots: 2}, first.Add(time.Second))

	got, _, _ := r.backdropDupReportSnapshot()
	if got.ExactRedundantSlots != 2 {
		t.Errorf("ExactRedundantSlots = %d, want 2: a newer scan must replace an older cached report", got.ExactRedundantSlots)
	}
}

// TestBackdropDuplicatesRemediate_UnknownPerceptualCountIsNull pins the one
// case where the surviving-perceptual count is genuinely UNKNOWN rather than
// merely stale: no scan has ever landed, so the cache snapshot's ok bool is
// false and its zero value means "nothing measured", not "nothing remains".
//
// Reporting 0 there would be a clean bill of health nobody established -- the
// same false-clean claim #2716 exists to eliminate, relocated from the report
// page into the API. null says "not known" and cannot be misread as "clean".
//
// Serial for the same reason TestBackdropDuplicatesRemediate_Success is: the
// rescan path runs through the process-wide dupimages.Cache singleton, and a
// parallel test constructing its own router re-points that singleton mid-run.
func TestBackdropDuplicatesRemediate_UnknownPerceptualCountIsNull(t *testing.T) {
	pipeline := &fanartCapablePipeline{
		stubPipeline: &stubPipeline{},
		remediateFn: func(_ context.Context) (rule.FanartRepairResult, error) {
			return rule.FanartRepairResult{ArtistsProcessed: 1, SlotsRemoved: 2}, nil
		},
		// The rescan FAILS, so storeBackdropDupReport is never called and the
		// cache stays unpopulated -- the cold-start shape this test is about.
		scanFn: func(_ context.Context) (rule.FanartDupReport, error) {
			return rule.FanartDupReport{}, errors.New("scan unavailable")
		},
	}
	r := testRouterWithFanartPipeline(t, pipeline)

	// Precondition: assert the cache really is empty. Without this the test
	// would still pass if a snapshot HAD landed and simply happened to carry
	// zero perceptual slots, which is a different state entirely.
	if _, _, ok := r.backdropDupReportSnapshot(); ok {
		t.Fatal("precondition failed: the report cache must be unpopulated for this test to mean anything")
	}

	req := httptest.NewRequestWithContext(adminContext(), http.MethodPost, "/api/v1/reports/backdrop-duplicates/remediate", nil)
	w := httptest.NewRecorder()
	r.handleBackdropDuplicatesRemediate(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v (body: %s)", err, w.Body.String())
	}
	v, present := got["perceptual_duplicates_remain"]
	if !present {
		t.Fatal("perceptual_duplicates_remain must be PRESENT and null, not omitted: an absent field reads as an older API rather than an unknown count")
	}
	if v != nil {
		t.Errorf("perceptual_duplicates_remain = %v, want null when no scan has ever landed (0 would claim a clean state nothing measured)", v)
	}
	// The counts the run DID establish are still reported normally.
	if got["slots_removed"] != float64(2) {
		t.Errorf("slots_removed = %v, want 2", got["slots_removed"])
	}
}
