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

// TestHandleReportsPage_RendersWorkshell verifies that GET /reports
// returns 200 and renders the two-pane workspace shell
// (.sw-next-reports + .sw-rep-workspace) with the compliance report active.
func TestHandleReportsPage_RendersWorkshell(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	h := http.HandlerFunc(r.handleReportsPage)
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/reports", nil)
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
	if !strings.Contains(body, "/reports/compliance") {
		t.Errorf("compliance report rail link (/reports/compliance) absent from response")
	}
}

// TestHandleReportsPage_ServesStableChannel verifies that the promoted
// workspace is channel-agnostic (#1757 PR-4): a request carrying the stable
// UX channel gets the workspace, not a 404. The pre-promotion checkNextChannel
// guard (and its StableMode404 / OptOutHeader404 cases) is gone with the
// dedicated /next/reports routes.
func TestHandleReportsPage_ServesStableChannel(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	h := middleware.UX("stable", "")(http.HandlerFunc(r.handleReportsPage))
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/reports", nil)
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("stable channel: status = %d, want 200 (workspace is canonical)", w.Code)
	}
	if !strings.Contains(w.Body.String(), "sw-rep-workspace") {
		t.Errorf("stable channel must get the promoted reports workspace")
	}
}

// TestHandleReportsPage_TabQuerySelectsReport verifies that ?tab={name}
// selects the active report — the form handleCompliancePage's full-page
// redirect emits (/reports?tab=compliance) — here exercised with the health
// report.
func TestHandleReportsPage_TabQuerySelectsReport(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	h := http.HandlerFunc(r.handleReportsPage)
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/reports?tab=health", nil)
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("?tab=health: status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "sw-rep-simple-pane") {
		t.Errorf("?tab=health must render the health pane (sw-rep-simple-pane)")
	}
	if strings.Contains(body, "compliance-results") {
		t.Errorf("?tab=health must not render the compliance pane")
	}
}

// TestHandleReportsPage_UnauthRedirectsToLogin verifies that an
// unauthenticated request renders the login page (HTTP 200) rather than the
// workspace, matching the wrapOptionalAuth + requireAuth pattern used on all
// browser page routes.
func TestHandleReportsPage_UnauthRedirectsToLogin(t *testing.T) {
	t.Parallel()
	r := newTestRouterFull(t)

	h := http.HandlerFunc(r.handleReportsPage)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/reports", nil)
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

// TestHandleReportPage_ComplianceReport verifies that GET
// /reports/compliance renders the compliance report workspace with the
// compliance overview active in the rail and the compliance table present.
func TestHandleReportPage_ComplianceReport(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	h := http.HandlerFunc(r.handleReportPage)
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/reports/compliance", nil)
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

// TestHandleReportPage_UnknownReportShowsPlaceholder verifies that
// GET /reports/{name} for an unrecognized name renders the placeholder
// pane (sw-rep-placeholder) and does not attempt to load compliance data.
func TestHandleReportPage_UnknownReportShowsPlaceholder(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	h := http.HandlerFunc(r.handleReportPage)
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/reports/image-coverage", nil)
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

// TestHandleReportPage_RailContainsBuiltinReports verifies the reports
// rail lists all seven built-in reports and marks the active one with
// aria-current="page".
func TestHandleReportPage_RailContainsBuiltinReports(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	h := http.HandlerFunc(r.handleReportPage)
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/reports/compliance", nil)
	req.SetPathValue("name", "compliance")
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()

	wantLinks := []string{
		"/reports/compliance",
		"/reports/underrated-artists",
		"/reports/image-coverage",
		"/reports/connection-sync",
		"/reports/id-metadata-coverage",
		"/reports/state-records",
		"/reports/weekly-review-queue",
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

// TestHandleReportPage_EmptyNameDefaultsToCompliance verifies that when
// the {name} path value is absent or empty the handler defaults to the
// compliance report (decision 12 default). This exercises the name=="" branch.
func TestHandleReportPage_EmptyNameDefaultsToCompliance(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	h := http.HandlerFunc(r.handleReportPage)
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	// Do not call SetPathValue so PathValue("name") returns "".
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/reports/", nil)
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("empty name: status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "sw-next-reports") {
		t.Errorf("empty name must render the reports workspace shell")
	}
}

// TestHandleReportsPage_WithArtistsAndLibrary exercises the artist-row
// loop and library-service branch in loadReportsComplianceData. Adding a real
// artist ensures the pageIDs slice is non-empty, driving the ComplianceRow
// construction loop, the violations nil-check, and the totalPages increment.
// Wiring a library service (via testRouterWithLibrary) drives the
// r.libraryService != nil branch that appends available libraries to the page.
func TestHandleReportsPage_WithArtistsAndLibrary(t *testing.T) {
	t.Parallel()
	r, _, artistSvc := testRouterWithLibrary(t)

	addTestArtist(t, artistSvc, "LibraryReport Artist")

	h := http.HandlerFunc(r.handleReportsPage)
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/reports", nil)
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

// TestHandleReportsPage_ArtistListError verifies that when artistService.List
// fails (closed database) the handler returns 500 and does not panic. The
// serveReportsWorkspace !ok short-circuit is also exercised here since
// loadReportsComplianceData propagates the error up.
func TestHandleReportsPage_ArtistListError(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	if err := r.db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}

	h := http.HandlerFunc(r.handleReportsPage)
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/reports", nil)
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("closed db (artist list): status = %d, want 500", w.Code)
	}
}

// TestHandleReportsPage_ViolationsError verifies the 500 path when
// GetViolationsForArtists fails. An artist is seeded so pageIDs is non-empty
// and the rule service actually queries its database; the rule DB is then
// replaced with a closed one to force the error.
func TestHandleReportsPage_ViolationsError(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)

	addTestArtist(t, artistSvc, "ViolationsError Artist")

	ruleDB := newTestDB(t)
	r.ruleService = rule.NewService(ruleDB)
	if err := ruleDB.Close(); err != nil {
		t.Fatalf("closing rule db: %v", err)
	}

	h := http.HandlerFunc(r.handleReportsPage)
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/reports", nil)
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("closed rule db (violations): status = %d, want 500", w.Code)
	}
}

// TestHandleReportsPage_InvalidSortReturns400 verifies that an invalid sort
// parameter causes complianceListParams to write 400 and the handler to
// short-circuit, exercising the !ok branch in loadReportsComplianceData.
func TestHandleReportsPage_InvalidSortReturns400(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	h := http.HandlerFunc(r.handleReportsPage)
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/reports?sort=not_a_valid_field", nil)
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid sort: status = %d, want 400", w.Code)
	}
}

// TestHandleReportPage_HealthReport verifies that GET /reports/health
// renders the Library health pane (embedding HealthSummaryFragment) rather than
// the coming-soon placeholder.
func TestHandleReportPage_HealthReport(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	h := http.HandlerFunc(r.handleReportPage)
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/reports/health", nil)
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

// TestHandleReportPage_MetadataCompletenessReport verifies that GET
// /reports/metadata-completeness renders the Metadata completeness pane
// (embedding MetadataCompletenessFragment) rather than the placeholder. With an
// empty library the fragment shows the "No artists found." empty state.
func TestHandleReportPage_MetadataCompletenessReport(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	h := http.HandlerFunc(r.handleReportPage)
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/reports/metadata-completeness", nil)
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

// TestHandleReportPage_RulePassRatesReport verifies that GET
// /reports/rule-pass-rates renders the Rule pass-rates pane rather than the
// placeholder. With no rule evaluations the inline list shows its empty state.
func TestHandleReportPage_RulePassRatesReport(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	h := http.HandlerFunc(r.handleReportPage)
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/reports/rule-pass-rates", nil)
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

// TestHandleReportPage_RailListsHandlerBackedReports verifies the reports
// rail now links the three handler-backed built-ins (health,
// metadata-completeness, rule-pass-rates) in addition to compliance, so they
// are reachable from the workspace navigation.
func TestHandleReportPage_RailListsHandlerBackedReports(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	h := http.HandlerFunc(r.handleReportPage)
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/reports/compliance", nil)
	req.SetPathValue("name", "compliance")
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	for _, link := range []string{
		"/reports/health",
		"/reports/metadata-completeness",
		"/reports/rule-pass-rates",
	} {
		if !strings.Contains(body, link) {
			t.Errorf("rail link %q absent from response", link)
		}
	}
}

// TestHandleReportPage_HealthReportError_GetHealthStats verifies that when
// GetHealthStats fails (closed DB) the health pane returns 500 and does not
// render the workspace shell. This exercises the !ok early-return in
// serveReportsWorkspace and the first error branch of loadReportsHealthData.
func TestHandleReportPage_HealthReportError_GetHealthStats(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	if err := r.db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}

	h := http.HandlerFunc(r.handleReportPage)
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/reports/health", nil)
	req.SetPathValue("name", "health")
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("closed db (health GetHealthStats): status = %d, want 500", w.Code)
	}
}

// TestHandleReportPage_HealthReportError_TopFailingRules verifies that when
// TopFailingRuleResults fails the health pane returns 500. The artist DB is
// left open so GetHealthStats succeeds; a fresh closed rule DB forces the
// TopFailingRuleResults error path inside loadReportsHealthData.
func TestHandleReportPage_HealthReportError_TopFailingRules(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	ruleDB := newTestDB(t)
	r.ruleService = rule.NewService(ruleDB)
	if err := ruleDB.Close(); err != nil {
		t.Fatalf("closing rule db: %v", err)
	}

	h := http.HandlerFunc(r.handleReportPage)
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/reports/health", nil)
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

// TestHandleReportPage_HealthReportWithPassRates verifies that when real
// rule results are present GetRulePassRates returns data and the health pane
// renders correctly (covering the rates loop and toTemplateRulePassRateData).
func TestHandleReportPage_HealthReportWithPassRates(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)

	a := addTestArtist(t, artistSvc, "PassRateArtist")
	ruleID := firstEnabledRuleID(t, r.ruleService)
	if err := r.ruleService.UpsertRuleResultPass(
		context.Background(), a.ID, ruleID, time.Now(),
	); err != nil {
		t.Fatalf("seeding rule pass result: %v", err)
	}

	h := http.HandlerFunc(r.handleReportPage)
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/reports/health", nil)
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

// TestHandleReportPage_RulePassRatesWithData verifies that when rule results
// exist the rule-pass-rates pane renders the populated table (non-empty state)
// and toTemplateRulePassRateData is exercised for each result row.
func TestHandleReportPage_RulePassRatesWithData(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)

	a := addTestArtist(t, artistSvc, "RulePassRateArtist")
	ruleID := firstEnabledRuleID(t, r.ruleService)
	if err := r.ruleService.UpsertRuleResultPass(
		context.Background(), a.ID, ruleID, time.Now(),
	); err != nil {
		t.Fatalf("seeding rule pass result: %v", err)
	}

	h := http.HandlerFunc(r.handleReportPage)
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/reports/rule-pass-rates", nil)
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

// TestHandleReportPage_RulePassRatesReportError verifies that when
// GetRulePassRates fails the rule-pass-rates pane returns 500. A fresh closed
// rule DB forces the error path in loadReportsRulePassRatesData.
func TestHandleReportPage_RulePassRatesReportError(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	ruleDB := newTestDB(t)
	r.ruleService = rule.NewService(ruleDB)
	if err := ruleDB.Close(); err != nil {
		t.Fatalf("closing rule db: %v", err)
	}

	h := http.HandlerFunc(r.handleReportPage)
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/reports/rule-pass-rates", nil)
	req.SetPathValue("name", "rule-pass-rates")
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("closed rule db (rule-pass-rates): status = %d, want 500", w.Code)
	}
}

// TestHandleReportPage_MetadataCompletenessReportError verifies that when
// GetMetadataCompleteness fails (closed DB) the metadata-completeness pane
// returns 500. This covers the error branch in loadReportsMetadataData.
func TestHandleReportPage_MetadataCompletenessReportError(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	if err := r.db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}

	h := http.HandlerFunc(r.handleReportPage)
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/reports/metadata-completeness", nil)
	req.SetPathValue("name", "metadata-completeness")
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("closed db (metadata-completeness): status = %d, want 500", w.Code)
	}
}

// TestHandleReportPage_MetadataCompletenessWithLibrary verifies that when a
// library service is wired up loadReportsMetadataData populates the libNames map
// (exercising the libs loop and the libNames[libs[i].ID] = libs[i].Name path).
func TestHandleReportPage_MetadataCompletenessWithLibrary(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithLibrary(t)

	h := http.HandlerFunc(r.handleReportPage)
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/reports/metadata-completeness", nil)
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
