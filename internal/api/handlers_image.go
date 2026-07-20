package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
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
	"github.com/sydlexius/stillwater/internal/version"
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
		if err := os.MkdirAll(dir, 0o750); err != nil {
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

// imageSyncBehavior selects which platform-sync call (if any) finalizeImageSave
// performs for a given call site.
type imageSyncBehavior int

const (
	// syncNone skips platform sync entirely (upload's fanart-append branch:
	// platforms only support a single backdrop, and the primary was already
	// synced when first saved; re-syncing here would re-push the primary
	// rather than the new numbered variant).
	syncNone imageSyncBehavior = iota
	// syncSingle syncs the single saved imageType via SyncImageToPlatforms.
	syncSingle
	// syncAllFanart syncs the full fanart set via SyncAllFanartToPlatforms.
	syncAllFanart
)

// imagePublishOrder selects whether finalizeImageSave publishes the
// ArtistUpdated event before or after rule reevaluation. This MUST be set
// per call site rather than hardcoded: three of the four post-save tails
// publish before RunForArtist runs, but handleImageFetch's fanart-append
// branch publishes after, so SSE/event-bus consumers there observe the
// update only once violations have been recalculated. Homogenizing this
// order would silently change when consumers see the update. See #1552.
type imagePublishOrder int

const (
	publishBeforeRuleEval imagePublishOrder = iota
	publishAfterRuleEval
)

// imageSaveFinalization captures the behavioral variations between the four
// post-save tails (upload primary/fanart-append, fetch primary/fanart-append)
// that finalizeImageSave consolidates.
type imageSaveFinalization struct {
	// isHTMX selects the response shape: 204 + HX-Refresh when true, a JSON
	// body otherwise. Upload has no HTMX path and always passes false.
	isHTMX bool
	// syncBehavior selects the platform-sync call, if any.
	syncBehavior imageSyncBehavior
	// isFanartAppend selects the flag/count update: append branches only
	// bump the fanart count, primary-save branches set the image-type flag
	// (and additionally bump the fanart count when imageType == "fanart").
	isFanartAppend bool
	// publishOrder selects whether the ArtistUpdated event is published
	// before or after rule reevaluation. See imagePublishOrder.
	publishOrder imagePublishOrder
	// savedFanartSlot, when non-nil, is the exact fanart slot index the save
	// targeted (the per-slot Crop/Fetch controls, #2281). finalizeImageSave
	// uses it as the auto-lock target (#2533) instead of deriving one, so a
	// per-slot edit locks the slot it actually wrote rather than slot 0. Nil
	// for single-slot types (thumb/logo/banner -> slot 0) and for fanart
	// append (the appended file lands at FanartCount-1).
	savedFanartSlot *int
	// setTriggerOnJSON controls whether the JSON (non-HTMX) response path
	// also calls setSyncWarningTrigger before writing the body. The HTMX
	// response path always sets the trigger (its 204 body carries no
	// warnings, so the header is the only way to surface them client-side).
	setTriggerOnJSON bool
}

// finalizeImageSave runs the shared post-save tail common to
// handleImageUpload and handleImageFetch: cache-limit enforcement, flag/count
// updates, event publication, health-cache invalidation, rule reevaluation,
// platform sync, and response writing. The four call sites differ only in
// the dimensions captured by opts; see imageSaveFinalization and #1552.
func (r *Router) finalizeImageSave(ctx context.Context, w http.ResponseWriter, a *artist.Artist, imageType string, saved []string, opts imageSaveFinalization) {
	r.enforceCacheLimitIfNeeded(ctx, a)

	if opts.isFanartAppend {
		r.updateArtistFanartCount(ctx, a)
	} else {
		r.updateArtistImageFlag(ctx, a, imageType)
		if imageType == "fanart" {
			r.updateArtistFanartCount(ctx, a)
		}
	}

	// #2533: a hand-saved image is operator intent -- auto-lock its slot before
	// rule re-evaluation runs below, so the rule-engine carve-out
	// (Pipeline.imageSlotProtected) reliably suppresses the same-request
	// auto-fix that would otherwise clobber the just-saved crop. This is also
	// what durably protects the slot from later nightly sweeps. Best-effort:
	// a lock failure is logged but must not fail the save (consistent with the
	// other supplementary post-save calls here). Slot resolution: an explicit
	// per-slot fanart edit (#2281) carries its target slot in savedFanartSlot;
	// a fanart append lands at the highest slot (FanartCount-1 after the recount
	// above); every other save targets slot 0.
	lockSlot := 0
	switch {
	case opts.savedFanartSlot != nil:
		lockSlot = *opts.savedFanartSlot
	case opts.isFanartAppend && a.FanartCount > 0:
		lockSlot = a.FanartCount - 1
	}
	if err := r.artistService.SetImageLockBySlot(ctx, a.ID, imageType, lockSlot, true); err != nil {
		r.logger.Warn("auto-locking saved image slot",
			slog.String("artist_id", a.ID),
			slog.String("image_type", imageType),
			slog.Int("slot", lockSlot),
			slog.String("error", err.Error()))
	}

	// Record per-slot provenance for the file this request actually wrote
	// (#2564).
	//
	// The primary-save branch above already covers slot 0 via
	// setArtistImageFlag, which rediscovers the primary on disk and records it.
	// What it cannot cover is a write that does NOT target the primary: a fanart
	// append, and the per-slot Crop/Fetch edit (#2281). Those two were the only
	// writers of slot >0 and NEITHER recorded provenance at all -- the append
	// branch routes to updateArtistFanartCount, which never records, and the
	// per-slot edit fell through to setArtistImageFlag, which re-recorded the
	// untouched PRIMARY and left the edited slot alone.
	//
	// The result was that slots 1+ never received a phash. UpsertAll creates
	// their rows with empty provenance columns and then deliberately preserves
	// those columns on every rescan, so the emptiness was permanent rather than
	// transient. A per-slot phash reader over such a row finds nothing and
	// concludes the artist is clean because it had no data to judge -- a false
	// green inside a tool whose whole job is detecting corruption.
	//
	// The slot is the one resolved above for the auto-lock: it is the same
	// write, so it is the same slot by construction, and deriving it twice
	// would invite the two derivations to disagree.
	if len(saved) > 0 && (opts.isFanartAppend || opts.savedFanartSlot != nil) {
		r.recordImageProvenance(ctx, a.ID, imageType, filepath.Join(r.imageDir(a), saved[0]), lockSlot)
	}

	publish := func() {
		if r.eventBus != nil {
			r.eventBus.Publish(event.Event{
				Type: event.ArtistUpdated,
				Data: map[string]any{"artist_id": a.ID},
			})
		}
	}

	// Publish event and invalidate cache before platform sync (which can take
	// up to 30s) so health scores update within the 5-second target -- except
	// where opts.publishOrder says otherwise (see imagePublishOrder).
	if opts.publishOrder == publishBeforeRuleEval {
		publish()
	}
	r.InvalidateHealthCache()
	// Re-evaluate rules after a successful image write so image-related
	// violations (missing thumbnail, fanart count, etc.) auto-clear in auto
	// mode without waiting for the next scheduled scan. See #1028.
	r.runRulesAfterRefresh(ctx, a)
	if opts.publishOrder == publishAfterRuleEval {
		publish()
	}

	var warnings []string
	switch opts.syncBehavior {
	case syncSingle:
		syncCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		warnings = r.publisher.SyncImageToPlatforms(syncCtx, a, imageType)
	case syncAllFanart:
		syncCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		warnings = r.publisher.SyncAllFanartToPlatforms(syncCtx, a)
	case syncNone:
		warnings = []string{}
	}

	if opts.isHTMX {
		setSyncWarningTrigger(w, warnings)
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if opts.setTriggerOnJSON {
		setSyncWarningTrigger(w, warnings)
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

// handleImageUpload handles multipart file uploads for artist images.
// POST /api/v1/artists/{id}/images/upload
func (r *Router) handleImageUpload(w http.ResponseWriter, req *http.Request) {
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}
	if !r.gateImageWrite(w, req) {
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
	defer file.Close() //nolint:errcheck // Close error not actionable on cleanup

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
	// If mismatched, return the image data to the client for cropping instead of
	// saving. As on the fetch path, this is a 200 that did NOT save -- the client
	// must branch on needs_crop (#2415).
	skipCrop := req.URL.Query().Get("skip_crop") == "true"
	if !skipCrop {
		w2, h2, dimErr := img.GetDimensions(bytes.NewReader(data))
		if dimErr == nil {
			geo := img.CheckGeometry(w2, h2, imageType)
			if geo.NeedsCrop {
				// An upload carries no fanart slot, hence the nil slot.
				r.logNeedsCropShortCircuit(artistID, imageType, geo, nil)
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
					"append":         imageType == "fanart" && a.FanartExists,
				})
				return
			}
		}
	}

	// User uploads always get "user" source provenance.
	uploadMeta := &img.ExifMeta{Source: artist.ImageSourceUser, Fetched: time.Now().UTC(), Mode: "user"}

	// Fanart: append as next numbered file when fanart already exists.
	if imageType == "fanart" && a.FanartExists {
		saved, saveErr := r.processAndAppendFanart(req.Context(), r.newImageWriteScope(a), r.imageDir(a), data, uploadMeta)
		if saveErr != nil {
			r.logger.Error("appending fanart upload", "artist_id", artistID, "error", saveErr)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save image"})
			return
		}
		// Skip platform sync for fanart appends: platforms only support a single
		// backdrop image, and the primary (fanart.jpg) was already synced when
		// first saved. Re-syncing here would re-push the primary, not the new
		// variant (fanart2.jpg etc.), because syncImageToPlatforms discovers
		// files via findExistingImage which always returns the primary.
		r.finalizeImageSave(req.Context(), w, a, imageType, saved, imageSaveFinalization{
			isHTMX:         false,
			syncBehavior:   syncNone,
			isFanartAppend: true,
			publishOrder:   publishBeforeRuleEval,
		})
		return
	}

	saved, err := r.processAndSaveImage(req.Context(), r.newImageWriteScope(a), r.imageDir(a), imageType, data, uploadMeta)
	if err != nil {
		r.logger.Error("saving uploaded image", "artist_id", artistID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save image"})
		return
	}
	r.finalizeImageSave(req.Context(), w, a, imageType, saved, imageSaveFinalization{
		isHTMX:           false,
		syncBehavior:     syncSingle,
		isFanartAppend:   false,
		publishOrder:     publishBeforeRuleEval,
		setTriggerOnJSON: true,
	})
}

// fanartSlotError distinguishes a bad-request (invalid/out-of-range slot)
// from a server error (directory read failure) so callers can map it to the
// right HTTP status (#2281 QOL #48).
type fanartSlotError struct {
	status int
	msg    string
}

func (e *fanartSlotError) Error() string { return e.msg }

// validateFanartSlot checks an explicit fanart slot against the current
// on-disk set: it must reference an existing slot (no gaps), because this is
// an edit of a saved backdrop (crop/fetch-replace), not a way to create one.
// Shared by handleImageCrop and handleImageFetch's slot branches.
func (r *Router) validateFanartSlot(ctx context.Context, dir string, slot int) *fanartSlotError {
	primary := r.getActiveFanartPrimary(ctx)
	existing, discoverErr := img.DiscoverFanart(dir, primary)
	if discoverErr != nil && !errors.Is(discoverErr, os.ErrNotExist) {
		r.logger.Error("discovering fanart for slot validation", slog.String("dir", dir), slog.String("error", discoverErr.Error()))
		return &fanartSlotError{status: http.StatusInternalServerError, msg: "failed to read fanart directory"}
	}
	if slot < 0 || slot >= len(existing) {
		return &fanartSlotError{status: http.StatusBadRequest, msg: "invalid fanart slot"}
	}
	return nil
}

// handleImageCropFanartSlot saves a crop result to an explicit fanart slot
// (#2281 QOL #48: FanartManagementGallery's per-slot Crop control), preserving
// existing provenance for that specific file when present -- mirrors the
// non-slot recrop-of-primary path below. Writes the HTTP response itself.
func (r *Router) handleImageCropFanartSlot(w http.ResponseWriter, req *http.Request, a *artist.Artist, artistID string, slot int, imgData []byte) {
	dir := r.imageDir(a)
	if slotErr := r.validateFanartSlot(req.Context(), dir, slot); slotErr != nil {
		writeJSON(w, slotErr.status, map[string]string{"error": slotErr.msg})
		return
	}

	primary := r.getActiveFanartPrimary(req.Context())
	kodiNumbering := r.isKodiNumbering(req.Context())
	targetName := img.FanartFilename(primary, slot, kodiNumbering)
	slotMeta := &img.ExifMeta{Source: artist.ImageSourceUser}
	if existingPath, found := img.FindExistingImage(dir, []string{targetName}); found {
		if existingMeta, readErr := img.ReadProvenance(existingPath); readErr == nil && existingMeta != nil {
			slotMeta = existingMeta
		}
	}
	slotMeta.Fetched = time.Now().UTC()
	slotMeta.Mode = "user"
	slotMeta.DHash = "" // Force recomputation from the cropped image data.

	// #2622: routed through saveFanartSlotChecked for the #2540 cross-artist
	// collision check. imgData is written RAW here -- this path has no
	// ConvertFormat step of its own (the crop already produced encoded bytes) --
	// so imgData is exactly what the verdict must hash and what lands on disk.
	saved, saveErr := r.saveFanartSlotChecked(req.Context(), r.newImageWriteScope(a), dir, []string{targetName}, imgData, slotMeta)
	if saveErr != nil {
		r.logger.Error("saving cropped fanart slot",
			slog.String("artist_id", artistID), slog.Int("slot", slot), slog.String("error", saveErr.Error()))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save image"})
		return
	}
	r.finalizeImageSave(req.Context(), w, a, "fanart", saved, imageSaveFinalization{
		isHTMX:          false,
		syncBehavior:    syncAllFanart,
		isFanartAppend:  false,
		publishOrder:    publishBeforeRuleEval,
		savedFanartSlot: &slot,
	})
}

// handleImageFetchFanartSlot saves a fetched image to an explicit fanart slot
// (#2281 QOL #48: FanartManagementGallery's per-slot Fetch/Replace control).
// Unlike the crop counterpart, it does not preserve prior provenance -- this
// mirrors the non-slot fetch path below, which always builds fresh metadata
// rather than reading back the file it is about to overwrite. Writes the HTTP
// response itself.
//
// #2331 CR-1: the caller (handleImageFetch) validates the slot BEFORE the
// slow network fetch, so re-validate here, immediately before the write --
// the fanart set can change (reorder, delete, another save) during the
// network round-trip, and a stale slot number must not silently land on the
// wrong (or now out-of-range) file. handleImageCropFanartSlot does not need
// this: it is synchronous with no network call between its own validation
// and its write.
func (r *Router) handleImageFetchFanartSlot(w http.ResponseWriter, req *http.Request, a *artist.Artist, artistID string, slot int, data []byte, imageURL string) {
	dir := r.imageDir(a)
	if slotErr := r.validateFanartSlot(req.Context(), dir, slot); slotErr != nil {
		writeJSON(w, slotErr.status, map[string]string{"error": slotErr.msg})
		return
	}

	primary := r.getActiveFanartPrimary(req.Context())
	kodiNumbering := r.isKodiNumbering(req.Context())
	targetName := img.FanartFilename(primary, slot, kodiNumbering)
	slotMeta := &img.ExifMeta{Source: artist.ImageSourceUser, Fetched: time.Now().UTC(), URL: imageURL, Mode: "user"}

	// #2622: routed through saveFanartSlotChecked for the #2540 cross-artist
	// collision check. NOTE the byte selection: unlike every other fanart write in
	// this file, this path saves the RAW fetched data and never calls
	// ConvertFormat. So data -- not a conversion of it -- is both what the verdict
	// hashes and what lands on disk, because the helper takes a single slice and
	// uses it for both.
	saved, saveErr := r.saveFanartSlotChecked(req.Context(), r.newImageWriteScope(a), dir, []string{targetName}, data, slotMeta)
	if saveErr != nil {
		r.logger.Error("saving fetched fanart slot",
			slog.String("artist_id", artistID), slog.Int("slot", slot), slog.String("error", saveErr.Error()))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save image"})
		return
	}
	r.finalizeImageSave(req.Context(), w, a, "fanart", saved, imageSaveFinalization{
		isHTMX:          isHTMXRequest(req),
		syncBehavior:    syncAllFanart,
		isFanartAppend:  false,
		publishOrder:    publishBeforeRuleEval,
		savedFanartSlot: &slot,
	})
}

// validateImageFetchInput checks the fetch request's url/type before any
// network or filesystem work. Returns a zero status/empty message on success;
// otherwise the HTTP status and generic error message the caller should
// write. Extracted from handleImageFetch to keep its cognitive complexity
// down (gocognit).
func validateImageFetchInput(ctx context.Context, imageURL, imageType string) (status int, errMsg string) {
	if imageURL == "" {
		return http.StatusBadRequest, "url is required"
	}
	if !validImageTypes[imageType] {
		return http.StatusBadRequest, "invalid image type, must be: thumb, fanart, logo, banner"
	}
	if !strings.HasPrefix(imageURL, "http://") && !strings.HasPrefix(imageURL, "https://") {
		return http.StatusBadRequest, "url must start with http:// or https://"
	}
	if isPrivateURL(ctx, imageURL) {
		return http.StatusBadRequest, "url points to a private or reserved address"
	}
	return 0, ""
}

// logNeedsCropShortCircuit records that an image save was short-circuited for
// cropping rather than written to disk (#2415).
//
// This is the one image-save outcome that used to log nothing at all, which is
// exactly the outcome most worth logging: the response is a 200, so an operator
// reading access logs (or a client that ignores the needs_crop flag) sees a
// success while nothing was saved. Info level, because a short-circuit is a
// normal, expected outcome -- the silence was the defect, not the branch.
func (r *Router) logNeedsCropShortCircuit(artistID, imageType string, geo img.GeometryResult, slot *int) {
	attrs := []any{
		slog.String("artist_id", artistID),
		slog.String("type", imageType),
		slog.Float64("required_ratio", geo.RequiredRatio),
		slog.Float64("actual_ratio", geo.ActualRatio),
		slog.Int("width", geo.Width),
		slog.Int("height", geo.Height),
	}
	if slot != nil {
		attrs = append(attrs, slog.Int("slot", *slot))
	}
	r.logger.Info("image not saved: aspect ratio needs cropping", attrs...)
}

// fetchRespondIfNeedsCrop writes the needs_crop JSON response when the
// fetched image's aspect ratio does not match the target slot, and reports
// whether it did so (the caller must return immediately when true). Extracted
// from handleImageFetch to keep its cognitive complexity down (gocognit).
func (r *Router) fetchRespondIfNeedsCrop(w http.ResponseWriter, artistID string, imageType string, data []byte, fanartExists bool, slot *int) bool {
	w2, h2, dimErr := img.GetDimensions(bytes.NewReader(data))
	if dimErr != nil {
		return false
	}
	geo := img.CheckGeometry(w2, h2, imageType)
	if !geo.NeedsCrop {
		return false
	}
	r.logNeedsCropShortCircuit(artistID, imageType, geo, slot)
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
	resp := map[string]any{
		"status":         "needs_crop",
		"needs_crop":     true,
		"required_ratio": geo.RequiredRatio,
		"actual_ratio":   geo.ActualRatio,
		"width":          geo.Width,
		"height":         geo.Height,
		"image_data":     "data:" + mimeType + ";base64," + encoded,
		"type":           imageType,
		"append":         imageType == "fanart" && fanartExists,
	}
	// #2281: thread the slot through so the follow-up crop POST persists to
	// the same slot this fetch originated from. #2331 Copilot-1: slot is only
	// meaningful for fanart -- echoing it for another type would misleadingly
	// imply a per-slot save is possible there (the actual per-slot WRITE path
	// is already correctly gated on imageType == "fanart" elsewhere; this
	// gate keeps the needs_crop response's contract consistent with that).
	if slot != nil && imageType == "fanart" {
		resp["slot"] = *slot
	}
	writeJSON(w, http.StatusOK, resp)
	return true
}

// handleImageFetch fetches an image from a URL and saves it for the artist.
// POST /api/v1/artists/{id}/images/fetch
func (r *Router) handleImageFetch(w http.ResponseWriter, req *http.Request) {
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}
	if !r.gateImageWrite(w, req) {
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

	imageURL, imageType, slot, err := extractImageFetchParams(req)
	if err != nil {
		r.logger.Debug("invalid image fetch request body",
			slog.String("artist_id", artistID),
			slog.String("error", err.Error()))
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if status, msg := validateImageFetchInput(req.Context(), imageURL, imageType); msg != "" {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}

	// #2281 QOL #48: an explicit fanart slot must already exist (no gaps) --
	// fail fast before the network fetch below. slot is only meaningful for
	// fanart; a slot on any other type is ignored (extractImageFetchParams
	// does not gate on type).
	if imageType == "fanart" && slot != nil {
		if slotErr := r.validateFanartSlot(req.Context(), r.imageDir(a), *slot); slotErr != nil {
			writeJSON(w, slotErr.status, map[string]string{"error": slotErr.msg})
			return
		}
	}

	data, err := r.fetchImageFromURL(req.Context(), imageURL)
	if err != nil {
		r.logger.Warn("fetching image from URL", "url", imageURL, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "failed to fetch image"})
		return
	}

	// Check geometry before saving. If the aspect ratio does not match the slot
	// requirement, return the fetched image data for client-side cropping
	// INSTEAD OF SAVING. The response is a 200, so every caller must branch on
	// needs_crop; treating a 2xx as "saved" is wrong on this path.
	//
	// #2415: the previous comment here claimed the compare-panel Save button
	// "opts out explicitly via ?skip_crop=true on its hx-post URL". It does
	// not, and never did -- no UI surface sends skip_crop (grep: the string
	// appears in no .templ file). Every UI surface instead handles needs_crop
	// by opening the crop modal, which is the correct UX: an aspect mismatch is
	// a geometry fact, independent of whether the user already eyeballed the
	// image in the compare panel. skip_crop=true remains a documented
	// programmatic opt-out (see openapi.yaml) for API callers that genuinely
	// want a forced save at the source aspect ratio; it has no UI sender.
	skipCrop := req.URL.Query().Get("skip_crop") == "true"
	if !skipCrop && r.fetchRespondIfNeedsCrop(w, artistID, imageType, data, a.FanartExists, slot) {
		return
	}

	fetchMeta := &img.ExifMeta{Source: artist.ImageSourceUser, Fetched: time.Now().UTC(), URL: imageURL, Mode: "user"}

	// #2281 QOL #48: an explicit fanart slot replaces that specific saved
	// backdrop (already validated to exist, above), taking priority over the
	// append-next default below.
	if imageType == "fanart" && slot != nil {
		r.handleImageFetchFanartSlot(w, req, a, artistID, *slot, data, imageURL)
		return
	}

	// Fanart: append as next numbered file when fanart already exists.
	if imageType == "fanart" && a.FanartExists {
		saved, saveErr := r.processAndAppendFanart(req.Context(), r.newImageWriteScope(a), r.imageDir(a), data, fetchMeta)
		if saveErr != nil {
			r.logger.Error("appending fanart image", "artist_id", artistID, "error", saveErr)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save image"})
			return
		}
		// NOTE: unlike the other three post-save tails, this branch publishes
		// ArtistUpdated AFTER rule reevaluation (publishAfterRuleEval), not
		// before. Do not "fix" this to match the others -- see
		// imagePublishOrder and #1552.
		r.finalizeImageSave(req.Context(), w, a, imageType, saved, imageSaveFinalization{
			isHTMX:         isHTMXRequest(req),
			syncBehavior:   syncAllFanart,
			isFanartAppend: true,
			publishOrder:   publishAfterRuleEval,
		})
		return
	}

	saved, err := r.processAndSaveImage(req.Context(), r.newImageWriteScope(a), r.imageDir(a), imageType, data, fetchMeta)
	if err != nil {
		r.logger.Error("saving fetched image", "artist_id", artistID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save image"})
		return
	}
	r.finalizeImageSave(req.Context(), w, a, imageType, saved, imageSaveFinalization{
		isHTMX:         isHTMXRequest(req),
		syncBehavior:   syncSingle,
		isFanartAppend: false,
		publishOrder:   publishBeforeRuleEval,
	})
}

// handleImageStage downloads a remote provider image and returns it as a
// base64 data: URI, without saving anything (#2281 QOL #47). A provider image
// (e.g. Discogs) loaded directly into Cropper.js taints the canvas under the
// browser's same-origin policy, so the client stages it through this
// same-origin round-trip first; a data: URI never taints the canvas. This
// mirrors the existing needs_crop response shape but is otherwise a pure
// read: it does not save to disk, touch backups, publish events, or run
// rules. Reuses the same URL-scheme allowlist, isPrivateURL SSRF guard, and
// fetchImageFromURL helper as handleImageFetch.
// POST /api/v1/artists/{id}/images/stage
func (r *Router) handleImageStage(w http.ResponseWriter, req *http.Request) {
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	if _, err := r.artistService.GetByID(req.Context(), artistID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "artist not found"})
		return
	}

	var body struct {
		URL  string `json:"url"`
		Type string `json:"type"`
	}
	if !DecodeJSON(w, req, &body) {
		return
	}

	if !validImageTypes[body.Type] {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid image type, must be: thumb, fanart, logo, banner"})
		return
	}
	if body.URL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url is required"})
		return
	}
	if !strings.HasPrefix(body.URL, "http://") && !strings.HasPrefix(body.URL, "https://") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url must start with http:// or https://"})
		return
	}
	if isPrivateURL(req.Context(), body.URL) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url points to a private or reserved address"})
		return
	}

	data, err := r.fetchImageFromURL(req.Context(), body.URL)
	if err != nil {
		r.logger.Warn("staging image from URL", slog.String("url", body.URL), slog.String("error", err.Error()))
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "failed to fetch image"})
		return
	}

	// fetchImageFromURL already validated the bytes decode as a supported
	// image (returning the 502 above on failure), so a second error here is
	// unreachable for the SAME data -- DetectFormat is deterministic and was
	// already run successfully inside fetchImageFromURL. Re-run only to learn
	// the format for the data: URI mime type (mirrors handleImageFetch's
	// needs_crop branch, which does the same after its own fetchImageFromURL
	// call); the error is intentionally discarded rather than handled as a
	// second unreachable branch.
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
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "ok",
		"image_data": "data:" + mimeType + ";base64," + encoded,
		"type":       body.Type,
	})
}

// handleImageSearch searches for images from all providers for an artist.
// GET /api/v1/artists/{id}/images/search?type=thumb
func (r *Router) handleImageSearch(w http.ResponseWriter, req *http.Request) {
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	// Validate sort before any provider fetch/probe work so bad requests
	// fail fast with 400 rather than after expensive upstream calls.
	sortBy, ok := validateSortParam(w, req, allowedImageSearchSort)
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

	sortImageResults(images, sortBy)

	// Return HTML for HTMX requests, JSON for API requests
	if isHTMXRequest(req) {
		if typeFilter == "fanart" {
			renderTempl(w, req, templates.FanartSearchResults(artistID, images, sortBy, result.ImageProviderStatuses))
		} else {
			renderTempl(w, req, templates.ImageSearchResults(artistID, images, sortBy, a.FanartExists, result.ImageProviderStatuses))
		}
		return
	}

	// provider_statuses reports what actually happened per provider: queried,
	// skipped (and why), or errored (with a scrubbed message). "errors" carries
	// only the failures, so on its own it cannot distinguish a provider that was
	// never asked from one that was asked and found nothing -- which is the
	// invisibility this endpoint is being fixed for. An API client gets the same
	// facts the UI banner shows.
	writeJSON(w, http.StatusOK, map[string]any{
		"images":            images,
		"errors":            result.Errors,
		"provider_statuses": result.ImageProviderStatuses,
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

	sortBy, ok := validateSortParam(w, req, allowedImageSearchSort)
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

	// Normalize http:// URLs to https:// so they satisfy the img-src CSP
	// directive ("img-src 'self' data: https:"). Known web-search providers
	// already serve content over TLS; this is a defensive upgrade applied at
	// the single accumulation point rather than per-provider.
	for i := range allImages {
		allImages[i].URL = ensureHTTPS(allImages[i].URL)
	}

	sortImageResults(allImages, sortBy)

	if isHTMXRequest(req) {
		renderTempl(w, req, templates.WebImageSearchResults(artistID, allImages, sortBy, a.FanartExists))
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
	if !r.gateImageWrite(w, req) {
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
		Append    bool   `json:"append"`
		// Slot is the explicit fanart slot to overwrite (#2281 QOL #48: crop
		// on any saved backdrop, not just the primary). Omitting it (nil)
		// preserves the pre-#2281 canonical-slot/append behavior below.
		Slot *int `json:"slot,omitempty"`
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

	// #2281 QOL #48: an explicit fanart slot targets a specific saved backdrop
	// (FanartManagementGallery's per-slot Crop control), taking priority over
	// the Append branch below. The slot must already exist (no gaps): this is
	// an edit of a saved backdrop, not a way to append a new one.
	if body.Type == "fanart" && body.Slot != nil {
		r.handleImageCropFanartSlot(w, req, a, artistID, *body.Slot, imgData)
		return
	}

	// Fanart: append as next numbered file when the client signals an "add
	// fanart" crop and a primary already exists, mirroring upload's append
	// branch (syncNone). fetch's append branch uses syncAllFanart instead;
	// reconciling fanart-append sync behavior across upload/fetch/crop is
	// tracked in #2317. Otherwise fall through to the provenance-preserving
	// single-slot overwrite (recrop-of-primary and all non-fanart types).
	if body.Type == "fanart" && a.FanartExists && body.Append {
		appendMeta := &img.ExifMeta{Source: artist.ImageSourceUser, Mode: "user", Fetched: time.Now().UTC()}
		saved, saveErr := r.processAndAppendFanart(req.Context(), r.newImageWriteScope(a), r.imageDir(a), imgData, appendMeta)
		if saveErr != nil {
			r.logger.Error("appending cropped fanart", "artist_id", artistID, "error", saveErr)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save image"})
			return
		}
		r.finalizeImageSave(req.Context(), w, a, body.Type, saved, imageSaveFinalization{
			isHTMX:         false,
			syncBehavior:   syncNone,
			isFanartAppend: true,
			publishOrder:   publishBeforeRuleEval,
		})
		return
	}

	// Preserve existing provenance metadata if present, updating the timestamp.
	// Skipped for the append branch above, which never uses cropMeta.
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
		cropMeta = &img.ExifMeta{Source: artist.ImageSourceUser}
	}
	cropMeta.Fetched = time.Now().UTC()
	cropMeta.Mode = "user"
	cropMeta.DHash = "" // Force recomputation from the cropped image data.

	saved, err := r.processAndSaveImage(req.Context(), r.newImageWriteScope(a), r.imageDir(a), body.Type, imgData, cropMeta)
	if err != nil {
		r.logger.Error("saving cropped image", "artist_id", artistID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save image"})
		return
	}
	r.finalizeImageSave(req.Context(), w, a, body.Type, saved, imageSaveFinalization{
		isHTMX:         false,
		syncBehavior:   syncSingle,
		isFanartAppend: false,
		publishOrder:   publishBeforeRuleEval,
	})
}

// saveOrRestoreResult is the outcome of saveSingleSlotWithRollback: either the
// save succeeded (Saved set, SaveErr nil), or it failed and the helper
// automatically attempted to restore the pre-edit backup so a save-failure
// window can never leave the canonical image genuinely absent (#2339).
type saveOrRestoreResult struct {
	// Saved is the list of filenames written, set only when the save itself
	// succeeded.
	Saved []string
	// SaveErr is the original img.Save failure. Nil means the save succeeded
	// and the other fields are zero.
	SaveErr error
	// RestoreErr is set when the rollback attempt (img.RestoreSingleSlot)
	// itself failed after SaveErr, i.e. the worst case: neither the edit nor
	// the restore landed and manual recovery may be needed. Nil (with SaveErr
	// non-nil) means the rollback succeeded and the original is back in place.
	RestoreErr error
}

// saveSingleSlotWithRollback saves data for a single-slot image type
// (thumb/logo/banner) and, if the save fails, automatically restores the
// pre-edit backup written by an earlier BackupSingleSlot call so a save
// failure never leaves the artist without an image (#2339). Callers must have
// already backed up the original; fanart (multi-slot, no single-slot backup)
// must not use this helper.
func (r *Router) saveSingleSlotWithRollback(dir, imageType string, naming []string, useSymlinks bool, meta *img.ExifMeta, data []byte) saveOrRestoreResult {
	saved, err := img.Save(dir, imageType, data, naming, useSymlinks, meta, r.logger)
	if err == nil {
		return saveOrRestoreResult{Saved: saved}
	}
	// The save failed after a successful backup: restore the original rather
	// than leaving the canonical file missing or half-written. Pass a nil meta
	// so RestoreSingleSlot re-derives fresh provenance from the restored
	// bytes, matching handleImageRevert's convention.
	if restoreErr := img.RestoreSingleSlot(dir, imageType, naming, useSymlinks, nil, r.logger); restoreErr != nil {
		return saveOrRestoreResult{SaveErr: err, RestoreErr: restoreErr}
	}
	return saveOrRestoreResult{SaveErr: err}
}

// saveFanartSlotProtected is the API-side entry to the protected fanart write.
//
// The backup/save/rollback policy itself lives in img.SaveSlotProtected, next to the
// BackupSlot and RestoreSlot primitives it is built from, so that callers outside this
// package (the rule engine, which is package-level and cannot call a Router method)
// reach the SAME chokepoint rather than growing a second copy of it.
//
// What this wrapper adds is the one thing that is genuinely the Router's: registering
// the paths the save is about to touch with the filesystem watcher, so the watcher can
// tell Stillwater's own writes apart from an external editor's. The registration spans
// the rollback too -- a restore rewrites the same paths.
//
// useSymlinks is resolved from the active platform profile, the same single source of
// truth every other Router save/restore call site reads, instead of the literal false
// that used to be hardcoded inside the chokepoint (#2446). Save disables symlinking for
// fanart regardless of the flag, so this resolves to the same on-disk result today; it
// passes the value it MEANS rather than one that happens to agree.
func (r *Router) saveFanartSlotProtected(ctx context.Context, dir string, naming []string, data []byte, meta *img.ExifMeta) ([]string, error) {
	if r.expectedWrites != nil {
		expectedPaths := img.ExpectedPaths(dir, naming)
		r.expectedWrites.AddAll(expectedPaths)
		defer r.expectedWrites.RemoveAll(expectedPaths)
	}
	_, useSymlinks := r.getActiveNamingAndSymlinks(ctx, "fanart")
	return img.SaveSlotProtected(dir, "fanart", naming, data, useSymlinks, meta, r.logger)
}

// processAndSaveImage processes image data (convert format, optimize) and saves it.
// For logos, transparent borders are automatically trimmed before saving.
// meta is optional EXIF provenance metadata to embed in the saved image.
// scope carries the #2565 cross-artist collision check; it may be nil, which
// disables the check (fail-open) without affecting the write.
func (r *Router) processAndSaveImage(ctx context.Context, scope *imageWriteScope, dir string, imageType string, data []byte, meta *img.ExifMeta) ([]string, error) {
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

	// Non-destructive: keep a one-deep peer-inert backup of the current canonical
	// image before we overwrite it, so the edit is revertible (#1837).
	// BackupSingleSlot probes strictly (#1161): a transient stat error or a
	// backup-write failure returns an error and we ABORT the destructive save, so the
	// source-of-truth original is never overwritten without a recoverable backup.
	//
	// THIS APPLIES TO FANART TOO. It used to be excluded here on the reasoning that
	// "fanart is multi-slot (a new slot is appended elsewhere)" -- but this is the
	// OVERWRITE path. Appending is a different function (processAndAppendFanart),
	// called earlier in the handler. So the one operation that can destroy a primary
	// backdrop was the only one skipping the safety net (#2413).
	//
	// And it does destroy it. The loss is a DELETE, not an overwrite, which is why the
	// atomic write never protected anyone: img.Save calls CleanupConflictingFormats,
	// which REMOVES the other format of the same slot before writing. Overwrite a
	// fanart.png with JPEG data and the canonical name becomes fanart.jpg, so the
	// original fanart.png is deleted outright -- and if the write then fails, the
	// artwork is gone with nothing to put back.
	// FANART TAKES THE SLOT-SCOPED PATH. It is MULTI-slot, and BackupSingleSlot's
	// prune is one-deep PER TYPE -- it deletes every file in .sw-backup/fanart/ except
	// the one it just wrote. So backing the PRIMARY up through it would DESTROY every
	// numbered slot's backup, silently disarming the protection for fanart1.jpg,
	// fanart2.jpg and the rest. One image type, one backup mechanism.
	if imageType == "fanart" {
		// #2565 NOTIFY-ONLY, FANART ONLY. The cross-artist registry holds fanart
		// rows exclusively, so the check is scoped to this branch: a thumb, logo or
		// banner overwrite has nothing to compare against and must not pay for a
		// whole-library scan. The verdict is decided here from the converted bytes
		// and HELD until saveFanartSlotProtected confirms the write.
		collisionResult := scope.collisionVerdict(ctx, converted)

		saved, saveErr := r.saveFanartSlotProtected(ctx, dir, naming, converted, meta)
		if saveErr != nil {
			return nil, fmt.Errorf("saving fanart to %s: %w", dir, saveErr)
		}
		if len(saved) == 0 {
			// No file on disk means no artwork for a back-out fix to act on, so
			// this is a failed write for notification purposes.
			return nil, fmt.Errorf("saving fanart: produced no files in %s", dir)
		}
		// Write confirmed -- safe to announce. See notifyCollision.
		scope.notifyCollision(ctx, collisionResult)
		return saved, nil
	}

	if bErr := img.BackupSingleSlot(dir, imageType, naming); bErr != nil {
		return nil, fmt.Errorf("backing up original before overwrite (aborting destructive save): %w", bErr)
	}

	// Register expected write paths so the filesystem watcher can
	// distinguish Stillwater's own writes from external ones.
	if r.expectedWrites != nil {
		expectedPaths := img.ExpectedPaths(dir, naming)
		r.expectedWrites.AddAll(expectedPaths)
		defer r.expectedWrites.RemoveAll(expectedPaths)
	}

	// Route through the shared helper so a save failure after the successful backup
	// above triggers an automatic rollback instead of silently leaving the canonical
	// image missing (#2339).
	//
	// Fanart is no longer carved out. The old branch here called img.Save directly
	// with no rollback, justified by "a failed append leaves the primary untouched
	// (append writes a new numbered file)" -- append-path reasoning in the OVERWRITE
	// path. A failed fanart overwrite does NOT leave the primary untouched: the
	// conflicting-format cleanup has already deleted it (#2413).
	res := r.saveSingleSlotWithRollback(dir, imageType, naming, useSymlinks, meta, converted)
	if res.SaveErr != nil {
		// A first-ever upload for this slot has no prior original, so the
		// earlier BackupSingleSlot call was a no-op and RestoreSingleSlot
		// correctly finds nothing to restore (os.ErrNotExist). That is not
		// a failed rollback -- nothing was lost -- so it must not surface
		// as the "manual recovery may be needed" message.
		if res.RestoreErr != nil && !errors.Is(res.RestoreErr, os.ErrNotExist) {
			return nil, fmt.Errorf("saving image failed and automatic restore also failed (manual recovery may be needed): %w (restore: %w)", res.SaveErr, res.RestoreErr)
		}
		return nil, fmt.Errorf("saving image failed: %w", res.SaveErr)
	}
	return res.Saved, nil
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

// fetchImageFromURL downloads an image from the given URL with timeout and size limits.
// The supplied ctx is honored for request cancellation (e.g. when the caller's
// HTTP request is canceled); SSRF protection is enforced by the configured
// ssrfClient transport regardless of ctx.
func (r *Router) fetchImageFromURL(ctx context.Context, rawURL string) ([]byte, error) {
	client := r.ssrfClient

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	// Wikimedia Commons blocks requests without a proper User-Agent.
	req.Header.Set("User-Agent", version.UserAgent("Stillwater", "https://github.com/sydlexius/stillwater"))

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // Close error not actionable on HTTP response cleanup

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
			// Use the Router's ssrfClient so that tests can substitute a loopback-
			// capable client and production traffic uses the SSRF-safe transport.
			info, err := img.ProbeRemoteImageWithClient(ctx, images[i].URL, r.ssrfClient)
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

// sortImageResults sorts images by the given criterion. Valid values are
// "likes" (descending, then resolution), "resolution" (descending, then
// likes). An empty or unrecognized value defaults to "likes".
func sortImageResults(images []provider.ImageResult, sortBy string) {
	switch sortBy {
	case "resolution":
		sort.Slice(images, func(i, j int) bool {
			areaI := images[i].Width * images[i].Height
			areaJ := images[j].Width * images[j].Height
			if areaI != areaJ {
				return areaI > areaJ
			}
			return images[i].Likes > images[j].Likes
		})
	default: // "likes" or empty
		sort.Slice(images, func(i, j int) bool {
			if images[i].Likes != images[j].Likes {
				return images[i].Likes > images[j].Likes
			}
			areaI := images[i].Width * images[i].Height
			areaJ := images[j].Width * images[j].Height
			return areaI > areaJ
		})
	}
}

// setArtistImageFlag sets the image existence, low-resolution, and placeholder flags and persists them.
// When exists is true the image file is probed for dimensions and a LQIP placeholder is generated.
// After persisting, provenance metadata (phash, source, file format, mtime) is read from the
// image file and recorded in the artist_images table.
// When exists is false all flags and the placeholder are cleared.
//
//nolint:gocognit // Probes file existence then conditionally derives dimensions, low-res flag, placeholder, phash, source format, and mtime; each step's error handling is local to its concern and splitting would just shuffle the conditionals across helpers.
func (r *Router) setArtistImageFlag(ctx context.Context, a *artist.Artist, imageType string, exists bool) {
	var lowRes bool
	var placeholder string
	var resolvedPath string // path to the image file on disk, used for provenance readback
	if exists {
		patterns := r.getActiveNamingConfig(ctx, imageType)
		if filePath, found := img.FindExistingImage(r.imageDir(a), patterns); found {
			if f, openErr := os.Open(filePath); openErr == nil { //nolint:gosec // path from trusted naming patterns
				resolvedPath = filePath
				defer f.Close() //nolint:errcheck // Close error not actionable on cleanup
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

	if err := r.persistImageFlag(ctx, a, imageType, exists); err != nil {
		r.logger.Warn("setting artist image flag",
			slog.String("artist_id", a.ID),
			slog.String("image_type", imageType),
			slog.String("error", err.Error()))
		return
	}

	// Record provenance evidence (phash, source, file format, write timestamp)
	// from the saved image file. This is supplementary data -- failures here are
	// logged as warnings but do not affect the image save operation.
	//
	// Slot 0 is correct here and not an assumption: resolvedPath comes from
	// FindExistingImage over getActiveNamingConfig, which only ever yields the
	// PRIMARY filename (never a numbered fanart variant), and the primary is
	// DiscoverFanart ordinal 0 by definition. Writes that target a non-primary
	// slot never reach this function; finalizeImageSave records those against
	// their real slot (#2564).
	if resolvedPath != "" {
		r.recordImageProvenanceSlot0(ctx, a.ID, imageType, resolvedPath)
	}
}

// persistImageFlag writes the artist row, then states a CLEARED flag to the
// image registry outright.
//
// The second step is load-bearing because Update is declarative: it acts only
// on the slots the Artist names (issue #2635), and extractImageMetadata emits
// no row at all for a type whose Exists, LowRes, and Placeholder are all
// empty. An exists=false call would therefore leave the stored row still
// reading exists_flag=1, with the UI showing artwork that is gone. This used
// to work only as a side effect of the shared write path deleting every absent
// slot -- the behavior that destroyed the registry.
//
// It clears the flag rather than deleting the row: this caller probed one
// image type and learned only that its file is missing, which is grounds for
// marking the slot empty, not for discarding the row's provenance. Slot 0 is
// correct for the same reason documented at the provenance call in
// setArtistImageFlag.
func (r *Router) persistImageFlag(ctx context.Context, a *artist.Artist, imageType string, exists bool) error {
	if err := r.artistService.Update(ctx, a); err != nil {
		return err
	}
	if exists {
		return nil
	}
	return r.artistService.ClearImageFlag(ctx, a.ID, imageType, 0)
}

// updateArtistImageFlag sets the image existence flag to true and persists it.
func (r *Router) updateArtistImageFlag(ctx context.Context, a *artist.Artist, imageType string) {
	r.setArtistImageFlag(ctx, a, imageType, true)
}

// recordImageProvenanceSlot0 records provenance for a file known to occupy slot
// 0. Single-slot types (thumb/logo/banner) have no other slot, and a fanart
// write that targets the primary name is slot 0 by definition: DiscoverFanart
// always sorts the primary to ordinal 0.
func (r *Router) recordImageProvenanceSlot0(ctx context.Context, artistID, imageType, filePath string) {
	r.recordImageProvenance(ctx, artistID, imageType, filePath, 0)
}

// recordImageProvenance reads Stillwater provenance metadata and file mtime from
// the image at filePath, then records the phash, source, file format, and write
// timestamp against the artist_images row for slotIndex. Errors are logged as
// warnings -- this is supplementary evidence collection and must not fail the
// image save.
//
// slotIndex is the DiscoverFanart ordinal, matching what every other per-slot
// writer keys on (see imageDupRowPath in internal/rule/image_duplicates.go). It
// used to be hard-coded to 0 here, which was correct only because every caller
// at the time wrote the primary. It is now a parameter because the callers that
// write a NON-primary slot -- a fanart append, and the per-slot Crop/Fetch edit
// (#2281) -- must record against the slot they actually wrote. Passing 0 for
// those would aim the UPDATE at another slot's row: with no slot-0 row it
// silently matches nothing, and with one it stamps a DIFFERENT file's phash and
// content hash onto slot 0. Both outcomes are worse than useless to a per-slot
// phash reader, which is why this is a parameter rather than a default (#2564).
func (r *Router) recordImageProvenance(ctx context.Context, artistID, imageType, filePath string, slotIndex int) {
	log := r.logger.With(
		slog.String("artist_id", artistID),
		slog.String("image_type", imageType),
		slog.String("path", filePath),
		slog.Int("slot_index", slotIndex),
	)

	d := img.CollectProvenance(filePath, log)
	if d.IsEmpty() {
		log.Warn("no provenance data collected, skipping update")
		return
	}
	if err := r.artistService.UpdateImageProvenance(ctx, artistID, imageType, slotIndex, d.PHash, d.ContentHash, d.Source, d.FileFormat, d.LastWrittenAt); err != nil {
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
	// Strict variant: a non-ENOENT stat error must NOT clear the exists_flag.
	// A permission-denied or unmounted-filesystem hiccup that returned EACCES
	// or EIO would otherwise drop a flag that is still correct on disk. See #1161.
	filePath, found, statErr := img.FindExistingImageStrict(dir, patterns)
	if statErr != nil {
		r.logger.Warn("serve image: stat error probing artist dir; preserving exists_flag",
			slog.String("artist_id", a.ID),
			slog.String("image_type", imageType),
			slog.String("error", statErr.Error()))
		http.NotFound(w, req)
		return
	}
	if !found {
		// If the DB flag says the image exists but the file is genuinely gone
		// (every probe returned ENOENT), clear the stale flag so subsequent UI
		// renders show a placeholder instead of a broken image tag. Best-effort.
		if imageExistsFlag(a, imageType) {
			// context.WithoutCancel propagates request-scoped values (trace
			// IDs, logging context) while detaching cancellation so the
			// goroutine survives after the response is written. Matches the
			// pattern in handlers_conflict / handlers_fix / handlers_refresh.
			go r.clearImageFlagAsync(context.WithoutCancel(req.Context()), a.ID, imageType)
		}
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

// imageExistsFlag returns the value of the exists flag for the given image
// type on the artist model. Returns false for unknown image types.
func imageExistsFlag(a *artist.Artist, imageType string) bool {
	switch imageType {
	case "thumb":
		return a.ThumbExists
	case "fanart":
		return a.FanartExists
	case "logo":
		return a.LogoExists
	case "banner":
		return a.BannerExists
	default:
		return false
	}
}

// clearImageFlagAsync clears the exists_flag for a stale image entry in a
// background goroutine that outlives the HTTP request. The caller is
// expected to pass context.WithoutCancel(req.Context()) so request-scoped
// values still propagate. A panic in the dependency
// (artistService.ClearImageFlag) or in our log emission must not crash the
// process: this background path is intentionally best-effort, so we recover,
// log structurally at slog.Error, and let the next serve request re-attempt
// the cleanup. The recovered panic value is included as a string attribute
// so operators can correlate the log with later artist-state confusion
// (e.g. a UI still showing a broken-image tile).
func (r *Router) clearImageFlagAsync(ctx context.Context, artistID, imageType string) {
	defer func() {
		if rec := recover(); rec != nil {
			r.logger.Error("panic in clearImageFlagAsync",
				slog.String("artist_id", artistID),
				slog.String("image_type", imageType),
				slog.Any("panic", rec))
		}
	}()

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := r.artistService.ClearImageFlag(ctx, artistID, imageType, 0); err != nil {
		r.logger.Warn("failed to clear stale image flag",
			slog.String("artist_id", artistID),
			slog.String("image_type", imageType),
			slog.String("error", err.Error()))
		return
	}
	r.logger.Info("cleared stale image flag",
		slog.String("artist_id", artistID),
		slog.String("image_type", imageType))
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

	stat, err := os.Stat(filePath)
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

	// backup_exists lets the UI conditionally surface a "Revert" affordance for
	// single-slot kinds (thumb/logo/banner): true when a one-deep .sw-backup
	// original is present. Fanart is multi-slot and has no single-slot backup,
	// so it is always false here (#1336 cross-plan delta, #1837).
	backupExists := imageType != "fanart" && img.HasBackup(dir, imageType)

	writeJSON(w, http.StatusOK, map[string]any{
		"type":          imageType,
		"filename":      filepath.Base(filePath),
		"width":         width,
		"height":        height,
		"size":          stat.Size(),
		"modified":      stat.ModTime().UTC().Format(time.RFC3339),
		"backup_exists": backupExists,
	})
}

// handleDeleteImage deletes a local artist image file.
// DELETE /api/v1/artists/{id}/images/{type}
//
//nolint:gocognit // Fanart delete is a distinct branch that walks every numbered variant before invoking platform-side delete; the regular-image branch uses #1161 strict-stat semantics so a transient stat error preserves the exists_flag. Each branch's response shape (HTMX preview-card vs JSON) is API contract.
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
			if err := r.fileRemover.Remove(p); err == nil {
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

	// Strict variant: only clear the exists_flag when every probe returned
	// ENOENT (file genuinely gone). A transient stat error means we cannot
	// confirm absence and must leave the flag alone. See #1161.
	if _, found, statErr := img.FindExistingImageStrict(r.imageDir(a), patterns); statErr != nil {
		r.logger.Warn("delete image: post-delete stat error; preserving exists_flag",
			slog.String("artist_id", a.ID),
			slog.String("image_type", imageType),
			slog.String("error", statErr.Error()))
	} else if !found {
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
			deleter = emby.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), r.logger)
		case connection.TypeJellyfin:
			deleter = jellyfin.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), r.logger)
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
		if err := remover.Remove(p); err == nil {
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
			if err := remover.Remove(altPath); err == nil {
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
// extractImageFetchParams returns the requested URL, normalized image type,
// and an optional fanart slot (#2281 QOL #48: per-slot fetch/replace). slot is
// nil when omitted, preserving the pre-#2281 append-next/overwrite-primary
// behavior in the caller.
func extractImageFetchParams(req *http.Request) (string, string, *int, error) {
	var rawURL, rawType string
	var slot *int
	if strings.HasPrefix(req.Header.Get("Content-Type"), "application/json") {
		var body struct {
			URL  string `json:"url"`
			Type string `json:"type"`
			Slot *int   `json:"slot,omitempty"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return "", "", nil, fmt.Errorf("invalid request body: %w", err)
		}
		rawURL, rawType, slot = body.URL, body.Type, body.Slot
	} else {
		rawURL, rawType = req.FormValue("url"), req.FormValue("type")
		if slotStr := req.FormValue("slot"); slotStr != "" {
			// #2331 CR-3: a malformed slot must error, matching the JSON
			// branch's decode-failure behavior above. Silently dropping it
			// (falling back to slot=nil, i.e. the append/overwrite-primary
			// default) would let a typo'd slot ("1O") silently take the
			// wrong action instead of surfacing the mistake.
			n, err := strconv.Atoi(slotStr)
			if err != nil {
				return "", "", nil, fmt.Errorf("invalid slot: %w", err)
			}
			slot = &n
		}
	}
	return rawURL, normalizeImageType(rawType), slot, nil
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

// degenerateTrimReason validates the output of img.TrimAlpha before it is
// allowed anywhere near backup/save, returning a non-empty reason string when
// the data is unusable (empty, or does not decode to a positive-dimension
// image), or "" when it is valid.
//
// TrimAlpha does not currently return empty bytes with a nil error (it
// re-encodes and returns the full original when no visible pixels are
// found), so this guard is defensive: it protects handleLogoTrim from ever
// touching disk with a degenerate result even if that invariant changes
// later (#2339).
func degenerateTrimReason(data []byte) string {
	if len(data) == 0 {
		return "empty trimmed image"
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		// Reason strings are server-log-only (the caller passes this to slog and
		// returns a static client message); keep it a descriptive phrase,
		// consistent with the other reasons here, rather than a raw error dump.
		return fmt.Sprintf("trimmed image did not decode: %v", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return fmt.Sprintf("invalid trimmed image dimensions (%dx%d)", cfg.Width, cfg.Height)
	}
	return ""
}

// handleLogoTrim trims the transparent border from an artist's existing logo.
// POST /api/v1/artists/{id}/images/logo/trim
func (r *Router) handleLogoTrim(w http.ResponseWriter, req *http.Request) {
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}
	if !r.gateImageWrite(w, req) {
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
	// Bound the read (#2620, same defect class as #2618). The PATH is trusted
	// -- it comes from FindExistingImage over naming patterns -- but the
	// file's CONTENTS are not sized by us: this is an operator-supplied
	// library directory, and an arbitrarily large file sitting in one used to
	// be read whole into memory right here, on a REQUEST-reachable path. Go
	// has no allocation-failure path, so an over-budget allocation is a fatal
	// runtime error rather than an error value: under a container memory
	// limit that is a SIGKILL and a restart loop, not something this handler
	// could recover from.
	//
	// io.LimitReader, not os.Stat: a Stat-then-read has a TOCTOU window in
	// which the file can grow between the size check and the read, so it
	// bounds a MEASUREMENT while the read stays unbounded. LimitReader bounds
	// the allocation itself. Reading one byte PAST the bound is what
	// distinguishes "exactly at the bound" from "over it" -- without the +1
	// an oversized file reads exactly MaxDecodeBytes, LimitReader returns a
	// clean EOF, and a truncated prefix gets trimmed and SAVED OVER the
	// original as though it were the whole logo. That silent-corruption
	// failure mode is worse than the unbounded read this guard replaces.
	data, readErr := io.ReadAll(io.LimitReader(f, img.MaxDecodeBytes+1))
	_ = f.Close()
	if readErr != nil {
		r.logger.Error("reading logo for trim",
			slog.String("artist_id", artistID),
			slog.String("path", filePath),
			slog.String("error", readErr.Error()))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read logo"})
		return
	}
	if int64(len(data)) > img.MaxDecodeBytes {
		r.logger.Error("logo exceeds the decode bound; trim aborted before any read of the full file",
			slog.String("artist_id", artistID),
			slog.String("path", filePath),
			slog.Int64("max_bytes", img.MaxDecodeBytes),
			slog.String("error", img.ErrImageTooLarge.Error()))
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "logo exceeds 25MB limit; not trimmed"})
		return
	}

	trimmed, _, err := img.TrimAlpha(bytes.NewReader(data), 10)
	if err != nil {
		r.logger.Error("trimming logo alpha", "artist_id", artistID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to trim logo"})
		return
	}

	// Degeneracy guard (#2339): TrimAlpha does not currently return empty
	// bytes with a nil error (it re-encodes and returns the full original
	// when no visible pixels are found), but this is a cheap defensive check
	// against that class of bug: reject a trim result that is empty or does
	// not decode to a valid positive-dimension image BEFORE touching backup
	// or save, so the canonical original is never disturbed by a degenerate
	// result. No success-implying HX-Trigger is set on this path.
	if reason := degenerateTrimReason(trimmed); reason != "" {
		r.logger.Error("logo trim produced a degenerate result; aborting before backup/save",
			slog.String("artist_id", artistID), slog.String("reason", reason))
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "logo trim produced no usable image; original kept"})
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

	// Non-destructive: preserve the pre-trim original (#1837). On a backup
	// failure (transient stat error or write error) ABORT the trim with 500 and
	// do NOT call Save, so the original logo is never destroyed without a
	// recoverable backup.
	if bErr := img.BackupSingleSlot(r.imageDir(a), "logo", patterns); bErr != nil {
		r.logger.Error("backing up logo before trim; aborting",
			slog.String("artist_id", artistID), slog.String("error", bErr.Error()))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to preserve pre-trim original; trim aborted"})
		return
	}

	_, useSymlinks := r.getActiveNamingAndSymlinks(req.Context(), "logo")
	res := r.saveSingleSlotWithRollback(r.imageDir(a), "logo", patterns, useSymlinks, trimMeta, trimmed)
	if res.SaveErr != nil {
		if res.RestoreErr != nil {
			// Worst case: the save failed AND the automatic rollback also
			// failed. Neither the trim nor the original landed cleanly, so
			// this must never be silent (#2339).
			r.logger.Error("saving trimmed logo failed and automatic restore also failed; manual recovery may be needed",
				slog.String("artist_id", artistID),
				slog.String("save_error", res.SaveErr.Error()),
				slog.String("restore_error", res.RestoreErr.Error()))
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save trimmed logo and automatic recovery also failed; manual recovery may be needed"})
			return
		}
		r.logger.Error("saving trimmed logo failed; original restored",
			slog.String("artist_id", artistID), slog.String("error", res.SaveErr.Error()))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save trimmed logo; original restored"})
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

// handleImageRevert restores the most recent pre-edit original for a single-slot
// kind (thumb/logo/banner) from the peer-inert backup in the .sw-backup subdir,
// or drops the newest derived fanart slot for the multi-slot fanart kind. It is a
// write, so it goes through the conflict gate. #1837.
// POST /api/v1/artists/{id}/images/{type}/revert
func (r *Router) handleImageRevert(w http.ResponseWriter, req *http.Request) {
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}
	// Destructive (drops the newest fanart slot or rebuilds over the canonical):
	// fail CLOSED if the gate cannot be evaluated.
	if !r.gateImageWriteStrict(w, req) {
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
	dir := r.imageDir(a)

	if imageType == "fanart" {
		// Multi-slot: revert == drop the newest derived slot (highest index),
		// leaving the original slot 0 intact. 404 when only the original exists.
		primary := r.getActiveFanartPrimary(req.Context())
		paths, discErr := img.DiscoverFanart(dir, primary)
		if discErr != nil {
			r.logger.Error("discovering fanart for revert", slog.String("artist_id", artistID), slog.String("error", discErr.Error()))
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read fanart directory"})
			return
		}
		if len(paths) < 2 {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no derived fanart slot to revert"})
			return
		}
		newest := paths[len(paths)-1]
		if rmErr := r.fileRemover.Remove(newest); rmErr != nil {
			r.logger.Error("dropping derived fanart slot", slog.String("artist_id", artistID), slog.String("path", newest), slog.String("error", rmErr.Error()))
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to revert fanart"})
			return
		}
		r.updateArtistFanartCount(req.Context(), a)
		warnings := r.revertSideEffects(req.Context(), a, imageType)
		writeJSON(w, http.StatusOK, map[string]any{"status": "reverted", "type": imageType, "count": a.FanartCount, "sync_warnings": warnings})
		return
	}

	// Single-slot: restore the original from its one-deep backup. RestoreSingleSlot
	// routes the backup bytes through img.Save so ALL configured names + symlinks
	// are rebuilt and the post-edit format (e.g. a jpg written over a png crop)
	// is cleaned up. The backup is keyed by image TYPE, so a format-changing edit
	// is still revertible (#1837).
	naming, useSymlinks := r.getActiveNamingAndSymlinks(req.Context(), imageType)
	if restoreErr := img.RestoreSingleSlot(dir, imageType, naming, useSymlinks, nil, r.logger); restoreErr != nil {
		if os.IsNotExist(restoreErr) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no backup to revert"})
			return
		}
		r.logger.Error("reverting single-slot image", slog.String("artist_id", artistID), slog.String("type", imageType), slog.String("error", restoreErr.Error()))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to revert image"})
		return
	}
	// DB bookkeeping (exists flag, low-res, placeholder) is intentionally
	// best-effort on the revert path: setArtistImageFlag logs any Update failure
	// at Warn and the on-disk file is already the source of truth, so a stale
	// flag self-heals on the next scan. This matches every other image write
	// path; we do not fail the revert on a DB hiccup (#1837, F7).
	r.updateArtistImageFlag(req.Context(), a, imageType)
	warnings := r.revertSideEffects(req.Context(), a, imageType)
	writeJSON(w, http.StatusOK, map[string]any{"status": "reverted", "type": imageType, "sync_warnings": warnings})
}

// revertSideEffects performs the post-write bookkeeping shared by both revert
// branches: cache eviction, event publish, health invalidation, rule re-eval,
// and platform sync of the now-canonical image. It RETURNS the platform-sync
// warnings so both revert responses can surface them under "sync_warnings",
// matching every other write path (#1837).
func (r *Router) revertSideEffects(ctx context.Context, a *artist.Artist, imageType string) []string {
	r.enforceCacheLimitIfNeeded(ctx, a)
	if r.eventBus != nil {
		r.eventBus.Publish(event.Event{Type: event.ArtistUpdated, Data: map[string]any{"artist_id": a.ID}})
	}
	r.InvalidateHealthCache()
	r.runRulesAfterRefresh(ctx, a)
	syncCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if imageType == "fanart" {
		return r.publisher.SyncAllFanartToPlatforms(syncCtx, a)
	}
	return r.publisher.SyncImageToPlatforms(syncCtx, a, imageType)
}

// ensureHTTPS rewrites an http:// URL to https:// for CSP compatibility.
// URLs already using https://, data: URIs, and any other scheme are returned
// unchanged. Only the scheme prefix is replaced; path, host, and query are
// preserved exactly.
func ensureHTTPS(u string) string {
	if strings.HasPrefix(u, "http://") {
		return "https://" + u[len("http://"):]
	}
	return u
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

// fanartNamesStrict returns EVERY fanart filename that could name this
// artist's fanart, in preference order, and surfaces a profile lookup failure
// instead of papering over it.
//
// getActiveFanartPrimary substitutes the built-in defaults when GetActive
// errors, which is fine for the read-only callers that only need a name to
// display or to write a new file under. It is not fine for enumeration. An
// enumeration's count is a positive claim about what exists, and a guessed
// convention that misses produces a confident zero against a directory full of
// artwork. Refusing is the only safe answer to "we could not determine the
// naming convention" (#2635).
func (r *Router) fanartNamesStrict(ctx context.Context) ([]string, error) {
	profile, err := r.platformService.GetActive(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolving active platform profile: %w", err)
	}
	var configured []string
	if profile != nil {
		configured = profile.ImageNaming.NamesForType("fanart")
	}
	// The profile-names-UNION-defaults resolution (profile first, case-insensitive
	// dedup, empty-result-is-an-error) is shared with the rule engine via
	// img.ResolveFanartNames so the two never disagree about one directory; the
	// active-profile lookup and its error propagation stay here because they are
	// platform-coupled.
	return img.ResolveFanartNames(configured)
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
// scope carries the #2565 cross-artist collision check; it may be nil, which
// disables the check (fail-open) without affecting the write.
func (r *Router) processAndAppendFanart(ctx context.Context, scope *imageWriteScope, dir string, data []byte, meta *img.ExifMeta) ([]string, error) {
	converted, _, err := img.ConvertFormat(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("converting format: %w", err)
	}

	// #2565 NOTIFY-ONLY: decide the cross-artist collision verdict HERE, while the
	// converted bytes are in hand, but HOLD it. It is announced further down, only
	// once img.Save has confirmed the file exists -- see notifyCollision for why
	// the ordering is load-bearing.
	collisionResult := scope.collisionVerdict(ctx, converted)

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
	if len(saved) == 0 {
		// Save reported success but produced no file. Treat this as a failed write
		// rather than a silent success: the collision notification below must never
		// fire for artwork that is not on disk.
		return nil, fmt.Errorf("saving: produced no files in %s", dir)
	}

	// The save is confirmed (no error, at least one file written), so the image the
	// collision was detected on genuinely exists. Only now is it correct to raise
	// the notification, whose durable half carries a back-out auto-fix. The append
	// itself was never blocked -- this runs after the write, not instead of it.
	scope.notifyCollision(ctx, collisionResult)

	return saved, nil
}

// updateArtistFanartCount discovers fanart files and updates both the exists
// flag and count on the artist record.
func (r *Router) updateArtistFanartCount(ctx context.Context, a *artist.Artist) {
	names, namesErr := r.fanartNamesStrict(ctx)
	if namesErr != nil {
		r.logger.Error("resolving fanart naming convention for count update; skipping DB update",
			slog.String("artist_id", a.ID),
			slog.String("error", namesErr.Error()))
		return
	}
	_, existing, discoverErr := img.ResolveFanart(r.imageDir(a), names)
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
		if f, err := os.Open(existing[0]); err == nil {
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
		if stat, statErr := os.Stat(p); statErr == nil {
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
			renderTempl(w, req, templates.FanartGallery(artistID, items, r.getActiveProfileName(req.Context())))
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

	// Prevent conditional 304 responses for fanart-by-index URLs.
	// After a reorder, os.Rename preserves mtime, so a promoted image
	// may have an older mtime than what the browser cached for this
	// index, causing http.ServeFile to return a stale 304. Stripping
	// the conditional headers forces ServeFile to always send content;
	// no-store prevents the browser from caching the response.
	req.Header.Del("If-Modified-Since")
	req.Header.Del("If-None-Match")
	w.Header().Set("Cache-Control", "no-store")
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
	// Destructive (deletes bytes): fail CLOSED if the gate cannot be evaluated.
	if !r.gateImageWriteStrict(w, req) {
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
		if err := r.fileRemover.Remove(paths[idx]); err == nil {
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
	} else if renumberErr := img.RenumberFanart(req.Context(), r.artistService, a.ID, r.imageDir(a), primary, survivors, kodi); renumberErr != nil {
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
	if !r.gateImageWrite(w, req) {
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

	// #2565: ONE collision scope for the whole batch. The identity index it builds
	// lazily is a WHOLE-LIBRARY scan, and this loop pushes up to maxBatchURLs
	// images through it -- creating a scope per image would repeat that scan for
	// every URL. Hoisting it here is what makes the once-per-scope contract
	// (design-2540.md section 4) hold on the batch path.
	collisionScope := r.newImageWriteScope(a)

	var allSaved []string
	var errMsgs []string
	for _, u := range body.URLs {
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			errMsgs = append(errMsgs, fmt.Sprintf("invalid url: %s", u))
			continue
		}
		if isPrivateURL(req.Context(), u) {
			errMsgs = append(errMsgs, fmt.Sprintf("private/reserved address: %s", u))
			continue
		}
		data, fetchErr := r.fetchImageFromURL(req.Context(), u)
		if fetchErr != nil {
			r.logger.Warn("fetching fanart image", "url", u, "error", fetchErr)
			errMsgs = append(errMsgs, fmt.Sprintf("fetch failed: %s", u))
			continue
		}
		batchMeta := &img.ExifMeta{Source: artist.ImageSourceUser, Fetched: time.Now().UTC(), URL: u, Mode: "user"}
		saved, saveErr := r.processAndAppendFanart(req.Context(), collisionScope, r.imageDir(a), data, batchMeta)
		if saveErr != nil {
			r.logger.Error("saving fanart image", "url", u, "error", saveErr)
			errMsgs = append(errMsgs, fmt.Sprintf("save failed: %s", u))
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
		"errors":        errMsgs,
		"count":         a.FanartCount,
		"sync_warnings": syncWarnings,
	})
}

// handleRandomBackdrop serves a random artist fanart file for the ambient
// backdrop feature. It queries all artists with exists_flag=1 in random order,
// serves the first one whose file is actually present on disk, and clears the
// flag for every stale entry it encounters along the way. This self-heals the
// DB so that exists_flag=1 stays accurate without a separate cleanup pass.
// GET /api/v1/images/random-backdrop
func (r *Router) handleRandomBackdrop(w http.ResponseWriter, req *http.Request) {
	// Resolve naming patterns before opening the rows cursor. Drain the cursor
	// into a slice before doing per-artist lookups so that the single-connection
	// pool is never held by two concurrent queries.
	patterns := r.getActiveNamingConfig(req.Context(), "fanart")

	rows, err := r.db.QueryContext(req.Context(),
		`SELECT artist_id FROM artist_images
		 WHERE image_type = 'fanart' AND slot_index = 0 AND exists_flag = 1
		 ORDER BY RANDOM()`)
	if err != nil {
		r.logger.Error("random backdrop query failed", slog.String("error", err.Error()))
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	defer func() {
		if err := rows.Close(); err != nil {
			r.logger.Warn("random backdrop rows close failed", slog.String("error", err.Error()))
		}
	}()

	var artistIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			r.logger.Error("random backdrop scan failed", slog.String("error", err.Error()))
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		artistIDs = append(artistIDs, id)
	}
	if err := rows.Err(); err != nil {
		r.logger.Error("random backdrop iteration failed", slog.String("error", err.Error()))
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	for _, artistID := range artistIDs {
		a, err := r.artistService.GetByID(req.Context(), artistID)
		if err != nil {
			r.logger.Warn("random backdrop artist lookup failed",
				slog.String("artist_id", artistID),
				slog.String("error", err.Error()))
			continue
		}

		dir := r.imageDir(a)
		if dir == "" {
			continue
		}

		// Strict variant: only clear the exists_flag when every probe returned
		// ENOENT. A transient stat error must skip this artist without
		// touching the flag (see #1161).
		filePath, found, statErr := img.FindExistingImageStrict(dir, patterns)
		if statErr != nil {
			r.logger.Warn("random backdrop: stat error probing artist dir; preserving exists_flag",
				slog.String("artist_id", a.ID),
				slog.String("error", statErr.Error()))
			continue
		}
		if !found {
			// File is genuinely gone despite exists_flag=1; clear the stale flag and keep looking.
			if err := r.artistService.ClearImageFlag(req.Context(), a.ID, "fanart", 0); err != nil {
				r.logger.Warn("failed to clear stale backdrop flag",
					slog.String("artist_id", a.ID),
					slog.String("error", err.Error()))
			} else {
				r.logger.Info("cleared stale backdrop flag", slog.String("artist_id", a.ID))
			}
			continue
		}

		// no-store: the backing file changes on each call, so the browser
		// must not cache the response at all. http.ServeContent with a zero
		// modtime suppresses ETag and Last-Modified so no conditional
		// request can produce a stale 304.
		f, err := os.Open(filePath) //nolint:gosec // path from img.FindExistingImage, not user input
		if err != nil {
			r.logger.Warn("random backdrop open failed",
				slog.String("artist_id", a.ID),
				slog.String("path", filePath),
				slog.String("error", err.Error()))
			continue
		}
		w.Header().Set("Cache-Control", "no-store")
		http.ServeContent(w, req, filepath.Base(filePath), time.Time{}, f)
		if err := f.Close(); err != nil {
			r.logger.Warn("random backdrop file close failed",
				slog.String("artist_id", a.ID),
				slog.String("path", filePath),
				slog.String("error", err.Error()))
		}
		return
	}

	// No valid fanart found (pool empty or all entries were stale).
	http.NotFound(w, req)
}
