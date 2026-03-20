package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
	"github.com/sydlexius/stillwater/internal/connection/jellyfin"
	img "github.com/sydlexius/stillwater/internal/image"
	templates "github.com/sydlexius/stillwater/web/templates"
)

// backdropClient can fetch artist detail (including backdrop count) and
// download individual backdrop images from a media platform.
type backdropClient interface {
	connection.ArtistStateGetter
	GetArtistBackdrop(ctx context.Context, artistID string, index int) ([]byte, string, error)
}

// newBackdropClient instantiates a backdropClient for the given connection type.
func (r *Router) newBackdropClient(conn *connection.Connection) (backdropClient, error) {
	switch conn.Type {
	case connection.TypeEmby:
		return emby.New(conn.URL, conn.APIKey, conn.PlatformUserID, r.logger), nil
	case connection.TypeJellyfin:
		return jellyfin.New(conn.URL, conn.APIKey, conn.PlatformUserID, r.logger), nil
	default:
		return nil, fmt.Errorf("connection type %q does not support backdrop operations", conn.Type)
	}
}

// platformBackdropItem represents a single backdrop available on a platform.
type platformBackdropItem struct {
	Index        int    `json:"index"`
	ThumbnailURL string `json:"thumbnail_url"`
}

// platformBackdropConnection represents a connection and its available backdrops.
type platformBackdropConnection struct {
	ConnectionID   string                 `json:"connection_id"`
	ConnectionName string                 `json:"connection_name"`
	ConnectionType string                 `json:"connection_type"`
	Backdrops      []platformBackdropItem `json:"backdrops"`
}

// handlePlatformBackdrops lists available backdrops from all connected platforms.
// GET /api/v1/artists/{id}/platform-backdrops
func (r *Router) handlePlatformBackdrops(w http.ResponseWriter, req *http.Request) {
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		r.logger.Warn("getting artist for backdrop listing",
			slog.String("artist_id", artistID),
			slog.String("error", err.Error()))
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "artist not found"})
		return
	}

	platformIDs, err := r.artistService.GetPlatformIDs(req.Context(), a.ID)
	if err != nil {
		r.logger.Error("fetching platform IDs for backdrop listing",
			slog.String("artist_id", artistID),
			slog.String("error", err.Error()))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to fetch platform mappings"})
		return
	}

	var connections []platformBackdropConnection
	for _, pid := range platformIDs {
		conn, connErr := r.connectionService.GetByID(req.Context(), pid.ConnectionID)
		if connErr != nil {
			r.logger.Warn("skipping connection for backdrop listing",
				slog.String("connection_id", pid.ConnectionID),
				slog.String("error", connErr.Error()))
			continue
		}
		if !conn.Enabled || conn.Status != "ok" {
			continue
		}

		client, clientErr := r.newBackdropClient(conn)
		if clientErr != nil {
			r.logger.Warn("skipping connection for backdrop listing",
				slog.String("connection_id", conn.ID),
				slog.String("type", conn.Type),
				slog.String("error", clientErr.Error()))
			continue
		}

		state, stateErr := client.GetArtistDetail(req.Context(), pid.PlatformArtistID)
		if stateErr != nil {
			r.logger.Warn("fetching artist detail for backdrop listing",
				slog.String("connection_id", conn.ID),
				slog.String("platform_artist_id", pid.PlatformArtistID),
				slog.String("error", stateErr.Error()))
			continue
		}

		if state.BackdropCount == 0 {
			continue
		}

		backdrops := make([]platformBackdropItem, state.BackdropCount)
		for i := range state.BackdropCount {
			backdrops[i] = platformBackdropItem{
				Index:        i,
				ThumbnailURL: fmt.Sprintf("/api/v1/artists/%s/platform-backdrops/%s/%d/thumbnail", a.ID, conn.ID, i),
			}
		}

		connections = append(connections, platformBackdropConnection{
			ConnectionID:   conn.ID,
			ConnectionName: conn.Name,
			ConnectionType: conn.Type,
			Backdrops:      backdrops,
		})
	}

	if connections == nil {
		connections = []platformBackdropConnection{}
	}

	if isHTMXRequest(req) {
		// Compute next available fanart slot for the assign button.
		primary := r.getActiveFanartPrimary(req.Context())
		discovered, discoverErr := img.DiscoverFanart(r.imageDir(a), primary)
		if discoverErr != nil && !errors.Is(discoverErr, os.ErrNotExist) {
			r.logger.Error("discovering fanart for backdrop listing",
				slog.String("artist_id", a.ID),
				slog.String("error", discoverErr.Error()))
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to determine next fanart slot"})
			return
		}
		nextSlot := len(discovered) // 0 when dir does not exist

		var templConns []templates.PlatformBackdropConnectionData
		for _, c := range connections {
			tc := templates.PlatformBackdropConnectionData{
				ConnectionID:   c.ConnectionID,
				ConnectionName: c.ConnectionName,
				ConnectionType: c.ConnectionType,
			}
			for _, bd := range c.Backdrops {
				tc.Backdrops = append(tc.Backdrops, templates.PlatformBackdropItemData{
					Index:        bd.Index,
					ThumbnailURL: bd.ThumbnailURL,
					NextSlot:     nextSlot,
				})
			}
			templConns = append(templConns, tc)
		}
		renderTempl(w, req, templates.PlatformBackdropsPanel(a.ID, templConns))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"connections": connections})
}

// handlePlatformBackdropThumbnail proxies a platform backdrop image through
// Stillwater so browser requests do not need platform API keys.
// GET /api/v1/artists/{id}/platform-backdrops/{connectionId}/{index}/thumbnail
func (r *Router) handlePlatformBackdropThumbnail(w http.ResponseWriter, req *http.Request) {
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}
	connID, ok := RequirePathParam(w, req, "connectionId")
	if !ok {
		return
	}
	indexStr := req.PathValue("index")
	index, err := strconv.Atoi(indexStr)
	if err != nil || index < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid backdrop index"})
		return
	}

	// Look up the platform artist ID for this connection.
	platformArtistID, err := r.artistService.GetPlatformID(req.Context(), artistID, connID)
	if err != nil {
		r.logger.Error("looking up platform ID for backdrop thumbnail",
			slog.String("artist_id", artistID),
			slog.String("connection_id", connID),
			slog.String("error", err.Error()))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to look up platform mapping"})
		return
	}
	if platformArtistID == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no platform mapping for this connection"})
		return
	}

	conn, err := r.connectionService.GetByID(req.Context(), connID)
	if err != nil {
		r.logger.Error("getting connection for backdrop thumbnail",
			slog.String("connection_id", connID),
			slog.String("error", err.Error()))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load connection"})
		return
	}

	client, err := r.newBackdropClient(conn)
	if err != nil {
		r.logger.Error("creating backdrop client", slog.String("connection_id", connID), slog.String("error", err.Error()))
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported connection type"})
		return
	}

	data, contentType, err := client.GetArtistBackdrop(req.Context(), platformArtistID, index)
	if err != nil {
		r.logger.Warn("proxying platform backdrop",
			slog.String("connection_id", connID),
			slog.String("platform_artist_id", platformArtistID),
			slog.Int("index", index),
			slog.String("error", err.Error()))
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "failed to fetch backdrop from platform"})
		return
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data) //nolint:gosec // ResponseWriter.Write error after WriteHeader is unrecoverable
}

// handleFanartSlotAssign assigns a platform backdrop to a specific local fanart slot.
// POST /api/v1/artists/{id}/images/fanart/{slot}/assign
func (r *Router) handleFanartSlotAssign(w http.ResponseWriter, req *http.Request) {
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		r.logger.Warn("getting artist for slot assign",
			slog.String("artist_id", artistID),
			slog.String("error", err.Error()))
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "artist not found"})
		return
	}
	if !r.requireImageDir(w, req, a) {
		return
	}

	slotStr := req.PathValue("slot")
	slot, err := strconv.Atoi(slotStr)
	if err != nil || slot < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid slot index"})
		return
	}

	var body struct {
		ConnectionID  string `json:"connection_id"`
		PlatformIndex int    `json:"platform_index"`
	}
	if !DecodeJSON(w, req, &body) {
		return
	}
	if body.ConnectionID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "connection_id is required"})
		return
	}
	if body.PlatformIndex < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "platform_index must be non-negative"})
		return
	}

	// Validate slot density: reject gaps (slot must be <= current count).
	primary := r.getActiveFanartPrimary(req.Context())
	kodi := r.isKodiNumbering(req.Context())
	existing, discoverErr := img.DiscoverFanart(r.imageDir(a), primary)
	if discoverErr != nil && !errors.Is(discoverErr, os.ErrNotExist) {
		r.logger.Error("discovering fanart for slot assign",
			slog.String("artist_id", artistID),
			slog.String("error", discoverErr.Error()))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read fanart directory"})
		return
	}
	if slot > len(existing) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("slot %d would create a gap (current count: %d)", slot, len(existing)),
		})
		return
	}

	// Look up platform artist ID and download the backdrop.
	platformArtistID, err := r.artistService.GetPlatformID(req.Context(), artistID, body.ConnectionID)
	if err != nil {
		r.logger.Error("looking up platform ID for slot assign",
			slog.String("artist_id", artistID),
			slog.String("connection_id", body.ConnectionID),
			slog.String("error", err.Error()))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to look up platform mapping"})
		return
	}
	if platformArtistID == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no platform mapping for this connection"})
		return
	}

	conn, err := r.connectionService.GetByID(req.Context(), body.ConnectionID)
	if err != nil {
		r.logger.Error("getting connection for slot assign",
			slog.String("connection_id", body.ConnectionID),
			slog.String("error", err.Error()))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load connection"})
		return
	}

	client, err := r.newBackdropClient(conn)
	if err != nil {
		r.logger.Error("creating backdrop client for slot assign", slog.String("connection_id", body.ConnectionID), slog.String("error", err.Error()))
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported connection type"})
		return
	}

	data, _, err := client.GetArtistBackdrop(req.Context(), platformArtistID, body.PlatformIndex)
	if err != nil {
		r.logger.Error("downloading platform backdrop for slot assign",
			slog.String("artist_id", artistID),
			slog.String("connection_id", body.ConnectionID),
			slog.Int("platform_index", body.PlatformIndex),
			slog.String("error", err.Error()))
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "failed to download backdrop from platform"})
		return
	}

	// Convert WebP to JPG/PNG if needed.
	converted, _, convErr := img.ConvertFormat(bytes.NewReader(data))
	if convErr != nil {
		r.logger.Error("converting backdrop format",
			slog.String("artist_id", artistID),
			slog.String("error", convErr.Error()))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to process image format"})
		return
	}

	targetName := img.FanartFilename(primary, slot, kodi)
	dir := r.imageDir(a)

	_, saveErr := img.Save(dir, "fanart", converted, []string{targetName}, false, r.logger)
	if saveErr != nil {
		r.logger.Error("saving backdrop to slot",
			slog.String("artist_id", artistID),
			slog.Int("slot", slot),
			slog.String("error", saveErr.Error()))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save image"})
		return
	}

	r.updateArtistFanartCount(req.Context(), a)

	// Sync all fanart to connected platforms.
	syncCtx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
	defer cancel()
	syncWarnings := r.syncAllFanartToPlatforms(syncCtx, a)

	r.logger.Info("assigned platform backdrop to fanart slot",
		slog.String("artist_id", artistID),
		slog.String("connection_id", body.ConnectionID),
		slog.Int("platform_index", body.PlatformIndex),
		slog.Int("slot", slot))

	if isHTMXRequest(req) {
		setSyncWarningTrigger(w, syncWarnings)
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "assigned",
		"slot":          slot,
		"count":         a.FanartCount,
		"sync_warnings": syncWarnings,
	})
}

// handleFanartSlotDelete deletes a single local fanart slot and renumbers remaining files.
// DELETE /api/v1/artists/{id}/images/fanart/{slot}
func (r *Router) handleFanartSlotDelete(w http.ResponseWriter, req *http.Request) {
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		r.logger.Warn("getting artist for slot delete",
			slog.String("artist_id", artistID),
			slog.String("error", err.Error()))
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "artist not found"})
		return
	}
	if !r.requireImageDir(w, req, a) {
		return
	}

	slotStr := req.PathValue("slot")
	slot, err := strconv.Atoi(slotStr)
	if err != nil || slot < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid slot index"})
		return
	}

	primary := r.getActiveFanartPrimary(req.Context())
	kodi := r.isKodiNumbering(req.Context())
	paths, discoverErr := img.DiscoverFanart(r.imageDir(a), primary)
	if discoverErr != nil {
		r.logger.Error("discovering fanart for slot delete",
			slog.String("artist_id", artistID),
			slog.String("error", discoverErr.Error()))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read fanart directory"})
		return
	}

	if slot >= len(paths) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("slot %d is out of range (count: %d)", slot, len(paths)),
		})
		return
	}

	deleted := filepath.Base(paths[slot])
	if removeErr := r.fileRemover.Remove(paths[slot]); removeErr != nil { //nolint:gosec // path from trusted fanart discovery
		r.logger.Error("deleting fanart slot",
			slog.String("artist_id", artistID),
			slog.Int("slot", slot),
			slog.String("path", paths[slot]),
			slog.String("error", removeErr.Error()))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to delete fanart file"})
		return
	}

	r.logger.Info("deleted fanart slot",
		slog.String("artist_id", artistID),
		slog.Int("slot", slot),
		slog.String("file", deleted))

	// Collect survivors and renumber.
	survivors := make([]string, 0, len(paths)-1)
	for i, p := range paths {
		if i != slot {
			survivors = append(survivors, p)
		}
	}

	var renumberWarning string
	if renumberErr := img.RenumberFanart(r.imageDir(a), primary, survivors, kodi); renumberErr != nil {
		r.logger.Warn("renumbering fanart after slot delete",
			slog.String("artist_id", artistID),
			slog.String("error", renumberErr.Error()))
		renumberWarning = "fanart files could not be renumbered; gallery order may be incorrect"
	}

	r.updateArtistFanartCount(req.Context(), a)

	// Only sync to platforms if renumbering succeeded -- pushing misindexed
	// fanart would corrupt platform galleries too.
	var syncWarnings []string
	if renumberWarning == "" {
		syncCtx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
		defer cancel()
		syncWarnings = r.syncAllFanartToPlatforms(syncCtx, a)
	} else {
		renumberWarning += ", platform sync skipped"
		syncWarnings = append(syncWarnings, renumberWarning)
	}

	if isHTMXRequest(req) {
		setSyncWarningTrigger(w, syncWarnings)
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "deleted",
		"deleted":       deleted,
		"count":         a.FanartCount,
		"sync_warnings": syncWarnings,
	})
}

// handleFanartReorder reorders local fanart files according to a given permutation.
// POST /api/v1/artists/{id}/images/fanart/reorder
func (r *Router) handleFanartReorder(w http.ResponseWriter, req *http.Request) {
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		r.logger.Warn("getting artist for fanart reorder",
			slog.String("artist_id", artistID),
			slog.String("error", err.Error()))
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "artist not found"})
		return
	}
	if !r.requireImageDir(w, req, a) {
		return
	}

	var body struct {
		Order []int `json:"order"`
	}
	if !DecodeJSON(w, req, &body) {
		return
	}

	primary := r.getActiveFanartPrimary(req.Context())
	kodi := r.isKodiNumbering(req.Context())
	paths, discoverErr := img.DiscoverFanart(r.imageDir(a), primary)
	if discoverErr != nil {
		r.logger.Error("discovering fanart for reorder",
			slog.String("artist_id", artistID),
			slog.String("error", discoverErr.Error()))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read fanart directory"})
		return
	}

	if len(body.Order) != len(paths) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("order length %d does not match fanart count %d", len(body.Order), len(paths)),
		})
		return
	}

	// Validate permutation: all indices present, no duplicates.
	if !isValidPermutation(body.Order) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "order must be a valid permutation of indices"})
		return
	}

	dir := r.imageDir(a)

	// Phase 1: stage all files to temporary names to avoid collisions.
	type stagedFile struct {
		tmpPath     string
		originalExt string
	}
	staged := make([]stagedFile, len(paths))
	for i, srcIdx := range body.Order {
		ext := filepath.Ext(paths[srcIdx])
		tmpName := fmt.Sprintf("fanart_reorder_%d%s.tmp", i, ext)
		tmpPath := filepath.Join(dir, tmpName)
		// Remove any leftover temp file from a previous crashed operation.
		if removeErr := r.fileRemover.Remove(tmpPath); removeErr != nil && !os.IsNotExist(removeErr) { //nolint:gosec // tmpPath from trusted fanart discovery, not user input
			r.logger.Error("clearing stale temp file for reorder",
				slog.String("artist_id", artistID),
				slog.String("path", tmpPath),
				slog.String("error", removeErr.Error()))
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to clear stale temp file"})
			return
		}
		if renameErr := os.Rename(paths[srcIdx], tmpPath); renameErr != nil { //nolint:gosec // path from trusted fanart discovery
			r.logger.Error("staging fanart for reorder",
				slog.String("artist_id", artistID),
				slog.String("from", paths[srcIdx]),
				slog.String("to", tmpPath),
				slog.String("error", renameErr.Error()))
			// Best-effort rollback of already-staged files.
			var rollbackIncomplete bool
			for rollback := range i {
				if rbErr := os.Rename(staged[rollback].tmpPath, paths[body.Order[rollback]]); rbErr != nil { //nolint:gosec // rollback to original trusted paths
					rollbackIncomplete = true
					r.logger.Error("rollback: restoring staged file",
						slog.String("from", staged[rollback].tmpPath),
						slog.String("to", paths[body.Order[rollback]]),
						slog.String("error", rbErr.Error()))
				}
			}
			errMsg := "failed to reorder fanart files"
			if rollbackIncomplete {
				errMsg = "failed to reorder fanart files; rollback incomplete, a rescan may be needed"
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": errMsg})
			return
		}
		staged[i] = stagedFile{tmpPath: tmpPath, originalExt: ext}
	}

	// Phase 2: rename staged files to final names. On failure, attempt to
	// roll back all files to their original paths so the artist directory is
	// not left in a half-reordered state.
	type finalizedFile struct {
		finalPath string
		tmpPath   string
	}
	var finalized []finalizedFile
	var phase2Err error
	for i, sf := range staged {
		newName := img.FanartFilename(primary, i, kodi)
		newBase := strings.TrimSuffix(newName, filepath.Ext(newName))
		finalName := newBase + sf.originalExt
		finalPath := filepath.Join(dir, finalName)
		if renameErr := os.Rename(sf.tmpPath, finalPath); renameErr != nil { //nolint:gosec // paths built from controlled directory
			r.logger.Error("applying fanart reorder",
				slog.String("artist_id", artistID),
				slog.String("from", sf.tmpPath),
				slog.String("to", finalPath),
				slog.String("error", renameErr.Error()))
			phase2Err = renameErr
			break
		}
		finalized = append(finalized, finalizedFile{finalPath: finalPath, tmpPath: sf.tmpPath})
	}
	if phase2Err != nil {
		// Best-effort rollback: move finalized files back to tmp, then
		// restore all tmp files to their original paths.
		var rollbackIncomplete bool
		for _, ff := range finalized {
			if rbErr := os.Rename(ff.finalPath, ff.tmpPath); rbErr != nil {
				rollbackIncomplete = true
				r.logger.Error("rollback: reverting finalized file",
					slog.String("from", ff.finalPath),
					slog.String("to", ff.tmpPath),
					slog.String("error", rbErr.Error()))
			}
		}
		for i, sf := range staged {
			srcIdx := body.Order[i]
			if rbErr := os.Rename(sf.tmpPath, paths[srcIdx]); rbErr != nil { //nolint:gosec // rollback to original trusted paths
				rollbackIncomplete = true
				r.logger.Error("rollback: restoring original file",
					slog.String("from", sf.tmpPath),
					slog.String("to", paths[srcIdx]),
					slog.String("error", rbErr.Error()))
			}
		}
		errMsg := "failed to finalize fanart reorder"
		if rollbackIncomplete {
			errMsg = "failed to finalize fanart reorder; rollback incomplete, a rescan may be needed"
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": errMsg})
		return
	}

	r.updateArtistFanartCount(req.Context(), a)

	// Sync reordered fanart to connected platforms.
	syncCtx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
	defer cancel()
	syncWarnings := r.syncAllFanartToPlatforms(syncCtx, a)

	r.logger.Info("reordered fanart",
		slog.String("artist_id", artistID),
		slog.Int("count", len(paths)))

	if isHTMXRequest(req) {
		setSyncWarningTrigger(w, syncWarnings)
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "reordered",
		"count":         a.FanartCount,
		"sync_warnings": syncWarnings,
	})
}

// fanartSyncSlotConnection holds the sync state for one connection on a fanart slot.
type fanartSyncSlotConnection struct {
	ConnectionID   string `json:"connection_id"`
	ConnectionName string `json:"name"`
	ConnectionType string `json:"type"`
	Synced         bool   `json:"synced"`
}

// fanartSyncSlot holds the aggregate sync state for one fanart index.
type fanartSyncSlot struct {
	Index       int                        `json:"index"`
	State       string                     `json:"state"`
	Connections []fanartSyncSlotConnection `json:"connections"`
}

// handleFanartSyncState returns per-image, per-connection sync state for all fanart slots.
// GET /api/v1/artists/{id}/fanart-sync-state
func (r *Router) handleFanartSyncState(w http.ResponseWriter, req *http.Request) {
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		r.logger.Warn("getting artist for sync state",
			slog.String("artist_id", artistID),
			slog.String("error", err.Error()))
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "artist not found"})
		return
	}

	primary := r.getActiveFanartPrimary(req.Context())
	discovered, discoverErr := img.DiscoverFanart(r.imageDir(a), primary)
	if discoverErr != nil && !errors.Is(discoverErr, os.ErrNotExist) {
		r.logger.Error("discovering fanart for sync state",
			slog.String("artist_id", artistID),
			slog.String("error", discoverErr.Error()))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read fanart directory"})
		return
	}
	if errors.Is(discoverErr, os.ErrNotExist) {
		r.logger.Debug("fanart directory does not exist for sync state",
			slog.String("artist_id", artistID),
			slog.String("dir", r.imageDir(a)))
	}
	localCount := len(discovered)

	platformIDs, err := r.artistService.GetPlatformIDs(req.Context(), a.ID)
	if err != nil {
		r.logger.Error("fetching platform IDs for sync state",
			slog.String("artist_id", artistID),
			slog.String("error", err.Error()))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to fetch platform mappings"})
		return
	}

	// Gather backdrop counts from each enabled Emby/Jellyfin connection.
	type connState struct {
		id            string
		name          string
		connType      string
		backdropCount int
	}
	var connStates []connState

	for _, pid := range platformIDs {
		conn, connErr := r.connectionService.GetByID(req.Context(), pid.ConnectionID)
		if connErr != nil {
			r.logger.Warn("skipping connection for sync state",
				slog.String("connection_id", pid.ConnectionID),
				slog.String("error", connErr.Error()))
			continue
		}
		if !conn.Enabled || conn.Status != "ok" {
			continue
		}

		client, clientErr := r.newBackdropClient(conn)
		if clientErr != nil {
			r.logger.Warn("skipping connection for sync state",
				slog.String("connection_id", conn.ID),
				slog.String("type", conn.Type),
				slog.String("error", clientErr.Error()))
			continue
		}

		state, stateErr := client.GetArtistDetail(req.Context(), pid.PlatformArtistID)
		if stateErr != nil {
			r.logger.Warn("fetching artist detail for sync state",
				slog.String("connection_id", conn.ID),
				slog.String("platform_artist_id", pid.PlatformArtistID),
				slog.String("error", stateErr.Error()))
			continue
		}

		connStates = append(connStates, connState{
			id:            conn.ID,
			name:          conn.Name,
			connType:      conn.Type,
			backdropCount: state.BackdropCount,
		})
	}

	// If no Emby/Jellyfin connections found, report no_connections.
	if len(connStates) == 0 {
		if isHTMXRequest(req) {
			// No badges to render; return empty response.
			w.WriteHeader(http.StatusOK)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"slots": []fanartSyncSlot{},
			"state": "no_connections",
		})
		return
	}

	// Build per-slot sync state.
	slots := make([]fanartSyncSlot, localCount)
	for i := range localCount {
		syncedCount := 0
		conns := make([]fanartSyncSlotConnection, len(connStates))
		for j, cs := range connStates {
			synced := cs.backdropCount > i
			conns[j] = fanartSyncSlotConnection{
				ConnectionID:   cs.id,
				ConnectionName: cs.name,
				ConnectionType: cs.connType,
				Synced:         synced,
			}
			if synced {
				syncedCount++
			}
		}

		var state string
		switch {
		case syncedCount == len(connStates):
			state = "synced"
		case syncedCount > 0:
			state = "partial"
		default:
			state = "unsynced"
		}

		slots[i] = fanartSyncSlot{
			Index:       i,
			State:       state,
			Connections: conns,
		}
	}

	if isHTMXRequest(req) {
		var badgeData []templates.FanartSyncBadgeData
		for _, s := range slots {
			tooltip := buildSyncTooltip(s)
			badgeData = append(badgeData, templates.FanartSyncBadgeData{
				Index:   s.Index,
				State:   s.State,
				Tooltip: tooltip,
			})
		}
		renderTempl(w, req, templates.FanartSyncBadges(badgeData))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"slots": slots})
}

// buildSyncTooltip builds a human-readable tooltip for a fanart sync slot.
func buildSyncTooltip(s fanartSyncSlot) string {
	var parts []string
	for _, c := range s.Connections {
		status := "not synced"
		if c.Synced {
			status = "synced"
		}
		parts = append(parts, c.ConnectionName+" ("+c.ConnectionType+"): "+status)
	}
	return strings.Join(parts, "\n")
}

// isValidPermutation checks that order is a valid permutation of [0, len(order)).
func isValidPermutation(order []int) bool {
	n := len(order)
	if n == 0 {
		return false
	}
	seen := make([]bool, n)
	for _, idx := range order {
		if idx < 0 || idx >= n || seen[idx] {
			return false
		}
		seen[idx] = true
	}
	return true
}
