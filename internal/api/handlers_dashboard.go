package api

import (
	"net/http"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/rule"
	"github.com/sydlexius/stillwater/web/templates"
)

// handleDashboardActionQueue renders the action queue fragment for the dashboard.
// It loads active violations sorted by severity, category counts for filter chips,
// and compact health metrics for the header.
// GET /dashboard/actions
func (r *Router) handleDashboardActionQueue(w http.ResponseWriter, req *http.Request) {
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	ctx := req.Context()

	category := req.URL.Query().Get("category")
	limit := intQuery(req, "limit", 25)
	offset := intQuery(req, "offset", 0)

	// Cap limit to prevent abuse.
	if limit > 100 {
		limit = 100
	}

	params := rule.ViolationListParams{
		Status:   "active",
		Category: category,
		Sort:     "artist_name",
		Order:    "asc",
		Limit:    limit,
		Offset:   offset,
	}

	violations, total, err := r.ruleService.ListViolationsFilteredPaged(ctx, params)
	if err != nil {
		r.logger.Error("dashboard action queue", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to load violations")
		return
	}

	categoryCounts, err := r.ruleService.CountActiveViolationsByCategory(ctx)
	if err != nil {
		r.logger.Error("dashboard category counts", "error", err)
		// Non-fatal: render with empty counts so the page still loads.
		categoryCounts = map[string]int{}
	}

	// Fetch health summary for the compact metrics in the header.
	// Uses the same GetHealthStats call as handleReportHealth.
	var healthScore float64
	var totalArtists, compliantArtists int
	stats, err := r.artistService.GetHealthStats(ctx, "")
	if err == nil {
		healthScore = stats.Score
		totalArtists = stats.TotalArtists
		compliantArtists = stats.CompliantArtists
	} else {
		r.logger.Warn("dashboard health stats", "error", err)
	}

	data := templates.ActionQueueData{
		Violations:       violations,
		Total:            total,
		Limit:            limit,
		Offset:           offset,
		CategoryFilter:   category,
		CategoryCounts:   categoryCounts,
		HealthScore:      healthScore,
		TotalArtists:     totalArtists,
		CompliantArtists: compliantArtists,
		BasePath:         r.basePath,
	}

	// Load-more requests (offset > 0) return just the new rows + updated
	// load-more button, appending to the existing list.
	if offset > 0 {
		renderTempl(w, req, templates.DashboardActionMoreRows(data))
		return
	}
	renderTempl(w, req, templates.DashboardActionQueue(data))
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
