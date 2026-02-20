package api

import (
	"net/http"
	"time"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
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

// handleReportHealth returns the current library health summary.
// Evaluates all artists against active rules and records a health snapshot.
// GET /api/v1/reports/health
func (r *Router) handleReportHealth(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	// Get all non-excluded artists
	params := artist.ListParams{
		Page:     1,
		PageSize: 200,
		Sort:     "name",
		Order:    "asc",
		Filter:   "not_excluded",
	}
	params.Validate()

	allArtists, total, err := r.artistService.List(ctx, params)
	if err != nil {
		r.logger.Error("listing artists for health report", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to generate health report")
		return
	}

	// Fetch remaining pages if total > 200
	for len(allArtists) < total {
		params.Page++
		more, _, err := r.artistService.List(ctx, params)
		if err != nil {
			r.logger.Error("listing artists for health report (page)", "page", params.Page, "error", err)
			writeError(w, req, http.StatusInternalServerError, "failed to generate health report")
			return
		}
		allArtists = append(allArtists, more...)
	}

	// Evaluate all artists to compute violations
	results, err := r.ruleEngine.EvaluateAll(ctx, allArtists)
	if err != nil {
		r.logger.Error("evaluating artists for health report", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to evaluate artists")
		return
	}

	summary := buildHealthSummary(allArtists, results)

	// Record a health snapshot
	if err := r.ruleService.RecordHealthSnapshot(ctx, summary.TotalArtists, summary.CompliantArtists, summary.Score); err != nil {
		r.logger.Warn("recording health snapshot", "error", err)
	}

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

// handleReportCompliance returns a paginated compliance report.
// GET /api/v1/reports/compliance
func (r *Router) handleReportCompliance(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	params := artist.ListParams{
		Page:     intQuery(req, "page", 1),
		PageSize: intQuery(req, "page_size", 50),
		Sort:     "name",
		Order:    "asc",
		Search:   req.URL.Query().Get("search"),
		Filter:   req.URL.Query().Get("filter"),
	}

	// Handle status filter (compliant/non_compliant)
	status := req.URL.Query().Get("status")
	if status == "compliant" && params.Filter == "" {
		params.Filter = "compliant"
	} else if status == "non_compliant" && params.Filter == "" {
		params.Filter = "non_compliant"
	}

	params.Validate()

	artists, total, err := r.artistService.List(ctx, params)
	if err != nil {
		r.logger.Error("listing artists for compliance report", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to list artists")
		return
	}

	// Evaluate each artist
	results, err := r.ruleEngine.EvaluateAll(ctx, artists)
	if err != nil {
		r.logger.Error("evaluating artists for compliance", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to evaluate artists")
		return
	}

	rows := make([]templates.ComplianceRow, len(artists))
	for i := range artists {
		rows[i] = templates.ComplianceRow{
			Artist:      artists[i],
			HealthScore: results[i].HealthScore,
			Violations:  results[i].Violations,
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"rows":      rows,
		"total":     total,
		"page":      params.Page,
		"page_size": params.PageSize,
	})
}

func buildHealthSummary(artists []artist.Artist, results []rule.EvaluationResult) healthSummary {
	var s healthSummary
	s.TotalArtists = len(artists)

	violationCounts := make(map[string]*violationSummary)

	for i, a := range artists {
		if results[i].HealthScore >= 100.0 {
			s.CompliantArtists++
		}
		if !a.NFOExists {
			s.MissingNFO++
		}
		if !a.ThumbExists {
			s.MissingThumb++
		}
		if !a.FanartExists {
			s.MissingFanart++
		}
		if a.MusicBrainzID == "" {
			s.MissingMBID++
		}

		for _, v := range results[i].Violations {
			if vs, ok := violationCounts[v.RuleID]; ok {
				vs.Count++
			} else {
				violationCounts[v.RuleID] = &violationSummary{
					RuleID:   v.RuleID,
					RuleName: v.RuleName,
					Count:    1,
					Severity: v.Severity,
				}
			}
		}
	}

	// Compute overall score
	if s.TotalArtists > 0 {
		totalPassed := 0
		totalRules := 0
		for _, r := range results {
			totalPassed += r.RulesPassed
			totalRules += r.RulesTotal
		}
		if totalRules > 0 {
			s.Score = float64(totalPassed) / float64(totalRules) * 100.0
			// Round to 1 decimal
			s.Score = float64(int(s.Score*10)) / 10
		} else {
			s.Score = 100.0
		}
	} else {
		s.Score = 100.0
	}

	// Convert map to sorted slice (most violations first)
	for _, vs := range violationCounts {
		s.TopViolations = append(s.TopViolations, *vs)
	}
	sortViolations(s.TopViolations)

	// Limit to top 10
	if len(s.TopViolations) > 10 {
		s.TopViolations = s.TopViolations[:10]
	}

	return s
}

func sortViolations(vs []violationSummary) {
	for i := 0; i < len(vs); i++ {
		for j := i + 1; j < len(vs); j++ {
			if vs[j].Count > vs[i].Count {
				vs[i], vs[j] = vs[j], vs[i]
			}
		}
	}
}

// handleCompliancePage renders the compliance report HTML page.
// GET /reports/compliance
func (r *Router) handleCompliancePage(w http.ResponseWriter, req *http.Request) {
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		renderTempl(w, req, templates.LoginPage(r.assets()))
		return
	}

	ctx := req.Context()

	params := artist.ListParams{
		Page:     intQuery(req, "page", 1),
		PageSize: intQuery(req, "page_size", 50),
		Sort:     "name",
		Order:    "asc",
		Search:   req.URL.Query().Get("search"),
		Filter:   req.URL.Query().Get("filter"),
	}

	status := req.URL.Query().Get("status")
	if status == "compliant" && params.Filter == "" {
		params.Filter = "compliant"
	} else if status == "non_compliant" && params.Filter == "" {
		params.Filter = "non_compliant"
	}

	params.Validate()

	artists, total, err := r.artistService.List(ctx, params)
	if err != nil {
		r.logger.Error("listing artists for compliance page", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	results, err := r.ruleEngine.EvaluateAll(ctx, artists)
	if err != nil {
		r.logger.Error("evaluating artists for compliance page", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	rows := make([]templates.ComplianceRow, len(artists))
	for i := range artists {
		rows[i] = templates.ComplianceRow{
			Artist:      artists[i],
			HealthScore: results[i].HealthScore,
			Violations:  results[i].Violations,
		}
	}

	totalPages := total / params.PageSize
	if total%params.PageSize > 0 {
		totalPages++
	}

	data := templates.ComplianceData{
		Rows: rows,
		Pagination: components.PaginationData{
			CurrentPage: params.Page,
			TotalPages:  totalPages,
			PageSize:    params.PageSize,
			TotalItems:  total,
			BaseURL:     "/reports/compliance",
			Sort:        "name",
			Order:       "asc",
			Search:      params.Search,
			Filter:      params.Filter,
			TargetID:    "compliance-table",
		},
		Search: params.Search,
		Status: status,
		Filter: req.URL.Query().Get("filter"),
	}

	if isHTMXRequest(req) {
		renderTempl(w, req, templates.ComplianceTable(data))
		return
	}
	renderTempl(w, req, templates.CompliancePage(r.assets(), data))
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
