package api

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/sydlexius/stillwater/internal/api/filterparams"
	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/internal/rule"
	"github.com/sydlexius/stillwater/web/templates"
)

// dashboardFilterParams holds the parsed filter state from a dashboard request.
// Each filter dimension is a tri-state include/exclude set (rule.TriFilter): the
// flyout repeats the same query key once per value, prefixing "+" for include
// and "-" for exclude. A bare value with no prefix is treated as include for
// backward compatibility with older links and the legacy single-select form.
type dashboardFilterParams struct {
	Search    string
	Severity  rule.TriFilter
	Category  rule.TriFilter
	LibraryID rule.TriFilter
	RuleID    rule.TriFilter
	Fixable   rule.TriFilter // values: "yes", "no"
}

// parseTriFilter reads a single multi-valued query parameter into a tri-state
// include/exclude set. Each value is classified by its prefix:
//
//   - "+value" -> include
//   - "-value" -> exclude
//   - "value"  (bare, no prefix) -> include (backward compatible)
//
// Empty / whitespace-only values are skipped, as is a bare "+" or "-" with no
// value after the prefix (which would otherwise inject an empty-string filter
// value). The returned slices are nil when no value falls into that bucket, so
// an absent key yields a zero-value (empty) TriFilter that adds no SQL clause.
// The result is normalized (dedupe, exclude-wins, whitelist) before return.
func parseTriFilter(values []string) rule.TriFilter {
	var f rule.TriFilter
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		switch v[0] {
		case '+':
			if rest := v[1:]; rest != "" {
				f.Include = append(f.Include, rest)
			}
		case '-':
			if rest := v[1:]; rest != "" {
				f.Exclude = append(f.Exclude, rest)
			}
		default:
			// Bare value: include (back-compat with old single-select links).
			f.Include = append(f.Include, v)
		}
	}
	return f.Normalized()
}

// parseDashboardFilters reads search and filter parameters from the request
// query string. Keys: search, severity, category, library_id, rule, fixable.
//
// `library_id` is the canonical key (matches the artists and compliance
// pages); `library` is accepted as a parse-time alias so existing bookmarks
// from the old dashboard URL form keep working. Both keys' values are merged
// into the same tri-state filter.
func parseDashboardFilters(req *http.Request) dashboardFilterParams {
	q := req.URL.Query()
	// Merge the canonical `library_id` key with the legacy `library` alias so
	// old bookmarks keep working under the tri-state contract.
	libraryValues := append(append([]string{}, q["library_id"]...), q["library"]...)
	return dashboardFilterParams{
		Search:    strings.TrimSpace(q.Get("search")),
		Severity:  parseTriFilter(q["severity"]),
		Category:  parseTriFilter(q["category"]),
		LibraryID: parseTriFilter(libraryValues),
		RuleID:    parseTriFilter(q["rule"]),
		Fixable:   parseTriFilter(q["fixable"]),
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

	// A Prev/Next page-nav targets #action-queue-entries (innerHTML swap) and
	// must return the page-replace fragment for ANY offset, INCLUDING 0 (the
	// Prev->page-1 case). Detect it by HX-Target rather than offset>0: an
	// offset=0 page-nav answered with the FULL DashboardActionQueue fragment
	// (which carries its own #action-queue-entries wrapper + inline footer)
	// would nest a second pagination footer (the duplicate-footer bug, #1790).
	isPageNav := req.Header.Get("HX-Target") == "action-queue-entries"

	// Page-nav (any offset) and direct offset>0 requests need only violations
	// + pagination data -- skip the summary queries the header/flyout need.
	if offset > 0 || isPageNav {
		data := templates.ActionQueueData{
			Violations: violations,
			Total:      total,
			Limit:      limit,
			Offset:     offset,
			BasePath:   r.basePath,
			// Preserve filter state so dashboardLoadMoreURL can build the next page URL.
			Search:    filters.Search,
			Severity:  firstInclude(filters.Severity),
			Category:  firstInclude(filters.Category),
			LibraryID: firstInclude(filters.LibraryID),
			RuleID:    firstInclude(filters.RuleID),
			Fixable:   firstInclude(filters.Fixable),

			SeverityFilter: filters.Severity,
			CategoryFilter: filters.Category,
			LibraryFilter:  filters.LibraryID,
			RuleFilter:     filters.RuleID,
			FixableFilter:  filters.Fixable,
		}
		// The queue pages in place (M55 #1790): a Prev/Next request swaps a
		// fresh page of rows into #action-queue-entries (innerHTML) and
		// re-renders the paging footer out-of-band. The page size is the same
		// getUserPageSize limit the initial load used (carried in data.Limit),
		// so no page-size constant is introduced here.
		renderTempl(w, req, templates.DashboardActionQueuePage(data))
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
		Severity:  firstInclude(filters.Severity),
		Category:  firstInclude(filters.Category),
		LibraryID: firstInclude(filters.LibraryID),
		RuleID:    firstInclude(filters.RuleID),
		Fixable:   firstInclude(filters.Fixable),

		SeverityFilter: filters.Severity,
		CategoryFilter: filters.Category,
		LibraryFilter:  filters.LibraryID,
		RuleFilter:     filters.RuleID,
		FixableFilter:  filters.Fixable,

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
		// The dashboard is the app index, so the address bar tracks the app
		// root ("$basePath/") plus the active filter params.
		filterparams.WriteHXPushURL(w, r.basePath, urlValuesFromFilters(filters))
	}

	// The dashboard page (IndexPage) owns the header strip + sticky toolbar
	// (search, filter trigger, run-rules) AND the persistent page-level filter
	// flyout, so this fragment renders only the queue rows + paging footer.
	renderTempl(w, req, templates.DashboardActionQueue(data))
}

// buildDashboardFlyoutData assembles the filter-flyout state for an initial
// page render: the parsed tri-state filter selection plus the per-dimension
// facet counts and the library list the flyout needs to render its pills and
// badge counts. It is the page-render counterpart to the facet-count block in
// handleDashboardActionQueue, factored out so the next/ dashboard page can
// render the flyout in a PERSISTENT page-level container (not inside the
// HTMX-swapped #action-queue fragment, which would not exist until the first
// queue load and would be destroyed by the queue's error state).
//
// It deliberately does NOT load violations or health stats (the page header and
// the queue fragment own those); only the fields DashboardFilterFlyout reads are
// populated. Facet-count queries are best-effort: a failed dimension renders an
// empty map / list so the flyout still opens.
func (r *Router) buildDashboardFlyoutData(req *http.Request) templates.ActionQueueData {
	ctx := req.Context()
	filters := parseDashboardFilters(req)

	// Minimal routers (some integration tests) have no rule service; render the
	// flyout with empty facet counts rather than panicking. The library list
	// below has its own nil guard already.
	if r.ruleService == nil {
		r.logger.Warn("dashboard flyout counts unavailable", "error", "rule service not configured")
		return templates.ActionQueueData{
			BasePath: r.basePath,

			Search:    filters.Search,
			Severity:  firstInclude(filters.Severity),
			Category:  firstInclude(filters.Category),
			LibraryID: firstInclude(filters.LibraryID),
			RuleID:    firstInclude(filters.RuleID),
			Fixable:   firstInclude(filters.Fixable),

			SeverityFilter: filters.Severity,
			CategoryFilter: filters.Category,
			LibraryFilter:  filters.LibraryID,
			RuleFilter:     filters.RuleID,
			FixableFilter:  filters.Fixable,

			SeverityCounts: map[string]int{},
			CategoryCounts: map[string]int{},
			LibraryCounts:  map[string]int{},
			RuleCounts:     []rule.RuleViolationCount{},
		}
	}

	// Mirror the queue's count params (active status + current filter scope) so
	// the page-load facet counts match what the first queue load computes. Limit
	// and offset are irrelevant to the count queries.
	params := rule.ViolationListParams{
		Status:    "active",
		Search:    filters.Search,
		Severity:  filters.Severity,
		Category:  filters.Category,
		LibraryID: filters.LibraryID,
		RuleID:    filters.RuleID,
		Fixable:   filters.Fixable,
	}

	categoryCounts, err := r.ruleService.CountActiveViolationsByCategory(ctx, params)
	if err != nil {
		r.logger.Warn("dashboard flyout category counts", "error", err)
		categoryCounts = map[string]int{}
	}
	severityCounts, err := r.ruleService.CountActiveViolationsBySeverity(ctx, params)
	if err != nil {
		r.logger.Warn("dashboard flyout severity counts", "error", err)
		severityCounts = map[string]int{}
	}
	libraryCounts, err := r.ruleService.CountActiveViolationsByLibrary(ctx, params)
	if err != nil {
		r.logger.Warn("dashboard flyout library counts", "error", err)
		libraryCounts = map[string]int{}
	}
	ruleCounts, err := r.ruleService.CountActiveViolationsByRule(ctx, params)
	if err != nil {
		r.logger.Warn("dashboard flyout rule counts", "error", err)
		ruleCounts = []rule.RuleViolationCount{}
	}
	fixableYes, fixableNo, err := r.ruleService.CountActiveViolationsByFixable(ctx, params)
	if err != nil {
		r.logger.Warn("dashboard flyout fixable counts", "error", err)
		fixableYes, fixableNo = 0, 0
	}

	var libraries []library.Library
	if r.libraryService != nil {
		libs, err := r.libraryService.List(ctx)
		if err != nil {
			r.logger.Warn("dashboard flyout libraries", "error", err)
		} else {
			libraries = libs
		}
	}

	return templates.ActionQueueData{
		BasePath: r.basePath,

		Search:    filters.Search,
		Severity:  firstInclude(filters.Severity),
		Category:  firstInclude(filters.Category),
		LibraryID: firstInclude(filters.LibraryID),
		RuleID:    firstInclude(filters.RuleID),
		Fixable:   firstInclude(filters.Fixable),

		SeverityFilter: filters.Severity,
		CategoryFilter: filters.Category,
		LibraryFilter:  filters.LibraryID,
		RuleFilter:     filters.RuleID,
		FixableFilter:  filters.Fixable,

		SeverityCounts: severityCounts,
		CategoryCounts: categoryCounts,
		LibraryCounts:  libraryCounts,
		RuleCounts:     ruleCounts,
		FixableYes:     fixableYes,
		FixableNo:      fixableNo,
		Libraries:      libraries,
	}
}

// firstInclude returns the first included value of a tri-state filter, or "" if
// none. It is the lossy bridge that populates the dashboard template's scalar
// filter fields (which back the current single-select chip UI) from the full
// tri-state state.
func firstInclude(f rule.TriFilter) string {
	if len(f.Include) > 0 {
		return f.Include[0]
	}
	return ""
}

// addTriValues appends the tri-state values for one dimension to q under the
// given key, using the flyout's URL contract: "+value" for include and
// "-value" for exclude. It delegates to rule.TriFilter.AppendURLValues so the
// wire form has a single source of truth shared with the template helpers.
func addTriValues(q url.Values, key string, f rule.TriFilter) {
	f.AppendURLValues(q, key)
}

// urlValuesFromFilters converts a filter struct into url.Values, skipping
// empty fields. Each tri-state dimension is emitted as repeated key=value pairs
// carrying the "+"/"-" state prefix (the flyout URL contract). Always writes the
// canonical `library_id` key so the URL reflects the post-rename form; the
// legacy `library` alias is only honored on parse (parseDashboardFilters). Kept
// small and dedicated so the test can exercise it.
func urlValuesFromFilters(f dashboardFilterParams) url.Values {
	q := url.Values{}
	if f.Search != "" {
		q.Set("search", f.Search)
	}
	addTriValues(q, "severity", f.Severity)
	addTriValues(q, "category", f.Category)
	addTriValues(q, "library_id", f.LibraryID)
	addTriValues(q, "rule", f.RuleID)
	addTriValues(q, "fixable", f.Fixable)
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

	// The activity rail fragment's row markup matches the live SSE-appended
	// rows (so initial and live rows look identical), with an empty-on-boot
	// idle hint.
	renderTempl(w, req, templates.DashboardActivityFeed(data))
}
