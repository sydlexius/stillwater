package api

import (
	"errors"
	"net/http"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/web/templates"
)

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

	limit := intQuery(req, "limit", 50)
	offset := intQuery(req, "offset", 0)

	// Clamp limit and offset here so the response echoes the effective values
	// that were actually applied, matching the clamping in HistoryService.List.
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
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

	limit := intQuery(req, "limit", 25)
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
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "history service is not available"})
		return
	}

	changeID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	change, err := r.historyService.GetByID(req.Context(), changeID)
	if err != nil {
		if errors.Is(err, artist.ErrChangeNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "change not found"})
			return
		}
		r.logger.Error("fetching change for revert", "change_id", changeID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	if !artist.IsTrackableField(change.Field) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "field is not revertible"})
		return
	}

	// Inject "revert" as the history source so the auto-recorded change
	// is distinguishable from manual edits.
	ctx := artist.ContextWithSource(req.Context(), "revert")

	if change.OldValue == "" {
		if err := r.artistService.ClearField(ctx, change.ArtistID, change.Field); err != nil {
			r.logger.Error("reverting change (clear)", "change_id", changeID, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "revert failed"})
			return
		}
	} else {
		if err := r.artistService.UpdateField(ctx, change.ArtistID, change.Field, change.OldValue); err != nil {
			r.logger.Error("reverting change (update)", "change_id", changeID, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "revert failed"})
			return
		}
	}

	// For HTMX requests (undo button click), return an HTML fragment showing
	// the new history entry that was created by the revert. For API callers,
	// return JSON.
	if isHTMXRequest(req) {
		// Re-fetch the latest change for this artist+field to get the revert record.
		changes, _, err := r.historyService.List(req.Context(), change.ArtistID, 1, 0)
		if err != nil {
			r.logger.Warn("fetching revert confirmation", "change_id", changeID, "error", err)
		}
		if err != nil || len(changes) == 0 {
			// Fallback: the revert succeeded but we could not fetch the new record.
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`<div class="border-l-2 border-amber-400 dark:border-amber-500 pl-4 py-2"><p class="text-sm text-amber-600 dark:text-amber-400">Change reverted successfully.</p></div>`))
			return
		}
		renderTempl(w, req, templates.HistoryChangeRowFragment(changes[0]))
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

	q := req.URL.Query()
	filter := artist.GlobalHistoryFilter{
		ArtistID: q.Get("artist_id"),
		Limit:    intQuery(req, "limit", 50),
		Offset:   intQuery(req, "offset", 0),
	}
	if f := q.Get("field"); f != "" {
		filter.Fields = []string{f}
	}
	if s := q.Get("source"); s != "" {
		filter.Sources = []string{s}
	}

	// Clamp for response echo.
	if filter.Limit <= 0 {
		filter.Limit = 50
	}
	if filter.Limit > 200 {
		filter.Limit = 200
	}
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

// handleActivityPage renders the global activity page.
// GET /activity
func (r *Router) handleActivityPage(w http.ResponseWriter, req *http.Request) {
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		r.renderLoginPage(w, req)
		return
	}

	filter := artist.GlobalHistoryFilter{
		Limit:  25,
		Offset: 0,
	}

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
		Changes: changes,
		Total:   total,
		Limit:   filter.Limit,
		Offset:  filter.Offset,
	}
	renderTempl(w, req, templates.ActivityPage(r.assetsFor(req), data))
}

// handleActivityContent renders the activity list fragment for HTMX.
// GET /activity/content
func (r *Router) handleActivityContent(w http.ResponseWriter, req *http.Request) {
	if r.historyService == nil {
		renderTempl(w, req, templates.ActivityContent(templates.ActivityPageData{Limit: 25}))
		return
	}

	q := req.URL.Query()
	filter := artist.GlobalHistoryFilter{
		ArtistID: q.Get("artist_id"),
		Limit:    intQuery(req, "limit", 25),
		Offset:   intQuery(req, "offset", 0),
	}
	if f := q.Get("field"); f != "" {
		filter.Fields = []string{f}
	}
	if s := q.Get("source"); s != "" {
		filter.Sources = []string{s}
	}
	if filter.Limit <= 0 {
		filter.Limit = 25
	}
	if filter.Limit > 200 {
		filter.Limit = 200
	}
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
		FilterArtistID: filter.ArtistID,
		FilterFields:   filter.Fields,
		FilterSources:  filter.Sources,
	}
	renderTempl(w, req, templates.ActivityContent(data))
}
