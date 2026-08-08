package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
	"github.com/sydlexius/stillwater/internal/connection/jellyfin"
	img "github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/publish"
)

// handlePushMetadata pushes artist metadata to a specified platform connection.
// POST /api/v1/artists/{id}/push
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

	members, memberErr := r.artistService.ListMembersByArtistID(req.Context(), artistID)
	if memberErr != nil {
		r.logger.Warn("listing band members for push", "artist_id", artistID, "error", memberErr)
		members = nil
	}

	data := publish.BuildArtistPushData(a, members)

	pusher, ok := publish.NewMetadataPusher(conn, r.logger)
	if !ok {
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

// writeCanceledPush ends a push whose request was canceled or timed out.
//
// It answers 499 rather than 200-with-errors (#2934). The old shape recorded
// the cancellation as one more per-slot read failure and carried on, so the
// client received a SUCCESS carrying "fanart[2]: read failed" -- a response
// that names the operator's artwork for a fault that was never in the file,
// while the slots before it really were pushed to the peer. Neither half is
// something a client can act on.
//
// 499 (nginx's "Client Closed Request") is the honest code: no standard status
// describes "the caller went away", and every 2xx here would be a claim about
// work that did not finish. It is also unambiguous in a log, which matters
// because the alternative failure mode of this handler is a 200 that hides the
// abort entirely.
//
// The slots ALREADY uploaded are reported rather than dropped. They genuinely
// reached the peer, and a client that reconciles state needs to know which --
// omitting them would make a partial push look like no push at all, which is
// the same class of lie in the opposite direction.
func writeCanceledPush(w http.ResponseWriter, log *slog.Logger, artistName string, uploaded []string, cause error) {
	// TWO CAUSES, TWO ANSWERS, and conflating them blames the wrong party
	// (#2976 review). Both stop the push early, but a cancellation is the
	// CLIENT ending the request while a stalled-read cap refusal is the
	// SERVER unable to read its own library -- the request context is still
	// perfectly alive. Reporting the latter as 499 "the request ended" tells
	// an operator their browser gave up when what actually happened is that
	// their mount stopped answering, sending them to debug the wrong end of
	// the system entirely.
	if errors.Is(cause, img.ErrTooManyStalledReads) {
		log.Error("image push stopped: the library mount is not responding",
			slog.String("artist", artistName),
			slog.Int("uploaded_before_stall", len(uploaded)),
			slog.Any("error", cause))
		// The sentinel's own message names the condition without leaking a
		// path or an internal error chain, so it is safe to surface verbatim.
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error":    "push stopped: the library mount is not responding, so the remaining images could not be read",
			"uploaded": uploaded,
		})
		return
	}
	log.Warn("image push canceled by the client; stopping before any further upload",
		slog.String("artist", artistName),
		slog.Int("uploaded_before_cancel", len(uploaded)),
		slog.Any("error", cause))
	writeJSON(w, StatusClientClosedRequest, map[string]any{
		"error":    "push canceled: the request ended before all images could be read",
		"uploaded": uploaded,
	})
}

// StatusClientClosedRequest is nginx's non-standard 499. net/http has no
// constant for it because it is not in the RFC, and inventing a 4xx of our own
// would be worse: 499 is the code operators already recognize in a proxy log
// for exactly this condition.
const StatusClientClosedRequest = 499

// handlePushImages uploads artist images to an Emby/Jellyfin connection.
// POST /api/v1/artists/{id}/push/images
//
//nolint:gocognit // Per-image-type upload: enumerate poster/banner/clearart/disc/fanart with per-type file-presence check, MIME detection, connection-dispatch (Emby vs Jellyfin client), and outcome accounting; the per-type branches share enough state that helpers would have 8+ parameters.
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

	var client interface {
		connection.ImageUploader
		connection.IndexedImageUploader
	}
	switch conn.Type {
	case connection.TypeEmby:
		client = emby.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), r.logger)
	case connection.TypeJellyfin:
		client = jellyfin.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), r.logger)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "connection type does not support image upload"})
		return
	}

	var uploaded []string
	var uploadErrs []string

	for _, imgType := range body.ImageTypes {
		if !validImageTypes[imgType] {
			uploadErrs = append(uploadErrs, fmt.Sprintf("%s: invalid image type", imgType))
			continue
		}

		// Fanart: upload all discovered fanart files at their respective indices.
		if imgType == "fanart" {
			primary := r.getActiveFanartPrimary(req.Context())
			fanartPaths, discoverErr := img.DiscoverFanart(req.Context(), r.imageDir(a), primary)
			if discoverErr != nil {
				r.logger.Error("discovering fanart for push",
					slog.String("artist_id", a.ID),
					slog.String("error", discoverErr.Error()))
				uploadErrs = append(uploadErrs, "fanart: failed to read directory")
				continue
			}
			for i, fp := range fanartPaths {
				// Bounded, ctx-aware read (#2934): DiscoverFanart above already
				// honors the request context, so a bare os.ReadFile here left
				// the actual byte read as the one step a dead mount could wedge
				// forever -- with the request's own cancellation unable to
				// reach it. The bound also caps the allocation for an
				// arbitrarily large operator file.
				data, readErr := img.ReadImageFileBounded(req.Context(), fp)
				if readErr != nil {
					// A CANCELED REQUEST IS NOT A BAD FILE (#2934). Bounding
					// this read stopped the handler wedging on a dead mount; it
					// did not stop a canceled request answering 200 with an
					// errors list. ReadImageFileBounded returns the context
					// error in the same shape an unreadable file produces, so
					// classifying it per-slot let the loop carry on PUSHING the
					// remaining slots to the peer for a request the operator had
					// already abandoned. Interrogate the context, not the
					// error's contents; an ordinary read failure still reports
					// its own slot and continues.
					// The stalled-read cap says the same thing a cancellation
					// does, by a different route (#2933): the read did not
					// happen for a reason that applies to every remaining slot.
					// Classified per-slot, the loop kept PUSHING the rest to
					// the peer while unable to read any of them, and the
					// handler answered 200 with an errors list -- reporting a
					// push it could not actually perform.
					if distrust := img.ReadFailureDistrustsLoop(req.Context(), readErr); distrust != nil {
						writeCanceledPush(w, r.logger, a.Name, uploaded, distrust)
						return
					}
					r.logger.Error("reading fanart for push",
						slog.String("path", fp),
						slog.String("artist", a.Name),
						slog.Int("index", i),
						slog.String("error", readErr.Error()))
					uploadErrs = append(uploadErrs, fmt.Sprintf("fanart[%d]: read failed", i))
					continue
				}
				ct := "image/jpeg"
				if strings.EqualFold(filepath.Ext(fp), ".png") {
					ct = "image/png"
				}
				if uploadErr := client.UploadImageAtIndex(req.Context(), body.PlatformArtistID, imgType, i, data, ct); uploadErr != nil {
					r.logger.Error("uploading fanart",
						slog.String("artist", a.Name),
						slog.Int("index", i),
						slog.String("error", uploadErr.Error()))
					uploadErrs = append(uploadErrs, fmt.Sprintf("fanart[%d]: upload failed", i))
					continue
				}
				uploaded = append(uploaded, fmt.Sprintf("fanart[%d]", i))
			}
			continue
		}

		patterns := r.getActiveNamingConfig(req.Context(), imgType)
		filePath, found := img.FindExistingImage(req.Context(), r.imageDir(a), patterns)
		if !found {
			continue
		}

		// Bounded, ctx-aware read (#2934), same reasoning as the fanart branch:
		// FindExistingImage above is ctx-bound and this read was not.
		data, readErr := img.ReadImageFileBounded(req.Context(), filePath)
		if readErr != nil {
			// Same distinction as the fanart branch above, and it is a separate
			// read reached through a different path, so it needs its own guard.
			if ctxErr := req.Context().Err(); ctxErr != nil {
				writeCanceledPush(w, r.logger, a.Name, uploaded, ctxErr)
				return
			}
			r.logger.Error("reading image for push",
				slog.String("path", filePath),
				slog.String("artist", a.Name),
				slog.String("type", imgType),
				slog.String("error", readErr.Error()))
			uploadErrs = append(uploadErrs, fmt.Sprintf("%s: read failed", imgType))
			continue
		}

		ct := "image/jpeg"
		if strings.EqualFold(filepath.Ext(filePath), ".png") {
			ct = "image/png"
		}

		if uploadErr := client.UploadImage(req.Context(), body.PlatformArtistID, imgType, data, ct); uploadErr != nil {
			r.logger.Error("uploading image",
				slog.String("artist", a.Name),
				slog.String("type", imgType),
				slog.String("error", uploadErr.Error()))
			uploadErrs = append(uploadErrs, fmt.Sprintf("%s: upload failed", imgType))
			continue
		}

		uploaded = append(uploaded, imgType)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"uploaded": uploaded,
		// The JSON KEY stays "errors" -- it is the wire contract clients read.
		// Only the Go identifier changed, to stop the local slice shadowing the
		// errors package now that this file calls errors.Is (#2976 review).
		"errors": uploadErrs,
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
		deleter = emby.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), r.logger)
	case connection.TypeJellyfin:
		deleter = jellyfin.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), r.logger)
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
