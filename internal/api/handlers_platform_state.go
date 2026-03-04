package api

import (
	"net/http"
	"strings"

	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
	"github.com/sydlexius/stillwater/internal/connection/jellyfin"
	"github.com/sydlexius/stillwater/web/templates"
)

// handleGetPlatformState fetches the current state of an artist on a platform connection
// and returns an HTML partial for HTMX lazy-loading.
// GET /api/v1/artists/{id}/platform-state?connection_id=X
func (r *Router) handleGetPlatformState(w http.ResponseWriter, req *http.Request) {
	artistID := req.PathValue("id")
	connectionID := req.URL.Query().Get("connection_id")
	if connectionID == "" {
		writeError(w, req, http.StatusBadRequest, "connection_id is required")
		return
	}

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		writeError(w, req, http.StatusNotFound, "artist not found")
		return
	}

	conn, err := r.connectionService.GetByID(req.Context(), connectionID)
	if err != nil {
		writeError(w, req, http.StatusNotFound, "connection not found")
		return
	}

	platformArtistID, err := r.artistService.GetPlatformID(req.Context(), artistID, connectionID)
	if err != nil {
		r.logger.Error("looking up platform id", "artist_id", artistID, "connection_id", connectionID, "error", err)
		writeError(w, req, http.StatusInternalServerError, "internal error")
		return
	}
	if platformArtistID == "" {
		writeError(w, req, http.StatusNotFound, "no platform ID stored for this artist on this connection")
		return
	}

	getter, err := r.newStateGetter(conn)
	if err != nil {
		writeError(w, req, http.StatusBadRequest, err.Error())
		return
	}

	state, err := getter.GetArtistDetail(req.Context(), platformArtistID)
	if err != nil {
		r.logger.Error("fetching platform state", "artist", a.Name, "connection", conn.Name, "error", err)
		renderTempl(w, req, templates.PlatformStateError(conn, err.Error()))
		return
	}

	renderTempl(w, req, templates.PlatformStateCard(a, conn, state))
}

// handlePullMetadata pulls metadata from a platform connection and overwrites
// the artist's biography, genres, and dates in Stillwater.
// POST /api/v1/artists/{id}/pull?connection_id=X
func (r *Router) handlePullMetadata(w http.ResponseWriter, req *http.Request) {
	artistID := req.PathValue("id")
	connectionID := req.URL.Query().Get("connection_id")
	if connectionID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "connection_id is required"})
		return
	}

	_, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "artist not found"})
		return
	}

	conn, err := r.connectionService.GetByID(req.Context(), connectionID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "connection not found"})
		return
	}
	if !conn.Enabled {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "connection is disabled"})
		return
	}

	platformArtistID, err := r.artistService.GetPlatformID(req.Context(), artistID, connectionID)
	if err != nil {
		r.logger.Error("looking up platform id", "artist_id", artistID, "connection_id", connectionID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if platformArtistID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no platform ID stored for this artist on this connection"})
		return
	}

	getter, err := r.newStateGetter(conn)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	state, err := getter.GetArtistDetail(req.Context(), platformArtistID)
	if err != nil {
		r.logger.Error("pulling platform state", "artist_id", artistID, "connection", conn.Name, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to fetch platform state: " + err.Error()})
		return
	}

	var updated []string

	if state.Biography != "" {
		if err := r.artistService.UpdateField(req.Context(), artistID, "biography", state.Biography); err != nil {
			r.logger.Warn("updating biography from platform", "error", err)
		} else {
			updated = append(updated, "biography")
		}
	}

	if len(state.Genres) > 0 {
		if err := r.artistService.UpdateField(req.Context(), artistID, "genres", strings.Join(state.Genres, ", ")); err != nil {
			r.logger.Warn("updating genres from platform", "error", err)
		} else {
			updated = append(updated, "genres")
		}
	}

	if state.PremiereDate != "" {
		if err := r.artistService.UpdateField(req.Context(), artistID, "formed", state.PremiereDate); err != nil {
			r.logger.Warn("updating formed date from platform", "error", err)
		} else {
			updated = append(updated, "formed")
		}
	}

	if state.EndDate != "" {
		if err := r.artistService.UpdateField(req.Context(), artistID, "disbanded", state.EndDate); err != nil {
			r.logger.Warn("updating disbanded date from platform", "error", err)
		} else {
			updated = append(updated, "disbanded")
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "pulled",
		"updated": updated,
	})
}

// newStateGetter instantiates an ArtistStateGetter for the given connection type.
func (r *Router) newStateGetter(conn *connection.Connection) (connection.ArtistStateGetter, error) {
	switch conn.Type {
	case connection.TypeEmby:
		return emby.New(conn.URL, conn.APIKey, r.logger), nil
	case connection.TypeJellyfin:
		return jellyfin.New(conn.URL, conn.APIKey, r.logger), nil
	default:
		return nil, errUnsupportedConnectionType
	}
}

// errUnsupportedConnectionType is returned when a connection type does not support platform state.
var errUnsupportedConnectionType = simpleError("connection type does not support platform state")

type simpleError string

func (e simpleError) Error() string { return string(e) }
