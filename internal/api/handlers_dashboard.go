package api

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/internal/rule"
	"github.com/sydlexius/stillwater/web/templates"
)

// dashboardFilterParams holds the parsed filter state from a dashboard request.
// Each field holds at most one value (the first non-empty entry wins) because
// the underlying rule.ViolationListParams is single-value per dimension.
type dashboardFilterParams struct {
	Search    string
	Severity  string
	Category  string
	LibraryID string
	RuleID    string
	Fixable   string // "yes", "no", or ""
}

// parseDashboardFilters reads search and filter parameters from the request
// query string. Keys: search, severity, category, library, rule, fixable.
func parseDashboardFilters(req *http.Request) dashboardFilterParams {
	q := req.URL.Query()
	return dashboardFilterParams{
		Search:    strings.TrimSpace(q.Get("search")),
		Severity:  q.Get("severity"),
		Category:  q.Get("category"),
		LibraryID: q.Get("library"),
		RuleID:    q.Get("rule"),
		Fixable:   q.Get("fixable"),
	}
}

// handleDashboardActionQueue renders the action queue fragment for the dashboard.
// It loads active violations sorted by severity, the counts needed for the
// filter flyout badges, and compact health metrics for the header.
// GET /dashboard/actions
func (r *Router) handleDashboardActionQueue(w http.ResponseWriter, req *http.Request) {
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	ctx := req.Context()

	filters := parseDashboardFilters(req)
	limit := r.getUserPageSize(ctx, userID, intQuery(req, "limit", 0))
	offset := intQuery(req, "offset", 0)

	// Clamp pagination values to prevent abuse and invalid paging.
	if offset < 0 {
		offset = 0
	}

	params := rule.ViolationListParams{
		Status:    "active",
		Search:    filters.Search,
		Severity:  filters.Severity,
		Category:  filters.Category,
		LibraryID: filters.LibraryID,
		RuleID:    filters.RuleID,
		Fixable:   filters.Fixable,
		Sort:      "severity",
		Order:     "desc",
		Limit:     limit,
		Offset:    offset,
	}

	violations, total, err := r.ruleService.ListViolationsFilteredPaged(ctx, params)
	if err != nil {
		r.logger.Error("dashboard action queue", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to load violations")
		return
	}

	// Load-more requests (offset > 0) only need violations and pagination
	// data -- skip the summary queries that the header/flyout need.
	if offset > 0 {
		data := templates.ActionQueueData{
			Violations: violations,
			Total:      total,
			Limit:      limit,
			Offset:     offset,
			BasePath:   r.basePath,
			// Preserve filter state so dashboardLoadMoreURL can build the next page URL.
			Search:    filters.Search,
			Severity:  filters.Severity,
			Category:  filters.Category,
			LibraryID: filters.LibraryID,
			RuleID:    filters.RuleID,
			Fixable:   filters.Fixable,
		}
		renderTempl(w, req, templates.DashboardActionMoreRows(data))
		return
	}

	// Fetch facet counts for each filter dimension. Each call clears its
	// own dimension from the filter so the user can see "how many of each
	// X exist within the current OTHER filters". Best-effort: on failure
	// we render an empty map so the filter flyout still renders.
	categoryCounts, err := r.ruleService.CountActiveViolationsByCategory(ctx, params)
	if err != nil {
		r.logger.Warn("dashboard category counts", "error", err)
		categoryCounts = map[string]int{}
	}

	severityCounts, err := r.ruleService.CountActiveViolationsBySeverity(ctx, params)
	if err != nil {
		r.logger.Warn("dashboard severity counts", "error", err)
		severityCounts = map[string]int{}
	}

	libraryCounts, err := r.ruleService.CountActiveViolationsByLibrary(ctx, params)
	if err != nil {
		r.logger.Warn("dashboard library counts", "error", err)
		libraryCounts = map[string]int{}
	}

	ruleCounts, err := r.ruleService.CountActiveViolationsByRule(ctx, params)
	if err != nil {
		r.logger.Warn("dashboard rule counts", "error", err)
		ruleCounts = []rule.RuleViolationCount{}
	}

	fixableYes, fixableNo, err := r.ruleService.CountActiveViolationsByFixable(ctx, params)
	if err != nil {
		r.logger.Warn("dashboard fixable counts", "error", err)
		fixableYes, fixableNo = 0, 0
	}

	// Load libraries for the filter flyout. Libraries without any active
	// violation are still shown so the filter options match what the user
	// sees elsewhere. On failure we render with an empty list.
	var libraries []library.Library
	if r.libraryService != nil {
		libs, err := r.libraryService.List(ctx)
		if err != nil {
			r.logger.Warn("dashboard libraries", "error", err)
		} else {
			libraries = libs
		}
	}

	// Fetch health summary for the compact metrics in the header.
	var healthScore float64
	var totalArtists, compliantArtists int
	var healthStatsError bool
	stats, err := r.artistService.GetHealthStats(ctx, "")
	if err == nil {
		healthScore = stats.Score
		totalArtists = stats.TotalArtists
		compliantArtists = stats.CompliantArtists
	} else {
		r.logger.Warn("dashboard health stats", "error", err)
		healthStatsError = true
	}

	data := templates.ActionQueueData{
		Violations:       violations,
		Total:            total,
		Limit:            limit,
		Offset:           offset,
		HealthScore:      healthScore,
		TotalArtists:     totalArtists,
		CompliantArtists: compliantArtists,
		BasePath:         r.basePath,
		HealthStatsError: healthStatsError,

		Search:    filters.Search,
		Severity:  filters.Severity,
		Category:  filters.Category,
		LibraryID: filters.LibraryID,
		RuleID:    filters.RuleID,
		Fixable:   filters.Fixable,

		SeverityCounts: severityCounts,
		CategoryCounts: categoryCounts,
		LibraryCounts:  libraryCounts,
		RuleCounts:     ruleCounts,
		FixableYes:     fixableYes,
		FixableNo:      fixableNo,
		Libraries:      libraries,
	}

	// Ensure the browser address bar tracks the user-facing URL ("/?...")
	// after an HTMX swap, not the internal fetch endpoint ("/dashboard/actions?...").
	// This matters for the search input where the push URL can't be computed
	// at template render time. Only set on HTMX requests (non-HTMX callers
	// already have the correct URL in their bar).
	if req.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Push-Url", dashboardPushURLFromFilters(r.basePath, filters))
	}
	renderTempl(w, req, templates.DashboardActionQueue(data))
}

// dashboardPushURLFromFilters builds the user-facing URL (rooted at basePath
// + "/") reflecting the current filter state. The basePath prefix matters
// when Stillwater is reverse-proxied under a sub-path.
func dashboardPushURLFromFilters(basePath string, f dashboardFilterParams) string {
	q := urlValuesFromFilters(f)
	base := basePath + "/"
	if len(q) == 0 {
		return base
	}
	return base + "?" + q.Encode()
}

// urlValuesFromFilters converts a filter struct into url.Values, skipping
// empty fields. Kept small and dedicated so the test can exercise it.
func urlValuesFromFilters(f dashboardFilterParams) url.Values {
	q := url.Values{}
	if f.Search != "" {
		q.Set("search", f.Search)
	}
	if f.Severity != "" {
		q.Set("severity", f.Severity)
	}
	if f.Category != "" {
		q.Set("category", f.Category)
	}
	if f.LibraryID != "" {
		q.Set("library", f.LibraryID)
	}
	if f.RuleID != "" {
		q.Set("rule", f.RuleID)
	}
	if f.Fixable != "" {
		q.Set("fixable", f.Fixable)
	}
	return q
}

// handleDashboardActivityFeed renders the recent activity sidebar fragment.
// It loads the 10 most recent metadata changes across all artists.
// GET /dashboard/activity
func (r *Router) handleDashboardActivityFeed(w http.ResponseWriter, req *http.Request) {
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	ctx := req.Context()

	var changes []artist.MetadataChangeWithArtist
	if r.historyService != nil {
		filter := artist.GlobalHistoryFilter{
			Limit: 10,
		}

		var err error
		changes, _, err = r.historyService.ListGlobal(ctx, filter)
		if err != nil {
			r.logger.Error("dashboard activity feed", "error", err)
			writeError(w, req, http.StatusInternalServerError, "failed to load activity")
			return
		}
	}
	if changes == nil {
		changes = []artist.MetadataChangeWithArtist{}
	}

	data := templates.ActivityFeedData{
		Changes:  changes,
		BasePath: r.basePath,
	}

	renderTempl(w, req, templates.DashboardActivityFeed(data))
}
