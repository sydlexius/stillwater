package api

import (
	"context"
	"encoding/csv"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/i18n"
	img "github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/internal/rule"
	"github.com/sydlexius/stillwater/web/components"
	"github.com/sydlexius/stillwater/web/templates"
)

// rulePassRateRow is the JSON shape returned in the /reports/health
// envelope so the dashboard can hydrate the per-rule widget without a
// second HTTP call. Mirrors rule.RulePassRate field-for-field; held
// locally to keep the rule package free of HTTP concerns.
type rulePassRateRow struct {
	RuleID    string  `json:"rule_id"`
	RuleName  string  `json:"rule_name"`
	Severity  string  `json:"severity"`
	Passed    int     `json:"passed"`
	Failed    int     `json:"failed"`
	Evaluated int     `json:"evaluated"`
	PassRate  float64 `json:"pass_rate"`
}

// healthSummary is the JSON response for the dashboard health endpoint.
type healthSummary struct {
	Score            float64            `json:"score"`
	TotalArtists     int                `json:"total_artists"`
	CompliantArtists int                `json:"compliant_artists"`
	MissingNFO       int                `json:"missing_nfo"`
	MissingThumb     int                `json:"missing_thumb"`
	MissingFanart    int                `json:"missing_fanart"`
	MissingMBID      int                `json:"missing_mbid"`
	TopViolations    []violationSummary `json:"top_violations"`
	RulePassRates    []rulePassRateRow  `json:"rule_pass_rates,omitempty"`
}

// violationSummary tracks how many artists fail a specific rule.
type violationSummary struct {
	RuleID   string `json:"rule_id"`
	RuleName string `json:"rule_name"`
	Count    int    `json:"count"`
	Severity string `json:"severity"`
}

// librarySummary is the per-library health breakdown in the by-library endpoint.
type librarySummary struct {
	LibraryID        string  `json:"library_id"`
	LibraryName      string  `json:"library_name"`
	TotalArtists     int     `json:"total_artists"`
	CompliantArtists int     `json:"compliant_artists"`
	Score            float64 `json:"score"`
	MissingNFO       int     `json:"missing_nfo"`
	MissingThumb     int     `json:"missing_thumb"`
	MissingFanart    int     `json:"missing_fanart"`
	MissingMBID      int     `json:"missing_mbid"`
}

// handleReportHealth returns the current library health summary.
// Reads stored per-artist health scores via SQL aggregation instead of
// running EvaluateAll on every request. Scores are kept fresh by the
// HealthSubscriber that processes ArtistUpdated events.
// GET /api/v1/reports/health
func (r *Router) handleReportHealth(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	stats, err := r.artistService.GetHealthStats(ctx, "")
	if err != nil {
		r.logger.Error("querying health stats", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to generate health report")
		return
	}

	topViolations, err := r.ruleService.TopFailingRuleResults(ctx, 10)
	if err != nil {
		r.logger.Error("querying top failing rule results", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to generate health report")
		return
	}

	// Pass-rate widget is non-critical: a failure here should still let the
	// rest of /reports/health render. Warn and fall through with an empty
	// slice instead of returning 500.
	rulePassRates, err := r.ruleService.GetRulePassRates(ctx)
	if err != nil {
		r.logger.Warn("querying rule pass rates for dashboard widget", "error", err)
		rulePassRates = []rule.RulePassRate{}
	}

	summary := healthSummary{
		Score:            stats.Score,
		TotalArtists:     stats.TotalArtists,
		CompliantArtists: stats.CompliantArtists,
		MissingNFO:       stats.MissingNFO,
		MissingThumb:     stats.MissingThumb,
		MissingFanart:    stats.MissingFanart,
		MissingMBID:      stats.MissingMBID,
		TopViolations:    make([]violationSummary, 0, len(topViolations)),
	}

	for _, v := range topViolations {
		summary.TopViolations = append(summary.TopViolations, violationSummary{
			RuleID:   v.RuleID,
			RuleName: v.RuleName,
			Count:    v.Count,
			Severity: v.Severity,
		})
	}

	summary.RulePassRates = make([]rulePassRateRow, 0, len(rulePassRates))
	for _, p := range rulePassRates {
		summary.RulePassRates = append(summary.RulePassRates, rulePassRateRow{
			RuleID:    p.RuleID,
			RuleName:  p.RuleName,
			Severity:  p.Severity,
			Passed:    p.Passed,
			Failed:    p.Failed,
			Evaluated: p.Evaluated,
			PassRate:  p.PassRate,
		})
	}

	// Record a health snapshot (throttled)
	if err := r.ruleService.RecordHealthSnapshot(ctx, summary.TotalArtists, summary.CompliantArtists, summary.Score); err != nil {
		r.logger.Warn("recording health snapshot", "error", err)
	}

	r.renderHealthResponse(w, req, summary)
}

// renderHealthResponse writes the health summary as either an HTMX HTML
// fragment or a JSON response, depending on the request headers.
func (r *Router) renderHealthResponse(w http.ResponseWriter, req *http.Request, summary healthSummary) {
	if isHTMXRequest(req) {
		data := templates.HealthSummaryData{
			Score:            summary.Score,
			TotalArtists:     summary.TotalArtists,
			CompliantArtists: summary.CompliantArtists,
			MissingNFO:       summary.MissingNFO,
			MissingThumb:     summary.MissingThumb,
			MissingFanart:    summary.MissingFanart,
			MissingMBID:      summary.MissingMBID,
			TopViolations:    toTemplateViolations(summary.TopViolations),
			RulePassRates:    toTemplateRulePassRates(summary.RulePassRates),
			BasePath:         r.basePath,
		}
		renderTempl(w, req, templates.HealthSummaryFragment(data))
		return
	}

	writeJSON(w, http.StatusOK, summary)
}

// InvalidateHealthCache is a no-op retained for API compatibility with
// callers added by PR #700. Health scores are now read from stored
// per-artist values (updated via the event bus), so there is no
// in-memory cache to invalidate.
func (r *Router) InvalidateHealthCache() {}

// handleReportHealthHistory returns health history data for charting.
// GET /api/v1/reports/health/history?from=2024-01-01&to=2024-06-01
func (r *Router) handleReportHealthHistory(w http.ResponseWriter, req *http.Request) {
	var from, to time.Time

	if v := req.URL.Query().Get("from"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			from = t
		} else if t, err := time.Parse(time.DateOnly, v); err == nil {
			from = t
		}
	}
	if v := req.URL.Query().Get("to"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			to = t
		} else if t, err := time.Parse(time.DateOnly, v); err == nil {
			// Date-only `to` is inclusive of the entire day. Without this
			// adjustment "to=2026-12-31" would parse to 2026-12-31T00:00:00Z
			// and the SQL BETWEEN clause would exclude any snapshot recorded
			// later that day, which is surprising for a day-range query.
			to = t.Add(24*time.Hour - time.Second)
		}
	}

	history, err := r.ruleService.GetHealthHistory(req.Context(), from, to)
	if err != nil {
		r.logger.Error("fetching health history", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to fetch health history")
		return
	}

	if history == nil {
		history = []rule.HealthSnapshot{}
	}

	writeJSON(w, http.StatusOK, map[string]any{"history": history})
}

// handleReportHealthByLibrary returns per-library health breakdown.
// Reads stored per-artist health scores via SQL aggregation per library.
// GET /api/v1/reports/health/by-library
func (r *Router) handleReportHealthByLibrary(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	libs, err := r.libraryService.List(ctx)
	if err != nil {
		r.logger.Error("listing libraries for health by-library", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to list libraries")
		return
	}

	summaries := make([]librarySummary, 0, len(libs))
	for i := range libs {
		lib := &libs[i]
		stats, err := r.artistService.GetHealthStats(ctx, lib.ID)
		if err != nil {
			r.logger.Error("querying health stats for library", "library", lib.Name, "error", err)
			continue
		}
		if stats.TotalArtists == 0 {
			continue
		}
		summaries = append(summaries, librarySummary{
			LibraryID:        lib.ID,
			LibraryName:      lib.Name,
			TotalArtists:     stats.TotalArtists,
			CompliantArtists: stats.CompliantArtists,
			Score:            stats.Score,
			MissingNFO:       stats.MissingNFO,
			MissingThumb:     stats.MissingThumb,
			MissingFanart:    stats.MissingFanart,
			MissingMBID:      stats.MissingMBID,
		})
	}

	// Overall across all libraries
	overallStats, err := r.artistService.GetHealthStats(ctx, "")
	if err != nil {
		r.logger.Error("querying overall health stats", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to compute overall health")
		return
	}

	overall := librarySummary{
		TotalArtists:     overallStats.TotalArtists,
		CompliantArtists: overallStats.CompliantArtists,
		Score:            overallStats.Score,
		MissingNFO:       overallStats.MissingNFO,
		MissingThumb:     overallStats.MissingThumb,
		MissingFanart:    overallStats.MissingFanart,
		MissingMBID:      overallStats.MissingMBID,
	}

	if isHTMXRequest(req) {
		renderTempl(w, req, templates.ComplianceSummaryFragment(templates.ComplianceSummaryData{
			Libraries:   toTemplateSummaries(summaries),
			Overall:     toTemplateSummary(overall),
			ProfileName: r.getActiveProfileName(req.Context()),
		}))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"libraries": summaries,
		"overall":   overall,
	})
}

// handleViolationTrend returns daily violation creation and resolution counts.
// GET /api/v1/violations/trend?days=30
func (r *Router) handleViolationTrend(w http.ResponseWriter, req *http.Request) {
	days := intQuery(req, "days", 30)
	if days <= 0 || days > 365 {
		days = 30
	}

	trend, err := r.ruleService.GetViolationTrend(req.Context(), days)
	if err != nil {
		r.logger.Error("fetching violation trend", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to fetch violation trend")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"trend": trend})
}

// handleReportCompliance returns a paginated compliance report.
// Reads stored health scores from the artists table and active violations
// (open and pending_choice) from rule_violations rather than calling
// EvaluateAll on every request.
// GET /api/v1/reports/compliance
func (r *Router) handleReportCompliance(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	params, ok := r.complianceListParams(w, req)
	if !ok {
		return
	}

	artists, total, err := r.artistService.List(ctx, params)
	if err != nil {
		r.logger.Error("listing artists for compliance report", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to list artists")
		return
	}

	// Collect artist IDs for the batch violation lookup.
	ids := make([]string, len(artists))
	for i := range artists {
		ids[i] = artists[i].ID
	}

	violations, err := r.ruleService.GetViolationsForArtists(ctx, ids)
	if err != nil {
		r.logger.Error("loading violations for compliance report", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to load violations")
		return
	}

	// Issue #699: batch-load per-artist pass/fail counts from rule_results.
	// A failure here is logged but non-fatal: the compliance grid still
	// renders with violation data, and the new counts fall back to zero so
	// the UI is not blocked when rule_results is empty (fresh install, or
	// an artist whose first Run Rules pass has not finished).
	resultCounts, rcErr := r.ruleService.GetRuleResultCounts(ctx, ids)
	if rcErr != nil {
		r.logger.Warn("loading rule_result counts for compliance report", "error", rcErr)
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

	writeJSON(w, http.StatusOK, map[string]any{
		"rows":      rows,
		"total":     total,
		"page":      params.Page,
		"page_size": params.PageSize,
	})
}

// handleReportComplianceExport streams a CSV export of the compliance report.
// GET /api/v1/reports/compliance/export
func (r *Router) handleReportComplianceExport(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	params, ok := r.complianceListParams(w, req)
	if !ok {
		return
	}
	params.Page = 1
	params.PageSize = 200

	var allArtists []artist.Artist
	for {
		page, _, err := r.artistService.List(ctx, params)
		if err != nil {
			r.logger.Error("listing artists for compliance export", "error", err)
			writeError(w, req, http.StatusInternalServerError, "failed to list artists")
			return
		}
		allArtists = append(allArtists, page...)
		if len(page) < params.PageSize || len(allArtists) >= 10000 {
			break
		}
		params.Page++
	}

	// Collect artist IDs and batch-load stored violations.
	allIDs := make([]string, len(allArtists))
	for i := range allArtists {
		allIDs[i] = allArtists[i].ID
	}
	violations, err := r.ruleService.GetViolationsForArtists(ctx, allIDs)
	if err != nil {
		r.logger.Error("loading violations for compliance export", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to load violations")
		return
	}

	// Look up library names for the export
	libs, err := r.libraryService.List(ctx)
	if err != nil {
		r.logger.Warn("listing libraries for compliance export", "error", err)
	}
	libNames := make(map[string]string, len(libs))
	for i := range libs {
		libNames[libs[i].ID] = libs[i].Name
	}

	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", `attachment; filename="compliance-report.csv"`)
	w.WriteHeader(http.StatusOK)

	cw := csv.NewWriter(w)
	profileName := r.getActiveProfileName(req.Context())
	if err := cw.Write([]string{"Artist Name", "Health Score", "Metadata", img.ImageTermFor("thumb", profileName), img.ImageTermFor("fanart", profileName), img.ImageTermFor("logo", profileName), "MBID", "Library", "Violations"}); err != nil {
		r.logger.Error("writing CSV header", "error", err)
		return
	}

	for i := range allArtists {
		if ctx.Err() != nil {
			break
		}
		a := &allArtists[i]
		vs := violations[a.ID]
		var violationNames []string
		for j := range vs {
			violationNames = append(violationNames, vs[j].RuleName)
		}
		libName := libNames[a.LibraryID]

		if err := cw.Write([]string{
			sanitizeCSV(a.Name),
			fmt.Sprintf("%.0f", a.HealthScore),
			boolCSV(a.NFOExists),
			boolCSV(a.ThumbExists),
			boolCSV(a.FanartExists),
			boolCSV(a.LogoExists),
			boolCSV(a.MusicBrainzID != ""),
			sanitizeCSV(libName),
			sanitizeCSV(strings.Join(violationNames, "; ")),
		}); err != nil {
			r.logger.Error("writing CSV row", "artist", a.Name, "error", err)
			return
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		r.logger.Error("flushing CSV writer", "error", err)
	}
}

func boolCSV(b bool) string {
	if b {
		return "Yes"
	}
	return "No"
}

// sanitizeCSV guards against CSV formula injection by prefixing values that
// start with formula-trigger characters (=, +, -, @) with a single quote so
// spreadsheet applications treat them as plain text.
func sanitizeCSV(s string) string {
	if s == "" {
		return s
	}
	trimmed := strings.TrimLeft(s, " \t")
	if trimmed == "" {
		return s
	}
	switch trimmed[0] {
	case '=', '+', '-', '@':
		return "'" + s
	}
	return s
}

// complianceListParams extracts ListParams from a compliance report request.
// Validates the sort and order parameters against the artist allowlist; on
// unknown values it writes a 400 response and returns ok=false so the caller
// can stop processing. PageSize respects the per-user preference stored in the
// database; an explicit page_size query param overrides it.
func (r *Router) complianceListParams(w http.ResponseWriter, req *http.Request) (artist.ListParams, bool) {
	sortKey, ok := validateSortParam(w, req, allowedArtistSort)
	if !ok {
		return artist.ListParams{}, false
	}
	order, ok := validateOrderParam(w, req)
	if !ok {
		return artist.ListParams{}, false
	}
	if sortKey == "" {
		sortKey = "name"
	}
	if order == "" {
		order = "asc"
	}

	userID := middleware.UserIDFromContext(req.Context())
	params := artist.ListParams{
		Page:           intQuery(req, "page", 1),
		PageSize:       r.getUserPageSize(req.Context(), userID, intQuery(req, "page_size", 0)),
		Sort:           sortKey,
		Order:          order,
		Search:         req.URL.Query().Get("search"),
		Filter:         req.URL.Query().Get("filter"),
		LibraryID:      req.URL.Query().Get("library_id"),
		HealthScoreMin: intQuery(req, "health_min", 0),
		HealthScoreMax: intQuery(req, "health_max", 0),
	}

	// Handle status filter (compliant/non_compliant)
	status := req.URL.Query().Get("status")
	if status == "compliant" && params.Filter == "" {
		params.Filter = "compliant"
	} else if status == "non_compliant" && params.Filter == "" {
		params.Filter = "non_compliant"
	}

	params.Validate()
	return params, true
}

// handleCompliancePage serves GET /reports/compliance.
//
// HTMX requests receive the ComplianceResults fragment — the swap target the
// reports workspace's compliance pane (search, filter flyout, pagination,
// column sort, Run button) points its hx-get controls at. Full-page requests
// are 302-redirected to the reports workspace with the compliance tab active
// (#1757 PR-4); the query string is carried through so filtered deep links
// and bookmarks keep resolving. The redirect needs no auth of its own — the
// workspace handler gates with requireAuth.
func (r *Router) handleCompliancePage(w http.ResponseWriter, req *http.Request) {
	if !isHTMXRequest(req) {
		q := req.URL.Query()
		q.Set("tab", "compliance")
		target := r.basePath + "/reports?" + q.Encode()
		// gosec G710: target's path is server-controlled (r.basePath +
		// /reports); only the encoded query is carried through from the
		// request, which cannot redirect off-origin.
		http.Redirect(w, req, target, http.StatusFound) //nolint:gosec // G710: path is server-built; only the encoded query flows from req.
		return
	}

	if !r.requireAuth(w, req) {
		return
	}

	ctx := req.Context()
	params, ok := r.complianceListParams(w, req)
	if !ok {
		return
	}
	status := req.URL.Query().Get("status")

	artists, total, err := r.artistService.List(ctx, params)
	if err != nil {
		r.logger.Error("listing artists for compliance page", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to list artists")
		return
	}

	// Collect artist IDs and batch-load stored violations.
	pageIDs := make([]string, len(artists))
	for i := range artists {
		pageIDs[i] = artists[i].ID
	}
	pageViolations, err := r.ruleService.GetViolationsForArtists(ctx, pageIDs)
	if err != nil {
		r.logger.Error("loading violations for compliance page", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to load violations")
		return
	}

	rows := make([]templates.ComplianceRow, len(artists))
	for i := range artists {
		a := &artists[i]
		vs := pageViolations[a.ID]
		if vs == nil {
			vs = make([]rule.Violation, 0)
		}
		rows[i] = templates.ComplianceRow{
			Artist:      *a,
			HealthScore: a.HealthScore,
			Violations:  vs,
		}
	}

	totalPages := total / params.PageSize
	if total%params.PageSize > 0 {
		totalPages++
	}

	var libs []library.Library
	if r.libraryService != nil {
		var err error
		libs, err = r.libraryService.List(ctx)
		if err != nil {
			r.logger.Warn("listing libraries for compliance page", "error", err)
		}
	}

	data := templates.ComplianceData{
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
		ProfileName:    r.getActiveProfileName(req.Context()),
	}

	vals := complianceURLValues(params, status, req.URL.Query().Get("filter"), req.URL.Query())
	pushURL := r.basePath + "/reports/compliance"
	if len(vals) > 0 {
		pushURL += "?" + vals.Encode()
	}
	w.Header().Set("HX-Push-Url", pushURL)
	// Render the full results shell (hidden carriers + chips + table) so
	// the search input's hx-include reads fresh hidden values after a
	// chip dismiss or Apply/Clear cycle (CR feedback on PR #1653).
	renderTempl(w, req, templates.ComplianceResults(data))
}

// complianceURLValues converts the compliance list params + raw status/filter
// query values into url.Values for HX-Push-Url. Only writes the canonical
// keys the compliance page reads back on next load. Default values (page 1,
// "all" status, "name" sort, "asc" order) are dropped so the pushed URL stays
// minimal. page_size is only included when the caller explicitly provided it
// as a query parameter (rawQuery.Has("page_size")); when it is absent the
// user's stored preference is the effective default and the URL omits it.
func complianceURLValues(params artist.ListParams, status, filter string, rawQuery url.Values) url.Values {
	q := url.Values{}
	if params.Search != "" {
		q.Set("search", params.Search)
	}
	if status != "" && status != "all" {
		q.Set("status", status)
	}
	if filter != "" {
		q.Set("filter", filter)
	}
	if params.LibraryID != "" {
		q.Set("library_id", params.LibraryID)
	}
	if params.HealthScoreMin > 0 {
		q.Set("health_min", strconv.Itoa(params.HealthScoreMin))
	}
	if params.HealthScoreMax > 0 {
		q.Set("health_max", strconv.Itoa(params.HealthScoreMax))
	}
	if params.Sort != "" && params.Sort != "name" {
		q.Set("sort", params.Sort)
	}
	if params.Order != "" && params.Order != "asc" {
		q.Set("order", params.Order)
	}
	// Preserve pagination so HTMX navigation that lands on page N keeps the
	// address bar pointed at page N. Without this, a user on page 3 who
	// applies/clears a chip lands back on page 1 after a manual refresh.
	if params.Page > 1 {
		q.Set("page", strconv.Itoa(params.Page))
	}
	// Only echo page_size back to the URL when the caller passed it explicitly.
	// Without a query param the user's stored preference is the effective default
	// and echoing it would pollute bookmarked/shared URLs unnecessarily.
	if rawQuery.Has("page_size") {
		q.Set("page_size", strconv.Itoa(params.PageSize))
	}
	return q
}

func toTemplateViolations(vs []violationSummary) []templates.ViolationSummaryData {
	out := make([]templates.ViolationSummaryData, len(vs))
	for i, v := range vs {
		out[i] = templates.ViolationSummaryData{
			RuleID:   v.RuleID,
			RuleName: v.RuleName,
			Count:    v.Count,
			Severity: v.Severity,
		}
	}
	return out
}

func toTemplateSummaries(summaries []librarySummary) []templates.LibrarySummaryData {
	out := make([]templates.LibrarySummaryData, len(summaries))
	for i, s := range summaries {
		out[i] = toTemplateSummary(s)
	}
	return out
}

func toTemplateSummary(s librarySummary) templates.LibrarySummaryData {
	return templates.LibrarySummaryData{
		LibraryID:        s.LibraryID,
		LibraryName:      s.LibraryName,
		TotalArtists:     s.TotalArtists,
		CompliantArtists: s.CompliantArtists,
		Score:            s.Score,
		MissingNFO:       s.MissingNFO,
		MissingThumb:     s.MissingThumb,
		MissingFanart:    s.MissingFanart,
		MissingMBID:      s.MissingMBID,
	}
}

func toTemplateRulePassRates(rs []rulePassRateRow) []templates.RulePassRateData {
	out := make([]templates.RulePassRateData, len(rs))
	for i, p := range rs {
		out[i] = templates.RulePassRateData{
			RuleID:    p.RuleID,
			RuleName:  p.RuleName,
			Severity:  p.Severity,
			Passed:    p.Passed,
			Failed:    p.Failed,
			Evaluated: p.Evaluated,
			PassRate:  p.PassRate,
		}
	}
	return out
}

// handleReportMetadataCompleteness returns aggregate field-coverage metrics.
// GET /api/v1/reports/metadata-completeness
func (r *Router) handleReportMetadataCompleteness(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	libraryID := req.URL.Query().Get("library_id")

	// Build a library-name map so the report can label per-library entries.
	libNames := make(map[string]string)
	if r.libraryService != nil {
		if libs, err := r.libraryService.List(ctx); err != nil {
			r.logger.Warn("listing libraries for metadata completeness", "error", err)
		} else {
			for i := range libs {
				libNames[libs[i].ID] = libs[i].Name
			}
		}
	}

	report, err := r.artistService.GetMetadataCompleteness(ctx, libraryID, 10, libNames)
	if err != nil {
		r.logger.Error("computing metadata completeness", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to compute metadata completeness")
		return
	}

	if isHTMXRequest(req) {
		renderTempl(w, req, templates.MetadataCompletenessFragment(report))
		return
	}

	writeJSON(w, http.StatusOK, report)
}

// handleReportRulePassRates returns one row per enabled rule with its
// pass / fail counts and pass rate (#699 slice 2). Powers the dashboard
// per-rule pass-rate widget. Rules with no evaluations yet are omitted
// (the widget shows "no data yet" for the empty-list case rather than
// rendering them at 0%). JSON only -- the dashboard widget consumes this
// via fetch + a small HTMX swap.
//
// GET /api/v1/reports/rule-pass-rates
func (r *Router) handleReportRulePassRates(w http.ResponseWriter, req *http.Request) {
	rates, err := r.ruleService.GetRulePassRates(req.Context())
	if err != nil {
		r.logger.Error("querying rule pass rates", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to query rule pass rates")
		return
	}
	if rates == nil {
		rates = []rule.RulePassRate{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"rates": rates})
}

// complianceCountTTL bounds the load that sidebar polling places on the DB.
// With a 60s sidebar poll per active tab, this TTL means at most one Count
// query every 5 minutes regardless of tab count.
const complianceCountTTL = 5 * time.Minute

// complianceCountState memoizes the most recent non-compliant-artist count so
// the sidebar badge endpoint does not re-query on every poll. Module-level
// (rather than Router-scoped) so the cache survives across hypothetical
// multi-router test setups; in production there is one Router.
type complianceCountState struct {
	mu        sync.Mutex
	count     int
	expiresAt time.Time
}

// get returns the cached count when fresh; otherwise refreshes via fn and
// caches the result for complianceCountTTL. Concurrent callers serialize on
// mu so the refresh fires at most once per TTL window even under burst load.
func (c *complianceCountState) get(ctx context.Context, fn func(context.Context) (int, error)) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Now().Before(c.expiresAt) {
		return c.count, nil
	}
	n, err := fn(ctx)
	if err != nil {
		return 0, err
	}
	c.count = n
	c.expiresAt = time.Now().Add(complianceCountTTL)
	return n, nil
}

// invalidate drops the cached value, forcing the next get call to refresh.
// Exposed for tests; production code relies on TTL expiry.
func (c *complianceCountState) invalidate() {
	c.mu.Lock()
	c.count = 0
	c.expiresAt = time.Time{}
	c.mu.Unlock()
}

var complianceCount complianceCountState

// handleComplianceCount returns an HTML fragment for the sidebar's Compliance
// child link. Admin-only (mirrors handleArtistDuplicatesCount).
//
// GET /api/v1/reports/compliance/count
//
// Count semantic: number of non-compliant artists (health_score < 100).
// This matches the compliance page's "non_compliant" filter and is the
// natural "how many artists need attention" signal for the nav pill.
//
// Returns:
//   - empty body when the count is zero (HTMX innerHTML swap leaves the
//     parent <li> empty, hiding the item when the library is fully compliant);
//   - an <a> link populated with the count when count > 0.
//
// ?ch=next: caller is the next/ sidebar; the href uses the /next/ path and
// the fragment includes the chart-bar glyph so the hydrated item matches the
// icon-led subnav style. Stable callers omit the glyph.
//
// The 403 response uses a JSON envelope even though the success path emits
// text/html. This mirrors handleArtistDuplicatesCount and handleForeignFilesCount:
// the sidebar template only renders this placeholder for administrators, so a
// 403 should never reach a user. HTMX does not swap content on non-2xx
// responses by default, so the JSON body is never shown. The JSON envelope
// keeps the error contract consistent with the rest of /api/v1/.
func (r *Router) handleComplianceCount(w http.ResponseWriter, req *http.Request) {
	if middleware.RoleFromContext(req.Context()) != "administrator" {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":   "forbidden",
			"message": "administrator role required",
		})
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	svc := r.artistService
	count, err := complianceCount.get(req.Context(), func(ctx context.Context) (int, error) {
		if svc == nil {
			return 0, nil
		}
		return svc.Count(ctx, artist.CountParams{Filter: "non_compliant"})
	})
	if err != nil {
		// Fail-safe: log and emit an empty body so the sidebar simply does not
		// show the Compliance child. Surfacing the error inline would clutter
		// every sidebar; the compliance page itself surfaces query failures.
		r.logger.Warn("compliance count refresh failed", "error", err)
		w.WriteHeader(http.StatusOK)
		return
	}

	w.WriteHeader(http.StatusOK)
	if count <= 0 {
		return
	}

	label := html.EscapeString(i18n.TFromCtx(req.Context()).T("nav.reports.compliance"))
	// ?ch=next: caller is the promoted sidebar; include the chart-bar glyph so
	// the hydrated item matches the icon-led subnav style. The href is the
	// canonical /reports/compliance (matching the sidebar's static Compliance
	// link and its data-path); a full-page click 302s to /reports?tab=compliance.
	if req.URL.Query().Get("ch") == "next" {
		href := html.EscapeString(r.basePath + "/reports/compliance")
		fmt.Fprintf(w, //nolint:errcheck // Best-effort HTTP write; client disconnect is not actionable
			`<a href="%s" class="sw-sidebar-link sw-sidebar-subnav-link" data-path="/reports/compliance" aria-label="%s">`+
				`<svg class="sw-sidebar-icon" fill="none" viewBox="0 0 24 24" stroke-width="1.5" stroke="currentColor" aria-hidden="true">`+
				`<path stroke-linecap="round" stroke-linejoin="round" d="M3 13.125C3 12.504 3.504 12 4.125 12h2.25c.621 0 1.125.504 1.125 1.125v6.75C7.5 20.496 6.996 21 6.375 21h-2.25A1.125 1.125 0 0 1 3 19.875v-6.75ZM9.75 8.625c0-.621.504-1.125 1.125-1.125h2.25c.621 0 1.125.504 1.125 1.125v11.25c0 .621-.504 1.125-1.125 1.125h-2.25a1.125 1.125 0 0 1-1.125-1.125V8.625ZM16.5 4.125c0-.621.504-1.125 1.125-1.125h2.25C20.496 3 21 3.504 21 4.125v15.75c0 .621-.504 1.125-1.125 1.125h-2.25a1.125 1.125 0 0 1-1.125-1.125V4.125Z"></path></svg>`+
				`<span class="sw-sidebar-label">%s</span>`+
				`<span class="sw-sidebar-count-pill">%d</span>`+
				`</a>`,
			href, label, label, count)
		return
	}
	href := html.EscapeString(r.basePath + "/reports/compliance")
	fmt.Fprintf(w, //nolint:errcheck // Best-effort HTTP write; client disconnect is not actionable
		`<a href="%s" class="sw-sidebar-link sw-sidebar-subnav-link" data-path="/reports/compliance" aria-label="%s">`+
			`<span class="sw-sidebar-label">%s</span>`+
			`<span class="sw-sidebar-badge-pill">%d</span>`+
			`</a>`,
		href, label, label, count)
}

// handleReportsPage serves GET /reports (M55 #1337; promoted to the canonical
// route in #1757 PR-4) — the reports two-pane workspace. Defaults to the
// Compliance overview built-in report; ?tab={name} selects another (the form
// handleCompliancePage's full-page redirect emits). wrapOptionalAuth is used
// on the route so unauthenticated visitors receive the login page rather than
// a 401 JSON error.
func (r *Router) handleReportsPage(w http.ResponseWriter, req *http.Request) {
	name := req.URL.Query().Get("tab")
	if name == "" {
		name = "compliance"
	}
	r.serveReportsWorkspace(w, req, name)
}

// handleReportPage serves GET /reports/{name} (M55 #1337; promoted in #1757
// PR-4). The {name} path value selects which built-in report is active in the
// workspace rail. "compliance", "health", "metadata-completeness", and
// "rule-pass-rates" have fully implemented right-panes (they have backing API
// handlers); other names render a placeholder. More specific routes
// (/reports/compliance, /reports/duplicates) take precedence in the mux over
// this pattern so they are unaffected.
func (r *Router) handleReportPage(w http.ResponseWriter, req *http.Request) {
	name := req.PathValue("name")
	if name == "" {
		name = "compliance"
	}
	r.serveReportsWorkspace(w, req, name)
}

// serveReportsWorkspace is the shared implementation for both reports
// workspace routes. It gates on auth (unauthenticated → login page), loads
// report data for recognized report names, and renders the two-pane
// workspace shell.
func (r *Router) serveReportsWorkspace(w http.ResponseWriter, req *http.Request, reportName string) {
	if !r.requireAuth(w, req) {
		return
	}

	data := templates.ReportsPageData{
		ActiveReport: reportName,
	}

	// Each recognized report name loads its own data and renders a dedicated
	// right-pane component. Unrecognized names fall through with no data and
	// render the "coming soon" placeholder. A loader that writes an error
	// response returns ok==false, in which case we must not also render.
	switch reportName {
	case "compliance":
		cd, ok := r.loadReportsComplianceData(w, req)
		if !ok {
			return
		}
		data.ComplianceData = cd
	case "health":
		hd, ok := r.loadReportsHealthData(w, req)
		if !ok {
			return
		}
		data.HealthData = hd
	case "metadata-completeness":
		md, ok := r.loadReportsMetadataData(w, req)
		if !ok {
			return
		}
		data.MetadataReport = md
	case "rule-pass-rates":
		rd, ok := r.loadReportsRulePassRatesData(w, req)
		if !ok {
			return
		}
		data.RulePassRates = rd
	}

	renderTempl(w, req, templates.ReportsPage(r.assetsFor(req), data))
}

// loadReportsComplianceData builds a ComplianceData value for the reports
// workspace using the same logic as handleCompliancePage's fragment path. The
// Pagination.BaseURL uses the stable /reports/compliance path so HTMX
// pagination links call that handler (which returns the ComplianceResults
// fragment on HTMX requests), matching the swap contract ComplianceTable expects.
func (r *Router) loadReportsComplianceData(w http.ResponseWriter, req *http.Request) (templates.ComplianceData, bool) {
	ctx := req.Context()

	params, ok := r.complianceListParams(w, req)
	if !ok {
		return templates.ComplianceData{}, false
	}
	status := req.URL.Query().Get("status")

	artists, total, err := r.artistService.List(ctx, params)
	if err != nil {
		r.logger.Error("listing artists for reports workspace", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return templates.ComplianceData{}, false
	}

	pageIDs := make([]string, len(artists))
	for i := range artists {
		pageIDs[i] = artists[i].ID
	}

	violations, err := r.ruleService.GetViolationsForArtists(ctx, pageIDs)
	if err != nil {
		r.logger.Error("loading violations for reports workspace", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return templates.ComplianceData{}, false
	}

	resultCounts, rcErr := r.ruleService.GetRuleResultCounts(ctx, pageIDs)
	if rcErr != nil {
		r.logger.Warn("loading rule result counts for reports workspace", "error", rcErr)
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
			r.logger.Warn("listing libraries for reports workspace", "error", err)
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

// loadReportsHealthData builds the HealthSummaryData for the reports
// workspace "health" report. It mirrors handleReportHealth's read path
// (stored per-artist health scores + top failing rules + per-rule pass
// rates) but assembles the templates view model directly. The pass-rate
// widget is non-critical: a failure there warns and falls through with an
// empty slice rather than failing the whole pane.
func (r *Router) loadReportsHealthData(w http.ResponseWriter, req *http.Request) (templates.HealthSummaryData, bool) {
	ctx := req.Context()

	stats, err := r.artistService.GetHealthStats(ctx, "")
	if err != nil {
		r.logger.Error("loading health stats for reports workspace", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return templates.HealthSummaryData{}, false
	}

	topViolations, err := r.ruleService.TopFailingRuleResults(ctx, 10)
	if err != nil {
		r.logger.Error("loading top failing rules for reports workspace", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return templates.HealthSummaryData{}, false
	}

	rates, err := r.ruleService.GetRulePassRates(ctx)
	if err != nil {
		r.logger.Warn("loading rule pass rates for reports health pane", "error", err)
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

// loadReportsMetadataData builds the MetadataCompletenessReport for the
// reports workspace "metadata-completeness" report, mirroring
// handleReportMetadataCompleteness's read path (per-library name map +
// completeness aggregation with the lowest-10 artists).
func (r *Router) loadReportsMetadataData(w http.ResponseWriter, req *http.Request) (*artist.MetadataCompletenessReport, bool) {
	ctx := req.Context()

	libNames := make(map[string]string)
	if r.libraryService != nil {
		if libs, err := r.libraryService.List(ctx); err != nil {
			r.logger.Warn("listing libraries for reports metadata completeness", "error", err)
		} else {
			for i := range libs {
				libNames[libs[i].ID] = libs[i].Name
			}
		}
	}

	report, err := r.artistService.GetMetadataCompleteness(ctx, "", 10, libNames)
	if err != nil {
		r.logger.Error("computing metadata completeness for reports workspace", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return nil, false
	}

	return report, true
}

// loadReportsRulePassRatesData builds the per-rule pass-rate rows for the
// reports workspace "rule-pass-rates" report. The JSON endpoint
// (handleReportRulePassRates) is JSON-only, so this loader maps the rule
// service rows into the templ view model the inline pane renders.
func (r *Router) loadReportsRulePassRatesData(w http.ResponseWriter, req *http.Request) ([]templates.RulePassRateData, bool) {
	rates, err := r.ruleService.GetRulePassRates(req.Context())
	if err != nil {
		r.logger.Error("querying rule pass rates for reports workspace", "error", err)
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
