package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/rule"
)

// TestHandleNextReportsPage_RendersWorkshell verifies that GET /next/reports
// returns 200 on the "next" channel and renders the two-pane workspace shell
// (.sw-next-reports + .sw-rep-workspace) with the compliance report active.
func TestHandleNextReportsPage_RendersWorkshell(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextReportsPage))
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/reports", nil)
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "sw-next-reports") {
		t.Errorf("workspace scope (sw-next-reports) absent from response")
	}
	if !strings.Contains(body, "sw-rep-workspace") {
		t.Errorf("workspace grid (sw-rep-workspace) absent from response")
	}
	if !strings.Contains(body, "sw-rep-rail") {
		t.Errorf("reports rail (sw-rep-rail) absent from response")
	}
	if !strings.Contains(body, "/next/reports/compliance") {
		t.Errorf("compliance report rail link (/next/reports/compliance) absent from response")
	}
}

// TestHandleNextReportsPage_StableMode404 verifies that GET /next/reports
// returns 404 when the stable UX channel is active. The UX middleware blocks
// /next/* requests before the handler runs (decision 12).
func TestHandleNextReportsPage_StableMode404(t *testing.T) {
	t.Parallel()
	r := newTestRouterFull(t)

	h := middleware.UX("stable", "")(http.HandlerFunc(r.handleNextReportsPage))
	req := httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/next/reports", nil)
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("stable mode: status = %d, want 404", w.Code)
	}
}

// TestHandleNextReportsPage_UnauthRedirectsToLogin verifies that an
// unauthenticated request renders the login page (HTTP 200) rather than the
// workspace, matching the wrapOptionalAuth + requireAuth pattern used on all
// next/ browser pages.
func TestHandleNextReportsPage_UnauthRedirectsToLogin(t *testing.T) {
	t.Parallel()
	r := newTestRouterFull(t)

	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextReportsPage))
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/next/reports", nil)
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unauthenticated: status = %d, want 200 (login page)", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "sw-next-reports") {
		t.Error("unauthenticated visitor must not see the reports workspace")
	}
	if !strings.Contains(body, "/api/v1/auth/login") {
		t.Error("login page must include the login form action")
	}
}

// TestHandleNextReportPage_ComplianceReport verifies that GET
// /next/reports/compliance renders the compliance report workspace with the
// compliance overview active in the rail and the compliance table present.
func TestHandleNextReportPage_ComplianceReport(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextReportPage))
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/reports/compliance", nil)
	req.SetPathValue("name", "compliance")
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "sw-next-reports") {
		t.Errorf("workspace scope (sw-next-reports) absent")
	}
	if !strings.Contains(body, "compliance-results") {
		t.Errorf("compliance results (compliance-results) absent — compliance pane should render the compliance table")
	}
	if !strings.Contains(body, "sw-rep-tab-results") {
		t.Errorf("Results tab panel (sw-rep-tab-results) absent")
	}
	if !strings.Contains(body, "sw-rep-tab-matrix") {
		t.Errorf("Matrix tab panel (sw-rep-tab-matrix) absent")
	}
}

// TestHandleNextReportPage_UnknownReportShowsPlaceholder verifies that
// GET /next/reports/{name} for an unrecognized name renders the placeholder
// pane (sw-rep-placeholder) and does not attempt to load compliance data.
func TestHandleNextReportPage_UnknownReportShowsPlaceholder(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextReportPage))
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/reports/image-coverage", nil)
	req.SetPathValue("name", "image-coverage")
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "sw-rep-placeholder") {
		t.Errorf("placeholder pane (sw-rep-placeholder) absent for unimplemented report")
	}
	if strings.Contains(body, "compliance-results") {
		t.Errorf("compliance table must not appear for non-compliance reports")
	}
}

// TestHandleNextReportPage_RailContainsBuiltinReports verifies the reports
// rail lists all seven built-in reports and marks the active one with
// aria-current="page".
func TestHandleNextReportPage_RailContainsBuiltinReports(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextReportPage))
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/reports/compliance", nil)
	req.SetPathValue("name", "compliance")
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()

	wantLinks := []string{
		"/next/reports/compliance",
		"/next/reports/underrated-artists",
		"/next/reports/image-coverage",
		"/next/reports/connection-sync",
		"/next/reports/id-metadata-coverage",
		"/next/reports/state-records",
		"/next/reports/weekly-review-queue",
	}
	for _, link := range wantLinks {
		if !strings.Contains(body, link) {
			t.Errorf("rail link %q absent from response", link)
		}
	}

	// The active report must carry aria-current="page".
	if !strings.Contains(body, `aria-current="page"`) {
		t.Errorf("active rail item must have aria-current=page")
	}
}

// TestHandleNextReportsPage_OptOutHeader404 verifies the decision-12 per-request
// opt-out: when the UX channel in context is stable (simulating an
// X-Stillwater-UX: stable header) the handler returns 404 via checkNextChannel
// even when the lane itself is enabled. This exercises the !checkNextChannel
// early-return in handleNextReportsPage.
func TestHandleNextReportsPage_OptOutHeader404(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	ctx := middleware.WithTestUXChannel(context.Background(), middleware.UXStable)
	ctx = middleware.WithTestUserID(ctx, "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/reports", nil)
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	r.handleNextReportsPage(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("opt-out stable channel: status = %d, want 404 (decision 12)", w.Code)
	}
}

// TestHandleNextReportPage_OptOutHeader404 verifies the same decision-12 guard
// on the named-report handler (handleNextReportPage).
func TestHandleNextReportPage_OptOutHeader404(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	ctx := middleware.WithTestUXChannel(context.Background(), middleware.UXStable)
	ctx = middleware.WithTestUserID(ctx, "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/reports/compliance", nil)
	req.SetPathValue("name", "compliance")
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	r.handleNextReportPage(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("opt-out stable channel: status = %d, want 404 (decision 12)", w.Code)
	}
}

// TestHandleNextReportPage_EmptyNameDefaultsToCompliance verifies that when
// the {name} path value is absent or empty the handler defaults to the
// compliance report (decision 12 default). This exercises the name=="" branch.
func TestHandleNextReportPage_EmptyNameDefaultsToCompliance(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextReportPage))
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	// Do not call SetPathValue so PathValue("name") returns "".
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/reports/", nil)
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("empty name: status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "sw-next-reports") {
		t.Errorf("empty name must render the next/reports workspace shell")
	}
}

// TestHandleNextReportsPage_WithArtistsAndLibrary exercises the artist-row
// loop and library-service branch in loadNextComplianceData. Adding a real
// artist ensures the pageIDs slice is non-empty, driving the ComplianceRow
// construction loop, the violations nil-check, and the totalPages increment.
// Wiring a library service (via testRouterWithLibrary) drives the
// r.libraryService != nil branch that appends available libraries to the page.
func TestHandleNextReportsPage_WithArtistsAndLibrary(t *testing.T) {
	t.Parallel()
	r, _, artistSvc := testRouterWithLibrary(t)

	addTestArtist(t, artistSvc, "LibraryReport Artist")

	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextReportsPage))
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/reports", nil)
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("with artists + library: status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "compliance-results") {
		t.Errorf("compliance table (compliance-results) absent when artists are present")
	}
}

// TestHandleNextReportsPage_ArtistListError verifies that when artistService.List
// fails (closed database) the handler returns 500 and does not panic. The
// serveNextReportsWorkspace !ok short-circuit is also exercised here since
// loadNextComplianceData propagates the error up.
func TestHandleNextReportsPage_ArtistListError(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	if err := r.db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}

	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextReportsPage))
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/reports", nil)
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("closed db (artist list): status = %d, want 500", w.Code)
	}
}

// TestHandleNextReportsPage_ViolationsError verifies the 500 path when
// GetViolationsForArtists fails. An artist is seeded so pageIDs is non-empty
// and the rule service actually queries its database; the rule DB is then
// replaced with a closed one to force the error.
func TestHandleNextReportsPage_ViolationsError(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)

	addTestArtist(t, artistSvc, "ViolationsError Artist")

	ruleDB := newTestDB(t)
	r.ruleService = rule.NewService(ruleDB)
	if err := ruleDB.Close(); err != nil {
		t.Fatalf("closing rule db: %v", err)
	}

	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextReportsPage))
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/reports", nil)
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("closed rule db (violations): status = %d, want 500", w.Code)
	}
}

// TestHandleNextReportsPage_InvalidSortReturns400 verifies that an invalid sort
// parameter causes complianceListParams to write 400 and the handler to
// short-circuit, exercising the !ok branch in loadNextComplianceData.
func TestHandleNextReportsPage_InvalidSortReturns400(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextReportsPage))
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/reports?sort=not_a_valid_field", nil)
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid sort: status = %d, want 400", w.Code)
	}
}

// TestHandleNextReportPage_HealthReport verifies that GET /next/reports/health
// renders the Library health pane (embedding HealthSummaryFragment) rather than
// the coming-soon placeholder.
func TestHandleNextReportPage_HealthReport(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextReportPage))
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/reports/health", nil)
	req.SetPathValue("name", "health")
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "sw-rep-simple-pane") {
		t.Errorf("health pane (sw-rep-simple-pane) absent")
	}
	if !strings.Contains(body, "Compliance Score") {
		t.Errorf("health pane must embed the HealthSummaryFragment (Compliance Score card)")
	}
	if strings.Contains(body, "sw-rep-placeholder") {
		t.Errorf("health report must not render the placeholder pane")
	}
	if strings.Contains(body, "compliance-results") {
		t.Errorf("compliance table must not appear on the health report")
	}
}

// TestHandleNextReportPage_MetadataCompletenessReport verifies that GET
// /next/reports/metadata-completeness renders the Metadata completeness pane
// (embedding MetadataCompletenessFragment) rather than the placeholder. With an
// empty library the fragment shows the "No artists found." empty state.
func TestHandleNextReportPage_MetadataCompletenessReport(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextReportPage))
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/reports/metadata-completeness", nil)
	req.SetPathValue("name", "metadata-completeness")
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "sw-rep-simple-pane") {
		t.Errorf("metadata pane (sw-rep-simple-pane) absent")
	}
	if !strings.Contains(body, "No artists found.") {
		t.Errorf("metadata pane must embed MetadataCompletenessFragment (empty-state text)")
	}
	if strings.Contains(body, "sw-rep-placeholder") {
		t.Errorf("metadata report must not render the placeholder pane")
	}
}

// TestHandleNextReportPage_RulePassRatesReport verifies that GET
// /next/reports/rule-pass-rates renders the Rule pass-rates pane rather than the
// placeholder. With no rule evaluations the inline list shows its empty state.
func TestHandleNextReportPage_RulePassRatesReport(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextReportPage))
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/reports/rule-pass-rates", nil)
	req.SetPathValue("name", "rule-pass-rates")
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "sw-rep-simple-pane") {
		t.Errorf("rule-pass-rates pane (sw-rep-simple-pane) absent")
	}
	if !strings.Contains(body, "sw-rep-rate-empty") {
		t.Errorf("rule-pass-rates pane must render its empty state with no evaluations")
	}
	if strings.Contains(body, "sw-rep-placeholder") {
		t.Errorf("rule-pass-rates report must not render the placeholder pane")
	}
}

// TestHandleNextReportPage_RailListsHandlerBackedReports verifies the reports
// rail now links the three handler-backed built-ins (health,
// metadata-completeness, rule-pass-rates) in addition to compliance, so they
// are reachable from the workspace navigation.
func TestHandleNextReportPage_RailListsHandlerBackedReports(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextReportPage))
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/reports/compliance", nil)
	req.SetPathValue("name", "compliance")
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	for _, link := range []string{
		"/next/reports/health",
		"/next/reports/metadata-completeness",
		"/next/reports/rule-pass-rates",
	} {
		if !strings.Contains(body, link) {
			t.Errorf("rail link %q absent from response", link)
		}
	}
}

// TestHandleNextReportPage_HealthReportError_GetHealthStats verifies that when
// GetHealthStats fails (closed DB) the health pane returns 500 and does not
// render the workspace shell. This exercises the !ok early-return in
// serveNextReportsWorkspace and the first error branch of loadNextHealthData.
func TestHandleNextReportPage_HealthReportError_GetHealthStats(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	if err := r.db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}

	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextReportPage))
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/reports/health", nil)
	req.SetPathValue("name", "health")
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("closed db (health GetHealthStats): status = %d, want 500", w.Code)
	}
}

// TestHandleNextReportPage_HealthReportError_TopFailingRules verifies that when
// TopFailingRuleResults fails the health pane returns 500. The artist DB is
// left open so GetHealthStats succeeds; a fresh closed rule DB forces the
// TopFailingRuleResults error path inside loadNextHealthData.
func TestHandleNextReportPage_HealthReportError_TopFailingRules(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	ruleDB := newTestDB(t)
	r.ruleService = rule.NewService(ruleDB)
	if err := ruleDB.Close(); err != nil {
		t.Fatalf("closing rule db: %v", err)
	}

	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextReportPage))
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/reports/health", nil)
	req.SetPathValue("name", "health")
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("closed rule db (health TopFailingRules): status = %d, want 500", w.Code)
	}
}

// firstEnabledRuleID returns the ID of the first enabled rule in the service,
// or skips the test if none are present.
func firstEnabledRuleID(t *testing.T, svc *rule.Service) string {
	t.Helper()
	rules, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("listing rules: %v", err)
	}
	for _, ru := range rules {
		if ru.Enabled {
			return ru.ID
		}
	}
	t.Skip("no enabled rules seeded — skipping pass-rate test")
	return ""
}

// TestHandleNextReportPage_HealthReportWithPassRates verifies that when real
// rule results are present GetRulePassRates returns data and the health pane
// renders correctly (covering the rates loop and toTemplateRulePassRateData).
func TestHandleNextReportPage_HealthReportWithPassRates(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)

	a := addTestArtist(t, artistSvc, "PassRateArtist")
	ruleID := firstEnabledRuleID(t, r.ruleService)
	if err := r.ruleService.UpsertRuleResultPass(
		context.Background(), a.ID, ruleID, time.Now(),
	); err != nil {
		t.Fatalf("seeding rule pass result: %v", err)
	}

	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextReportPage))
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/reports/health", nil)
	req.SetPathValue("name", "health")
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("with pass rates (health): status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "sw-rep-simple-pane") {
		t.Errorf("health pane (sw-rep-simple-pane) absent with real pass-rate data")
	}
}

// TestHandleNextReportPage_RulePassRatesWithData verifies that when rule results
// exist the rule-pass-rates pane renders the populated table (non-empty state)
// and toTemplateRulePassRateData is exercised for each result row.
func TestHandleNextReportPage_RulePassRatesWithData(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)

	a := addTestArtist(t, artistSvc, "RulePassRateArtist")
	ruleID := firstEnabledRuleID(t, r.ruleService)
	if err := r.ruleService.UpsertRuleResultPass(
		context.Background(), a.ID, ruleID, time.Now(),
	); err != nil {
		t.Fatalf("seeding rule pass result: %v", err)
	}

	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextReportPage))
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/reports/rule-pass-rates", nil)
	req.SetPathValue("name", "rule-pass-rates")
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("with pass rates (rule-pass-rates): status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "sw-rep-simple-pane") {
		t.Errorf("rule-pass-rates pane (sw-rep-simple-pane) absent with real data")
	}
	// With real results the empty-state element must NOT appear.
	if strings.Contains(body, "sw-rep-rate-empty") {
		t.Errorf("rule-pass-rates pane must not show empty state when results are present")
	}
}

// TestHandleNextReportPage_RulePassRatesReportError verifies that when
// GetRulePassRates fails the rule-pass-rates pane returns 500. A fresh closed
// rule DB forces the error path in loadNextRulePassRatesData.
func TestHandleNextReportPage_RulePassRatesReportError(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	ruleDB := newTestDB(t)
	r.ruleService = rule.NewService(ruleDB)
	if err := ruleDB.Close(); err != nil {
		t.Fatalf("closing rule db: %v", err)
	}

	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextReportPage))
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/reports/rule-pass-rates", nil)
	req.SetPathValue("name", "rule-pass-rates")
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("closed rule db (rule-pass-rates): status = %d, want 500", w.Code)
	}
}

// TestHandleNextReportPage_MetadataCompletenessReportError verifies that when
// GetMetadataCompleteness fails (closed DB) the metadata-completeness pane
// returns 500. This covers the error branch in loadNextMetadataData.
func TestHandleNextReportPage_MetadataCompletenessReportError(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	if err := r.db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}

	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextReportPage))
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/reports/metadata-completeness", nil)
	req.SetPathValue("name", "metadata-completeness")
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("closed db (metadata-completeness): status = %d, want 500", w.Code)
	}
}

// TestHandleNextReportPage_MetadataCompletenessWithLibrary verifies that when a
// library service is wired up loadNextMetadataData populates the libNames map
// (exercising the libs loop and the libNames[libs[i].ID] = libs[i].Name path).
func TestHandleNextReportPage_MetadataCompletenessWithLibrary(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithLibrary(t)

	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextReportPage))
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/reports/metadata-completeness", nil)
	req.SetPathValue("name", "metadata-completeness")
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("with library (metadata-completeness): status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "sw-rep-simple-pane") {
		t.Errorf("metadata pane (sw-rep-simple-pane) absent with library service wired")
	}
}
