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

	// Propagate the new lock state to every connected platform immediately
	// so Emby/Jellyfin show the pin without requiring a manual push.
	r.publisher.PushLocks(req.Context(), updated)

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

	r.publisher.PushLocks(req.Context(), updated)

	writeJSON(w, http.StatusOK, updated)
}

// handleLockArtistField marks a single metadata field as locked on an artist.
// POST /api/v1/artists/{id}/field-locks/{field}
// The field name is case-insensitive and normalized to lowercase before
// persistence.
func (r *Router) handleLockArtistField(w http.ResponseWriter, req *http.Request) {
	id, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}
	field, ok := RequirePathParam(w, req, "field")
	if !ok {
		return
	}

	if err := r.artistService.AddLockedField(req.Context(), id, field); err != nil {
		if errors.Is(err, artist.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "artist not found"})
			return
		}
		r.logger.Error("locking artist field", "id", id, "field", field, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	updated, err := r.artistService.GetByID(req.Context(), id)
	if err != nil {
		r.logger.Error("getting artist after field lock", "id", id, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	r.publisher.PushLocks(req.Context(), updated)
	writeJSON(w, http.StatusOK, updated)
}

// handleUnlockArtistField removes a single field lock from an artist.
// DELETE /api/v1/artists/{id}/field-locks/{field}
func (r *Router) handleUnlockArtistField(w http.ResponseWriter, req *http.Request) {
	id, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}
	field, ok := RequirePathParam(w, req, "field")
	if !ok {
		return
	}

	if err := r.artistService.RemoveLockedField(req.Context(), id, field); err != nil {
		if errors.Is(err, artist.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "artist not found"})
			return
		}
		r.logger.Error("unlocking artist field", "id", id, "field", field, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	updated, err := r.artistService.GetByID(req.Context(), id)
	if err != nil {
		r.logger.Error("getting artist after field unlock", "id", id, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	// Parity with handleLockArtistField: push the new per-field lock state
	// to connected platforms immediately so the pin disappears in Emby/
	// Jellyfin without waiting for a manual sync.
	r.publisher.PushLocks(req.Context(), updated)
	writeJSON(w, http.StatusOK, updated)
}

// handleLockArtistImage sets the lock flag on a single artist image slot.
// POST /api/v1/artists/{id}/image-locks/{imageId}
func (r *Router) handleLockArtistImage(w http.ResponseWriter, req *http.Request) {
	r.setImageLock(w, req, true)
}

// handleUnlockArtistImage clears the lock flag on a single artist image slot.
// DELETE /api/v1/artists/{id}/image-locks/{imageId}
func (r *Router) handleUnlockArtistImage(w http.ResponseWriter, req *http.Request) {
	r.setImageLock(w, req, false)
}

// setImageLock is the shared implementation for the image lock toggle
// handlers. It verifies that the target image belongs to the path artist
// before mutating so that cross-artist tampering through the imageId path
// parameter is rejected.
func (r *Router) setImageLock(w http.ResponseWriter, req *http.Request, locked bool) {
	id, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}
	imageID, ok := RequirePathParam(w, req, "imageId")
	if !ok {
		return
	}

	// Verify the image belongs to the artist in the path. We look up all
	// images for the artist and check for the requested ID; a missing id
	// returns 404 rather than 500 so clients see a clean error.
	images, err := r.artistService.GetImagesForArtist(req.Context(), id)
	if err != nil {
		if errors.Is(err, artist.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "artist not found"})
			return
		}
		r.logger.Error("listing images for lock toggle", "id", id, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	found := false
	for _, img := range images {
		if img.ID == imageID {
			found = true
			break
		}
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "image not found for artist"})
		return
	}

	if err := r.artistService.SetImageLock(req.Context(), imageID, locked); err != nil {
		r.logger.Error("setting image lock", "image_id", imageID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	updated, err := r.artistService.GetImagesForArtist(req.Context(), id)
	if err != nil {
		r.logger.Error("listing images after lock toggle", "id", id, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"images": updated})
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
