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
	"os"
	"path/filepath"
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

	imageURL, imageType := extractImageFetchParams(req)

	if imageURL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url is required"})
		return
	}
	if !validImageTypes[imageType] {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid image type, must be: thumb, fanart, logo, banner"})
		return
	}
	if !strings.HasPrefix(imageURL, "http://") && !strings.HasPrefix(imageURL, "https://") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url must start with http:// or https://"})
		return
	}

	data, err := r.fetchImageFromURL(imageURL)
	if err != nil {
		r.logger.Warn("fetching image from URL", "url", imageURL, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("failed to fetch image: %v", err)})
		return
	}

	saved, err := r.processAndSaveImage(req.Context(), a.Path, imageType, data)
	if err != nil {
		r.logger.Error("saving fetched image", "artist_id", artistID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save image"})
		return
	}

	r.updateArtistImageFlag(req.Context(), a, imageType)

	if isHTMXRequest(req) {
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"saved":  saved,
		"type":   imageType,
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

	// Probe dimensions for images that have none (e.g., Fanart.tv)
	images = r.probeImageDimensions(req.Context(), images)

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

// validWebSearchImageTypes is the set of image types supported by web search.
var validWebSearchImageTypes = map[string]bool{
	"thumb": true, "fanart": true, "logo": true, "banner": true,
}

// handleWebImageSearch queries enabled web search providers for artist images.
// GET /api/v1/artists/{id}/images/websearch?type=thumb
func (r *Router) handleWebImageSearch(w http.ResponseWriter, req *http.Request) {
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

	typeFilter := req.URL.Query().Get("type")
	if typeFilter == "" || !validWebSearchImageTypes[typeFilter] {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "type is required (thumb, fanart, logo, banner)"})
		return
	}

	imageType := provider.ImageType(typeFilter)

	var allImages []provider.ImageResult
	for _, p := range r.webSearchRegistry.All() {
		enabled, err := r.providerSettings.IsWebSearchEnabled(req.Context(), p.Name())
		if err != nil || !enabled {
			continue
		}
		images, err := p.SearchImages(req.Context(), a.Name, imageType)
		if err != nil {
			r.logger.Warn("web image search failed",
				slog.String("provider", string(p.Name())),
				slog.String("artist", a.Name),
				slog.String("error", err.Error()))
			continue
		}
		allImages = append(allImages, images...)
	}

	if isHTMXRequest(req) {
		renderTempl(w, req, templates.WebImageSearchResults(artistID, allImages))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"images": allImages})
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

	naming := r.getActiveNamingConfig(ctx, imageType)

	saved, err := img.Save(dir, imageType, resized, naming, r.logger)
	if err != nil {
		return nil, fmt.Errorf("saving: %w", err)
	}

	return saved, nil
}

// getActiveNamingConfig returns the filenames for the given image type from the active platform profile.
func (r *Router) getActiveNamingConfig(ctx context.Context, imageType string) []string {
	profile, err := r.platformService.GetActive(ctx)
	if err != nil || profile == nil {
		return img.FileNamesForType(img.DefaultFileNames, imageType)
	}
	names := img.FileNamesForType(profile.ImageNaming.ToMap(), imageType)
	if len(names) == 0 {
		return img.FileNamesForType(img.DefaultFileNames, imageType)
	}
	return names
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

// probeImageDimensions probes remote images that are missing dimensions.
// It runs probes concurrently with a cap on parallelism.
func (r *Router) probeImageDimensions(ctx context.Context, images []provider.ImageResult) []provider.ImageResult {
	const maxConcurrent = 5

	// Find indices that need probing
	var needProbe []int
	for i := range images {
		if images[i].Width == 0 && images[i].Height == 0 {
			needProbe = append(needProbe, i)
		}
	}
	if len(needProbe) == 0 {
		return images
	}

	type probeResult struct {
		idx    int
		width  int
		height int
	}

	results := make(chan probeResult, len(needProbe))
	sem := make(chan struct{}, maxConcurrent)

	for _, idx := range needProbe {
		sem <- struct{}{}
		go func(i int) {
			defer func() { <-sem }()
			info, err := img.ProbeRemoteImage(ctx, images[i].URL)
			if err != nil {
				r.logger.Debug("probing remote image dimensions",
					slog.String("url", images[i].URL),
					slog.String("error", err.Error()))
				return
			}
			results <- probeResult{idx: i, width: info.Width, height: info.Height}
		}(idx)
	}

	// Wait for all goroutines to finish
	for range maxConcurrent {
		sem <- struct{}{}
	}
	close(results)

	for pr := range results {
		images[pr.idx].Width = pr.width
		images[pr.idx].Height = pr.height
	}

	return images
}

// setArtistImageFlag sets the image existence (and low-resolution) flags and persists them.
// When exists is true the image file is probed for dimensions so the low-res flag is accurate.
// When exists is false the low-res flag is cleared.
func (r *Router) setArtistImageFlag(ctx context.Context, a *artist.Artist, imageType string, exists bool) {
	var lowRes bool
	if exists {
		patterns := r.getActiveNamingConfig(ctx, imageType)
		if filePath, found := findExistingImage(a.Path, patterns); found {
			if f, err := os.Open(filePath); err == nil { //nolint:gosec // path from trusted naming patterns
				w, h, _ := img.GetDimensions(f)
				_ = f.Close()
				lowRes = img.IsLowResolution(w, h, imageType)
			}
		}
	}

	switch imageType {
	case "thumb":
		a.ThumbExists = exists
		a.ThumbLowRes = lowRes
	case "fanart":
		a.FanartExists = exists
		a.FanartLowRes = lowRes
	case "logo":
		a.LogoExists = exists
		a.LogoLowRes = lowRes
	case "banner":
		a.BannerExists = exists
		a.BannerLowRes = lowRes
	}

	if err := r.artistService.Update(ctx, a); err != nil {
		r.logger.Warn("setting artist image flag",
			slog.String("artist_id", a.ID),
			slog.String("image_type", imageType),
			slog.String("error", err.Error()))
	}
}

// updateArtistImageFlag sets the image existence flag to true and persists it.
func (r *Router) updateArtistImageFlag(ctx context.Context, a *artist.Artist, imageType string) {
	r.setArtistImageFlag(ctx, a, imageType, true)
}

// handleServeImage serves a local artist image file from disk.
// GET /api/v1/artists/{id}/images/{type}/file
func (r *Router) handleServeImage(w http.ResponseWriter, req *http.Request) {
	artistID := req.PathValue("id")
	if artistID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing artist id"})
		return
	}

	imageType := req.PathValue("type")
	if !validImageTypes[imageType] {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid image type"})
		return
	}

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "artist not found"})
		return
	}

	patterns := r.getActiveNamingConfig(req.Context(), imageType)
	filePath, found := findExistingImage(a.Path, patterns)
	if !found {
		http.NotFound(w, req)
		return
	}

	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeFile(w, req, filePath)
}

// handleImageInfo returns metadata about a local artist image (dimensions, file size).
// GET /api/v1/artists/{id}/images/{type}/info
func (r *Router) handleImageInfo(w http.ResponseWriter, req *http.Request) {
	artistID := req.PathValue("id")
	if artistID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing artist id"})
		return
	}

	imageType := req.PathValue("type")
	if !validImageTypes[imageType] {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid image type"})
		return
	}

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "artist not found"})
		return
	}

	patterns := r.getActiveNamingConfig(req.Context(), imageType)
	filePath, found := findExistingImage(a.Path, patterns)
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "image not found"})
		return
	}

	stat, err := os.Stat(filePath) //nolint:gosec // path built from trusted naming patterns, not user input
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to stat image"})
		return
	}

	var width, height int
	f, err := os.Open(filePath) //nolint:gosec // path is constructed from trusted patterns
	if err == nil {
		defer func() { _ = f.Close() }()
		width, height, _ = img.GetDimensions(f)
	}

	if req.Header.Get("HX-Request") == "true" {
		renderTempl(w, req, templates.ImageInfoBadge(width, height, stat.Size()))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"type":     imageType,
		"filename": filepath.Base(filePath),
		"width":    width,
		"height":   height,
		"size":     stat.Size(),
		"modified": stat.ModTime().UTC().Format(time.RFC3339),
	})
}

// handleDeleteImage deletes a local artist image file.
// DELETE /api/v1/artists/{id}/images/{type}
func (r *Router) handleDeleteImage(w http.ResponseWriter, req *http.Request) {
	artistID := req.PathValue("id")
	if artistID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing artist id"})
		return
	}

	imageType := req.PathValue("type")
	if !validImageTypes[imageType] {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid image type"})
		return
	}

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "artist not found"})
		return
	}

	patterns := r.getActiveNamingConfig(req.Context(), imageType)
	deleted := deleteImageFiles(a.Path, patterns, r.logger)

	r.clearArtistImageFlag(req.Context(), a, imageType)

	if req.Header.Get("HX-Request") == "true" {
		renderTempl(w, req, templates.ImagePreviewCard(a.ID, imageType, false, imageTypeLabel(imageType)))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "deleted",
		"deleted": deleted,
	})
}

// clearArtistImageFlag sets the image existence flag to false and persists it.
func (r *Router) clearArtistImageFlag(ctx context.Context, a *artist.Artist, imageType string) {
	r.setArtistImageFlag(ctx, a, imageType, false)
}

// findExistingImage searches for the first matching image file in a directory.
// Patterns are trusted naming conventions, not user input.
func findExistingImage(dir string, patterns []string) (string, bool) {
	for _, pattern := range patterns {
		p := filepath.Join(dir, pattern)
		if _, err := os.Stat(p); err == nil { //nolint:gosec // path from trusted naming patterns
			return p, true
		}
	}
	return "", false
}

// deleteImageFiles removes all matching image files from a directory and returns deleted filenames.
// Patterns are trusted naming conventions, not user input.
func deleteImageFiles(dir string, patterns []string, logger *slog.Logger) []string {
	var deleted []string
	for _, pattern := range patterns {
		p := filepath.Join(dir, pattern)
		if err := os.Remove(p); err == nil { //nolint:gosec // path from trusted naming patterns
			logger.Info("deleted image file", slog.String("path", p))
			deleted = append(deleted, pattern)
		}
	}
	return deleted
}

// extractImageFetchParams reads the URL and type from an image fetch request.
// Supports both form-encoded (HTMX) and JSON payloads (API clients).
// Provider image types (hdlogo, widethumb, background) are normalized to their
// base types (logo, thumb, fanart) for filesystem naming.
func extractImageFetchParams(req *http.Request) (string, string) {
	var rawURL, rawType string
	if strings.HasPrefix(req.Header.Get("Content-Type"), "application/json") {
		var body struct {
			URL  string `json:"url"`
			Type string `json:"type"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err == nil {
			rawURL, rawType = body.URL, body.Type
		}
	} else {
		rawURL, rawType = req.FormValue("url"), req.FormValue("type")
	}
	return rawURL, normalizeImageType(rawType)
}

// normalizeImageType maps provider-specific image types to the base types
// used by the filesystem naming system.
func normalizeImageType(t string) string {
	switch t {
	case "hdlogo":
		return "logo"
	case "widethumb":
		return "thumb"
	case "background":
		return "fanart"
	default:
		return t
	}
}

// imageTypeLabel returns a human-readable label for an image type.
func imageTypeLabel(imageType string) string {
	switch imageType {
	case "thumb":
		return "Thumbnail"
	case "fanart":
		return "Fanart"
	case "logo":
		return "Logo"
	case "banner":
		return "Banner"
	default:
		return imageType
	}
}
