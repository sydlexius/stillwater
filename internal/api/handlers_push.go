package api

import (
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
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "artist not found"})
		return
	}

	var body struct {
		ConnectionID     string `json:"connection_id"`
		PlatformArtistID string `json:"platform_artist_id"`
	}
	if !DecodeJSON(w, req, &body) {
		return
	}
	if body.ConnectionID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "connection_id is required"})
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

	// Auto-lookup platform artist ID if not provided.
	if body.PlatformArtistID == "" {
		stored, lookupErr := r.artistService.GetPlatformID(req.Context(), artistID, body.ConnectionID)
		if lookupErr != nil {
			r.logger.Error("looking up platform id", "artist_id", artistID, "connection_id", body.ConnectionID, "error", lookupErr)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		if stored == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "platform_artist_id is required (no stored mapping found)"})
			return
		}
		body.PlatformArtistID = stored
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
		pusher = emby.New(conn.URL, conn.APIKey, conn.PlatformUserID, r.logger)
	case connection.TypeJellyfin:
		pusher = jellyfin.New(conn.URL, conn.APIKey, conn.PlatformUserID, r.logger)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "connection type does not support metadata push"})
		return
	}

	if err := pusher.PushMetadata(req.Context(), body.PlatformArtistID, data); err != nil {
		r.logger.Error("pushing metadata", "artist", a.Name, "connection", conn.Name, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "push failed"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "pushed"})
}

// handlePushImages uploads artist images to an Emby/Jellyfin connection.
// POST /api/v1/artists/{id}/push/images
func (r *Router) handlePushImages(w http.ResponseWriter, req *http.Request) {
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

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
	if !DecodeJSON(w, req, &body) {
		return
	}
	if body.ConnectionID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "connection_id is required"})
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

	// Auto-lookup platform artist ID if not provided.
	if body.PlatformArtistID == "" {
		stored, lookupErr := r.artistService.GetPlatformID(req.Context(), artistID, body.ConnectionID)
		if lookupErr != nil {
			r.logger.Error("looking up platform id", "artist_id", artistID, "connection_id", body.ConnectionID, "error", lookupErr)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		if stored == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "platform_artist_id is required (no stored mapping found)"})
			return
		}
		body.PlatformArtistID = stored
	}

	if len(body.ImageTypes) == 0 {
		body.ImageTypes = []string{"thumb", "fanart", "logo", "banner"}
	}

	var uploader connection.ImageUploader
	switch conn.Type {
	case connection.TypeEmby:
		uploader = emby.New(conn.URL, conn.APIKey, conn.PlatformUserID, r.logger)
	case connection.TypeJellyfin:
		uploader = jellyfin.New(conn.URL, conn.APIKey, conn.PlatformUserID, r.logger)
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

// handleDeletePushImage deletes an image from an Emby/Jellyfin connection.
// DELETE /api/v1/artists/{id}/push/images/{type}
func (r *Router) handleDeletePushImage(w http.ResponseWriter, req *http.Request) {
	imageType, ok := RequirePathParam(w, req, "type")
	if !ok {
		return
	}
	if !validImageTypes[imageType] {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid image type, must be: thumb, fanart, logo, banner"})
		return
	}

	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}
	if _, err := r.artistService.GetByID(req.Context(), artistID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "artist not found"})
		return
	}

	var body struct {
		ConnectionID     string `json:"connection_id"`
		PlatformArtistID string `json:"platform_artist_id"`
	}
	if !DecodeJSON(w, req, &body) {
		return
	}
	if body.ConnectionID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "connection_id is required"})
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

	// Auto-lookup platform artist ID if not provided.
	if body.PlatformArtistID == "" {
		stored, lookupErr := r.artistService.GetPlatformID(req.Context(), artistID, body.ConnectionID)
		if lookupErr != nil {
			r.logger.Error("looking up platform id", "artist_id", artistID, "connection_id", body.ConnectionID, "error", lookupErr)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		if stored == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "platform_artist_id is required (no stored mapping found)"})
			return
		}
		body.PlatformArtistID = stored
	}

	var deleter connection.ImageDeleter
	switch conn.Type {
	case connection.TypeEmby:
		deleter = emby.New(conn.URL, conn.APIKey, conn.PlatformUserID, r.logger)
	case connection.TypeJellyfin:
		deleter = jellyfin.New(conn.URL, conn.APIKey, conn.PlatformUserID, r.logger)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "connection type does not support image delete"})
		return
	}

	if err := deleter.DeleteImage(req.Context(), body.PlatformArtistID, imageType); err != nil {
		r.logger.Error("deleting image from platform", "artist_id", artistID, "type", imageType, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "delete failed"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
