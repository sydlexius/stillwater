package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

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
		Name:           a.Name,
		SortName:       a.SortName,
		Biography:      a.Biography,
		Genres:         a.Genres,
		Styles:         a.Styles,
		Moods:          a.Moods,
		Disambiguation: a.Disambiguation,
		Born:           a.Born,
		Formed:         a.Formed,
		Died:           a.Died,
		Disbanded:      a.Disbanded,
		YearsActive:    a.YearsActive,
		MusicBrainzID:  a.MusicBrainzID,
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

// handlePushImages uploads artist images to an Emby/Jellyfin connection.
// POST /api/v1/artists/{id}/push/images
func (r *Router) handlePushImages(w http.ResponseWriter, req *http.Request) {
	artistID := req.PathValue("id")

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "artist not found"})
		return
	}

	var body struct {
		ConnectionID     string   `json:"connection_id"`
		PlatformArtistID string   `json:"platform_artist_id"`
		ImageTypes       []string `json:"image_types"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if body.ConnectionID == "" || body.PlatformArtistID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "connection_id and platform_artist_id are required"})
		return
	}
	if len(body.ImageTypes) == 0 {
		body.ImageTypes = []string{"thumb", "fanart", "logo", "banner"}
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

	var uploader connection.ImageUploader
	switch conn.Type {
	case connection.TypeEmby:
		uploader = emby.New(conn.URL, conn.APIKey, r.logger)
	case connection.TypeJellyfin:
		uploader = jellyfin.New(conn.URL, conn.APIKey, r.logger)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "connection type does not support image upload"})
		return
	}

	var uploaded []string
	var errors []string

	for _, imgType := range body.ImageTypes {
		if !validImageTypes[imgType] {
			continue
		}
		patterns := r.getActiveNamingConfig(req.Context(), imgType)
		filePath, found := findExistingImage(a.Path, patterns)
		if !found {
			continue
		}

		data, readErr := os.ReadFile(filePath) //nolint:gosec // path from trusted naming patterns
		if readErr != nil {
			errors = append(errors, fmt.Sprintf("%s: read failed", imgType))
			continue
		}

		ct := "image/jpeg"
		if strings.EqualFold(filepath.Ext(filePath), ".png") {
			ct = "image/png"
		}

		if uploadErr := uploader.UploadImage(req.Context(), body.PlatformArtistID, imgType, data, ct); uploadErr != nil {
			r.logger.Error("uploading image", "artist", a.Name, "type", imgType, "error", uploadErr)
			errors = append(errors, fmt.Sprintf("%s: %v", imgType, uploadErr))
			continue
		}

		uploaded = append(uploaded, imgType)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"uploaded": uploaded,
		"errors":   errors,
	})
}
