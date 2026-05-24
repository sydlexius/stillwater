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
// Response (200):
//
//	{
//	  "status": "renamed",
//	  "new_path": "...",
//	  "platforms": [
//	    {"connection_id": "...", "result": "ok"},
//	    {"connection_id": "...", "result": "failed", "error": "..."}
//	  ]
//	}
//
// The platforms array carries one entry per artist_platform_ids row found
// for this artist (Emby/Jellyfin/Lidarr). A single platform failure does
// NOT roll back the rename: the local filesystem + DB are already
// consistent, and a stale item-to-path mapping on a peer is recoverable.
// The array is empty when the artist has no platform mappings (#1222,
// #1231).
//
// Status codes:
//   - 400 invalid input (empty / contains separator / no change)
//   - 404 artist not found
//   - 409 artist locked, has no path, or destination already exists
//   - 500 filesystem or DB error
func (r *Router) handleArtistRenameDirectory(w http.ResponseWriter, req *http.Request) {
	artistID := req.PathValue("id")

	// Log every 4xx rejection (parsing failure, mapped service errors) at
	// Warn so operators can diagnose rename failures from server logs
	// without reproducing them. The 5xx default still uses Error. Field
	// naming matches the rest of internal/api
	// (slog.String("error", err.Error())).
	newName, err := extractRenameDirname(req)
	if err != nil {
		r.logger.Warn("rename artist directory rejected",
			slog.String("artist_id", artistID),
			slog.Int("status", http.StatusBadRequest),
			slog.String("error", err.Error()))
		writeError(w, req, http.StatusBadRequest, err.Error())
		return
	}

	newPath, platforms, err := r.artistService.RenameDirectory(req.Context(), artistID, newName)
	if err != nil {
		logRejected := func(status int) {
			r.logger.Warn("rename artist directory rejected",
				slog.String("artist_id", artistID),
				slog.String("new_dirname", newName),
				slog.Int("status", status),
				slog.String("error", err.Error()))
		}
		switch {
		case errors.Is(err, artist.ErrRenameInvalidName), errors.Is(err, artist.ErrRenameNoChange):
			logRejected(http.StatusBadRequest)
			writeError(w, req, http.StatusBadRequest, err.Error())
		case errors.Is(err, artist.ErrNotFound):
			// RenameDirectory propagates GetByID's not-found sentinel.
			// Map to 404 so the client distinguishes "no such artist"
			// from server-side or conflict failures.
			logRejected(http.StatusNotFound)
			writeError(w, req, http.StatusNotFound, "artist not found")
		case errors.Is(err, artist.ErrRenameLocked),
			errors.Is(err, artist.ErrRenameNoPath),
			errors.Is(err, artist.ErrRenameDestExists):
			logRejected(http.StatusConflict)
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

	// Always emit a non-nil platforms slice so the response shape is stable
	// across "no mappings" (omitting the field would force every client to
	// nil-check) and "platforms present" cases. JSON serializes a nil slice
	// as null and an empty slice as []; clients can range over [] safely.
	if platforms == nil {
		platforms = []artist.PlatformRemapResult{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "renamed",
		"new_path":  newPath,
		"platforms": platforms,
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
