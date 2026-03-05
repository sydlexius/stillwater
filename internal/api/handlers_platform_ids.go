package api

import (
	"errors"
	"net/http"

	"github.com/sydlexius/stillwater/internal/artist"
)

// handleGetPlatformIDs returns all platform artist IDs for an artist.
// GET /api/v1/artists/{id}/platform-ids
func (r *Router) handleGetPlatformIDs(w http.ResponseWriter, req *http.Request) {
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	ids, err := r.artistService.GetPlatformIDs(req.Context(), artistID)
	if err != nil {
		r.logger.Error("listing platform ids", "artist_id", artistID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if ids == nil {
		ids = []artist.PlatformID{}
	}
	writeJSON(w, http.StatusOK, ids)
}

// handleSetPlatformID stores or updates a platform artist ID mapping.
// PUT /api/v1/artists/{id}/platform-ids/{connectionId}
func (r *Router) handleSetPlatformID(w http.ResponseWriter, req *http.Request) {
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}
	connectionID, ok := RequirePathParam(w, req, "connectionId")
	if !ok {
		return
	}

	var body struct {
		PlatformArtistID string `json:"platform_artist_id"`
	}
	if !DecodeJSON(w, req, &body) {
		return
	}
	if body.PlatformArtistID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "platform_artist_id is required"})
		return
	}

	if _, err := r.artistService.GetByID(req.Context(), artistID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "artist not found"})
		return
	}
	if _, err := r.connectionService.GetByID(req.Context(), connectionID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "connection not found"})
		return
	}

	if err := r.artistService.SetPlatformID(req.Context(), artistID, connectionID, body.PlatformArtistID); err != nil {
		r.logger.Error("setting platform id", "artist_id", artistID, "connection_id", connectionID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleDeletePlatformID removes a platform artist ID mapping.
// DELETE /api/v1/artists/{id}/platform-ids/{connectionId}
func (r *Router) handleDeletePlatformID(w http.ResponseWriter, req *http.Request) {
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}
	connectionID, ok := RequirePathParam(w, req, "connectionId")
	if !ok {
		return
	}

	if err := r.artistService.DeletePlatformID(req.Context(), artistID, connectionID); err != nil {
		if errors.Is(err, artist.ErrPlatformIDNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "platform id not found"})
			return
		}
		r.logger.Error("deleting platform id", "artist_id", artistID, "connection_id", connectionID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
