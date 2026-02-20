package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	img "github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/web/templates"
)

const (
	maxUploadSize = 25 << 20 // 25 MB
	fetchTimeout  = 30 * time.Second
)

// validImageTypes is the set of accepted image type values.
var validImageTypes = map[string]bool{
	"thumb": true, "fanart": true, "logo": true, "banner": true,
}

// validContentTypes maps MIME types to accepted image formats.
var validContentTypes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/webp": true,
}

// handleImageUpload handles multipart file uploads for artist images.
// POST /api/v1/artists/{id}/images/upload
func (r *Router) handleImageUpload(w http.ResponseWriter, req *http.Request) {
	artistID := req.PathValue("id")
	if artistID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing artist id"})
		return
	}

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "artist not found"})
		return
	}

	if err := req.ParseMultipartForm(maxUploadSize); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request too large or invalid multipart form"})
		return
	}

	imageType := req.FormValue("type")
	if !validImageTypes[imageType] {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid image type, must be: thumb, fanart, logo, banner"})
		return
	}

	file, header, err := req.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing file field"})
		return
	}
	defer file.Close() //nolint:errcheck

	ct := header.Header.Get("Content-Type")
	if !validContentTypes[ct] {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("unsupported content type: %s", ct)})
		return
	}

	data, err := io.ReadAll(io.LimitReader(file, maxUploadSize+1))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read uploaded file"})
		return
	}
	if len(data) > maxUploadSize {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "file exceeds 25MB limit"})
		return
	}

	saved, err := r.processAndSaveImage(req.Context(), a.Path, imageType, data)
	if err != nil {
		r.logger.Error("saving uploaded image", "artist_id", artistID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save image"})
		return
	}

	r.updateArtistImageFlag(req.Context(), a, imageType)

	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"saved":  saved,
		"type":   imageType,
	})
}

// handleImageFetch fetches an image from a URL and saves it for the artist.
// POST /api/v1/artists/{id}/images/fetch
func (r *Router) handleImageFetch(w http.ResponseWriter, req *http.Request) {
	artistID := req.PathValue("id")
	if artistID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing artist id"})
		return
	}

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "artist not found"})
		return
	}

	var body struct {
		URL  string `json:"url"`
		Type string `json:"type"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if body.URL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url is required"})
		return
	}
	if !validImageTypes[body.Type] {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid image type, must be: thumb, fanart, logo, banner"})
		return
	}
	if !strings.HasPrefix(body.URL, "http://") && !strings.HasPrefix(body.URL, "https://") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url must start with http:// or https://"})
		return
	}

	data, err := r.fetchImageFromURL(body.URL)
	if err != nil {
		r.logger.Warn("fetching image from URL", "url", body.URL, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("failed to fetch image: %v", err)})
		return
	}

	saved, err := r.processAndSaveImage(req.Context(), a.Path, body.Type, data)
	if err != nil {
		r.logger.Error("saving fetched image", "artist_id", artistID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save image"})
		return
	}

	r.updateArtistImageFlag(req.Context(), a, body.Type)

	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"saved":  saved,
		"type":   body.Type,
	})
}

// handleImageSearch searches for images from all providers for an artist.
// GET /api/v1/artists/{id}/images/search?type=thumb
func (r *Router) handleImageSearch(w http.ResponseWriter, req *http.Request) {
	artistID := req.PathValue("id")
	if artistID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing artist id"})
		return
	}

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "artist not found"})
		return
	}

	if a.MusicBrainzID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "artist has no MusicBrainz ID, cannot search providers"})
		return
	}

	result, err := r.orchestrator.FetchImages(req.Context(), a.MusicBrainzID)
	if err != nil {
		r.logger.Error("fetching images from providers", "artist_id", artistID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to fetch images from providers"})
		return
	}

	images := result.Images
	typeFilter := req.URL.Query().Get("type")
	if typeFilter != "" {
		var filtered []provider.ImageResult
		for _, im := range images {
			if string(im.Type) == typeFilter {
				filtered = append(filtered, im)
			}
		}
		images = filtered
	}

	// Sort by likes (descending), then by resolution (descending)
	sort.Slice(images, func(i, j int) bool {
		if images[i].Likes != images[j].Likes {
			return images[i].Likes > images[j].Likes
		}
		areaI := images[i].Width * images[i].Height
		areaJ := images[j].Width * images[j].Height
		return areaI > areaJ
	})

	// Return HTML for HTMX requests, JSON for API requests
	if isHTMXRequest(req) {
		renderTempl(w, req, templates.ImageSearchResults(artistID, images))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"images": images,
		"errors": result.Errors,
	})
}

// handleImageCrop accepts cropped image data and saves it.
// POST /api/v1/artists/{id}/images/crop
func (r *Router) handleImageCrop(w http.ResponseWriter, req *http.Request) {
	artistID := req.PathValue("id")
	if artistID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing artist id"})
		return
	}

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "artist not found"})
		return
	}

	var body struct {
		ImageData string `json:"image_data"` // base64-encoded image (optionally with data URI prefix)
		Type      string `json:"type"`
		X         int    `json:"x"`
		Y         int    `json:"y"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if !validImageTypes[body.Type] {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid image type"})
		return
	}
	if body.ImageData == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "image_data is required"})
		return
	}

	// Strip data URI prefix if present: "data:image/png;base64,..."
	b64Data := body.ImageData
	if idx := strings.Index(b64Data, ","); idx != -1 {
		b64Data = b64Data[idx+1:]
	}

	imgData, err := base64.StdEncoding.DecodeString(b64Data)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid base64 image data"})
		return
	}

	// Apply server-side crop if coordinates are provided
	if body.Width > 0 && body.Height > 0 {
		cropped, _, cropErr := img.Crop(bytes.NewReader(imgData), body.X, body.Y, body.Width, body.Height)
		if cropErr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("crop failed: %v", cropErr)})
			return
		}
		imgData = cropped
	}

	saved, err := r.processAndSaveImage(req.Context(), a.Path, body.Type, imgData)
	if err != nil {
		r.logger.Error("saving cropped image", "artist_id", artistID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save image"})
		return
	}

	r.updateArtistImageFlag(req.Context(), a, body.Type)

	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"saved":  saved,
		"type":   body.Type,
	})
}

// processAndSaveImage processes image data (resize if oversized, optimize) and saves it.
func (r *Router) processAndSaveImage(ctx context.Context, dir string, imageType string, data []byte) ([]string, error) {
	// Resize if oversized (max 3000px on any dimension)
	resized, _, err := img.Resize(bytes.NewReader(data), 3000, 3000)
	if err != nil {
		return nil, fmt.Errorf("resizing: %w", err)
	}

	naming, err := r.getActiveNamingConfig(ctx, imageType)
	if err != nil {
		return nil, err
	}

	saved, err := img.Save(dir, imageType, resized, naming, r.logger)
	if err != nil {
		return nil, fmt.Errorf("saving: %w", err)
	}

	return saved, nil
}

// getActiveNamingConfig returns the filenames for the given image type from the active platform profile.
func (r *Router) getActiveNamingConfig(ctx context.Context, imageType string) ([]string, error) {
	profile, err := r.platformService.GetActive(ctx)
	if err != nil || profile == nil {
		return img.FileNamesForType(img.DefaultFileNames, imageType), nil
	}
	names := img.FileNamesForType(profile.ImageNaming.ToMap(), imageType)
	if len(names) == 0 {
		return img.FileNamesForType(img.DefaultFileNames, imageType), nil
	}
	return names, nil
}

// fetchImageFromURL downloads an image from the given URL with timeout and size limits.
func (r *Router) fetchImageFromURL(rawURL string) ([]byte, error) {
	client := &http.Client{Timeout: fetchTimeout}

	resp, err := client.Get(rawURL) //nolint:gosec,noctx // G107: URL is validated by caller; background fetch is acceptable
	if err != nil {
		return nil, fmt.Errorf("fetching: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server returned %d", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if !validContentTypes[ct] {
		r.logger.Debug("non-image content type from URL, will detect from data",
			slog.String("content_type", ct))
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxUploadSize+1))
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	if len(data) > maxUploadSize {
		return nil, fmt.Errorf("image exceeds 25MB limit")
	}

	if _, _, err := img.DetectFormat(bytes.NewReader(data)); err != nil {
		return nil, fmt.Errorf("downloaded file is not a valid image")
	}

	return data, nil
}

// updateArtistImageFlag updates the image existence flag on the artist and persists it.
func (r *Router) updateArtistImageFlag(ctx context.Context, a *artist.Artist, imageType string) {
	switch imageType {
	case "thumb":
		a.ThumbExists = true
	case "fanart":
		a.FanartExists = true
	case "logo":
		a.LogoExists = true
	case "banner":
		a.BannerExists = true
	}

	if err := r.artistService.Update(ctx, a); err != nil {
		r.logger.Warn("updating artist image flags",
			slog.String("artist_id", a.ID),
			slog.String("error", err.Error()))
	}
}
