package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"log/slog"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/auth"
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

// TestBackdropDuplicatesPage_AuthenticatedRendersPage is the
// authenticated-path regression test: an admin request with a pipeline that
// implements fanartDuplicateRepairer must reach the real report, rendering
// the totals and the per-artist table from the scan result.
func TestBackdropDuplicatesPage_AuthenticatedRendersPage(t *testing.T) {
	t.Parallel()
	pipeline := &fanartCapablePipeline{
		stubPipeline: &stubPipeline{},
		scanFn: func(_ context.Context) (rule.FanartDupReport, error) {
			return rule.FanartDupReport{
				ArtistsAffected:     2,
				ExactRedundantSlots: 3,
				PerArtist: []rule.ArtistFanartDup{
					{ArtistID: "artist-1", Name: "Test Artist One", ExactDrops: 2},
					{ArtistID: "artist-2", Name: "Test Artist Two", ExactDrops: 1},
				},
			}, nil
		},
	}
	r := testRouterWithFanartPipeline(t, pipeline)

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
}

// TestBackdropDuplicatesPage_PartialScanShowsNotice asserts that a
// nonzero ScanErrors always surfaces the partial-scan notice, so a partial
// report is never mistaken for a clean/complete one.
func TestBackdropDuplicatesPage_PartialScanShowsNotice(t *testing.T) {
	t.Parallel()
	pipeline := &fanartCapablePipeline{
		stubPipeline: &stubPipeline{},
		scanFn: func(_ context.Context) (rule.FanartDupReport, error) {
			return rule.FanartDupReport{ScanErrors: 4}, nil
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
	if !strings.Contains(body, `id="backdrop-duplicates-partial-notice"`) {
		t.Error("ScanErrors > 0 must render the partial-scan notice")
	}
	if !strings.Contains(body, "4") {
		t.Errorf("partial-scan notice must surface the skipped-artist count; body: %s", body)
	}
}

// TestBackdropDuplicatesPage_PipelineMissingCapability_FailsLoud pins
// the fail-loud contract: if r.pipeline does not implement
// fanartDuplicateRepairer, the handler must return 500 with a logged error,
// never a silent empty report.
func TestBackdropDuplicatesPage_PipelineMissingCapability_FailsLoud(t *testing.T) {
	t.Parallel()
	r := testRouterWithFanartPipeline(t, &stubPipeline{})

	req := withI18nCtx(t, httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/reports/backdrop-duplicates", nil))
	w := httptest.NewRecorder()
	r.handleBackdropDuplicatesPage(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 when pipeline lacks fanartDuplicateRepairer", w.Code)
	}
}

// TestBuildBackdropDuplicatesView pins the report-to-view-model conversion:
// every PerArtist entry becomes a row and the totals pass through unchanged.
func TestBuildBackdropDuplicatesView(t *testing.T) {
	report := rule.FanartDupReport{
		ArtistsAffected:     1,
		ExactRedundantSlots: 2,
		ScanErrors:          1,
		PerArtist: []rule.ArtistFanartDup{
			{ArtistID: "a1", Name: "Artist One", ExactDrops: 2},
		},
	}
	view := buildBackdropDuplicatesView(report)

	if view.ArtistsAffected != 1 || view.ExactRedundantSlots != 2 || view.ScanErrors != 1 {
		t.Fatalf("totals did not pass through: %+v", view)
	}
	if len(view.Rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(view.Rows))
	}
	row := view.Rows[0]
	if row.ArtistID != "a1" || row.Name != "Artist One" || row.ExactDrops != 2 {
		t.Errorf("row mismatch: %+v", row)
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
func TestBackdropDuplicatesRemediate_Success(t *testing.T) {
	t.Parallel()
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
	}
	r := testRouterWithFanartPipeline(t, pipeline)

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
	for _, want := range []string{`"artists_processed":2`, `"slots_removed":3`, `"failures":1`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; body: %s", want, body)
		}
	}
}
