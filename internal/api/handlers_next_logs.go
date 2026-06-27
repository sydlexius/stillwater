package api

import (
	"net/http"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/logging"
	"github.com/sydlexius/stillwater/web/templates/next"
)

// handleNextLogsPage serves the next/ channel live log viewer (M55 #1338, PR 5B).
//
// In stable mode (SW_UX=stable) the UX middleware 404s any /next/* request
// before this handler runs (decision 12 in architecture-decisions.md). The
// in-handler channel guard below is therefore only reachable when the lane IS
// enabled (next/dual mode) and the resolved channel is not "next" -- triggered
// by an explicit X-Stillwater-UX: stable header. In that edge case it returns
// 404 (decision 12: all handleNext* handlers return 404 on an explicit /next/
// path with the stable opt-out; the path does not serve stable content).
//
// Logs are administrator-only, matching the API stream it consumes
// (GET /api/v1/logs/stream is wrapped in middleware.RequireAdmin in router.go).
// The page route uses wrapOptionalAuth (so an unauthenticated browser visitor
// gets the login page, not a 401 JSON body), then this handler applies the
// admin gate in-handler: unauthenticated -> login page, authenticated
// non-administrator -> 403. This mirrors the requireForeignAdmin pattern.
//
// The page itself renders only the chrome (toolbar, empty viewer, throttle
// banner, filter flyout, keyboard tips). The log lines -- both the ring-buffer
// backfill and the live tail -- arrive over the SSE stream, which self-backfills
// on connect (emitLogBackfill in handlers_logs.go), so the page does not also
// fetch /api/v1/logs separately. Initial filter state is parsed from the URL so
// a deep-link such as /next/logs?level=error&artist_id=1234 opens pre-filtered.
func (r *Router) handleNextLogsPage(w http.ResponseWriter, req *http.Request) {
	if !checkNextChannel(w, req) {
		return
	}

	// Auth check (populated by OptionalAuth middleware on this route): render
	// the login page for an unauthenticated browser visitor.
	if !r.requireAuth(w, req) {
		return
	}
	// Admin-only: the live log feed can carry sensitive operational detail, so
	// it matches the administrator gate on the underlying API stream.
	if middleware.RoleFromContext(req.Context()) != "administrator" {
		http.Error(w, "Forbidden: administrator role required", http.StatusForbidden)
		return
	}

	// Parse the initial filter state from the query string. Only level/component
	// (scope) and search (q) are wired to the server-side stream filter; the
	// artist_id and rule deep-links are applied as client-side attribute filters
	// (the stream filter predicate covers only level/scope/search). An invalid
	// level is dropped rather than rejected so a stale bookmark still opens the
	// page (the client simply starts unfiltered on that axis).
	q := req.URL.Query()
	level := q.Get("level")
	if level != "" && !logging.ValidLevel(level) {
		level = ""
	}
	filter := next.LogsFilterState{
		Level:     level,
		Component: q.Get("component"),
		Search:    q.Get("q"),
		ArtistID:  q.Get("artist_id"),
		Rule:      q.Get("rule"),
	}

	// Distinct components currently in the ring buffer become the Component
	// filter pills (#1338 W1-B). Computed directly from the buffer here (no HTTP
	// round-trip); the /api/v1/logs/components endpoint exposes the same set for
	// client refresh. A nil log manager / buffer just yields no component pills.
	var components []string
	if r.logManager != nil {
		if rb := r.logManager.RingBuffer(); rb != nil {
			components = rb.Components()
		}
	}

	renderTempl(w, req, next.LogsPage(r.assetsFor(req), filter, components))
}
