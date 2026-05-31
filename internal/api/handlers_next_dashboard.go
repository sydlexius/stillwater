package api

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/web/templates/next"
)

// handleNextDashboardPage serves the next/ channel dashboard (M55 #1334).
//
// When the resolved UI channel is not "next" (the user is on stable or opted
// back via sw_ux=stable) it delegates to handleIndex so the /next/dashboard
// path never 404s and never dead-ends (decision 12 in architecture-decisions.md).
//
// When the channel is "next" it runs the same auth + onboarding checks as
// handleIndex (checking auth from context, then the onboarding.completed
// setting), fetches the health stats for the compact header strip, and renders
// next.DashboardPageNext with the forwarded initial filter query.
//
// Health-stat fetch errors are non-fatal: healthStatsError=true causes the
// template to show "---" placeholders rather than misleading zeros, matching
// the defensive pattern used by DashboardActionHeader in the stable channel.
func (r *Router) handleNextDashboardPage(w http.ResponseWriter, req *http.Request) {
	if middleware.UXChannelFromContext(req.Context()) != middleware.UXNext {
		r.handleIndex(w, req)
		return
	}

	// Auth check (populated by OptionalAuth middleware on this route).
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		r.renderLoginPage(w, req)
		return
	}

	// Onboarding check: redirect to the wizard when setup is incomplete.
	// The query is identical to the one in handleIndex; kept inline to avoid
	// adding a shared helper for what is a two-line guard.
	var completed string
	err := r.db.QueryRowContext(req.Context(),
		`SELECT value FROM settings WHERE key = 'onboarding.completed'`).Scan(&completed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		r.logger.Error("checking onboarding status for next dashboard", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if completed != "true" {
		http.Redirect(w, req, r.basePath+"/setup/wizard", http.StatusSeeOther)
		return
	}

	// Fetch health stats for the compact header strip. The empty library-ID
	// argument returns aggregate stats across all libraries, matching the
	// stable DashboardActionHeader behavior.
	stats, healthErr := r.artistService.GetHealthStats(req.Context(), "")
	healthStatsError := healthErr != nil
	if healthErr != nil {
		r.logger.Warn("health stats unavailable for next dashboard", "error", healthErr)
	}

	// Forward recognized filter query params into the initial HTMX load so a
	// bookmark like /next/dashboard?severity=warning opens with that filter
	// already applied. Unknown keys are discarded; buildDashboardInitialQuery
	// is shared with handleIndex.
	initialQuery := buildDashboardInitialQuery(req.URL.Query())

	renderTempl(w, req, next.DashboardPageNext(r.assetsFor(req), stats, healthStatsError, initialQuery))
}
