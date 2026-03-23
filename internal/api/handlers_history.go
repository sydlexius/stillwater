package api

import (
	"errors"
	"net/http"

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
