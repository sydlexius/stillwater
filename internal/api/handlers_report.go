package api

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/internal/rule"
	"github.com/sydlexius/stillwater/web/components"
	"github.com/sydlexius/stillwater/web/templates"
)

// healthCacheTTL is how long a computed health summary is considered fresh.
// During this window, subsequent requests receive the cached result without
// re-running EvaluateAll, which is the most expensive operation in the
// reports pipeline.
const healthCacheTTL = 30 * time.Second

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
// Evaluates all artists against active rules and records a health snapshot.
//
// Results are cached for healthCacheTTL (30 seconds). Concurrent requests
// are coalesced via singleflight so only one goroutine runs the expensive
// EvaluateAll call at a time. The cache is invalidated when artists or rules
// are mutated (see InvalidateHealthCache).
//
// GET /api/v1/reports/health
func (r *Router) handleReportHealth(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	// Fast path: check the cache under a read lock. Multiple concurrent
	// readers can check simultaneously without blocking each other.
	r.healthCacheMu.RLock()
	if r.healthResult != nil && time.Since(r.healthCachedAt) < healthCacheTTL {
		cached := *r.healthResult
		r.healthCacheMu.RUnlock()
		r.renderHealthResponse(w, req, cached)
		return
	}
	r.healthCacheMu.RUnlock()

	// Cache is stale or empty. Use singleflight to ensure only one goroutine
	// runs the expensive computation. All other concurrent callers block here
	// and receive the same result when the first caller finishes.
	//
	// Detach from the winning request's lifecycle: if the first caller's HTTP
	// connection drops, its context cancels. Without WithoutCancel, that would
	// abort the computation for all coalesced callers waiting on the same
	// singleflight key. The detached context preserves request-scoped values
	// (logging, tracing) while preventing one client's cancellation from
	// failing everyone else.
	sfCtx, sfCancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer sfCancel()
	val, err, _ := r.healthFlight.Do("health", func() (any, error) {
		// Double-check: another goroutine may have refreshed the cache
		// between the RUnlock above and this singleflight invocation.
		r.healthCacheMu.RLock()
		if r.healthResult != nil && time.Since(r.healthCachedAt) < healthCacheTTL {
			cached := *r.healthResult
			r.healthCacheMu.RUnlock()
			return cached, nil
		}
		r.healthCacheMu.RUnlock()

		summary, compErr := r.computeHealthSummary(sfCtx)
		if compErr != nil {
			return nil, compErr
		}

		// Store the result in the cache under a write lock.
		r.healthCacheMu.Lock()
		r.healthResult = &summary
		r.healthCachedAt = time.Now()
		r.healthCacheMu.Unlock()

		// Record a health snapshot once per computation (not per cache hit)
		// so the history table stays accurate without being flooded.
		if err := r.ruleService.RecordHealthSnapshot(sfCtx, summary.TotalArtists, summary.CompliantArtists, summary.Score); err != nil {
			r.logger.Warn("recording health snapshot", "error", err)
		}

		return summary, nil
	})

	if err != nil {
		r.logger.Error("computing health summary", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to generate health report")
		return
	}

	summary := val.(healthSummary)
	r.renderHealthResponse(w, req, summary)
}

// computeHealthSummary fetches all non-excluded artists, evaluates them
// against active rules, and builds the aggregate health summary. This is
// the expensive part that the cache and singleflight protect.
func (r *Router) computeHealthSummary(ctx context.Context) (healthSummary, error) {
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
		return healthSummary{}, fmt.Errorf("listing artists: %w", err)
	}

	// Fetch remaining pages if total > 200
	for len(allArtists) < total {
		params.Page++
		more, _, err := r.artistService.List(ctx, params)
		if err != nil {
			return healthSummary{}, fmt.Errorf("listing artists page %d: %w", params.Page, err)
		}
		allArtists = append(allArtists, more...)
	}

	results, err := r.ruleEngine.EvaluateAll(ctx, allArtists)
	if err != nil {
		return healthSummary{}, fmt.Errorf("evaluating artists: %w", err)
	}

	return buildHealthSummary(allArtists, results), nil
}

// renderHealthResponse writes the health summary as either an HTMX HTML
// fragment or a JSON response, depending on the request headers. This is
// separated from the computation so that each concurrent caller renders
// its own response from the shared data (the singleflight only computes
// the data, it does not render).
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

// InvalidateHealthCache clears the cached health summary so the next
// request triggers a fresh computation. Call this after any mutation that
// could change artist data or rule configuration (e.g. field updates,
// rule changes, scanner imports).
func (r *Router) InvalidateHealthCache() {
	r.healthCacheMu.Lock()
	r.healthResult = nil
	r.healthCachedAt = time.Time{}
	r.healthCacheMu.Unlock()
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

// handleReportHealthByLibrary returns per-library health breakdown.
// GET /api/v1/reports/health/by-library
func (r *Router) handleReportHealthByLibrary(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	libs, err := r.libraryService.List(ctx)
	if err != nil {
		r.logger.Error("listing libraries for health by-library", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to list libraries")
		return
	}

	// Build per-library summaries
	var summaries []librarySummary
	var allArtists []artist.Artist
	var allResults []rule.EvaluationResult

	for _, lib := range libs {
		artists, err := r.listAllArtists(ctx, lib.ID)
		if err != nil {
			r.logger.Error("listing artists for library health", "library", lib.Name, "error", err)
			continue
		}
		if len(artists) == 0 {
			continue
		}

		results, err := r.ruleEngine.EvaluateAll(ctx, artists)
		if err != nil {
			r.logger.Error("evaluating artists for library health", "library", lib.Name, "error", err)
			continue
		}

		allArtists = append(allArtists, artists...)
		allResults = append(allResults, results...)

		summaries = append(summaries, buildLibrarySummary(lib, artists, results))
	}

	// Build overall summary from all artists
	overall := buildOverallLibrarySummary(allArtists, allResults)

	if isHTMXRequest(req) {
		renderTempl(w, req, templates.ComplianceSummaryFragment(templates.ComplianceSummaryData{
			Libraries: toTemplateSummaries(summaries),
			Overall:   toTemplateSummary(overall),
		}))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"libraries": summaries,
		"overall":   overall,
	})
}

// listAllArtists fetches all non-excluded artists for a library via pagination.
func (r *Router) listAllArtists(ctx context.Context, libraryID string) ([]artist.Artist, error) {
	const pageSize = 200
	params := artist.ListParams{
		Page:      1,
		PageSize:  pageSize,
		Sort:      "name",
		Order:     "asc",
		Filter:    "not_excluded",
		LibraryID: libraryID,
	}
	params.Validate()

	var all []artist.Artist
	for {
		page, _, err := r.artistService.List(ctx, params)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < pageSize {
			break
		}
		params.Page++
	}
	return all, nil
}

func buildLibrarySummary(lib library.Library, artists []artist.Artist, results []rule.EvaluationResult) librarySummary {
	ls := librarySummary{
		LibraryID:    lib.ID,
		LibraryName:  lib.Name,
		TotalArtists: len(artists),
	}

	totalPassed, totalRules := 0, 0
	for i, a := range artists {
		if results[i].HealthScore >= 100.0 {
			ls.CompliantArtists++
		}
		if !a.NFOExists {
			ls.MissingNFO++
		}
		if !a.ThumbExists {
			ls.MissingThumb++
		}
		if !a.FanartExists {
			ls.MissingFanart++
		}
		if a.MusicBrainzID == "" {
			ls.MissingMBID++
		}
		totalPassed += results[i].RulesPassed
		totalRules += results[i].RulesTotal
	}

	if totalRules > 0 {
		ls.Score = float64(int(float64(totalPassed)/float64(totalRules)*1000)) / 10
	} else if ls.TotalArtists > 0 {
		ls.Score = 100.0
	}

	return ls
}

func buildOverallLibrarySummary(artists []artist.Artist, results []rule.EvaluationResult) librarySummary {
	ls := librarySummary{TotalArtists: len(artists)}

	totalPassed, totalRules := 0, 0
	for i, a := range artists {
		if results[i].HealthScore >= 100.0 {
			ls.CompliantArtists++
		}
		if !a.NFOExists {
			ls.MissingNFO++
		}
		if !a.ThumbExists {
			ls.MissingThumb++
		}
		if !a.FanartExists {
			ls.MissingFanart++
		}
		if a.MusicBrainzID == "" {
			ls.MissingMBID++
		}
		totalPassed += results[i].RulesPassed
		totalRules += results[i].RulesTotal
	}

	if totalRules > 0 {
		ls.Score = float64(int(float64(totalPassed)/float64(totalRules)*1000)) / 10
	} else if ls.TotalArtists > 0 {
		ls.Score = 100.0
	}

	return ls
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

	results, err := r.ruleEngine.EvaluateAll(ctx, allArtists)
	if err != nil {
		r.logger.Error("evaluating artists for compliance export", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to evaluate artists")
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
	if err := cw.Write([]string{"Artist Name", "Health Score", "Metadata", "Thumb", "Fanart", "Logo", "MBID", "Library", "Violations"}); err != nil {
		r.logger.Error("writing CSV header", "error", err)
		return
	}

	for i, a := range allArtists {
		if ctx.Err() != nil {
			break
		}
		var violations []string
		for _, v := range results[i].Violations {
			violations = append(violations, v.RuleName)
		}
		libName := libNames[a.LibraryID]

		if err := cw.Write([]string{
			sanitizeCSV(a.Name),
			fmt.Sprintf("%.0f", results[i].HealthScore),
			boolCSV(a.NFOExists),
			boolCSV(a.ThumbExists),
			boolCSV(a.FanartExists),
			boolCSV(a.LogoExists),
			boolCSV(a.MusicBrainzID != ""),
			sanitizeCSV(libName),
			sanitizeCSV(strings.Join(violations, "; ")),
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
	params := complianceListParams(req)
	status := req.URL.Query().Get("status")

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
