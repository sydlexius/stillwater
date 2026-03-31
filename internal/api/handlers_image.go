package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
	"github.com/sydlexius/stillwater/internal/connection/jellyfin"
	"github.com/sydlexius/stillwater/internal/event"
	img "github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/web/templates"
)

const (
	maxUploadSize = 25 << 20 // 25 MB
	fetchTimeout  = 30 * time.Second
)

// imageDir returns the directory where images for this artist should be stored
// and served from. If the artist has a filesystem path (from a local library scan),
// that path is used. Otherwise, a managed cache directory under the data volume
// is returned so that platform-sourced images can be saved and served without
// requiring direct filesystem access to the music library.
func (r *Router) imageDir(a *artist.Artist) string {
	if a.Path != "" {
		return a.Path
	}
	if r.imageCacheDir != "" && a.ID != "" {
		return filepath.Join(r.imageCacheDir, a.ID)
	}
	return ""
}

// requireArtistPath checks that the artist has a filesystem path.
// Use for NFO and other filesystem operations that need a.Path.
func (r *Router) requireArtistPath(w http.ResponseWriter, req *http.Request, a *artist.Artist) bool {
	if a.Path == "" {
		writeError(w, req, http.StatusUnprocessableEntity,
			"filesystem operations are not available for this artist (library has no path configured)")
		return false
	}
	return true
}

// requireImageDir checks that the artist has an image directory (either a
// filesystem path or a cache dir). Use for image operations.
func (r *Router) requireImageDir(w http.ResponseWriter, req *http.Request, a *artist.Artist) bool {
	dir := r.imageDir(a)
	if dir == "" {
		writeError(w, req, http.StatusUnprocessableEntity,
			"no image directory available for this artist (no filesystem path or cache configured)")
		return false
	}
	// Ensure directory exists (cache dirs may not exist yet).
	if a.Path == "" {
		if err := os.MkdirAll(dir, 0o750); err != nil { //nolint:gosec // dir from imageDir(), trusted config + artist ID
			writeError(w, req, http.StatusInternalServerError, "failed to create image directory")
			return false
		}
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
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "artist not found"})
		return
	}
	if !r.requireImageDir(w, req, a) {
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

	// Check whether the image dimensions match the slot's required aspect ratio.
	// If mismatched, return the image data to the client for cropping instead of saving.
	skipCrop := req.URL.Query().Get("skip_crop") == "true"
	if !skipCrop {
		w2, h2, dimErr := img.GetDimensions(bytes.NewReader(data))
		if dimErr == nil {
			geo := img.CheckGeometry(w2, h2, imageType)
			if geo.NeedsCrop {
				encoded := base64.StdEncoding.EncodeToString(data)
				detectedCT := http.DetectContentType(data)
				if detectedCT == "application/octet-stream" && ct != "" {
					detectedCT = ct
				}
				dataURI := "data:" + detectedCT + ";base64," + encoded
				writeJSON(w, http.StatusOK, map[string]any{
					"status":         "needs_crop",
					"needs_crop":     true,
					"required_ratio": geo.RequiredRatio,
					"actual_ratio":   geo.ActualRatio,
					"width":          geo.Width,
					"height":         geo.Height,
					"image_data":     dataURI,
					"type":           imageType,
				})
				return
			}
		}
	}

	// User uploads always get "user" source provenance.
	uploadMeta := &img.ExifMeta{Source: "user", Fetched: time.Now().UTC(), Mode: "user"}

	// Fanart: append as next numbered file when fanart already exists.
	if imageType == "fanart" && a.FanartExists {
		saved, saveErr := r.processAndAppendFanart(req.Context(), r.imageDir(a), data, uploadMeta)
		if saveErr != nil {
			r.logger.Error("appending fanart upload", "artist_id", artistID, "error", saveErr)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save image"})
			return
		}
		r.enforceCacheLimitIfNeeded(req.Context(), a)
		r.updateArtistFanartCount(req.Context(), a)
		if r.eventBus != nil {
			r.eventBus.Publish(event.Event{
				Type: event.ArtistUpdated,
				Data: map[string]any{"artist_id": a.ID},
			})
		}
		r.InvalidateHealthCache()
		// Skip platform sync for fanart appends: platforms only support a single
		// backdrop image, and the primary (fanart.jpg) was already synced when
		// first saved. Re-syncing here would re-push the primary, not the new
		// variant (fanart2.jpg etc.), because syncImageToPlatforms discovers
		// files via findExistingImage which always returns the primary.
		writeJSON(w, http.StatusOK, map[string]any{
			"status":        "ok",
			"saved":         saved,
			"type":          imageType,
			"count":         a.FanartCount,
			"sync_warnings": []string{},
		})
		return
	}

	saved, err := r.processAndSaveImage(req.Context(), r.imageDir(a), imageType, data, uploadMeta)
	if err != nil {
		r.logger.Error("saving uploaded image", "artist_id", artistID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save image"})
		return
	}
	r.enforceCacheLimitIfNeeded(req.Context(), a)

	r.updateArtistImageFlag(req.Context(), a, imageType)
	if imageType == "fanart" {
		r.updateArtistFanartCount(req.Context(), a)
	}

	// Publish event and invalidate cache before platform sync (which can
	// take up to 30s) so health scores update within the 5-second target.
	if r.eventBus != nil {
		r.eventBus.Publish(event.Event{
			Type: event.ArtistUpdated,
			Data: map[string]any{"artist_id": a.ID},
		})
	}
	r.InvalidateHealthCache()

	syncCtx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
	defer cancel()
	warnings := r.publisher.SyncImageToPlatforms(syncCtx, a, imageType)

	resp := map[string]any{
		"status":        "ok",
		"saved":         saved,
		"type":          imageType,
		"sync_warnings": warnings,
	}
	if imageType == "fanart" {
		resp["count"] = a.FanartCount
	}
	setSyncWarningTrigger(w, warnings)
	writeJSON(w, http.StatusOK, resp)
}

// handleImageFetch fetches an image from a URL and saves it for the artist.
// POST /api/v1/artists/{id}/images/fetch
func (r *Router) handleImageFetch(w http.ResponseWriter, req *http.Request) {
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "artist not found"})
		return
	}
	if !r.requireImageDir(w, req, a) {
		return
	}

	imageURL, imageType, err := extractImageFetchParams(req)
	if err != nil {
		r.logger.Debug("invalid image fetch request body",
			slog.String("artist_id", artistID),
			slog.String("error", err.Error()))
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

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
	if isPrivateURL(req.Context(), imageURL) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url points to a private or reserved address"})
		return
	}

	data, err := r.fetchImageFromURL(imageURL)
	if err != nil {
		r.logger.Warn("fetching image from URL", "url", imageURL, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("failed to fetch image: %v", err)})
		return
	}

	// Check geometry before saving. If the aspect ratio does not match the slot
	// requirement, return the fetched image data for client-side cropping.
	skipCrop := req.URL.Query().Get("skip_crop") == "true"
	if !skipCrop && !isHTMXRequest(req) {
		w2, h2, dimErr := img.GetDimensions(bytes.NewReader(data))
		if dimErr == nil {
			geo := img.CheckGeometry(w2, h2, imageType)
			if geo.NeedsCrop {
				format, _, _ := img.DetectFormat(bytes.NewReader(data))
				var mimeType string
				switch format {
				case img.FormatPNG:
					mimeType = "image/png"
				case img.FormatWebP:
					mimeType = "image/webp"
				default:
					mimeType = "image/jpeg"
				}
				encoded := base64.StdEncoding.EncodeToString(data)
				dataURI := "data:" + mimeType + ";base64," + encoded
				writeJSON(w, http.StatusOK, map[string]any{
					"status":         "needs_crop",
					"needs_crop":     true,
					"required_ratio": geo.RequiredRatio,
					"actual_ratio":   geo.ActualRatio,
					"width":          geo.Width,
					"height":         geo.Height,
					"image_data":     dataURI,
					"type":           imageType,
				})
				return
			}
		}
	}

	fetchMeta := &img.ExifMeta{Source: "user", Fetched: time.Now().UTC(), URL: imageURL, Mode: "user"}

	// Fanart: append as next numbered file when fanart already exists.
	if imageType == "fanart" && a.FanartExists {
		saved, saveErr := r.processAndAppendFanart(req.Context(), r.imageDir(a), data, fetchMeta)
		if saveErr != nil {
			r.logger.Error("appending fanart image", "artist_id", artistID, "error", saveErr)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save image"})
			return
		}
		r.enforceCacheLimitIfNeeded(req.Context(), a)
		r.updateArtistFanartCount(req.Context(), a)
		r.InvalidateHealthCache()

		if r.eventBus != nil {
			r.eventBus.Publish(event.Event{
				Type: event.ArtistUpdated,
				Data: map[string]any{"artist_id": a.ID},
			})
		}

		syncCtx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
		defer cancel()
		syncWarnings := r.publisher.SyncAllFanartToPlatforms(syncCtx, a)

		if isHTMXRequest(req) {
			setSyncWarningTrigger(w, syncWarnings)
			w.Header().Set("HX-Refresh", "true")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":        "ok",
			"saved":         saved,
			"type":          imageType,
			"count":         a.FanartCount,
			"sync_warnings": syncWarnings,
		})
		return
	}

	saved, err := r.processAndSaveImage(req.Context(), r.imageDir(a), imageType, data, fetchMeta)
	if err != nil {
		r.logger.Error("saving fetched image", "artist_id", artistID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save image"})
		return
	}
	r.enforceCacheLimitIfNeeded(req.Context(), a)

	r.updateArtistImageFlag(req.Context(), a, imageType)
	// Sync fanart count after initial save
	if imageType == "fanart" {
		r.updateArtistFanartCount(req.Context(), a)
	}

	if r.eventBus != nil {
		r.eventBus.Publish(event.Event{
			Type: event.ArtistUpdated,
			Data: map[string]any{"artist_id": a.ID},
		})
	}
	r.InvalidateHealthCache()

	syncCtx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
	defer cancel()
	warnings := r.publisher.SyncImageToPlatforms(syncCtx, a, imageType)

	if isHTMXRequest(req) {
		setSyncWarningTrigger(w, warnings)
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	resp := map[string]any{
		"status":        "ok",
		"saved":         saved,
		"type":          imageType,
		"sync_warnings": warnings,
	}
	if imageType == "fanart" {
		resp["count"] = a.FanartCount
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleImageSearch searches for images from all providers for an artist.
// GET /api/v1/artists/{id}/images/search?type=thumb
func (r *Router) handleImageSearch(w http.ResponseWriter, req *http.Request) {
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
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

	result, err := r.orchestrator.FetchImages(req.Context(), a.MusicBrainzID, a.ProviderIDMap())
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
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
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

// handleImageCrop accepts base64-encoded image data, optionally applies a server-side crop
// when coordinates are provided, saves the result, then syncs it to all configured platform
// connections. Sync failures are returned as non-blocking warnings in the response.
// POST /api/v1/artists/{id}/images/crop
func (r *Router) handleImageCrop(w http.ResponseWriter, req *http.Request) {
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "artist not found"})
		return
	}
	if !r.requireImageDir(w, req, a) {
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
	if !DecodeJSON(w, req, &body) {
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

	// Preserve existing provenance metadata if present, updating the timestamp.
	var cropMeta *img.ExifMeta
	patterns := r.getActiveNamingConfig(req.Context(), body.Type)
	if filePath, found := img.FindExistingImage(r.imageDir(a), patterns); found {
		if existing, readErr := img.ReadProvenance(filePath); readErr == nil && existing != nil {
			cropMeta = existing
		} else if readErr != nil {
			r.logger.Debug("could not read existing provenance for crop, using fresh metadata",
				slog.String("artist_id", artistID), slog.String("path", filePath), slog.String("error", readErr.Error()))
		}
	}
	if cropMeta == nil {
		cropMeta = &img.ExifMeta{Source: "user"}
	}
	cropMeta.Fetched = time.Now().UTC()
	cropMeta.Mode = "user"
	cropMeta.DHash = "" // Force recomputation from the cropped image data.

	saved, err := r.processAndSaveImage(req.Context(), r.imageDir(a), body.Type, imgData, cropMeta)
	if err != nil {
		r.logger.Error("saving cropped image", "artist_id", artistID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save image"})
		return
	}
	r.enforceCacheLimitIfNeeded(req.Context(), a)

	r.updateArtistImageFlag(req.Context(), a, body.Type)

	if r.eventBus != nil {
		r.eventBus.Publish(event.Event{
			Type: event.ArtistUpdated,
			Data: map[string]any{"artist_id": a.ID},
		})
	}
	r.InvalidateHealthCache()

	syncCtx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
	defer cancel()
	warnings := r.publisher.SyncImageToPlatforms(syncCtx, a, body.Type)

	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "ok",
		"saved":         saved,
		"type":          body.Type,
		"sync_warnings": warnings,
	})
}

// processAndSaveImage processes image data (convert format, optimize) and saves it.
// For logos, transparent borders are automatically trimmed before saving.
// meta is optional EXIF provenance metadata to embed in the saved image.
func (r *Router) processAndSaveImage(ctx context.Context, dir string, imageType string, data []byte, meta *img.ExifMeta) ([]string, error) {
	converted, _, err := img.ConvertFormat(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("converting format: %w", err)
	}

	// Logos: trim transparent borders so the image renders without padding.
	if imageType == "logo" {
		if trimmed, _, trimErr := img.TrimAlpha(bytes.NewReader(converted), 10); trimErr == nil {
			converted = trimmed
		}
	}

	naming, useSymlinks := r.getActiveNamingAndSymlinks(ctx, imageType)

	// Register expected write paths so the filesystem watcher can
	// distinguish Stillwater's own writes from external ones.
	if r.expectedWrites != nil {
		expectedPaths := img.ExpectedPaths(dir, naming)
		r.expectedWrites.AddAll(expectedPaths)
		defer r.expectedWrites.RemoveAll(expectedPaths)
	}

	saved, err := img.Save(dir, imageType, converted, naming, useSymlinks, meta, r.logger)
	if err != nil {
		return nil, fmt.Errorf("saving: %w", err)
	}

	return saved, nil
}

// getActiveNamingConfig returns the filenames for the given image type from the
// active platform profile. Returns the full array so that image saves write
// copies for every configured filename.
// getActiveProfileName returns the name of the currently active platform profile
// (e.g. "Kodi", "Emby", "Jellyfin"). Returns empty string if no profile is active.
func (r *Router) getActiveProfileName(ctx context.Context) string {
	if r.platformService == nil {
		return ""
	}
	profile, err := r.platformService.GetActive(ctx)
	if err != nil || profile == nil {
		return ""
	}
	return profile.Name
}

func (r *Router) getActiveNamingConfig(ctx context.Context, imageType string) []string {
	names, _ := r.getActiveNamingAndSymlinks(ctx, imageType)
	return names
}

// getActiveNamingAndSymlinks returns the filenames for the given image type and
// the UseSymlinks flag from the active platform profile.
func (r *Router) getActiveNamingAndSymlinks(ctx context.Context, imageType string) ([]string, bool) {
	if r.platformService == nil {
		return img.FileNamesForType(img.DefaultFileNames, imageType), false
	}
	profile, err := r.platformService.GetActive(ctx)
	if err != nil || profile == nil {
		return img.FileNamesForType(img.DefaultFileNames, imageType), false
	}
	names := profile.ImageNaming.NamesForType(imageType)
	if len(names) == 0 {
		return img.FileNamesForType(img.DefaultFileNames, imageType), profile.UseSymlinks
	}
	return names, profile.UseSymlinks
}

// isPrivateURL returns true if the URL's hostname resolves to a loopback,
// private, link-local, or unspecified IP address.
func isPrivateURL(ctx context.Context, rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return true
	}
	host := parsed.Hostname()
	resolver := net.DefaultResolver
	addrs, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return true
	}
	for _, addr := range addrs {
		ip := addr.IP
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
			ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			return true
		}
	}
	return false
}

// ssrfSafeTransport returns an http.Transport that validates resolved IPs at
// connection time, preventing TOCTOU / DNS-rebinding attacks where the hostname
// resolves to a safe address during the isPrivateURL pre-check but to a
// private address when the actual connection is made.
//
// It clones http.DefaultTransport to preserve TLS timeouts, idle connection
// settings, proxy support, and HTTP/2 -- only the DialContext is overridden.
func ssrfSafeTransport() *http.Transport {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		base = &http.Transport{}
	}
	t := base.Clone()
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("DNS lookup for %s returned no addresses", host)
		}
		for _, ip := range ips {
			if ip.IP.IsLoopback() || ip.IP.IsPrivate() || ip.IP.IsLinkLocalUnicast() ||
				ip.IP.IsLinkLocalMulticast() || ip.IP.IsUnspecified() {
				return nil, fmt.Errorf("resolved address %s is private or reserved", ip.IP)
			}
		}
		// Connect to the first safe IP directly to avoid re-resolution.
		return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
	}
	return t
}

// fetchImageFromURL downloads an image from the given URL with timeout and size limits.
func (r *Router) fetchImageFromURL(rawURL string) ([]byte, error) {
	client := r.ssrfClient

	req, err := http.NewRequest(http.MethodGet, rawURL, nil) //nolint:gosec,noctx // G107/G704: URL is validated by caller; background fetch is acceptable
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	// Wikimedia Commons blocks requests without a proper User-Agent.
	req.Header.Set("User-Agent", "Stillwater/1.0 (https://github.com/sydlexius/stillwater)")

	resp, err := client.Do(req) //nolint:gosec // G107/G704: URL is validated by caller
	if err != nil {
		return nil, fmt.Errorf("fetching: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
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
		_, _ = io.Copy(io.Discard, resp.Body)
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

// setArtistImageFlag sets the image existence, low-resolution, and placeholder flags and persists them.
// When exists is true the image file is probed for dimensions and a LQIP placeholder is generated.
// After persisting, provenance metadata (phash, source, file format, mtime) is read from the
// image file and recorded in the artist_images table.
// When exists is false all flags and the placeholder are cleared.
func (r *Router) setArtistImageFlag(ctx context.Context, a *artist.Artist, imageType string, exists bool) {
	var lowRes bool
	var placeholder string
	var resolvedPath string // path to the image file on disk, used for provenance readback
	if exists {
		patterns := r.getActiveNamingConfig(ctx, imageType)
		if filePath, found := img.FindExistingImage(r.imageDir(a), patterns); found {
			if f, openErr := os.Open(filePath); openErr == nil { //nolint:gosec // path from trusted naming patterns
				resolvedPath = filePath
				defer f.Close() //nolint:errcheck
				w, h, dimErr := img.GetDimensions(f)
				if dimErr != nil {
					r.logger.Warn("reading image dimensions",
						slog.String("artist_id", a.ID),
						slog.String("image_type", imageType),
						slog.String("error", dimErr.Error()))
				}
				lowRes = img.IsLowResolution(w, h, imageType)
				if _, seekErr := f.Seek(0, io.SeekStart); seekErr != nil {
					r.logger.Warn("seeking image file for placeholder",
						slog.String("artist_id", a.ID),
						slog.String("image_type", imageType),
						slog.String("error", seekErr.Error()))
				} else {
					ph, phErr := img.GeneratePlaceholder(f, imageType)
					if phErr != nil {
						r.logger.Warn("generating image placeholder",
							slog.String("artist_id", a.ID),
							slog.String("image_type", imageType),
							slog.String("error", phErr.Error()))
					} else {
						placeholder = ph
					}
				}
			} else {
				r.logger.Warn("opening image file",
					slog.String("artist_id", a.ID),
					slog.String("image_type", imageType),
					slog.String("path", filePath),
					slog.String("error", openErr.Error()))
			}
		}
	}

	switch imageType {
	case "thumb":
		a.ThumbExists = exists
		a.ThumbLowRes = lowRes
		if placeholder != "" || !exists {
			a.ThumbPlaceholder = placeholder
		}
	case "fanart":
		a.FanartExists = exists
		a.FanartLowRes = lowRes
		if placeholder != "" || !exists {
			a.FanartPlaceholder = placeholder
		}
	case "logo":
		a.LogoExists = exists
		a.LogoLowRes = lowRes
		if placeholder != "" || !exists {
			a.LogoPlaceholder = placeholder
		}
	case "banner":
		a.BannerExists = exists
		a.BannerLowRes = lowRes
		if placeholder != "" || !exists {
			a.BannerPlaceholder = placeholder
		}
	}

	if err := r.artistService.Update(ctx, a); err != nil {
		r.logger.Warn("setting artist image flag",
			slog.String("artist_id", a.ID),
			slog.String("image_type", imageType),
			slog.String("error", err.Error()))
		return
	}

	// Record provenance evidence (phash, source, file format, write timestamp)
	// from the saved image file. This is supplementary data -- failures here are
	// logged as warnings but do not affect the image save operation.
	if resolvedPath != "" {
		r.recordImageProvenance(ctx, a.ID, imageType, resolvedPath)
	}
}

// updateArtistImageFlag sets the image existence flag to true and persists it.
func (r *Router) updateArtistImageFlag(ctx context.Context, a *artist.Artist, imageType string) {
	r.setArtistImageFlag(ctx, a, imageType, true)
}

// recordImageProvenance reads Stillwater provenance metadata and file mtime from
// the image at filePath, then records the phash, source, file format, and write
// timestamp in the artist_images table. Errors are logged as warnings -- this
// is supplementary evidence collection and must not fail the image save.
func (r *Router) recordImageProvenance(ctx context.Context, artistID, imageType, filePath string) {
	log := r.logger.With(
		slog.String("artist_id", artistID),
		slog.String("image_type", imageType),
		slog.String("path", filePath),
	)

	d := img.CollectProvenance(filePath, log)
	if d.IsEmpty() {
		log.Warn("no provenance data collected, skipping update")
		return
	}
	if err := r.artistService.UpdateImageProvenance(ctx, artistID, imageType, 0, d.PHash, d.Source, d.FileFormat, d.LastWrittenAt); err != nil {
		log.Warn("recording image provenance",
			slog.String("error", err.Error()))
	}
}

// handleServeImage serves a local artist image file from disk.
// GET /api/v1/artists/{id}/images/{type}/file
func (r *Router) handleServeImage(w http.ResponseWriter, req *http.Request) {
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
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

	dir := r.imageDir(a)
	if dir == "" {
		http.NotFound(w, req)
		return
	}

	patterns := r.getActiveNamingConfig(req.Context(), imageType)
	filePath, found := img.FindExistingImage(dir, patterns)
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
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
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

	dir := r.imageDir(a)
	if dir == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "image not found"})
		return
	}

	patterns := r.getActiveNamingConfig(req.Context(), imageType)
	filePath, found := img.FindExistingImage(dir, patterns)
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
		var dimErr error
		width, height, dimErr = img.GetDimensions(f)
		if dimErr != nil {
			r.logger.Warn("reading image dimensions for info",
				slog.String("artist_id", artistID),
				slog.String("image_type", imageType),
				slog.String("path", filePath),
				slog.String("error", dimErr.Error()))
		}
	} else {
		r.logger.Warn("opening image for info",
			slog.String("artist_id", artistID),
			slog.String("image_type", imageType),
			slog.String("path", filePath),
			slog.String("error", err.Error()))
	}

	if isHTMXRequest(req) {
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
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
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
	if !r.requireImageDir(w, req, a) {
		return
	}

	// For fanart, delete ALL numbered variants as well.
	if imageType == "fanart" {
		primary := r.getActiveFanartPrimary(req.Context())
		fanartPaths, fanartErr := img.DiscoverFanart(r.imageDir(a), primary)
		if fanartErr != nil {
			r.logger.Error("discovering fanart for delete",
				slog.String("artist_id", artistID),
				slog.String("error", fanartErr.Error()))
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read fanart directory"})
			return
		}
		var deleted []string
		var removeFailed bool
		for _, p := range fanartPaths {
			if err := r.fileRemover.Remove(p); err == nil { //nolint:gosec // path from DiscoverFanart, not user input
				deleted = append(deleted, filepath.Base(p))
				r.logger.Info("deleted fanart", slog.String("path", p))
			} else {
				removeFailed = true
				r.logger.Warn("failed to delete fanart",
					slog.String("path", p),
					slog.String("error", err.Error()))
			}
		}
		// Also clean up the standard naming config patterns (alternate names)
		patterns := r.getActiveNamingConfig(req.Context(), imageType)
		patternDeleted, patternFailed := deleteImageFiles(r.fileRemover, r.imageDir(a), patterns, r.logger)
		deleted = append(deleted, patternDeleted...)
		if patternFailed {
			removeFailed = true
		}
		r.updateArtistFanartCount(req.Context(), a)
		if r.eventBus != nil {
			r.eventBus.Publish(event.Event{
				Type: event.ArtistUpdated,
				Data: map[string]any{"artist_id": a.ID},
			})
		}
		r.InvalidateHealthCache()
		fanartWarnings := make([]string, 0)
		if removeFailed {
			fanartWarnings = append(fanartWarnings, "some fanart files could not be deleted from disk")
		}
		if len(deleted) > 0 && !removeFailed {
			delCtx, delCancel := context.WithTimeout(req.Context(), 30*time.Second)
			defer delCancel()
			fanartWarnings = append(fanartWarnings, r.deleteImageFromPlatforms(delCtx, a, imageType)...)
		}
		if isHTMXRequest(req) {
			setSyncWarningTrigger(w, fanartWarnings)
			renderTempl(w, req, templates.ImagePreviewCard(a.ID, imageType, false, imageTypeLabel(imageType), 0))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":        "deleted",
			"deleted":       deleted,
			"sync_warnings": fanartWarnings,
		})
		return
	}

	patterns := r.getActiveNamingConfig(req.Context(), imageType)
	deleted, deleteFailed := deleteImageFiles(r.fileRemover, r.imageDir(a), patterns, r.logger)

	if _, found := img.FindExistingImage(r.imageDir(a), patterns); !found {
		r.clearArtistImageFlag(req.Context(), a, imageType)
	}
	if r.eventBus != nil {
		r.eventBus.Publish(event.Event{
			Type: event.ArtistUpdated,
			Data: map[string]any{"artist_id": a.ID},
		})
	}
	r.InvalidateHealthCache()
	warnings := make([]string, 0)
	if deleteFailed {
		warnings = append(warnings, "some image files could not be deleted from disk")
	}
	if len(deleted) > 0 && !deleteFailed {
		delCtx, delCancel := context.WithTimeout(req.Context(), 30*time.Second)
		defer delCancel()
		warnings = append(warnings, r.deleteImageFromPlatforms(delCtx, a, imageType)...)
	}

	if isHTMXRequest(req) {
		setSyncWarningTrigger(w, warnings)
		renderTempl(w, req, templates.ImagePreviewCard(a.ID, imageType, false, imageTypeLabel(imageType), 0))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "deleted",
		"deleted":       deleted,
		"sync_warnings": warnings,
	})
}

// clearArtistImageFlag sets the image existence flag to false and persists it.
func (r *Router) clearArtistImageFlag(ctx context.Context, a *artist.Artist, imageType string) {
	r.setArtistImageFlag(ctx, a, imageType, false)
}

// deleteImageFromPlatforms removes the image from every platform connection that
// has a stored artist ID mapping. Errors are logged and returned as warning
// strings so the caller can surface them to the client. The local operation
// already succeeded, so failures here are non-fatal.
func (r *Router) deleteImageFromPlatforms(ctx context.Context, a *artist.Artist, imageType string) []string {
	warnings := make([]string, 0)

	platformIDs, err := r.artistService.GetPlatformIDs(ctx, a.ID)
	if err != nil {
		r.logger.Error("getting platform IDs for image delete sync", "artist_id", a.ID, "type", imageType, "error", err)
		warnings = append(warnings, "platform delete sync skipped: failed to load platform mappings")
		return warnings
	}
	if len(platformIDs) == 0 {
		return warnings
	}

	for _, pid := range platformIDs {
		conn, connErr := r.connectionService.GetByID(ctx, pid.ConnectionID)
		if connErr != nil {
			r.logger.Error("getting connection for image delete sync", "connection_id", pid.ConnectionID, "error", connErr)
			warnings = append(warnings, truncateWarning(fmt.Sprintf("connection %s: failed to load", pid.ConnectionID)))
			continue
		}
		if !conn.Enabled {
			r.logger.Debug("skipping disabled connection for image delete sync", "connection", conn.Name, "type", imageType)
			continue
		}

		var deleter connection.ImageDeleter
		switch conn.Type {
		case connection.TypeEmby:
			deleter = emby.New(conn.URL, conn.APIKey, conn.PlatformUserID, r.logger)
		case connection.TypeJellyfin:
			deleter = jellyfin.New(conn.URL, conn.APIKey, conn.PlatformUserID, r.logger)
		default:
			r.logger.Warn("unsupported connection type for image delete sync", "type", conn.Type)
			warnings = append(warnings, truncateWarning(fmt.Sprintf("%s: unsupported connection type %q", conn.Name, conn.Type)))
			continue
		}

		if delErr := deleter.DeleteImage(ctx, pid.PlatformArtistID, imageType); delErr != nil {
			r.logger.Error("deleting image from platform", "artist", a.Name, "connection", conn.Name, "type", imageType, "error", delErr)
			warnings = append(warnings, truncateWarning(fmt.Sprintf("%s (%s): image delete failed", conn.Name, conn.Type)))
		}
	}
	return warnings
}

const (
	maxWarningRunes = 200
	maxHeaderBytes  = 1000
)

// setSyncWarningTrigger encodes sync warnings as an HX-Trigger header so the
// HTMX frontend can display them as non-blocking toast notifications.
// Truncation is applied in two stages: first, this function caps each warning
// at maxWarningRunes runes. Second, if the full JSON payload still exceeds
// maxHeaderBytes, all individual messages are replaced with a single summary
// count string. Both limits prevent HTTP 431 (Request Header Fields Too Large)
// errors from intermediary proxies.
func setSyncWarningTrigger(w http.ResponseWriter, warnings []string) {
	if len(warnings) == 0 {
		return
	}
	truncated := make([]string, len(warnings))
	for i, msg := range warnings {
		if runes := []rune(msg); len(runes) > maxWarningRunes {
			truncated[i] = string(runes[:maxWarningRunes]) + " (truncated)"
		} else {
			truncated[i] = msg
		}
	}
	// The two json.Marshal calls in this function are guaranteed to succeed:
	// map[string][]string contains no values that json.Marshal rejects.
	// Errors are intentionally ignored.
	data, _ := json.Marshal(map[string][]string{"syncWarning": truncated})
	if len(data) > maxHeaderBytes {
		data, _ = json.Marshal(map[string][]string{
			"syncWarning": {fmt.Sprintf("%d platform sync warnings (see server log for details)", len(warnings))},
		})
	}
	w.Header().Set("HX-Trigger", string(data))
}

// truncateWarning caps a warning string at maxWarningRunes so that platform
// error messages (which may embed full HTTP response bodies) cannot inflate
// JSON response bodies or HX-Trigger headers to unreasonable sizes.
func truncateWarning(msg string) string {
	if runes := []rune(msg); len(runes) > maxWarningRunes {
		return string(runes[:maxWarningRunes]) + " (truncated)"
	}
	return msg
}

// deleteImageFiles removes all matching image files from a directory and returns deleted filenames
// and whether any removal failed. For each pattern, it also probes alternate extensions
// (.jpg, .jpeg, .png) to catch cases where the saved format differs from the configured name.
// Patterns are trusted naming conventions. Files that do not exist are not counted as failures.
func deleteImageFiles(remover FileRemover, dir string, patterns []string, logger *slog.Logger) (deleted []string, failed bool) {
	for _, pattern := range patterns {
		p := filepath.Join(dir, pattern)
		if err := remover.Remove(p); err == nil { //nolint:gosec // path from trusted naming patterns
			logger.Info("deleted image file", slog.String("path", p))
			deleted = append(deleted, pattern)
		} else if !errors.Is(err, os.ErrNotExist) {
			failed = true
			logger.Warn("failed to delete image file",
				slog.String("path", p),
				slog.String("error", err.Error()))
		}
		// Also try alternate extensions in case format diverged from config.
		base := strings.TrimSuffix(pattern, filepath.Ext(pattern))
		for _, ext := range []string{".jpg", ".jpeg", ".png"} {
			if ext == filepath.Ext(pattern) {
				continue
			}
			alt := base + ext
			altPath := filepath.Join(dir, alt)
			if err := remover.Remove(altPath); err == nil { //nolint:gosec // path from trusted naming patterns
				logger.Info("deleted image file (alt ext)", slog.String("path", altPath))
				deleted = append(deleted, alt)
			} else if !errors.Is(err, os.ErrNotExist) {
				failed = true
				logger.Warn("failed to delete image file (alt ext)",
					slog.String("path", altPath),
					slog.String("error", err.Error()))
			}
		}
	}
	return deleted, failed
}

// extractImageFetchParams reads the URL and type from an image fetch request.
// Supports both form-encoded (HTMX) and JSON payloads (API clients).
// Provider image types (hdlogo, widethumb, background) are normalized to their
// base types (logo, thumb, fanart) for filesystem naming.
func extractImageFetchParams(req *http.Request) (string, string, error) {
	var rawURL, rawType string
	if strings.HasPrefix(req.Header.Get("Content-Type"), "application/json") {
		var body struct {
			URL  string `json:"url"`
			Type string `json:"type"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return "", "", fmt.Errorf("invalid request body: %w", err)
		}
		rawURL, rawType = body.URL, body.Type
	} else {
		rawURL, rawType = req.FormValue("url"), req.FormValue("type")
	}
	return rawURL, normalizeImageType(rawType), nil
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
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "artist not found"})
		return
	}
	if !r.requireImageDir(w, req, a) {
		return
	}

	patterns := r.getActiveNamingConfig(req.Context(), "logo")
	filePath, found := img.FindExistingImage(r.imageDir(a), patterns)
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

	// Preserve existing provenance metadata if present, updating the rule and timestamp.
	var trimMeta *img.ExifMeta
	if existing, readErr := img.ReadProvenance(filePath); readErr == nil && existing != nil {
		trimMeta = existing
		trimMeta.Rule = "logo_trim_api"
	} else {
		trimMeta = &img.ExifMeta{Rule: "logo_trim_api"}
	}
	trimMeta.Fetched = time.Now().UTC()
	trimMeta.Mode = "user"
	trimMeta.DHash = "" // Force recomputation from the trimmed image data.

	_, useSymlinks := r.getActiveNamingAndSymlinks(req.Context(), "logo")
	if _, err := img.Save(r.imageDir(a), "logo", trimmed, patterns, useSymlinks, trimMeta, r.logger); err != nil {
		r.logger.Error("saving trimmed logo", "artist_id", artistID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save trimmed logo"})
		return
	}
	r.enforceCacheLimitIfNeeded(req.Context(), a)

	r.updateArtistImageFlag(req.Context(), a, "logo")
	if r.eventBus != nil {
		r.eventBus.Publish(event.Event{
			Type: event.ArtistUpdated,
			Data: map[string]any{"artist_id": a.ID},
		})
	}
	r.InvalidateHealthCache()

	syncCtx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
	defer cancel()
	warnings := r.publisher.SyncImageToPlatforms(syncCtx, a, "logo")

	if isHTMXRequest(req) {
		setSyncWarningTrigger(w, warnings)
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "ok",
		"sync_warnings": warnings,
	})
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
	return strings.EqualFold(profile.ID, "kodi")
}

// processAndAppendFanart processes image data and saves it as the next
// numbered fanart file. Returns the saved filenames.
// meta is optional EXIF provenance metadata to embed in the saved image.
func (r *Router) processAndAppendFanart(ctx context.Context, dir string, data []byte, meta *img.ExifMeta) ([]string, error) {
	converted, _, err := img.ConvertFormat(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("converting format: %w", err)
	}

	primary := r.getActiveFanartPrimary(ctx)
	kodi := r.isKodiNumbering(ctx)
	maxIdx, err := img.MaxFanartIndex(dir, primary)
	if err != nil {
		return nil, fmt.Errorf("scanning fanart: %w", err)
	}
	nextIndex := img.NextFanartIndex(maxIdx, kodi)
	nextName := img.FanartFilename(primary, nextIndex, kodi)

	// Register expected write paths so the filesystem watcher can
	// distinguish Stillwater's own writes from external ones.
	if r.expectedWrites != nil {
		expectedPaths := img.ExpectedPaths(dir, []string{nextName})
		r.expectedWrites.AddAll(expectedPaths)
		defer r.expectedWrites.RemoveAll(expectedPaths)
	}

	saved, err := img.Save(dir, "fanart", converted, []string{nextName}, false, meta, r.logger)
	if err != nil {
		return nil, fmt.Errorf("saving: %w", err)
	}

	return saved, nil
}

// updateArtistFanartCount discovers fanart files and updates both the exists
// flag and count on the artist record.
func (r *Router) updateArtistFanartCount(ctx context.Context, a *artist.Artist) {
	primary := r.getActiveFanartPrimary(ctx)
	existing, discoverErr := img.DiscoverFanart(r.imageDir(a), primary)
	if discoverErr != nil {
		r.logger.Warn("discovering fanart for count update; skipping DB update",
			slog.String("artist_id", a.ID),
			slog.String("error", discoverErr.Error()))
		return
	}
	count := len(existing)
	a.FanartExists = count > 0
	a.FanartCount = count

	// Update low-res flag from primary fanart
	a.FanartLowRes = false
	if count > 0 {
		if f, err := os.Open(existing[0]); err == nil { //nolint:gosec // path from discovery
			w, h, dimErr := img.GetDimensions(f)
			_ = f.Close()
			if dimErr != nil {
				r.logger.Warn("reading fanart dimensions",
					slog.String("artist_id", a.ID),
					slog.String("path", existing[0]),
					slog.String("error", dimErr.Error()))
			}
			a.FanartLowRes = img.IsLowResolution(w, h, "fanart")
		} else {
			r.logger.Warn("opening primary fanart for dimension check",
				slog.String("artist_id", a.ID),
				slog.String("path", existing[0]),
				slog.String("error", err.Error()))
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
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "artist not found"})
		return
	}

	primary := r.getActiveFanartPrimary(req.Context())
	paths, discoverErr := img.DiscoverFanart(r.imageDir(a), primary)
	if discoverErr != nil {
		r.logger.Error("discovering fanart for gallery",
			slog.String("artist_id", artistID),
			slog.String("error", discoverErr.Error()))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read fanart directory"})
		return
	}

	items := make([]templates.FanartGalleryItem, 0, len(paths))
	for i, p := range paths {
		item := templates.FanartGalleryItem{Index: i, Filename: filepath.Base(p)}
		if stat, statErr := os.Stat(p); statErr == nil { //nolint:gosec // path from DiscoverFanart, not user input
			item.Size = stat.Size()
		} else {
			r.logger.Warn("stat fanart for gallery",
				slog.String("artist_id", artistID),
				slog.String("path", p),
				slog.String("error", statErr.Error()))
		}
		if f, openErr := os.Open(p); openErr == nil { //nolint:gosec // path from discovery
			w, h, dimErr := img.GetDimensions(f)
			_ = f.Close()
			if dimErr != nil {
				r.logger.Warn("reading fanart dimensions for gallery",
					slog.String("artist_id", artistID),
					slog.String("path", p),
					slog.String("error", dimErr.Error()))
			}
			item.Width = w
			item.Height = h
		} else {
			r.logger.Warn("opening fanart for gallery",
				slog.String("artist_id", artistID),
				slog.String("path", p),
				slog.String("error", openErr.Error()))
		}
		items = append(items, item)
	}

	if isHTMXRequest(req) {
		if req.URL.Query().Get("management") == "true" {
			renderTempl(w, req, templates.FanartManagementGallery(artistID, items))
		} else {
			renderTempl(w, req, templates.FanartGallery(artistID, items))
		}
		return
	}

	writeJSON(w, http.StatusOK, items)
}

// handleServeFanartByIndex serves a specific fanart image by 0-based index.
// GET /api/v1/artists/{id}/images/fanart/{index}/file
func (r *Router) handleServeFanartByIndex(w http.ResponseWriter, req *http.Request) {
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
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
	paths, discoverErr := img.DiscoverFanart(r.imageDir(a), primary)
	if discoverErr != nil {
		r.logger.Error("discovering fanart for serve",
			slog.String("artist_id", artistID),
			slog.String("error", discoverErr.Error()))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read fanart directory"})
		return
	}
	if index >= len(paths) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "fanart index out of range"})
		return
	}

	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFile(w, req, paths[index])
}

// handleFanartBatchDelete deletes selected fanart by indices and re-numbers
// remaining files to close gaps.
// DELETE /api/v1/artists/{id}/images/fanart/batch
func (r *Router) handleFanartBatchDelete(w http.ResponseWriter, req *http.Request) {
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "artist not found"})
		return
	}
	if !r.requireImageDir(w, req, a) {
		return
	}

	var body struct {
		Indices []int `json:"indices"`
	}
	if !DecodeJSON(w, req, &body) {
		return
	}
	if len(body.Indices) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no indices specified"})
		return
	}

	primary := r.getActiveFanartPrimary(req.Context())
	kodi := r.isKodiNumbering(req.Context())
	paths, discoverErr := img.DiscoverFanart(r.imageDir(a), primary)
	if discoverErr != nil {
		r.logger.Error("discovering fanart for batch delete",
			slog.String("artist_id", artistID),
			slog.String("error", discoverErr.Error()))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read fanart directory"})
		return
	}

	// Build set of indices to delete
	deleteSet := make(map[int]bool, len(body.Indices))
	for _, idx := range body.Indices {
		if idx >= 0 && idx < len(paths) {
			deleteSet[idx] = true
		}
	}

	// Delete the selected files
	var deleted []string
	var removeFailed bool
	for idx := range deleteSet {
		if err := r.fileRemover.Remove(paths[idx]); err == nil { //nolint:gosec // path from DiscoverFanart, not user input
			deleted = append(deleted, filepath.Base(paths[idx]))
			r.logger.Info("deleted fanart",
				slog.String("artist_id", artistID),
				slog.String("file", paths[idx]))
		} else {
			removeFailed = true
			r.logger.Warn("failed to delete fanart",
				slog.String("artist_id", artistID),
				slog.String("path", paths[idx]),
				slog.String("error", err.Error()))
		}
	}

	// Collect surviving files in order
	var survivors []string
	for i, p := range paths {
		if !deleteSet[i] {
			survivors = append(survivors, p)
		}
	}

	// Re-number survivors sequentially. Skip when some deletes failed --
	// renaming survivors while un-deleted files still occupy their original
	// names risks overwriting data.
	var renumberWarning bool
	if removeFailed {
		renumberWarning = true
		r.logger.Warn("skipping fanart renumber due to failed deletes",
			slog.String("artist_id", artistID))
	} else if renumberErr := img.RenumberFanart(r.imageDir(a), primary, survivors, kodi); renumberErr != nil {
		renumberWarning = true
		r.logger.Warn("renumbering fanart after batch delete",
			slog.String("artist_id", artistID),
			slog.String("error", renumberErr.Error()))
	}

	r.updateArtistFanartCount(req.Context(), a)
	if r.eventBus != nil {
		r.eventBus.Publish(event.Event{
			Type: event.ArtistUpdated,
			Data: map[string]any{"artist_id": a.ID},
		})
	}
	r.InvalidateHealthCache()

	// Only sync to platforms if renumbering succeeded -- pushing misindexed
	// fanart would corrupt platform galleries too.
	var syncWarnings []string
	if !renumberWarning {
		syncCtx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
		defer cancel()
		syncWarnings = r.publisher.SyncAllFanartToPlatforms(syncCtx, a)
	}

	warnings := make([]string, 0)
	if removeFailed {
		warnings = append(warnings, "some fanart files could not be deleted from disk")
	}
	if renumberWarning {
		warnings = append(warnings, "fanart files could not be renumbered; gallery order may be incorrect, platform sync skipped")
	}
	warnings = append(warnings, syncWarnings...)

	if isHTMXRequest(req) {
		setSyncWarningTrigger(w, warnings)
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "deleted",
		"deleted":       deleted,
		"count":         a.FanartCount,
		"sync_warnings": warnings,
	})
}

// handleFanartBatchFetch downloads multiple URLs and saves them as additional fanart.
// POST /api/v1/artists/{id}/images/fanart/fetch-batch
func (r *Router) handleFanartBatchFetch(w http.ResponseWriter, req *http.Request) {
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "artist not found"})
		return
	}
	if !r.requireImageDir(w, req, a) {
		return
	}

	var body struct {
		URLs []string `json:"urls"`
	}
	if !DecodeJSON(w, req, &body) {
		return
	}
	if len(body.URLs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no urls specified"})
		return
	}

	// Deduplicate URLs before checking the limit so that duplicate-heavy
	// payloads are not rejected unnecessarily.
	seen := make(map[string]bool, len(body.URLs))
	var unique []string
	for _, u := range body.URLs {
		if !seen[u] {
			seen[u] = true
			unique = append(unique, u)
		}
	}
	body.URLs = unique

	const maxBatchURLs = 20
	if len(body.URLs) > maxBatchURLs {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("too many urls (max %d)", maxBatchURLs)})
		return
	}

	var allSaved []string
	var errors []string
	for _, u := range body.URLs {
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			errors = append(errors, fmt.Sprintf("invalid url: %s", u))
			continue
		}
		if isPrivateURL(req.Context(), u) {
			errors = append(errors, fmt.Sprintf("private/reserved address: %s", u))
			continue
		}
		data, fetchErr := r.fetchImageFromURL(u)
		if fetchErr != nil {
			r.logger.Warn("fetching fanart image", "url", u, "error", fetchErr)
			errors = append(errors, fmt.Sprintf("fetch failed: %s", u))
			continue
		}
		batchMeta := &img.ExifMeta{Source: "user", Fetched: time.Now().UTC(), URL: u, Mode: "user"}
		saved, saveErr := r.processAndAppendFanart(req.Context(), r.imageDir(a), data, batchMeta)
		if saveErr != nil {
			r.logger.Error("saving fanart image", "url", u, "error", saveErr)
			errors = append(errors, fmt.Sprintf("save failed: %s", u))
			continue
		}
		allSaved = append(allSaved, saved...)
	}

	if len(allSaved) > 0 {
		r.enforceCacheLimitIfNeeded(req.Context(), a)
	}
	r.updateArtistFanartCount(req.Context(), a)
	if r.eventBus != nil {
		r.eventBus.Publish(event.Event{
			Type: event.ArtistUpdated,
			Data: map[string]any{"artist_id": a.ID},
		})
	}
	r.InvalidateHealthCache()

	// Sync all fanart to connected platforms (synchronous with timeout).
	var syncWarnings []string
	if len(allSaved) > 0 {
		syncCtx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
		defer cancel()
		syncWarnings = r.publisher.SyncAllFanartToPlatforms(syncCtx, a)
	}

	if isHTMXRequest(req) {
		setSyncWarningTrigger(w, syncWarnings)
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "ok",
		"saved":         allSaved,
		"errors":        errors,
		"count":         a.FanartCount,
		"sync_warnings": syncWarnings,
	})
}

// handleRandomBackdrop picks a random artist that has a fanart image and
// redirects to its image file endpoint. Used by the ambient backdrop feature
// to display a blurred background in the layout shell.
// GET /api/v1/images/random-backdrop
func (r *Router) handleRandomBackdrop(w http.ResponseWriter, req *http.Request) {
	// Query a random artist ID that has at least one fanart image.
	var artistID string
	err := r.db.QueryRowContext(req.Context(),
		`SELECT artist_id FROM artist_images
		 WHERE image_type = 'fanart' AND slot_index = 0 AND exists_flag = 1
		 ORDER BY RANDOM() LIMIT 1`).Scan(&artistID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			r.logger.Error("random backdrop query failed", slog.String("error", err.Error()))
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		// No artists with fanart found; return 404 Not Found.
		http.NotFound(w, req)
		return
	}

	// Redirect to the standard image serve endpoint for this artist's fanart.
	target := r.basePath + "/api/v1/artists/" + url.PathEscape(artistID) + "/images/fanart/file"
	http.Redirect(w, req, target, http.StatusTemporaryRedirect)
}
