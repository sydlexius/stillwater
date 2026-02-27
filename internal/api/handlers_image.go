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
	"strconv"
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

// requireArtistPath checks that the artist has a filesystem path. Returns true
// if the path is present (caller may proceed). Returns false and writes a 409
// response if the artist belongs to a degraded (pathless) library.
func (r *Router) requireArtistPath(w http.ResponseWriter, req *http.Request, a *artist.Artist) bool {
	if a.Path == "" {
		writeError(w, req, http.StatusConflict,
			"filesystem operations are not available for this artist (library has no path configured)")
		return false
	}
	return true
}

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
	if !r.requireArtistPath(w, req, a) {
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

	// Fanart: append as next numbered file when fanart already exists.
	if imageType == "fanart" && a.FanartExists {
		saved, _, saveErr := r.processAndAppendFanart(req.Context(), a.Path, data)
		if saveErr != nil {
			r.logger.Error("appending fanart upload", "artist_id", artistID, "error", saveErr)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save image"})
			return
		}
		r.updateArtistFanartCount(req.Context(), a)
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "ok",
			"saved":  saved,
			"type":   imageType,
			"count":  a.FanartCount,
		})
		return
	}

	saved, err := r.processAndSaveImage(req.Context(), a.Path, imageType, data)
	if err != nil {
		r.logger.Error("saving uploaded image", "artist_id", artistID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save image"})
		return
	}

	r.updateArtistImageFlag(req.Context(), a, imageType)
	if imageType == "fanart" {
		r.updateArtistFanartCount(req.Context(), a)
	}

	resp := map[string]any{
		"status": "ok",
		"saved":  saved,
		"type":   imageType,
	}
	if imageType == "fanart" {
		resp["count"] = a.FanartCount
	}
	writeJSON(w, http.StatusOK, resp)
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
	if !r.requireArtistPath(w, req, a) {
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

	// Fanart: append as next numbered file when fanart already exists.
	if imageType == "fanart" && a.FanartExists {
		saved, _, saveErr := r.processAndAppendFanart(req.Context(), a.Path, data)
		if saveErr != nil {
			r.logger.Error("appending fanart image", "artist_id", artistID, "error", saveErr)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save image"})
			return
		}
		r.updateArtistFanartCount(req.Context(), a)
		if isHTMXRequest(req) {
			w.Header().Set("HX-Refresh", "true")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "ok",
			"saved":  saved,
			"type":   imageType,
			"count":  a.FanartCount,
		})
		return
	}

	saved, err := r.processAndSaveImage(req.Context(), a.Path, imageType, data)
	if err != nil {
		r.logger.Error("saving fetched image", "artist_id", artistID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save image"})
		return
	}

	r.updateArtistImageFlag(req.Context(), a, imageType)
	// Sync fanart count after initial save
	if imageType == "fanart" {
		r.updateArtistFanartCount(req.Context(), a)
	}

	if isHTMXRequest(req) {
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	resp := map[string]any{
		"status": "ok",
		"saved":  saved,
		"type":   imageType,
	}
	if imageType == "fanart" {
		resp["count"] = a.FanartCount
	}
	writeJSON(w, http.StatusOK, resp)
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

	result, err := r.orchestrator.FetchImages(req.Context(), a.MusicBrainzID, map[provider.ProviderName]string{
		provider.NameDeezer: a.DeezerID,
	})
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
		if typeFilter == "fanart" {
			renderTempl(w, req, templates.FanartSearchResults(artistID, images))
		} else {
			renderTempl(w, req, templates.ImageSearchResults(artistID, images))
		}
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
	if !r.requireArtistPath(w, req, a) {
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
// For logos, transparent borders are automatically trimmed before saving.
func (r *Router) processAndSaveImage(ctx context.Context, dir string, imageType string, data []byte) ([]string, error) {
	// Resize if oversized (max 3000px on any dimension)
	resized, _, err := img.Resize(bytes.NewReader(data), 3000, 3000)
	if err != nil {
		return nil, fmt.Errorf("resizing: %w", err)
	}

	// Logos: trim transparent borders so the image renders without padding.
	if imageType == "logo" {
		if trimmed, _, trimErr := img.TrimAlpha(bytes.NewReader(resized), 10); trimErr == nil {
			resized = trimmed
		}
	}

	naming := r.getActiveNamingConfig(ctx, imageType)

	saved, err := img.Save(dir, imageType, resized, naming, r.logger)
	if err != nil {
		return nil, fmt.Errorf("saving: %w", err)
	}

	return saved, nil
}

// getActiveNamingConfig returns the filenames for the given image type from the
// active platform profile. Returns the full array so that image saves write
// copies for every configured filename.
func (r *Router) getActiveNamingConfig(ctx context.Context, imageType string) []string {
	profile, err := r.platformService.GetActive(ctx)
	if err != nil || profile == nil {
		return img.FileNamesForType(img.DefaultFileNames, imageType)
	}
	names := profile.ImageNaming.NamesForType(imageType)
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

	// no-cache: browser must revalidate before reusing. http.ServeFile sets
	// ETag/Last-Modified from the file mtime, so an unchanged file returns
	// 304 Not Modified with no data transfer, while a replaced image is
	// served fresh on the very next request (including a normal F5 reload).
	w.Header().Set("Cache-Control", "no-cache")
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
	if !r.requireArtistPath(w, req, a) {
		return
	}

	// For fanart, delete ALL numbered variants as well.
	if imageType == "fanart" {
		primary := r.getActiveFanartPrimary(req.Context())
		fanartPaths := img.DiscoverFanart(a.Path, primary)
		var deleted []string
		for _, p := range fanartPaths {
			if err := os.Remove(p); err == nil {
				deleted = append(deleted, filepath.Base(p))
				r.logger.Info("deleted fanart", slog.String("path", p))
			}
		}
		// Also clean up the standard naming config patterns (alternate names)
		patterns := r.getActiveNamingConfig(req.Context(), imageType)
		deleted = append(deleted, deleteImageFiles(a.Path, patterns, r.logger)...)
		r.updateArtistFanartCount(req.Context(), a)
		if req.Header.Get("HX-Request") == "true" {
			renderTempl(w, req, templates.ImagePreviewCard(a.ID, imageType, false, imageTypeLabel(imageType), false, 0))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "deleted",
			"deleted": deleted,
		})
		return
	}

	patterns := r.getActiveNamingConfig(req.Context(), imageType)
	deleted := deleteImageFiles(a.Path, patterns, r.logger)

	r.clearArtistImageFlag(req.Context(), a, imageType)

	if req.Header.Get("HX-Request") == "true" {
		renderTempl(w, req, templates.ImagePreviewCard(a.ID, imageType, false, imageTypeLabel(imageType), false, 0))
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
// For each configured pattern it first checks the exact filename, then probes
// alternate supported extensions (.jpg, .png) to handle cases where the saved
// format differs from the configured name (e.g. a PNG crop saved over folder.jpg).
func findExistingImage(dir string, patterns []string) (string, bool) {
	for _, pattern := range patterns {
		p := filepath.Join(dir, pattern)
		if _, err := os.Stat(p); err == nil { //nolint:gosec // path from trusted naming patterns
			return p, true
		}
		// Check alternate extensions in case the format changed after save.
		base := strings.TrimSuffix(pattern, filepath.Ext(pattern))
		for _, ext := range []string{".jpg", ".jpeg", ".png"} {
			if ext == filepath.Ext(pattern) {
				continue
			}
			alt := filepath.Join(dir, base+ext)
			if _, err := os.Stat(alt); err == nil { //nolint:gosec // path from trusted naming patterns
				return alt, true
			}
		}
	}
	return "", false
}

// deleteImageFiles removes all matching image files from a directory and returns deleted filenames.
// For each pattern, it also probes alternate extensions (.jpg, .jpeg, .png) to catch cases where
// the saved format differs from the configured name. Patterns are trusted naming conventions.
func deleteImageFiles(dir string, patterns []string, logger *slog.Logger) []string {
	var deleted []string
	for _, pattern := range patterns {
		p := filepath.Join(dir, pattern)
		if err := os.Remove(p); err == nil { //nolint:gosec // path from trusted naming patterns
			logger.Info("deleted image file", slog.String("path", p))
			deleted = append(deleted, pattern)
		}
		// Also try alternate extensions in case format diverged from config.
		base := strings.TrimSuffix(pattern, filepath.Ext(pattern))
		for _, ext := range []string{".jpg", ".jpeg", ".png"} {
			if ext == filepath.Ext(pattern) {
				continue
			}
			alt := base + ext
			altPath := filepath.Join(dir, alt)
			if err := os.Remove(altPath); err == nil { //nolint:gosec // path from trusted naming patterns
				logger.Info("deleted image file (alt ext)", slog.String("path", altPath))
				deleted = append(deleted, alt)
			}
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

// handleLogoTrim trims the transparent border from an artist's existing logo.
// POST /api/v1/artists/{id}/images/logo/trim
func (r *Router) handleLogoTrim(w http.ResponseWriter, req *http.Request) {
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
	if !r.requireArtistPath(w, req, a) {
		return
	}

	patterns := r.getActiveNamingConfig(req.Context(), "logo")
	filePath, found := findExistingImage(a.Path, patterns)
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "logo not found"})
		return
	}

	f, err := os.Open(filePath) //nolint:gosec // path built from trusted naming patterns
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read logo"})
		return
	}
	data, readErr := io.ReadAll(f)
	_ = f.Close()
	if readErr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read logo"})
		return
	}

	trimmed, _, err := img.TrimAlpha(bytes.NewReader(data), 10)
	if err != nil {
		r.logger.Error("trimming logo alpha", "artist_id", artistID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to trim logo"})
		return
	}

	if _, err := img.Save(a.Path, "logo", trimmed, patterns, r.logger); err != nil {
		r.logger.Error("saving trimmed logo", "artist_id", artistID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save trimmed logo"})
		return
	}

	r.updateArtistImageFlag(req.Context(), a, "logo")

	if isHTMXRequest(req) {
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
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

// getActiveFanartPrimary returns the primary fanart filename from the active
// platform profile.
func (r *Router) getActiveFanartPrimary(ctx context.Context) string {
	profile, err := r.platformService.GetActive(ctx)
	if err != nil || profile == nil {
		return img.PrimaryFileName(img.DefaultFileNames, "fanart")
	}
	name := profile.ImageNaming.PrimaryName("fanart")
	if name == "" {
		return img.PrimaryFileName(img.DefaultFileNames, "fanart")
	}
	return name
}

// isKodiNumbering returns true if the active platform profile uses Kodi-style
// fanart numbering (fanart1.jpg, fanart2.jpg) instead of the Emby/Jellyfin
// convention (backdrop2.jpg, backdrop3.jpg).
func (r *Router) isKodiNumbering(ctx context.Context) bool {
	profile, err := r.platformService.GetActive(ctx)
	if err != nil || profile == nil {
		return false
	}
	return strings.EqualFold(profile.Name, "Kodi")
}

// processAndAppendFanart processes image data and saves it as the next
// numbered fanart file. Returns the saved filenames and new total count.
func (r *Router) processAndAppendFanart(ctx context.Context, dir string, data []byte) ([]string, int, error) {
	resized, _, err := img.Resize(bytes.NewReader(data), 3000, 3000)
	if err != nil {
		return nil, 0, fmt.Errorf("resizing: %w", err)
	}

	primary := r.getActiveFanartPrimary(ctx)
	kodi := r.isKodiNumbering(ctx)
	existing := img.DiscoverFanart(dir, primary)
	nextIndex := len(existing)
	nextName := img.FanartFilename(primary, nextIndex, kodi)

	saved, err := img.Save(dir, "fanart", resized, []string{nextName}, r.logger)
	if err != nil {
		return nil, 0, fmt.Errorf("saving: %w", err)
	}

	return saved, nextIndex + 1, nil
}

// updateArtistFanartCount discovers fanart files and updates both the exists
// flag and count on the artist record.
func (r *Router) updateArtistFanartCount(ctx context.Context, a *artist.Artist) {
	primary := r.getActiveFanartPrimary(ctx)
	existing := img.DiscoverFanart(a.Path, primary)
	count := len(existing)
	a.FanartExists = count > 0
	a.FanartCount = count

	// Update low-res flag from primary fanart
	a.FanartLowRes = false
	if count > 0 {
		if f, err := os.Open(existing[0]); err == nil { //nolint:gosec // path from discovery
			w, h, _ := img.GetDimensions(f)
			_ = f.Close()
			a.FanartLowRes = img.IsLowResolution(w, h, "fanart")
		}
	}

	if err := r.artistService.Update(ctx, a); err != nil {
		r.logger.Warn("updating fanart count",
			slog.String("artist_id", a.ID),
			slog.String("error", err.Error()))
	}
}

// handleFanartList returns metadata for all fanart images of an artist.
// GET /api/v1/artists/{id}/images/fanart/list
func (r *Router) handleFanartList(w http.ResponseWriter, req *http.Request) {
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

	primary := r.getActiveFanartPrimary(req.Context())
	paths := img.DiscoverFanart(a.Path, primary)

	items := make([]templates.FanartGalleryItem, 0, len(paths))
	for i, p := range paths {
		item := templates.FanartGalleryItem{Index: i, Filename: filepath.Base(p)}
		if stat, statErr := os.Stat(p); statErr == nil {
			item.Size = stat.Size()
		}
		if f, openErr := os.Open(p); openErr == nil { //nolint:gosec // path from discovery
			w, h, _ := img.GetDimensions(f)
			_ = f.Close()
			item.Width = w
			item.Height = h
		}
		items = append(items, item)
	}

	if isHTMXRequest(req) {
		renderTempl(w, req, templates.FanartGallery(artistID, items))
		return
	}

	writeJSON(w, http.StatusOK, items)
}

// handleServeFanartByIndex serves a specific fanart image by 0-based index.
// GET /api/v1/artists/{id}/images/fanart/{index}/file
func (r *Router) handleServeFanartByIndex(w http.ResponseWriter, req *http.Request) {
	artistID := req.PathValue("id")
	if artistID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing artist id"})
		return
	}

	indexStr := req.PathValue("index")
	index, err := strconv.Atoi(indexStr)
	if err != nil || index < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid index"})
		return
	}

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "artist not found"})
		return
	}

	primary := r.getActiveFanartPrimary(req.Context())
	paths := img.DiscoverFanart(a.Path, primary)
	if index >= len(paths) {
		http.NotFound(w, req)
		return
	}

	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFile(w, req, paths[index])
}

// handleFanartBatchDelete deletes selected fanart by indices and re-numbers
// remaining files to close gaps.
// DELETE /api/v1/artists/{id}/images/fanart/batch
func (r *Router) handleFanartBatchDelete(w http.ResponseWriter, req *http.Request) {
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
	if !r.requireArtistPath(w, req, a) {
		return
	}

	var body struct {
		Indices []int `json:"indices"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if len(body.Indices) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no indices specified"})
		return
	}

	primary := r.getActiveFanartPrimary(req.Context())
	kodi := r.isKodiNumbering(req.Context())
	paths := img.DiscoverFanart(a.Path, primary)

	// Build set of indices to delete
	deleteSet := make(map[int]bool, len(body.Indices))
	for _, idx := range body.Indices {
		if idx >= 0 && idx < len(paths) {
			deleteSet[idx] = true
		}
	}

	// Delete the selected files
	var deleted []string
	for idx := range deleteSet {
		if err := os.Remove(paths[idx]); err == nil {
			deleted = append(deleted, filepath.Base(paths[idx]))
			r.logger.Info("deleted fanart",
				slog.String("artist_id", artistID),
				slog.String("file", paths[idx]))
		}
	}

	// Collect surviving files in order
	var survivors []string
	for i, p := range paths {
		if !deleteSet[i] {
			survivors = append(survivors, p)
		}
	}

	// Re-number survivors sequentially
	for i, oldPath := range survivors {
		newName := img.FanartFilename(primary, i, kodi)
		// Preserve actual extension from the existing file
		actualExt := filepath.Ext(oldPath)
		newBase := strings.TrimSuffix(newName, filepath.Ext(newName))
		newName = newBase + actualExt
		newPath := filepath.Join(a.Path, newName)
		if oldPath != newPath {
			if renameErr := os.Rename(oldPath, newPath); renameErr != nil {
				r.logger.Warn("renaming fanart during re-number",
					slog.String("from", oldPath),
					slog.String("to", newPath),
					slog.String("error", renameErr.Error()))
			}
		}
	}

	r.updateArtistFanartCount(req.Context(), a)

	if isHTMXRequest(req) {
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "deleted",
		"deleted": deleted,
		"count":   a.FanartCount,
	})
}

// handleFanartBatchFetch downloads multiple URLs and saves them as additional fanart.
// POST /api/v1/artists/{id}/images/fanart/fetch-batch
func (r *Router) handleFanartBatchFetch(w http.ResponseWriter, req *http.Request) {
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
	if !r.requireArtistPath(w, req, a) {
		return
	}

	var body struct {
		URLs []string `json:"urls"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if len(body.URLs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no urls specified"})
		return
	}

	var allSaved []string
	var errors []string
	for _, u := range body.URLs {
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			errors = append(errors, fmt.Sprintf("invalid url: %s", u))
			continue
		}
		data, fetchErr := r.fetchImageFromURL(u)
		if fetchErr != nil {
			r.logger.Warn("fetching fanart image", "url", u, "error", fetchErr)
			errors = append(errors, fmt.Sprintf("fetch failed: %s", u))
			continue
		}
		saved, _, saveErr := r.processAndAppendFanart(req.Context(), a.Path, data)
		if saveErr != nil {
			r.logger.Error("saving fanart image", "url", u, "error", saveErr)
			errors = append(errors, fmt.Sprintf("save failed: %s", u))
			continue
		}
		allSaved = append(allSaved, saved...)
	}

	r.updateArtistFanartCount(req.Context(), a)

	if isHTMXRequest(req) {
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"saved":  allSaved,
		"errors": errors,
		"count":  a.FanartCount,
	})
}
