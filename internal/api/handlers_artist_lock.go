package api

import (
	"errors"
	"net/http"

	"github.com/sydlexius/stillwater/internal/artist"
)

// handleLockArtist sets the metadata lock on an artist.
// POST /api/v1/artists/{id}/lock
func (r *Router) handleLockArtist(w http.ResponseWriter, req *http.Request) {
	id, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	if err := r.artistService.Lock(req.Context(), id, "user"); err != nil {
		if errors.Is(err, artist.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "artist not found"})
			return
		}
		if errors.Is(err, artist.ErrAlreadyLocked) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "artist is already locked"})
			return
		}
		r.logger.Error("locking artist", "id", id, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	updated, err := r.artistService.GetByID(req.Context(), id)
	if err != nil {
		r.logger.Error("getting artist after lock", "id", id, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, updated)
}

// handleUnlockArtist removes the metadata lock from an artist.
// DELETE /api/v1/artists/{id}/lock
func (r *Router) handleUnlockArtist(w http.ResponseWriter, req *http.Request) {
	id, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	if err := r.artistService.Unlock(req.Context(), id); err != nil {
		if errors.Is(err, artist.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "artist not found"})
			return
		}
		if errors.Is(err, artist.ErrNotLocked) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "artist is not locked"})
			return
		}
		r.logger.Error("unlocking artist", "id", id, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	updated, err := r.artistService.GetByID(req.Context(), id)
	if err != nil {
		r.logger.Error("getting artist after unlock", "id", id, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, updated)
}

// handleListLockedArtists returns a paginated list of locked artists.
// GET /api/v1/artists/locked
func (r *Router) handleListLockedArtists(w http.ResponseWriter, req *http.Request) {
	params := artist.ListParams{
		Page:     intQuery(req, "page", 1),
		PageSize: intQuery(req, "page_size", 50),
		Sort:     req.URL.Query().Get("sort"),
		Order:    req.URL.Query().Get("order"),
		Filter:   "locked",
	}
	params.Validate()

	artists, total, err := r.artistService.List(req.Context(), params)
	if err != nil {
		r.logger.Error("listing locked artists", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"artists":   artists,
		"total":     total,
		"page":      params.Page,
		"page_size": params.PageSize,
	})
}
