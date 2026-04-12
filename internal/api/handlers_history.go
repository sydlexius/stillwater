package api

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/web/templates"
)

// parseFilterValues extracts filter values from a multi-valued query parameter,
// stripping the "+" include prefix that the filter flyout component emits.
// Bare values (no prefix) are treated as includes. Values with a "-" exclude
// prefix are ignored for now (exclude filtering is not yet implemented).
func parseFilterValues(values []string) []string {
	var result []string
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if strings.HasPrefix(v, "-") {
			continue // exclude not yet supported
		}
		result = append(result, strings.TrimPrefix(v, "+"))
	}
	return result
}

// splitSourceFilters separates source filter values into exact matches and
// prefix patterns. Values like "provider:*" and "rule:*" are treated as prefix
// patterns (matching any source that starts with "provider:" or "rule:").
// All other values are treated as exact matches.
func splitSourceFilters(sources []string) (exact []string, prefixes []string) {
	for _, s := range sources {
		if strings.HasSuffix(s, ":*") {
			// Convert "provider:*" to prefix "provider:"
			prefixes = append(prefixes, strings.TrimSuffix(s, "*"))
		} else {
			exact = append(exact, s)
		}
	}
	return exact, prefixes
}

// sliceContains reports whether s is present in the slice.
func sliceContains(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

// isPlainDate reports whether s is a plain YYYY-MM-DD date string.
func isPlainDate(s string) bool {
	if len(s) != 10 {
		return false
	}
	for i, c := range s {
		if i == 4 || i == 7 {
			if c != '-' {
				return false
			}
		} else if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// parseTimeParam parses a date/time value from a query parameter.
// Accepts both RFC 3339 timestamps (e.g. "2024-01-15T00:00:00Z") and plain
// YYYY-MM-DD date strings. Plain "from" dates are interpreted as UTC midnight.
// Plain "to" dates are interpreted as end-of-day UTC (23:59:59.999999999) so
// that the full day is included in range queries.
// Returns the zero value if the parameter is empty or unparsable.
func parseTimeParam(req *http.Request, name string) time.Time {
	raw := req.URL.Query().Get(name)
	if raw == "" {
		return time.Time{}
	}
	// Try RFC 3339 first (full timestamp).
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t
	}
	// Fall back to plain date (YYYY-MM-DD).
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		t = t.UTC()
		// For the "to" bound, advance to end-of-day so the full day is included.
		if name == "to" && isPlainDate(raw) {
			t = t.Add(24*time.Hour - time.Nanosecond)
		}
		return t
	}
	return time.Time{}
}

// buildGlobalFilter constructs a GlobalHistoryFilter from query parameters.
// Shared between the API handler and the page/content handlers.
func buildGlobalFilter(req *http.Request, limit int) artist.GlobalHistoryFilter {
	q := req.URL.Query()
	sources := parseFilterValues(q["source"])
	exactSources, sourcePrefixes := splitSourceFilters(sources)

	return artist.GlobalHistoryFilter{
		ArtistID:       q.Get("artist_id"),
		Fields:         parseFilterValues(q["field"]),
		Sources:        exactSources,
		SourcePrefixes: sourcePrefixes,
		From:           parseTimeParam(req, "from"),
		To:             parseTimeParam(req, "to"),
		Limit:          limit,
		Offset:         intQuery(req, "offset", 0),
	}
}

// buildGlobalFilterFromURL constructs a GlobalHistoryFilter from a full URL
// string (typically from HX-Current-URL). This preserves the active filters
// when rendering revert fragments so the "showing X of Y" counter stays
// consistent with the current feed view.
func buildGlobalFilterFromURL(rawURL string) artist.GlobalHistoryFilter {
	u, err := url.Parse(rawURL)
	if err != nil {
		return artist.GlobalHistoryFilter{}
	}
	q := u.Query()
	sources := parseFilterValues(q["source"])
	exactSources, sourcePrefixes := splitSourceFilters(sources)

	var from, to time.Time
	if raw := q.Get("from"); raw != "" {
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			from = t
		} else if t, err := time.Parse("2006-01-02", raw); err == nil {
			from = t.UTC()
		}
	}
	if raw := q.Get("to"); raw != "" {
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			to = t
		} else if t, err := time.Parse("2006-01-02", raw); err == nil {
			t = t.UTC()
			// For the "to" bound, advance to end-of-day so the full day is included.
			if isPlainDate(raw) {
				t = t.Add(24*time.Hour - time.Nanosecond)
			}
			to = t
		}
	}

	offset, _ := strconv.Atoi(q.Get("offset"))
	if offset < 0 {
		offset = 0
	}

	return artist.GlobalHistoryFilter{
		ArtistID:       q.Get("artist_id"),
		Fields:         parseFilterValues(q["field"]),
		Sources:        exactSources,
		SourcePrefixes: sourcePrefixes,
		From:           from,
		To:             to,
		Offset:         offset,
	}
}

// handleListArtistHistory returns paginated metadata change records for an artist.
// GET /api/v1/artists/{id}/history
func (r *Router) handleListArtistHistory(w http.ResponseWriter, req *http.Request) {
	if r.historyService == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "history service is not available"})
		return
	}

	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	// Verify the artist exists before returning history.
	if _, err := r.artistService.GetByID(req.Context(), artistID); err != nil {
		if errors.Is(err, artist.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "artist not found"})
			return
		}
		r.logger.Error("failed to verify artist for history", "artist_id", artistID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	userID := middleware.UserIDFromContext(req.Context())
	limit := r.getUserPageSize(req.Context(), userID, intQuery(req, "limit", 0))
	offset := intQuery(req, "offset", 0)
	if offset < 0 {
		offset = 0
	}

	changes, total, err := r.historyService.List(req.Context(), artistID, limit, offset)
	if err != nil {
		r.logger.Error("listing artist history", "artist_id", artistID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// Return an empty array instead of null when there are no changes.
	if changes == nil {
		changes = []artist.MetadataChange{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"changes": changes,
		"total":   total,
		"limit":   limit,
		"offset":  offset,
	})
}

// handleArtistHistoryTab renders the history tab HTML fragment for HTMX.
// GET /artists/{id}/history/tab
func (r *Router) handleArtistHistoryTab(w http.ResponseWriter, req *http.Request) {
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	if r.historyService == nil {
		r.logger.Warn("history tab requested but history service is not configured", "artist_id", artistID)
		// History service not wired; render empty state.
		renderTempl(w, req, templates.ArtistHistoryTab(templates.HistoryTabData{
			ArtistID: artistID,
		}))
		return
	}

	// Verify the artist exists before loading history.
	if _, err := r.artistService.GetByID(req.Context(), artistID); err != nil {
		if errors.Is(err, artist.ErrNotFound) {
			http.Error(w, "artist not found", http.StatusNotFound)
			return
		}
		r.logger.Error("failed to verify artist for history tab", "artist_id", artistID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	userID := middleware.UserIDFromContext(req.Context())
	limit := r.getUserPageSize(req.Context(), userID, intQuery(req, "limit", 0))
	offset := intQuery(req, "offset", 0)

	changes, total, err := r.historyService.List(req.Context(), artistID, limit, offset)
	if err != nil {
		r.logger.Error("loading history tab", "artist_id", artistID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	data := templates.HistoryTabData{
		ArtistID: artistID,
		Changes:  changes,
		Total:    total,
		Limit:    limit,
		Offset:   offset,
	}

	// Load-more requests use a different template to append rows.
	if offset > 0 {
		renderTempl(w, req, templates.ArtistHistoryMoreRows(data))
		return
	}

	renderTempl(w, req, templates.ArtistHistoryTab(data))
}

// handleRevertHistory reverts a single metadata change by restoring the old value.
// POST /api/v1/history/{id}/revert
func (r *Router) handleRevertHistory(w http.ResponseWriter, req *http.Request) {
	if r.historyService == nil {
		writeError(w, req, http.StatusServiceUnavailable, "history service is not available")
		return
	}

	changeID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	change, err := r.historyService.GetByID(req.Context(), changeID)
	if err != nil {
		if errors.Is(err, artist.ErrChangeNotFound) {
			writeError(w, req, http.StatusNotFound, "change not found")
			return
		}
		r.logger.Error("fetching change for revert", "change_id", changeID, "error", err)
		writeError(w, req, http.StatusInternalServerError, "internal error")
		return
	}

	if !artist.IsTrackableField(change.Field) {
		writeError(w, req, http.StatusBadRequest, "field is not revertible")
		return
	}

	if change.Source == "revert" {
		writeError(w, req, http.StatusBadRequest, "revert entries cannot be reverted")
		return
	}

	// Inject "revert" as the history source so the auto-recorded change
	// is distinguishable from manual edits.
	ctx := artist.ContextWithSource(req.Context(), "revert")

	// ClearField/UpdateField currently succeed silently when the artist ID
	// does not exist (UPDATE affects zero rows). The ErrNotFound guards below
	// are defensive: they will activate if the repo layer is updated to check
	// RowsAffected and return ErrNotFound for missing artists.
	if change.OldValue == "" {
		if err := r.artistService.ClearField(ctx, change.ArtistID, change.Field); err != nil {
			if errors.Is(err, artist.ErrNotFound) {
				writeError(w, req, http.StatusNotFound, "artist not found")
				return
			}
			r.logger.Error("reverting change (clear)", "change_id", changeID, "error", err)
			writeError(w, req, http.StatusInternalServerError, "revert failed")
			return
		}
	} else {
		if err := r.artistService.UpdateField(ctx, change.ArtistID, change.Field, change.OldValue); err != nil {
			if errors.Is(err, artist.ErrNotFound) {
				writeError(w, req, http.StatusNotFound, "artist not found")
				return
			}
			r.logger.Error("reverting change (update)", "change_id", changeID, "error", err)
			writeError(w, req, http.StatusInternalServerError, "revert failed")
			return
		}
	}

	// For HTMX requests (undo button click), return an HTML fragment showing
	// the new history entry that was created by the revert. For API callers,
	// return JSON.
	if isHTMXRequest(req) {
		// Determine whether the undo was triggered from the activity page or
		// the artist history tab so we can render the correct fragment type.
		fromActivity := strings.Contains(req.Header.Get("HX-Current-URL"), "/activity")

		if fromActivity {
			// Activity feed needs MetadataChangeWithArtist (includes artist name).
			// Fetch the most recent revert for this field to get the new entry.
			revertFilter := artist.GlobalHistoryFilter{
				ArtistID: change.ArtistID,
				Fields:   []string{change.Field},
				Sources:  []string{"revert"},
				Limit:    1,
			}
			globalChanges, _, err := r.historyService.ListGlobal(req.Context(), revertFilter)
			if err != nil {
				r.logger.Error("fetching revert confirmation for activity", "change_id", changeID, "error", err)
			}
			if err == nil && len(globalChanges) > 0 {
				// Rebuild the active filter from query params carried in
				// HX-Current-URL so the "showing X of Y" counter stays
				// accurate relative to the current feed view.
				activeFilter := buildGlobalFilterFromURL(req.Header.Get("HX-Current-URL"))

				// If the active feed filter restricts sources and does not
				// include "revert", the new revert row is outside the current
				// view. Skip the fragment injection so we don't insert a row
				// that would not normally appear in the feed.
				// Also suppress when SourcePrefixes is non-empty: "revert" does
				// not match any provider:/rule: prefix pattern.
				sourceFiltered := (len(activeFilter.Sources) > 0 && !sliceContains(activeFilter.Sources, "revert")) ||
					len(activeFilter.SourcePrefixes) > 0

				// Guard against active date-range bounds: if the new revert row
				// falls outside the current feed's from/to window, skip fragment
				// injection so we don't insert a row that would not appear in the feed.
				createdAt := globalChanges[0].CreatedAt
				dateFiltered := (!activeFilter.From.IsZero() && createdAt.Before(activeFilter.From)) ||
					(!activeFilter.To.IsZero() && createdAt.After(activeFilter.To))

				if sourceFiltered || dateFiltered {
					// Fall through to the plain-text fallback below.
				} else {
					userID := middleware.UserIDFromContext(req.Context())
					limit := r.getUserPageSize(req.Context(), userID, 0)
					activeFilter.Limit = limit
					_, total, _ := r.historyService.ListGlobal(req.Context(), activeFilter)
					showing := activeFilter.Offset + limit
					if showing > total {
						showing = total
					}
					renderTempl(w, req, templates.ActivityRevertFragment(changeID, globalChanges[0], r.basePath, showing, total))
					return
				}
			}
		} else {
			// Artist history tab needs MetadataChange (no artist name needed).
			userID := middleware.UserIDFromContext(req.Context())
			limit := r.getUserPageSize(req.Context(), userID, 0)
			changes, total, err := r.historyService.List(req.Context(), change.ArtistID, limit, 0)
			if err != nil {
				r.logger.Error("fetching revert confirmation", "change_id", changeID, "error", err)
			}
			var revertChange *artist.MetadataChange
			if err == nil {
				for i := range changes {
					if changes[i].Field == change.Field && changes[i].Source == "revert" {
						revertChange = &changes[i]
						break
					}
				}
			}
			if revertChange != nil {
				showing := len(changes)
				if showing > total {
					showing = total
				}
				renderTempl(w, req, templates.HistoryRevertFragment(changeID, *revertChange, showing, total))
				return
			}
		}

		// Fallback: the revert succeeded but we could not locate the new record.
		r.logger.Warn("revert record not found in recent history, using fallback confirmation",
			"change_id", changeID, "field", change.Field, "artist_id", change.ArtistID)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<div class="border-l-2 border-amber-400 dark:border-amber-500 pl-4 py-2"><p class="text-sm text-amber-600 dark:text-amber-400">Change reverted. Refresh the page to see the updated entry.</p></div>`))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"reverted":  true,
		"change_id": changeID,
	})
}

// handleListGlobalHistory returns paginated metadata changes across all artists.
// GET /api/v1/history
func (r *Router) handleListGlobalHistory(w http.ResponseWriter, req *http.Request) {
	if r.historyService == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "history service is not available"})
		return
	}

	userID := middleware.UserIDFromContext(req.Context())
	filter := buildGlobalFilter(req, r.getUserPageSize(req.Context(), userID, intQuery(req, "limit", 0)))
	if filter.Offset < 0 {
		filter.Offset = 0
	}

	changes, total, err := r.historyService.ListGlobal(req.Context(), filter)
	if err != nil {
		r.logger.Error("listing global history", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	if changes == nil {
		changes = []artist.MetadataChangeWithArtist{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"changes": changes,
		"total":   total,
		"limit":   filter.Limit,
		"offset":  filter.Offset,
	})
}

// rebuildSourceFilters reconstructs the combined source list from a
// GlobalHistoryFilter by merging exact sources with wildcard-suffixed prefixes.
// This is the inverse of splitSourceFilters and is used when passing filter
// state back to templates.
func rebuildSourceFilters(filter artist.GlobalHistoryFilter) []string {
	all := make([]string, 0, len(filter.Sources)+len(filter.SourcePrefixes))
	all = append(all, filter.Sources...)
	for _, p := range filter.SourcePrefixes {
		all = append(all, p+"*")
	}
	return all
}

// handleActivityPage renders the global activity page.
// GET /activity
func (r *Router) handleActivityPage(w http.ResponseWriter, req *http.Request) {
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		r.renderLoginPage(w, req)
		return
	}

	filter := buildGlobalFilter(req, r.getUserPageSize(req.Context(), userID, 0))
	filter.Offset = 0 // activity page always starts at offset 0

	var changes []artist.MetadataChangeWithArtist
	var total int
	if r.historyService != nil {
		var err error
		changes, total, err = r.historyService.ListGlobal(req.Context(), filter)
		if err != nil {
			r.logger.Error("loading activity page", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	if changes == nil {
		changes = []artist.MetadataChangeWithArtist{}
	}

	data := templates.ActivityPageData{
		Changes:        changes,
		Total:          total,
		Limit:          filter.Limit,
		Offset:         filter.Offset,
		BasePath:       r.basePath,
		FilterArtistID: filter.ArtistID,
		FilterFields:   filter.Fields,
		FilterSources:  rebuildSourceFilters(filter),
		FilterFrom:     filter.From,
		FilterTo:       filter.To,
	}
	renderTempl(w, req, templates.ActivityPage(r.assetsFor(req), data))
}

// handleActivityContent renders the activity list fragment for HTMX.
// GET /activity/content
func (r *Router) handleActivityContent(w http.ResponseWriter, req *http.Request) {
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	if r.historyService == nil {
		r.logger.Warn("activity content requested but history service is not configured")
		renderTempl(w, req, templates.ActivityContent(templates.ActivityPageData{Limit: r.getUserPageSize(req.Context(), userID, 0), BasePath: r.basePath}))
		return
	}

	filter := buildGlobalFilter(req, r.getUserPageSize(req.Context(), userID, intQuery(req, "limit", 0)))
	if filter.Offset < 0 {
		filter.Offset = 0
	}

	changes, total, err := r.historyService.ListGlobal(req.Context(), filter)
	if err != nil {
		r.logger.Error("loading activity content", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if changes == nil {
		changes = []artist.MetadataChangeWithArtist{}
	}

	data := templates.ActivityPageData{
		Changes:        changes,
		Total:          total,
		Limit:          filter.Limit,
		Offset:         filter.Offset,
		BasePath:       r.basePath,
		FilterArtistID: filter.ArtistID,
		FilterFields:   filter.Fields,
		FilterSources:  rebuildSourceFilters(filter),
		FilterFrom:     filter.From,
		FilterTo:       filter.To,
	}

	// Load-more requests (offset > 0) return just the new rows + updated
	// load-more button, appending to the existing list.
	if filter.Offset > 0 {
		renderTempl(w, req, templates.ActivityMoreRows(data))
		return
	}
	renderTempl(w, req, templates.ActivityContent(data))
}
