// Package api: handlers that mutate the artist row outside the field-level
// edit flow. The directory-rename handler in this file is intentionally
// separate from handleFieldUpdate ("name") because renaming an artist's
// on-disk directory has filesystem and platform-mapping consequences that
// must be opt-in, not a side-effect of saving a name change. See issue
// #1077: editing the Name field used to indirectly trigger an on-disk
// rename via the directory_name_mismatch rule fixer, which broke
// Emby/Jellyfin item-to-path mappings. The fix decouples the two: name
// edits go through PATCH /api/v1/artists/{id}/fields/name and stay in the
// DB+NFO; the directory rename below is a deliberate, user-driven action.
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/event"
)

// handleArtistRenameDirectory renames the artist's on-disk directory to the
// supplied new_dirname and updates the DB row. It does NOT touch the artist's
// display Name, NFO content, or platform mappings beyond what the rename
// itself implies. Callers wanting to also update the display name should
// PATCH /api/v1/artists/{id}/fields/name separately; the two operations are
// orthogonal by design.
//
// POST /api/v1/artists/{id}/rename-directory
//
// Body (JSON or form-encoded): { "new_dirname": "Some Artist" }
//
// Response (200): { "status": "renamed", "new_path": "..." }
//
// Status codes:
//   - 400 invalid input (empty / contains separator / no change)
//   - 404 artist not found
//   - 409 artist locked, has no path, or destination already exists
//   - 500 filesystem or DB error
func (r *Router) handleArtistRenameDirectory(w http.ResponseWriter, req *http.Request) {
	artistID := req.PathValue("id")

	newName, err := extractRenameDirname(req)
	if err != nil {
		writeError(w, req, http.StatusBadRequest, err.Error())
		return
	}

	newPath, err := r.artistService.RenameDirectory(req.Context(), artistID, newName)
	if err != nil {
		switch {
		case errors.Is(err, artist.ErrRenameInvalidName), errors.Is(err, artist.ErrRenameNoChange):
			writeError(w, req, http.StatusBadRequest, err.Error())
		case errors.Is(err, artist.ErrRenameLocked),
			errors.Is(err, artist.ErrRenameNoPath),
			errors.Is(err, artist.ErrRenameDestExists):
			writeError(w, req, http.StatusConflict, err.Error())
		default:
			r.logger.Error("renaming artist directory",
				slog.String("artist_id", artistID),
				slog.String("new_dirname", newName),
				slog.String("error", err.Error()))
			writeError(w, req, http.StatusInternalServerError, "failed to rename directory")
		}
		return
	}

	// Publish ArtistUpdated so subscribers (rule pipeline, health) re-evaluate
	// the artist. The path change can affect filesystem-derived rule checks
	// (e.g. directory_name_mismatch) so we want the next sweep to see it.
	if r.eventBus != nil {
		r.eventBus.Publish(event.Event{
			Type: event.ArtistUpdated,
			Data: map[string]any{"artist_id": artistID},
		})
	}
	r.InvalidateHealthCache()

	writeJSON(w, http.StatusOK, map[string]string{
		"status":   "renamed",
		"new_path": newPath,
	})
}

// extractRenameDirname reads the new directory name from a JSON or form body.
// JSON shape: {"new_dirname": "..."}; form key: "new_dirname". The form path
// uses PostForm only (not query parameters) to avoid CSRF-via-URL attacks.
// Returns a non-nil error with a user-safe message when the body is missing
// or malformed; the handler maps that to HTTP 400.
func extractRenameDirname(req *http.Request) (string, error) {
	if strings.HasPrefix(req.Header.Get("Content-Type"), "application/json") {
		var body struct {
			NewDirname string `json:"new_dirname"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return "", errors.New("invalid JSON body")
		}
		return strings.TrimSpace(body.NewDirname), nil
	}
	if err := req.ParseForm(); err != nil {
		return "", errors.New("invalid form body")
	}
	return strings.TrimSpace(req.PostForm.Get("new_dirname")), nil
}
