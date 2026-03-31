package api

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	img "github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/rule"
	"github.com/sydlexius/stillwater/web/components"
	"github.com/sydlexius/stillwater/web/templates"
)

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

	topViolations, err := r.ruleService.TopViolationSummaries(ctx, 10)
	if err != nil {
		r.logger.Error("querying top violations", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to generate health report")
		return
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
		} else if t, err := time.Parse("2006-01-02", v); err == nil {
			from = t
		}
	}
	if v := req.URL.Query().Get("to"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			to = t
		} else if t, err := time.Parse("2006-01-02", v); err == nil {
			to = t
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
	for _, lib := range libs {
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
	params := complianceListParams(req)

	artists, total, err := r.artistService.List(ctx, params)
	if err != nil {
		r.logger.Error("listing artists for compliance report", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to list artists")
		return
	}

	// Collect artist IDs for the batch violation lookup.
	ids := make([]string, len(artists))
	for i, a := range artists {
		ids[i] = a.ID
	}

	violations, err := r.ruleService.GetViolationsForArtists(ctx, ids)
	if err != nil {
		r.logger.Error("loading violations for compliance report", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to load violations")
		return
	}

	rows := make([]templates.ComplianceRow, len(artists))
	for i, a := range artists {
		vs := violations[a.ID]
		if vs == nil {
			vs = make([]rule.Violation, 0)
		}
		rows[i] = templates.ComplianceRow{
			Artist:      a,
			HealthScore: a.HealthScore,
			Violations:  vs,
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

	params := complianceListParams(req)
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
	for i, a := range allArtists {
		allIDs[i] = a.ID
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
	for _, l := range libs {
		libNames[l.ID] = l.Name
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

	for _, a := range allArtists {
		if ctx.Err() != nil {
			break
		}
		var violationNames []string
		for _, v := range violations[a.ID] {
			violationNames = append(violationNames, v.RuleName)
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
	if len(s) == 0 {
		return s
	}
	trimmed := strings.TrimLeft(s, " \t")
	if len(trimmed) == 0 {
		return s
	}
	switch trimmed[0] {
	case '=', '+', '-', '@':
		return "'" + s
	}
	return s
}

// complianceListParams extracts ListParams from a compliance report request.
func complianceListParams(req *http.Request) artist.ListParams {
	sort := req.URL.Query().Get("sort")
	order := req.URL.Query().Get("order")
	if sort == "" {
		sort = "name"
	}
	if order == "" {
		order = "asc"
	}

	params := artist.ListParams{
		Page:           intQuery(req, "page", 1),
		PageSize:       intQuery(req, "page_size", 50),
		Sort:           sort,
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
	return params
}

// handleCompliancePage renders the compliance report HTML page.
// GET /reports/compliance
func (r *Router) handleCompliancePage(w http.ResponseWriter, req *http.Request) {
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		r.renderLoginPage(w, req)
		return
	}

	ctx := req.Context()
	params := complianceListParams(req)
	status := req.URL.Query().Get("status")

	artists, total, err := r.artistService.List(ctx, params)
	if err != nil {
		r.logger.Error("listing artists for compliance page", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Collect artist IDs and batch-load stored violations.
	pageIDs := make([]string, len(artists))
	for i, a := range artists {
		pageIDs[i] = a.ID
	}
	pageViolations, err := r.ruleService.GetViolationsForArtists(ctx, pageIDs)
	if err != nil {
		r.logger.Error("loading violations for compliance page", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	rows := make([]templates.ComplianceRow, len(artists))
	for i, a := range artists {
		vs := pageViolations[a.ID]
		if vs == nil {
			vs = make([]rule.Violation, 0)
		}
		rows[i] = templates.ComplianceRow{
			Artist:      a,
			HealthScore: a.HealthScore,
			Violations:  vs,
		}
	}

	totalPages := total / params.PageSize
	if total%params.PageSize > 0 {
		totalPages++
	}

	libs, err := r.libraryService.List(ctx)
	if err != nil {
		r.logger.Warn("listing libraries for compliance page", "error", err)
	}

	data := templates.ComplianceData{
		Rows: rows,
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
			TargetID:       "compliance-table",
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

	if isHTMXRequest(req) {
		renderTempl(w, req, templates.ComplianceTable(data))
		return
	}
	renderTempl(w, req, templates.CompliancePage(r.assetsFor(req), data))
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
			for _, lib := range libs {
				libNames[lib.ID] = lib.Name
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
