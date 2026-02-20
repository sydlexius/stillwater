package api

import (
	"encoding/json"
	"net/http"

	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
	"github.com/sydlexius/stillwater/internal/connection/jellyfin"
)

func (r *Router) handlePushMetadata(w http.ResponseWriter, req *http.Request) {
	artistID := req.PathValue("id")

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "artist not found"})
		return
	}

	var body struct {
		ConnectionID     string `json:"connection_id"`
		PlatformArtistID string `json:"platform_artist_id"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if body.ConnectionID == "" || body.PlatformArtistID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "connection_id and platform_artist_id are required"})
		return
	}

	conn, err := r.connectionService.GetByID(req.Context(), body.ConnectionID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "connection not found"})
		return
	}
	if !conn.Enabled {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "connection is disabled"})
		return
	}

	data := connection.ArtistPushData{
		Name:      a.Name,
		SortName:  a.SortName,
		Biography: a.Biography,
		Genres:    a.Genres,
	}

	var pusher connection.MetadataPusher
	switch conn.Type {
	case connection.TypeEmby:
		pusher = emby.New(conn.URL, conn.APIKey, r.logger)
	case connection.TypeJellyfin:
		pusher = jellyfin.New(conn.URL, conn.APIKey, r.logger)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "connection type does not support metadata push"})
		return
	}

	if err := pusher.PushMetadata(req.Context(), body.PlatformArtistID, data); err != nil {
		r.logger.Error("pushing metadata", "artist", a.Name, "connection", conn.Name, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "push failed: " + err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "pushed"})
}
