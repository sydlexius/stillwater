package api

import (
	"net/http"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/internal/rule"
	"github.com/sydlexius/stillwater/web/components"
	"github.com/sydlexius/stillwater/web/templates"
	"github.com/sydlexius/stillwater/web/templates/next"
)

// handleNextReportsPage serves GET /next/reports (M55 #1337). Defaults to the
// Compliance overview built-in report. wrapOptionalAuth is used on the route so
// unauthenticated visitors receive the login page rather than a 401 JSON error.
func (r *Router) handleNextReportsPage(w http.ResponseWriter, req *http.Request) {
	if !checkNextChannel(w, req) {
		return
	}
	r.serveNextReportsWorkspace(w, req, "compliance")
}

// handleNextReportPage serves GET /next/reports/{name} (M55 #1337). The {name}
// path value selects which built-in report is active in the workspace rail.
// "compliance", "health", "metadata-completeness", and "rule-pass-rates" have
// fully implemented right-panes (they have backing API handlers); other names
// render a placeholder. More specific routes (/next/reports/foreign-files) take
// precedence in the mux over this pattern so they are unaffected.
func (r *Router) handleNextReportPage(w http.ResponseWriter, req *http.Request) {
	if !checkNextChannel(w, req) {
		return
	}
	name := req.PathValue("name")
	if name == "" {
		name = "compliance"
	}
	r.serveNextReportsWorkspace(w, req, name)
}

// serveNextReportsWorkspace is the shared implementation for both next/reports
// routes. It gates on auth (unauthenticated → login page), loads report data
// for recognized report names, and renders the two-pane workspace shell.
func (r *Router) serveNextReportsWorkspace(w http.ResponseWriter, req *http.Request, reportName string) {
	if !r.requireAuth(w, req) {
		return
	}

	data := next.ReportsPageData{
		ActiveReport: reportName,
	}

	// Each recognized report name loads its own data and renders a dedicated
	// right-pane component. Unrecognized names fall through with no data and
	// render the "coming soon" placeholder. A loader that writes an error
	// response returns ok==false, in which case we must not also render.
	switch reportName {
	case "compliance":
		cd, ok := r.loadNextComplianceData(w, req)
		if !ok {
			return
		}
		data.ComplianceData = cd
	case "health":
		hd, ok := r.loadNextHealthData(w, req)
		if !ok {
			return
		}
		data.HealthData = hd
	case "metadata-completeness":
		md, ok := r.loadNextMetadataData(w, req)
		if !ok {
			return
		}
		data.MetadataReport = md
	case "rule-pass-rates":
		rd, ok := r.loadNextRulePassRatesData(w, req)
		if !ok {
			return
		}
		data.RulePassRates = rd
	}

	renderTempl(w, req, next.ReportsPageNext(r.assetsFor(req), data))
}

// loadNextComplianceData builds a ComplianceData value for the next/ reports
// workspace using the same logic as handleCompliancePage. The
// Pagination.BaseURL uses the stable /reports/compliance path so HTMX
// pagination links call the stable handler (which returns the ComplianceResults
// fragment on HTMX requests), matching the swap contract ComplianceTable expects.
func (r *Router) loadNextComplianceData(w http.ResponseWriter, req *http.Request) (templates.ComplianceData, bool) {
	ctx := req.Context()

	params, ok := r.complianceListParams(w, req)
	if !ok {
		return templates.ComplianceData{}, false
	}
	status := req.URL.Query().Get("status")

	artists, total, err := r.artistService.List(ctx, params)
	if err != nil {
		r.logger.Error("listing artists for next reports workspace", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return templates.ComplianceData{}, false
	}

	pageIDs := make([]string, len(artists))
	for i := range artists {
		pageIDs[i] = artists[i].ID
	}

	violations, err := r.ruleService.GetViolationsForArtists(ctx, pageIDs)
	if err != nil {
		r.logger.Error("loading violations for next reports workspace", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return templates.ComplianceData{}, false
	}

	resultCounts, rcErr := r.ruleService.GetRuleResultCounts(ctx, pageIDs)
	if rcErr != nil {
		r.logger.Warn("loading rule result counts for next reports workspace", "error", rcErr)
		resultCounts = map[string]rule.RuleResultCount{}
	}

	rows := make([]templates.ComplianceRow, len(artists))
	for i := range artists {
		a := &artists[i]
		vs := violations[a.ID]
		if vs == nil {
			vs = make([]rule.Violation, 0)
		}
		counts := resultCounts[a.ID]
		rows[i] = templates.ComplianceRow{
			Artist:              *a,
			HealthScore:         a.HealthScore,
			Violations:          vs,
			RulesPassedCount:    counts.Passed,
			RulesEvaluatedCount: counts.Evaluated,
		}
	}

	totalPages := total / params.PageSize
	if total%params.PageSize > 0 {
		totalPages++
	}

	var libs []library.Library
	if r.libraryService != nil {
		libs, err = r.libraryService.List(ctx)
		if err != nil {
			r.logger.Warn("listing libraries for next reports workspace", "error", err)
		}
	}

	return templates.ComplianceData{
		Rows:     rows,
		BasePath: r.basePath,
		Pagination: components.PaginationData{
			CurrentPage:    params.Page,
			TotalPages:     totalPages,
			PageSize:       params.PageSize,
			TotalItems:     total,
			BaseURL:        "/reports/compliance",
			Sort:           params.Sort,
			Order:          params.Order,
			Search:         params.Search,
			Filter:         params.Filter,
			LibraryID:      params.LibraryID,
			TargetID:       "compliance-results",
			Status:         status,
			HealthScoreMin: params.HealthScoreMin,
			HealthScoreMax: params.HealthScoreMax,
		},
		Search:         params.Search,
		Status:         status,
		Filter:         req.URL.Query().Get("filter"),
		Libraries:      libs,
		LibraryID:      params.LibraryID,
		Sort:           params.Sort,
		Order:          params.Order,
		HealthScoreMin: params.HealthScoreMin,
		HealthScoreMax: params.HealthScoreMax,
		ProfileName:    r.getActiveProfileName(ctx),
	}, true
}

// loadNextHealthData builds the HealthSummaryData for the next/ reports
// workspace "health" report. It mirrors handleReportHealth's read path
// (stored per-artist health scores + top failing rules + per-rule pass
// rates) but assembles the templates view model directly. The pass-rate
// widget is non-critical: a failure there warns and falls through with an
// empty slice rather than failing the whole pane.
func (r *Router) loadNextHealthData(w http.ResponseWriter, req *http.Request) (templates.HealthSummaryData, bool) {
	ctx := req.Context()

	stats, err := r.artistService.GetHealthStats(ctx, "")
	if err != nil {
		r.logger.Error("loading health stats for next reports workspace", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return templates.HealthSummaryData{}, false
	}

	topViolations, err := r.ruleService.TopFailingRuleResults(ctx, 10)
	if err != nil {
		r.logger.Error("loading top failing rules for next reports workspace", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return templates.HealthSummaryData{}, false
	}

	rates, err := r.ruleService.GetRulePassRates(ctx)
	if err != nil {
		r.logger.Warn("loading rule pass rates for next reports health pane", "error", err)
		rates = []rule.RulePassRate{}
	}

	data := templates.HealthSummaryData{
		Score:            stats.Score,
		TotalArtists:     stats.TotalArtists,
		CompliantArtists: stats.CompliantArtists,
		MissingNFO:       stats.MissingNFO,
		MissingThumb:     stats.MissingThumb,
		MissingFanart:    stats.MissingFanart,
		MissingMBID:      stats.MissingMBID,
		BasePath:         r.basePath,
		TopViolations:    make([]templates.ViolationSummaryData, 0, len(topViolations)),
		RulePassRates:    make([]templates.RulePassRateData, 0, len(rates)),
	}
	for _, v := range topViolations {
		data.TopViolations = append(data.TopViolations, templates.ViolationSummaryData{
			RuleID:   v.RuleID,
			RuleName: v.RuleName,
			Count:    v.Count,
			Severity: v.Severity,
		})
	}
	for _, p := range rates {
		data.RulePassRates = append(data.RulePassRates, toTemplateRulePassRateData(p))
	}

	return data, true
}

// loadNextMetadataData builds the MetadataCompletenessReport for the next/
// reports workspace "metadata-completeness" report, mirroring
// handleReportMetadataCompleteness's read path (per-library name map +
// completeness aggregation with the lowest-10 artists).
func (r *Router) loadNextMetadataData(w http.ResponseWriter, req *http.Request) (*artist.MetadataCompletenessReport, bool) {
	ctx := req.Context()

	libNames := make(map[string]string)
	if r.libraryService != nil {
		if libs, err := r.libraryService.List(ctx); err != nil {
			r.logger.Warn("listing libraries for next reports metadata completeness", "error", err)
		} else {
			for i := range libs {
				libNames[libs[i].ID] = libs[i].Name
			}
		}
	}

	report, err := r.artistService.GetMetadataCompleteness(ctx, "", 10, libNames)
	if err != nil {
		r.logger.Error("computing metadata completeness for next reports workspace", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return nil, false
	}

	return report, true
}

// loadNextRulePassRatesData builds the per-rule pass-rate rows for the next/
// reports workspace "rule-pass-rates" report. The stable endpoint
// (handleReportRulePassRates) is JSON-only, so this loader maps the rule
// service rows into the templ view model the inline pane renders.
func (r *Router) loadNextRulePassRatesData(w http.ResponseWriter, req *http.Request) ([]templates.RulePassRateData, bool) {
	rates, err := r.ruleService.GetRulePassRates(req.Context())
	if err != nil {
		r.logger.Error("querying rule pass rates for next reports workspace", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return nil, false
	}

	out := make([]templates.RulePassRateData, 0, len(rates))
	for _, p := range rates {
		out = append(out, toTemplateRulePassRateData(p))
	}
	return out, true
}

// toTemplateRulePassRateData maps a rule.RulePassRate to its templ view model.
func toTemplateRulePassRateData(p rule.RulePassRate) templates.RulePassRateData {
	return templates.RulePassRateData{
		RuleID:    p.RuleID,
		RuleName:  p.RuleName,
		Severity:  p.Severity,
		Passed:    p.Passed,
		Failed:    p.Failed,
		Evaluated: p.Evaluated,
		PassRate:  p.PassRate,
	}
}
