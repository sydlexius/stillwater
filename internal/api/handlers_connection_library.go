package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
	"github.com/sydlexius/stillwater/internal/connection/jellyfin"
	"github.com/sydlexius/stillwater/internal/connection/lidarr"
	"github.com/sydlexius/stillwater/internal/dbutil"
	img "github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/web/templates"
)

// LibraryOpResult tracks the state of an async library operation.
type LibraryOpResult struct {
	LibraryID   string     `json:"library_id"`
	LibraryName string     `json:"library_name"`
	Operation   string     `json:"operation"`
	Status      string     `json:"status"`
	Message     string     `json:"message,omitempty"`
	StartedAt   time.Time  `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

// discoveredLibrary represents a library found on a connected service.
type discoveredLibrary struct {
	ExternalID string `json:"external_id"`
	Name       string `json:"name"`
	Imported   bool   `json:"imported"`
}

// importRequest is the request body for importing libraries from a connection.
type importRequest struct {
	Libraries []struct {
		ExternalID string `json:"external_id"`
		Name       string `json:"name"`
	} `json:"libraries"`
}

// populateResult summarizes the outcome of populating artists from a connection.
type populateResult struct {
	Total   int `json:"total"`
	Created int `json:"created"`
	Skipped int `json:"skipped"`
	Images  int `json:"images"`
}

// imageDownloader can retrieve raw image bytes from a media platform.
type imageDownloader interface {
	GetArtistImage(ctx context.Context, artistID, imageType string) ([]byte, string, error)
	GetArtistBackdrop(ctx context.Context, artistID string, index int) ([]byte, string, error)
}

// handleDiscoverLibraries lists music libraries available on a connection.
// GET /api/v1/connections/{id}/libraries
func (r *Router) handleDiscoverLibraries(w http.ResponseWriter, req *http.Request) {
	connID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}
	conn, err := r.connectionService.GetByID(req.Context(), connID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "connection not found"})
		return
	}
	if !conn.Enabled {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "connection is disabled"})
		return
	}
	if conn.Status != "ok" {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "connection must be tested successfully before discovering libraries",
		})
		return
	}

	var discovered []discoveredLibrary

	switch conn.Type {
	case connection.TypeEmby:
		client := emby.New(conn.URL, conn.APIKey, conn.PlatformUserID, r.logger)
		folders, libErr := client.GetMusicLibraries(req.Context())
		if libErr != nil {
			r.logger.Error("discovering emby libraries", "error", libErr)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "failed to discover libraries from " + conn.Type})
			return
		}
		for _, f := range folders {
			d := discoveredLibrary{ExternalID: f.ItemID, Name: f.Name}
			existing, lookupErr := r.libraryService.GetByConnectionAndExternalID(req.Context(), connID, f.ItemID)
			if lookupErr != nil {
				r.logger.Error("checking existing library", "external_id", f.ItemID, "error", lookupErr)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to check existing library"})
				return
			}
			d.Imported = existing != nil
			discovered = append(discovered, d)
		}

	case connection.TypeJellyfin:
		client := jellyfin.New(conn.URL, conn.APIKey, conn.PlatformUserID, r.logger)
		folders, libErr := client.GetMusicLibraries(req.Context())
		if libErr != nil {
			r.logger.Error("discovering jellyfin libraries", "error", libErr)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "failed to discover libraries from " + conn.Type})
			return
		}
		for _, f := range folders {
			d := discoveredLibrary{ExternalID: f.ItemID, Name: f.Name}
			existing, lookupErr := r.libraryService.GetByConnectionAndExternalID(req.Context(), connID, f.ItemID)
			if lookupErr != nil {
				r.logger.Error("checking existing library", "external_id", f.ItemID, "error", lookupErr)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to check existing library"})
				return
			}
			d.Imported = existing != nil
			discovered = append(discovered, d)
		}

	case connection.TypeLidarr:
		d := discoveredLibrary{
			ExternalID: "lidarr",
			Name:       conn.Name + " Library",
		}
		existing, lookupErr := r.libraryService.GetByConnectionAndExternalID(req.Context(), connID, "lidarr")
		if lookupErr != nil {
			r.logger.Error("checking existing library", "external_id", "lidarr", "error", lookupErr)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to check existing library"})
			return
		}
		d.Imported = existing != nil
		discovered = append(discovered, d)

	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported connection type"})
		return
	}

	if discovered == nil {
		discovered = []discoveredLibrary{}
	}

	// If HTMX request, render the checklist partial
	if isHTMXRequest(req) {
		templLibs := make([]templates.DiscoveredLib, len(discovered))
		for i, d := range discovered {
			templLibs[i] = templates.DiscoveredLib{
				ExternalID: d.ExternalID,
				Name:       d.Name,
				Imported:   d.Imported,
			}
		}
		isOOBE := strings.Contains(req.Header.Get("HX-Current-URL"), "/setup/wizard")
		renderTempl(w, req, templates.DiscoverResults(connID, templLibs, isOOBE))
		return
	}
	writeJSON(w, http.StatusOK, discovered)
}

// handleImportLibraries imports selected libraries from a connection.
// POST /api/v1/connections/{id}/libraries/import
func (r *Router) handleImportLibraries(w http.ResponseWriter, req *http.Request) {
	connID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}
	conn, err := r.connectionService.GetByID(req.Context(), connID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "connection not found"})
		return
	}
	if !conn.Enabled {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "connection is disabled"})
		return
	}
	if conn.Status != "ok" {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "connection must be tested successfully before importing libraries",
		})
		return
	}

	var body importRequest
	if !DecodeJSON(w, req, &body) {
		return
	}
	if len(body.Libraries) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no libraries selected"})
		return
	}

	var created []library.Library
	for _, entry := range body.Libraries {
		// Skip entries with missing required fields
		if entry.ExternalID == "" || entry.Name == "" {
			continue
		}

		// Skip if already imported
		existing, lookupErr := r.libraryService.GetByConnectionAndExternalID(req.Context(), connID, entry.ExternalID)
		if lookupErr != nil {
			r.logger.Error("checking existing library", "external_id", entry.ExternalID, "error", lookupErr)
			continue
		}
		if existing != nil {
			continue
		}

		name := entry.Name
		// Check for name conflict and suffix with connection name if needed
		lib := &library.Library{
			Name:         name,
			Path:         "",
			Type:         library.TypeRegular,
			Source:       conn.Type,
			ConnectionID: conn.ID,
			ExternalID:   entry.ExternalID,
		}
		if createErr := r.libraryService.Create(req.Context(), lib); createErr != nil {
			// If name conflict (unique constraint), retry with connection name suffix
			lower := strings.ToLower(createErr.Error())
			if strings.Contains(lower, "unique") || strings.Contains(lower, "duplicate") {
				lib.Name = fmt.Sprintf("%s (%s)", name, conn.Name)
				if retryErr := r.libraryService.Create(req.Context(), lib); retryErr != nil {
					r.logger.Error("importing library", "name", lib.Name, "error", retryErr)
					continue
				}
			} else {
				r.logger.Error("importing library", "name", lib.Name, "error", createErr)
				continue
			}
		}
		created = append(created, *lib)
	}

	writeJSON(w, http.StatusCreated, created)

	// Auto-populate each newly imported library in the background.
	for i := range created {
		lib := created[i]
		r.startPopulateBackground(context.WithoutCancel(req.Context()), conn, &lib)
	}
}

// startPopulateBackground registers a library populate operation and runs it
// in a background goroutine. Returns immediately if an operation is already
// running for this library. Use this for fire-and-forget populate triggers
// where no HTTP response for the operation status is needed at call time.
func (r *Router) startPopulateBackground(ctx context.Context, conn *connection.Connection, lib *library.Library) {
	r.libraryOpsMu.Lock()
	if existing, ok := r.libraryOps[lib.ID]; ok && existing.Status == "running" {
		r.libraryOpsMu.Unlock()
		return
	}
	op := &LibraryOpResult{
		LibraryID:   lib.ID,
		LibraryName: lib.Name,
		Operation:   "populate",
		Status:      "running",
		StartedAt:   time.Now().UTC(),
	}
	r.libraryOps[lib.ID] = op
	r.libraryOpsMu.Unlock()

	go r.runPopulate(ctx, conn, lib, op)
}

// handlePopulateLibrary populates artists from a connection into an imported library.
// POST /api/v1/connections/{id}/libraries/{libId}/populate
func (r *Router) handlePopulateLibrary(w http.ResponseWriter, req *http.Request) {
	connID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}
	libID, ok := RequirePathParam(w, req, "libId")
	if !ok {
		return
	}

	conn, err := r.connectionService.GetByID(req.Context(), connID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "connection not found"})
		return
	}
	if !conn.Enabled {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "connection is disabled"})
		return
	}
	if conn.Status != "ok" {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "connection must be tested successfully before populating libraries",
		})
		return
	}

	lib, err := r.libraryService.GetByID(req.Context(), libID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "library not found"})
		return
	}
	if lib.ConnectionID != conn.ID {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "library does not belong to this connection"})
		return
	}

	// Check for already-running operation on this library.
	r.libraryOpsMu.Lock()
	if existing, ok := r.libraryOps[libID]; ok && existing.Status == "running" {
		r.libraryOpsMu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]string{"error": "operation already running for this library"})
		return
	}
	op := &LibraryOpResult{
		LibraryID:   libID,
		LibraryName: lib.Name,
		Operation:   "populate",
		Status:      "running",
		StartedAt:   time.Now().UTC(),
	}
	r.libraryOps[libID] = op
	r.libraryOpsMu.Unlock()

	writeJSON(w, http.StatusAccepted, op)

	go r.runPopulate(context.WithoutCancel(req.Context()), conn, lib, op)
}

// runPopulate executes the populate operation in a background goroutine.
func (r *Router) runPopulate(ctx context.Context, conn *connection.Connection, lib *library.Library, op *LibraryOpResult) {
	defer func() {
		if v := recover(); v != nil {
			r.logger.Error("panic in populate goroutine",
				slog.String("library", lib.Name), slog.String("library_id", lib.ID),
				slog.Any("panic", v),
				slog.String("stack", string(debug.Stack())))
			r.libraryOpsMu.Lock()
			now := time.Now().UTC()
			op.CompletedAt = &now
			op.Status = "failed"
			op.Message = "populate failed unexpectedly"
			r.libraryOpsMu.Unlock()
			go r.scheduleOpCleanup(lib.ID, op)
		}
	}()

	result := populateResult{}
	var popErr error

	switch conn.Type {
	case connection.TypeEmby:
		client := emby.New(conn.URL, conn.APIKey, conn.PlatformUserID, r.logger)
		popErr = r.populateFromEmbyCtx(ctx, client, lib, &result)

	case connection.TypeJellyfin:
		client := jellyfin.New(conn.URL, conn.APIKey, conn.PlatformUserID, r.logger)
		popErr = r.populateFromJellyfinCtx(ctx, client, lib, &result)

	case connection.TypeLidarr:
		client := lidarr.New(conn.URL, conn.APIKey, r.logger)
		popErr = r.populateFromLidarrCtx(ctx, client, lib, &result)

	default:
		popErr = fmt.Errorf("unsupported connection type: %s", conn.Type)
	}

	// After a successful sync, check for external file modifications (Tier 2
	// shared-FS evidence). This runs outside the per-artist loop to avoid
	// repeated database updates during sync.
	if popErr == nil && !lib.IsPathless() {
		r.checkSyncMtimeEvidence(ctx, lib)
	}

	r.libraryOpsMu.Lock()
	now := time.Now().UTC()
	op.CompletedAt = &now
	if popErr != nil {
		op.Status = "failed"
		op.Message = fmt.Sprintf("populate failed for %s", lib.Name)
		r.logger.Error("populate failed", "library", lib.Name, "error", popErr)
	} else {
		op.Status = "completed"
		op.Message = fmt.Sprintf("Populated %d artists (%d images) from %s", result.Created, result.Images, lib.Name)
	}
	r.libraryOpsMu.Unlock()

	go r.scheduleOpCleanup(lib.ID, op)
}

// handleScanLibrary triggers an async API scan that checks metadata/image state.
// POST /api/v1/connections/{id}/libraries/{libId}/scan
func (r *Router) handleScanLibrary(w http.ResponseWriter, req *http.Request) {
	connID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}
	libID, ok := RequirePathParam(w, req, "libId")
	if !ok {
		return
	}

	conn, err := r.connectionService.GetByID(req.Context(), connID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "connection not found"})
		return
	}
	if !conn.Enabled {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "connection is disabled"})
		return
	}
	if conn.Status != "ok" {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "connection must be tested successfully before scanning",
		})
		return
	}

	lib, err := r.libraryService.GetByID(req.Context(), libID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "library not found"})
		return
	}
	if lib.ConnectionID != conn.ID {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "library does not belong to this connection"})
		return
	}

	r.libraryOpsMu.Lock()
	if existing, ok := r.libraryOps[libID]; ok && existing.Status == "running" {
		r.libraryOpsMu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]string{"error": "operation already running for this library"})
		return
	}
	op := &LibraryOpResult{
		LibraryID:   libID,
		LibraryName: lib.Name,
		Operation:   "scan",
		Status:      "running",
		StartedAt:   time.Now().UTC(),
	}
	r.libraryOps[libID] = op
	r.libraryOpsMu.Unlock()

	writeJSON(w, http.StatusAccepted, op)

	go r.runLibraryScan(context.WithoutCancel(req.Context()), conn, lib, op)
}

// runLibraryScan queries the platform API and updates *_exists flags for artists.
func (r *Router) runLibraryScan(ctx context.Context, conn *connection.Connection, lib *library.Library, op *LibraryOpResult) {
	defer func() {
		if v := recover(); v != nil {
			r.logger.Error("panic in library scan goroutine",
				slog.String("library", lib.Name), slog.String("library_id", lib.ID),
				slog.Any("panic", v),
				slog.String("stack", string(debug.Stack())))
			r.libraryOpsMu.Lock()
			now := time.Now().UTC()
			op.CompletedAt = &now
			op.Status = "failed"
			op.Message = "scan failed unexpectedly"
			r.libraryOpsMu.Unlock()
			go r.scheduleOpCleanup(lib.ID, op)
		}
	}()

	var updated int
	var scanErr error

	switch conn.Type {
	case connection.TypeEmby:
		client := emby.New(conn.URL, conn.APIKey, conn.PlatformUserID, r.logger)
		updated, scanErr = r.scanFromEmby(ctx, client, lib)

	case connection.TypeJellyfin:
		client := jellyfin.New(conn.URL, conn.APIKey, conn.PlatformUserID, r.logger)
		updated, scanErr = r.scanFromJellyfin(ctx, client, lib)

	case connection.TypeLidarr:
		client := lidarr.New(conn.URL, conn.APIKey, r.logger)
		updated, scanErr = r.scanFromLidarr(ctx, client, lib)

	default:
		scanErr = fmt.Errorf("unsupported connection type: %s", conn.Type)
	}

	r.libraryOpsMu.Lock()
	now := time.Now().UTC()
	op.CompletedAt = &now
	if scanErr != nil {
		op.Status = "failed"
		op.Message = fmt.Sprintf("scan failed for %s", lib.Name)
		r.logger.Error("library scan failed", "library", lib.Name, "error", scanErr)
	} else {
		op.Status = "completed"
		op.Message = fmt.Sprintf("Scan complete: %d artists updated in %s", updated, lib.Name)
	}
	r.libraryOpsMu.Unlock()

	go r.scheduleOpCleanup(lib.ID, op)
}

// scheduleOpCleanup removes a completed or failed operation from the in-memory
// map after a delay, preventing unbounded growth of the libraryOps map.
func (r *Router) scheduleOpCleanup(libraryID string, op *LibraryOpResult) {
	timer := time.NewTimer(5 * time.Minute)
	defer timer.Stop()
	<-timer.C

	r.libraryOpsMu.Lock()
	defer r.libraryOpsMu.Unlock()
	current, ok := r.libraryOps[libraryID]
	if !ok {
		return
	}
	// Only delete if this is still the same operation and it is no longer running.
	if current == op && current.Status != "running" {
		delete(r.libraryOps, libraryID)
	}
}

// handleLibraryOpStatus returns the current operation status for a library.
// GET /api/v1/libraries/{libId}/operation/status
func (r *Router) handleLibraryOpStatus(w http.ResponseWriter, req *http.Request) {
	libID, ok := RequirePathParam(w, req, "libId")
	if !ok {
		return
	}

	r.libraryOpsMu.Lock()
	op, ok := r.libraryOps[libID]
	if !ok {
		r.libraryOpsMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]string{"status": "idle"})
		return
	}
	snapshot := *op
	r.libraryOpsMu.Unlock()

	writeJSON(w, http.StatusOK, snapshot)
}

func (r *Router) populateFromEmbyCtx(ctx context.Context, client *emby.Client, lib *library.Library, result *populateResult) error {
	manualLibs := r.manualLibraries(ctx)
	startIndex := 0
	pageSize := 100
	for {
		resp, err := client.GetArtists(ctx, lib.ExternalID, startIndex, pageSize)
		if err != nil {
			return fmt.Errorf("fetching artists from emby: %w", err)
		}

		for _, item := range resp.Items {
			result.Total++
			mbid := item.ProviderIDs.MusicBrainzArtist

			var existing *artist.Artist
			if mbid != "" {
				var lookupErr error
				existing, lookupErr = r.artistService.GetByMBIDAndLibrary(ctx, mbid, lib.ID)
				if lookupErr != nil {
					r.logger.Warn("dedup lookup by mbid", "mbid", mbid, "error", lookupErr)
					result.Skipped++
					continue
				}
			}
			if existing == nil {
				nameMatch, lookupErr := r.artistService.GetByNameAndLibrary(ctx, item.Name, lib.ID)
				if lookupErr != nil {
					r.logger.Warn("dedup lookup by name", "name", item.Name, "error", lookupErr)
					result.Skipped++
					continue
				}
				if nameMatch != nil {
					if mbid != "" && nameMatch.MusicBrainzID != "" && nameMatch.MusicBrainzID != mbid {
						// Name matches but MBIDs conflict -- different artists
						// with the same name. Skip to avoid wrong association.
						r.logger.Warn("mbid conflict during name dedup, skipping",
							"name", item.Name, "platform_mbid", mbid,
							"existing_mbid", nameMatch.MusicBrainzID)
						result.Skipped++
						continue
					}
					existing = nameMatch
				}
			}

			if existing != nil {
				// Backfill MusicBrainzID if the platform provides one and the local record lacks it.
				if mbid != "" && existing.MusicBrainzID == "" {
					existing.MusicBrainzID = mbid
					if err := r.artistService.Update(ctx, existing); err != nil {
						r.logger.Warn("backfilling mbid from emby", "name", existing.Name, "error", err)
					}
				}
				// Store the platform-to-Stillwater artist ID mapping.
				if setErr := r.artistService.SetPlatformID(ctx, existing.ID, lib.ConnectionID, item.ID); setErr != nil {
					r.logger.Warn("storing emby platform id", "name", existing.Name, "error", setErr)
				}
				r.backfillPlatformIDToManualLibs(ctx, mbid, item.Name, lib.ConnectionID, item.ID, existing.ID, manualLibs)
				// Download any missing images.
				r.downloadPlatformImages(ctx, client, item.ID, item.ImageTags, item.BackdropImageTags, existing, "emby", result)
				result.Skipped++
				continue
			}

			sortName := item.Name
			if item.SortName != "" {
				sortName = item.SortName
			}
			a := &artist.Artist{
				Name:          item.Name,
				SortName:      sortName,
				MusicBrainzID: mbid,
				LibraryID:     lib.ID,
				Biography:     item.Overview,
				Genres:        item.Genres,
				Styles:        item.Tags,
				Formed:        item.PremiereDate,
				Disbanded:     item.EndDate,
				Path:          validatedArtistPath(item.Path, lib.Path),
			}
			if err := r.artistService.Create(ctx, a); err != nil {
				r.logger.Warn("creating artist from emby", "name", item.Name, "error", err)
				result.Skipped++
				continue
			}
			result.Created++

			// Store the platform-to-Stillwater artist ID mapping.
			if setErr := r.artistService.SetPlatformID(ctx, a.ID, lib.ConnectionID, item.ID); setErr != nil {
				r.logger.Warn("storing emby platform id", "name", a.Name, "error", setErr)
			}
			r.backfillPlatformIDToManualLibs(ctx, mbid, item.Name, lib.ConnectionID, item.ID, a.ID, manualLibs)

			r.downloadPlatformImages(ctx, client, item.ID, item.ImageTags, item.BackdropImageTags, a, "emby", result)
		}

		startIndex += pageSize
		if startIndex >= resp.TotalRecordCount {
			break
		}
	}
	return nil
}

func (r *Router) populateFromJellyfinCtx(ctx context.Context, client *jellyfin.Client, lib *library.Library, result *populateResult) error {
	manualLibs := r.manualLibraries(ctx)
	startIndex := 0
	pageSize := 100
	for {
		resp, err := client.GetArtists(ctx, lib.ExternalID, startIndex, pageSize)
		if err != nil {
			return fmt.Errorf("fetching artists from jellyfin: %w", err)
		}

		for _, item := range resp.Items {
			result.Total++
			mbid := item.ProviderIDs.MusicBrainzArtist

			var existing *artist.Artist
			if mbid != "" {
				var lookupErr error
				existing, lookupErr = r.artistService.GetByMBIDAndLibrary(ctx, mbid, lib.ID)
				if lookupErr != nil {
					r.logger.Warn("dedup lookup by mbid", "mbid", mbid, "error", lookupErr)
					result.Skipped++
					continue
				}
			}
			if existing == nil {
				nameMatch, lookupErr := r.artistService.GetByNameAndLibrary(ctx, item.Name, lib.ID)
				if lookupErr != nil {
					r.logger.Warn("dedup lookup by name", "name", item.Name, "error", lookupErr)
					result.Skipped++
					continue
				}
				if nameMatch != nil {
					if mbid != "" && nameMatch.MusicBrainzID != "" && nameMatch.MusicBrainzID != mbid {
						// Name matches but MBIDs conflict -- different artists
						// with the same name. Skip to avoid wrong association.
						r.logger.Warn("mbid conflict during name dedup, skipping",
							"name", item.Name, "platform_mbid", mbid,
							"existing_mbid", nameMatch.MusicBrainzID)
						result.Skipped++
						continue
					}
					existing = nameMatch
				}
			}

			if existing != nil {
				// Backfill MusicBrainzID if the platform provides one and the local record lacks it.
				if mbid != "" && existing.MusicBrainzID == "" {
					existing.MusicBrainzID = mbid
					if err := r.artistService.Update(ctx, existing); err != nil {
						r.logger.Warn("backfilling mbid from jellyfin", "name", existing.Name, "error", err)
					}
				}
				// Store the platform-to-Stillwater artist ID mapping.
				if setErr := r.artistService.SetPlatformID(ctx, existing.ID, lib.ConnectionID, item.ID); setErr != nil {
					r.logger.Warn("storing jellyfin platform id", "name", existing.Name, "error", setErr)
				}
				r.backfillPlatformIDToManualLibs(ctx, mbid, item.Name, lib.ConnectionID, item.ID, existing.ID, manualLibs)
				// Download any missing images.
				r.downloadPlatformImages(ctx, client, item.ID, item.ImageTags, item.BackdropImageTags, existing, "jellyfin", result)
				result.Skipped++
				continue
			}

			sortName := item.Name
			if item.SortName != "" {
				sortName = item.SortName
			}
			a := &artist.Artist{
				Name:          item.Name,
				SortName:      sortName,
				MusicBrainzID: mbid,
				LibraryID:     lib.ID,
				Biography:     item.Overview,
				Genres:        item.Genres,
				Styles:        item.Tags,
				Formed:        item.PremiereDate,
				Disbanded:     item.EndDate,
				Path:          validatedArtistPath(item.Path, lib.Path),
			}
			if err := r.artistService.Create(ctx, a); err != nil {
				r.logger.Warn("creating artist from jellyfin", "name", item.Name, "error", err)
				result.Skipped++
				continue
			}
			result.Created++

			// Store the platform-to-Stillwater artist ID mapping.
			if setErr := r.artistService.SetPlatformID(ctx, a.ID, lib.ConnectionID, item.ID); setErr != nil {
				r.logger.Warn("storing jellyfin platform id", "name", a.Name, "error", setErr)
			}
			r.backfillPlatformIDToManualLibs(ctx, mbid, item.Name, lib.ConnectionID, item.ID, a.ID, manualLibs)

			r.downloadPlatformImages(ctx, client, item.ID, item.ImageTags, item.BackdropImageTags, a, "jellyfin", result)
		}

		startIndex += pageSize
		if startIndex >= resp.TotalRecordCount {
			break
		}
	}
	return nil
}

func (r *Router) populateFromLidarrCtx(ctx context.Context, client *lidarr.Client, lib *library.Library, result *populateResult) error {
	manualLibs := r.manualLibraries(ctx)
	artists, err := client.GetArtists(ctx)
	if err != nil {
		return fmt.Errorf("fetching artists from lidarr: %w", err)
	}

	for _, la := range artists {
		result.Total++
		mbid := la.ForeignArtistID

		if mbid != "" {
			existing, lookupErr := r.artistService.GetByMBIDAndLibrary(ctx, mbid, lib.ID)
			if lookupErr != nil {
				r.logger.Warn("dedup lookup by mbid", "mbid", mbid, "error", lookupErr)
				result.Skipped++
				continue
			}
			if existing != nil {
				// Store the platform-to-Stillwater artist ID mapping.
				if setErr := r.artistService.SetPlatformID(ctx, existing.ID, lib.ConnectionID, fmt.Sprintf("%d", la.ID)); setErr != nil {
					r.logger.Warn("storing lidarr platform id", "name", existing.Name, "error", setErr)
				}
				r.backfillPlatformIDToManualLibs(ctx, mbid, la.ArtistName, lib.ConnectionID, fmt.Sprintf("%d", la.ID), existing.ID, manualLibs)
				result.Skipped++
				continue
			}
		} else {
			existing, lookupErr := r.artistService.GetByNameAndLibrary(ctx, la.ArtistName, lib.ID)
			if lookupErr != nil {
				r.logger.Warn("dedup lookup by name", "name", la.ArtistName, "error", lookupErr)
				result.Skipped++
				continue
			}
			if existing != nil {
				// Store the platform-to-Stillwater artist ID mapping.
				if setErr := r.artistService.SetPlatformID(ctx, existing.ID, lib.ConnectionID, fmt.Sprintf("%d", la.ID)); setErr != nil {
					r.logger.Warn("storing lidarr platform id", "name", existing.Name, "error", setErr)
				}
				r.backfillPlatformIDToManualLibs(ctx, mbid, la.ArtistName, lib.ConnectionID, fmt.Sprintf("%d", la.ID), existing.ID, manualLibs)
				result.Skipped++
				continue
			}
		}

		a := &artist.Artist{
			Name:          la.ArtistName,
			SortName:      la.ArtistName,
			MusicBrainzID: mbid,
			LibraryID:     lib.ID,
		}
		if err := r.artistService.Create(ctx, a); err != nil {
			r.logger.Warn("creating artist from lidarr", "name", la.ArtistName, "error", err)
			result.Skipped++
			continue
		}
		result.Created++

		// Store the platform-to-Stillwater artist ID mapping.
		if setErr := r.artistService.SetPlatformID(ctx, a.ID, lib.ConnectionID, fmt.Sprintf("%d", la.ID)); setErr != nil {
			r.logger.Warn("storing lidarr platform id", "name", a.Name, "error", setErr)
		}
		r.backfillPlatformIDToManualLibs(ctx, mbid, la.ArtistName, lib.ConnectionID, fmt.Sprintf("%d", la.ID), a.ID, manualLibs)
	}
	return nil
}

// validatedArtistPath returns the resolved item path only when it exists on
// disk as a directory and falls under libraryPath. Returns empty string if
// libraryPath is empty (pathless library), itemPath is empty, itemPath does
// not exist, or itemPath escapes the library root. Resolves symlinks to
// prevent escaping the library root via symlinked directories.
func validatedArtistPath(itemPath, libraryPath string) string {
	if libraryPath == "" || itemPath == "" {
		return ""
	}

	// Resolve symlinks for the library root when possible.
	libRoot, err := filepath.EvalSymlinks(libraryPath)
	if err != nil {
		// Path may not exist on disk (e.g. in tests or misconfig); fall back.
		libRoot, err = filepath.Abs(libraryPath)
		if err != nil {
			return ""
		}
	}

	// Resolve symlinks for the item path when possible.
	itemReal, err := filepath.EvalSymlinks(itemPath)
	if err != nil {
		// Path does not exist on disk or cannot be resolved; treat as
		// invalid so only verified, existing directories are persisted as
		// artist paths. Pathless artists can still use the image cache.
		return ""
	} else {
		// Path exists: verify it is a directory (not a file).
		info, statErr := os.Stat(itemReal) //nolint:gosec // itemReal resolved via filepath.EvalSymlinks from platform path
		if statErr != nil || !info.IsDir() {
			return ""
		}
	}

	rel, err := filepath.Rel(libRoot, itemReal)
	if err != nil {
		return ""
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return itemReal
}

// platformToStillwaterType maps Emby/Jellyfin ImageTags keys to Stillwater image types.
// Backdrops are excluded: they are returned in BackdropImageTags (not ImageTags) and
// downloaded separately via the indexed GetArtistBackdrop path.
var platformToStillwaterType = map[string]string{
	"Primary": "thumb",
	"Logo":    "logo",
	"Banner":  "banner",
}

// downloadPlatformImages downloads available images from a media platform for a single artist.
// connType identifies the platform source (e.g. "emby", "jellyfin") for provenance metadata.
// Errors are non-fatal: logged as warnings and skipped.
func (r *Router) downloadPlatformImages(ctx context.Context, dl imageDownloader, platformArtistID string, imageTags map[string]string, backdropTags []string, a *artist.Artist, connType string, result *populateResult) {
	dir := r.imageDir(a)
	if dir == "" {
		r.logger.Debug("skipping image download: no path or cache dir", "artist", a.Name)
		return
	}

	if a.Path == "" {
		// Cache directory: create if needed.
		if err := os.MkdirAll(dir, 0o750); err != nil { //nolint:gosec // dir from imageDir() uses trusted config path + artist ID
			r.logger.Warn("creating cache directory", "artist", a.Name, "dir", dir, "error", err)
			return
		}
	} else {
		// Filesystem path: must already exist from scan and be a directory.
		info, err := os.Stat(dir) //nolint:gosec // G703: dir from imageDir() uses validated artist path
		if err != nil {
			r.logger.Debug("artist directory not accessible, skipping images", "artist", a.Name, "dir", dir, "error", err)
			return
		}
		if !info.IsDir() {
			r.logger.Debug("artist path is not a directory, skipping images", "artist", a.Name, "dir", dir)
			return
		}
	}

	for platformKey, tagValue := range imageTags {
		if tagValue == "" {
			continue
		}
		stillwaterType, ok := platformToStillwaterType[platformKey]
		if !ok {
			continue
		}

		patterns := r.getActiveNamingConfig(ctx, stillwaterType)
		if _, found := findExistingImage(dir, patterns); found {
			r.logger.Debug("skipping existing image", "artist", a.Name, "type", stillwaterType)
			continue
		}

		data, _, err := dl.GetArtistImage(ctx, platformArtistID, stillwaterType)
		if err != nil {
			r.logger.Warn("downloading image from platform", "artist", a.Name, "type", stillwaterType, "error", err)
			continue
		}

		platformMeta := &img.ExifMeta{Source: connType, Fetched: time.Now().UTC(), Mode: "user"}
		if _, err := r.processAndSaveImage(ctx, dir, stillwaterType, data, platformMeta); err != nil {
			r.logger.Warn("saving downloaded image", "artist", a.Name, "type", stillwaterType, "error", err)
			continue
		}

		r.updateArtistImageFlag(ctx, a, stillwaterType)
		result.Images++
	}

	if len(backdropTags) > 0 {
		primary := r.getActiveFanartPrimary(ctx)
		kodi := r.isKodiNumbering(ctx)

		downloaded := 0
		anyExisted := false
		for i, tag := range backdropTags {
			if tag == "" {
				r.logger.Debug("skipping backdrop with empty tag", "artist", a.Name, "index", i)
				continue
			}
			filename := img.FanartFilename(primary, i, kodi)
			// Check all common image extensions for this slot. img.ConvertFormat converts WebP
			// to PNG, so a previously-saved file may have a different extension than the
			// current filename. FanartFilename preserves the extension from the active
			// primary name, so the saved file and the generated name may legitimately differ.
			base := strings.TrimSuffix(filename, filepath.Ext(filename))
			slotExists := false
			skipDownload := false
			for _, ext := range []string{".jpg", ".jpeg", ".png"} {
				candidate := filepath.Join(dir, base+ext)
				_, statErr := os.Stat(candidate) //nolint:gosec // path from validated dir + naming fn
				if statErr == nil {
					slotExists = true
					break
				}
				if !errors.Is(statErr, fs.ErrNotExist) {
					r.logger.Warn("checking backdrop existence", "artist", a.Name, "index", i, "file", base+ext, "error", statErr)
					skipDownload = true
					// Continue checking remaining extensions -- this candidate may be temporarily inaccessible.
				}
			}
			if slotExists {
				r.logger.Debug("skipping existing backdrop", "artist", a.Name, "index", i)
				anyExisted = true
				continue
			}
			if skipDownload {
				r.logger.Warn("skipping backdrop download due to filesystem error", "artist", a.Name, "index", i)
				continue
			}
			data, _, dlErr := dl.GetArtistBackdrop(ctx, platformArtistID, i)
			if dlErr != nil {
				r.logger.Warn("downloading backdrop from platform", "artist", a.Name, "index", i, "error", dlErr)
				continue
			}
			if len(data) == 0 {
				r.logger.Warn("empty backdrop response from platform", "artist", a.Name, "index", i)
				continue
			}
			converted, _, convertErr := img.ConvertFormat(bytes.NewReader(data))
			if convertErr != nil {
				r.logger.Warn("converting backdrop format", "artist", a.Name, "index", i, "error", convertErr)
				continue
			}
			backdropMeta := &img.ExifMeta{Source: connType, Fetched: time.Now().UTC(), Mode: "user"}
			saved, saveErr := img.Save(dir, "fanart", converted, []string{filename}, false, backdropMeta, r.logger)
			if saveErr != nil {
				r.logger.Warn("saving backdrop", "artist", a.Name, "index", i, "error", saveErr)
				continue
			}
			if len(saved) == 0 {
				r.logger.Warn("saving backdrop produced no files", "artist", a.Name, "index", i, "dir", dir, "filename", filename)
				continue
			}
			downloaded++
			result.Images++
		}
		if downloaded > 0 || anyExisted {
			// When backdrop index 0 failed (empty tag, download error, etc.)
			// but later indexes succeeded, no primary fanart file exists.
			// The UI serves the background image from /images/fanart/file which
			// only matches the primary name pattern. Compact the numbered files
			// so the lowest available becomes the primary -- same pattern used
			// by handleFanartBatchDelete.
			r.compactFanartIfNeeded(dir, primary, kodi)
			r.updateArtistImageFlag(ctx, a, "fanart")
			r.updateArtistFanartCount(ctx, a)
		}
	}
}

// compactFanartIfNeeded renumbers fanart files when the primary slot is missing
// but numbered files exist. This closes gaps so the primary filename always
// corresponds to the first available fanart.
func (r *Router) compactFanartIfNeeded(dir, primary string, kodi bool) {
	paths, discoverErr := img.DiscoverFanart(dir, primary)
	if discoverErr != nil {
		r.logger.Warn("discovering fanart for compact",
			slog.String("dir", dir),
			slog.String("error", discoverErr.Error()))
		return
	}
	if len(paths) == 0 {
		return
	}
	// Check whether the primary slot exists. DiscoverFanart returns paths
	// in index order, with the primary file (if present) appearing first.
	// Compare bases to confirm the primary is present.
	primaryBase := strings.TrimSuffix(primary, filepath.Ext(primary))
	firstBase := strings.TrimSuffix(filepath.Base(paths[0]), filepath.Ext(paths[0]))
	if strings.EqualFold(firstBase, primaryBase) {
		return // primary exists, nothing to compact
	}
	// Renumber all discovered files sequentially from index 0.
	if err := img.RenumberFanart(dir, primary, paths, kodi); err != nil {
		r.logger.Warn("compacting fanart after primary removal",
			slog.String("error", err.Error()))
	}
}

// backfillPlatformIDToManualLibs copies a platform ID mapping to any matching
// artist in the given manual-source (filesystem) libraries. It matches by
// MBID first, then case-insensitive name. This ensures that push operations
// from the primary filesystem artist can find the platform mapping.
func (r *Router) backfillPlatformIDToManualLibs(
	ctx context.Context,
	mbid, name, connectionID, platformArtistID, connArtistID string,
	manualLibs []library.Library,
) {
	for _, ml := range manualLibs {
		fsArtist, err := r.artistService.FindByMBIDOrName(ctx, mbid, name, ml.ID)
		if err != nil {
			r.logger.Warn("backfill: finding filesystem artist", "name", name, "library", ml.ID, "error", err)
			continue
		}
		if fsArtist == nil || fsArtist.ID == connArtistID {
			continue
		}
		if setErr := r.artistService.SetPlatformID(ctx, fsArtist.ID, connectionID, platformArtistID); setErr != nil {
			r.logger.Warn("backfill: storing platform id on filesystem artist", "name", fsArtist.Name, "error", setErr)
			continue
		}
		r.logger.Debug("backfill: platform id propagated to filesystem artist",
			"name", fsArtist.Name, "fs_artist_id", fsArtist.ID, "connection_id", connectionID)
	}
}

// manualLibraries returns all libraries with source "manual". Used by scan
// and populate functions to find filesystem libraries for platform ID backfill.
func (r *Router) manualLibraries(ctx context.Context) []library.Library {
	libs, err := r.libraryService.List(ctx)
	if err != nil {
		r.logger.Error("backfill: failed to list libraries, backfill will be skipped for this operation", "error", err)
		return nil
	}
	var manual []library.Library
	for _, lib := range libs {
		if lib.Source == library.SourceManual {
			manual = append(manual, lib)
		}
	}
	return manual
}

// resolveAndBackfillPlatformID finds the connection-library artist by MBID
// or exact name, stores the platform ID on it, and backfills the mapping to
// any matching filesystem-library artist. Returns the connection-library
// artist for the caller to update image flags, or nil if no match found.
func (r *Router) resolveAndBackfillPlatformID(
	ctx context.Context,
	mbid, name, connectionID, platformArtistID string,
	connLib *library.Library,
	manualLibs []library.Library,
) *artist.Artist {
	var a *artist.Artist
	var lookupErr error
	if mbid != "" {
		a, lookupErr = r.artistService.GetByMBIDAndLibrary(ctx, mbid, connLib.ID)
	}
	if a == nil && lookupErr == nil {
		a, lookupErr = r.artistService.GetByNameAndLibrary(ctx, name, connLib.ID)
	}
	if lookupErr != nil {
		r.logger.Warn("scan artist lookup", "name", name, "mbid", mbid, "platform", connLib.Source, "error", lookupErr)
		return nil
	}
	if a == nil {
		return nil
	}

	// Store platform ID on the connection-library artist.
	if setErr := r.artistService.SetPlatformID(ctx, a.ID, connectionID, platformArtistID); setErr != nil {
		r.logger.Warn("storing platform id during scan", "name", a.Name, "platform", connLib.Source, "error", setErr)
	}

	// Backfill to filesystem-library artists.
	r.backfillPlatformIDToManualLibs(ctx, mbid, name, connectionID, platformArtistID, a.ID, manualLibs)

	return a
}

// scanFromEmby pages through Emby artists and updates image existence flags.
func (r *Router) scanFromEmby(ctx context.Context, client *emby.Client, lib *library.Library) (int, error) {
	manualLibs := r.manualLibraries(ctx)
	updated := 0
	startIndex := 0
	pageSize := 100
	for {
		resp, err := client.GetArtists(ctx, lib.ExternalID, startIndex, pageSize)
		if err != nil {
			return updated, fmt.Errorf("fetching artists from emby: %w", err)
		}

		for _, item := range resp.Items {
			a := r.resolveAndBackfillPlatformID(ctx,
				item.ProviderIDs.MusicBrainzArtist, item.Name,
				lib.ConnectionID, item.ID, lib, manualLibs)
			if a == nil {
				continue
			}

			var thumbExists, fanartExists, logoExists, bannerExists bool
			if item.ImageTags != nil {
				thumbExists = item.ImageTags["Primary"] != ""
				logoExists = item.ImageTags["Logo"] != ""
				bannerExists = item.ImageTags["Banner"] != ""
			}
			fanartExists = len(item.BackdropImageTags) > 0

			if a.ThumbExists != thumbExists || a.FanartExists != fanartExists ||
				a.LogoExists != logoExists || a.BannerExists != bannerExists {
				a.ThumbExists = thumbExists
				a.FanartExists = fanartExists
				a.LogoExists = logoExists
				a.BannerExists = bannerExists
				if err := r.artistService.Update(ctx, a); err != nil {
					r.logger.Warn("updating artist image flags from emby", "name", a.Name, "error", err)
					continue
				}
				updated++
			}
		}

		startIndex += pageSize
		if startIndex >= resp.TotalRecordCount {
			break
		}
	}
	return updated, nil
}

// scanFromJellyfin pages through Jellyfin artists and updates image existence flags.
func (r *Router) scanFromJellyfin(ctx context.Context, client *jellyfin.Client, lib *library.Library) (int, error) {
	manualLibs := r.manualLibraries(ctx)
	updated := 0
	startIndex := 0
	pageSize := 100
	for {
		resp, err := client.GetArtists(ctx, lib.ExternalID, startIndex, pageSize)
		if err != nil {
			return updated, fmt.Errorf("fetching artists from jellyfin: %w", err)
		}

		for _, item := range resp.Items {
			a := r.resolveAndBackfillPlatformID(ctx,
				item.ProviderIDs.MusicBrainzArtist, item.Name,
				lib.ConnectionID, item.ID, lib, manualLibs)
			if a == nil {
				continue
			}

			var thumbExists, fanartExists, logoExists, bannerExists bool
			if item.ImageTags != nil {
				thumbExists = item.ImageTags["Primary"] != ""
				logoExists = item.ImageTags["Logo"] != ""
				bannerExists = item.ImageTags["Banner"] != ""
			}
			fanartExists = len(item.BackdropImageTags) > 0

			if a.ThumbExists != thumbExists || a.FanartExists != fanartExists ||
				a.LogoExists != logoExists || a.BannerExists != bannerExists {
				a.ThumbExists = thumbExists
				a.FanartExists = fanartExists
				a.LogoExists = logoExists
				a.BannerExists = bannerExists
				if err := r.artistService.Update(ctx, a); err != nil {
					r.logger.Warn("updating artist image flags from jellyfin", "name", a.Name, "error", err)
					continue
				}
				updated++
			}
		}

		startIndex += pageSize
		if startIndex >= resp.TotalRecordCount {
			break
		}
	}
	return updated, nil
}

// scanFromLidarr gets all Lidarr artists and updates image existence flags.
func (r *Router) scanFromLidarr(ctx context.Context, client *lidarr.Client, lib *library.Library) (int, error) {
	manualLibs := r.manualLibraries(ctx)
	artists, err := client.GetArtists(ctx)
	if err != nil {
		return 0, fmt.Errorf("fetching artists from lidarr: %w", err)
	}

	updated := 0
	for _, la := range artists {
		a := r.resolveAndBackfillPlatformID(ctx,
			la.ForeignArtistID, la.ArtistName,
			lib.ConnectionID, fmt.Sprintf("%d", la.ID), lib, manualLibs)
		if a == nil {
			continue
		}

		var thumbExists, fanartExists, bannerExists, logoExists bool
		for _, img := range la.Images {
			switch strings.ToLower(img.CoverType) {
			case "poster":
				thumbExists = true
			case "fanart":
				fanartExists = true
			case "banner":
				bannerExists = true
			case "logo":
				logoExists = true
			}
		}

		if a.ThumbExists != thumbExists || a.FanartExists != fanartExists ||
			a.LogoExists != logoExists || a.BannerExists != bannerExists {
			a.ThumbExists = thumbExists
			a.FanartExists = fanartExists
			a.LogoExists = logoExists
			a.BannerExists = bannerExists
			if err := r.artistService.Update(ctx, a); err != nil {
				r.logger.Warn("updating artist image flags from lidarr", "name", a.Name, "error", err)
				continue
			}
			updated++
		}
	}
	return updated, nil
}

// checkSyncMtimeEvidence performs Tier 2 shared-FS detection after a library
// sync. It compares the filesystem mtime of image files in each artist's
// directory against that artist's own newest last_written_at timestamp (not a
// global library-wide MAX). If any file has been modified externally (mtime
// newer than the artist's last write plus a 2-second tolerance), the library's
// shared-FS status is updated to "suspected."
//
// Using per-artist baselines avoids false negatives where a recently-written
// artist's timestamp masks an externally-modified older artist.
//
// This check is non-fatal: failures are logged at Debug/Warn level and do not
// affect the sync outcome.
func (r *Router) checkSyncMtimeEvidence(ctx context.Context, lib *library.Library) {
	// Skip if the library already has confirmed shared-FS status; do not
	// downgrade from confirmed to suspected.
	if lib.SharedFSStatus == library.SharedFSConfirmed {
		r.logger.Debug("skipping mtime check: library already confirmed as shared-FS",
			"library", lib.Name, "library_id", lib.ID)
		return
	}

	// Get per-artist newest write times for this library.
	writeTimesByArtist, err := r.artistService.NewestWriteTimesByArtistForLibrary(ctx, lib.ID)
	if err != nil {
		r.logger.Warn("mtime check: failed to query per-artist write times",
			"library", lib.Name, "library_id", lib.ID, "error", err)
		return
	}
	if len(writeTimesByArtist) == 0 {
		// No writes recorded yet -- nothing to compare against.
		r.logger.Debug("mtime check: no writes recorded for library, skipping",
			"library", lib.Name, "library_id", lib.ID)
		return
	}

	// Get all artist paths for this library (artistID -> directory path).
	artistDirs, err := r.artistService.ListPathsByLibrary(ctx, lib.ID)
	if err != nil {
		r.logger.Warn("mtime check: failed to list artist paths",
			"library", lib.Name, "library_id", lib.ID, "error", err)
		return
	}
	if len(artistDirs) == 0 {
		r.logger.Debug("mtime check: no artist paths for library, skipping",
			"library", lib.Name, "library_id", lib.ID)
		return
	}

	// Build a per-directory lastWrittenAt map using each artist's own newest
	// write time. This ensures that each artist's mtime comparison uses its
	// own baseline, rather than a single global MAX that could mask
	// modifications to artists with older write timestamps.
	lastWrittenAts := make(map[string]time.Time, len(artistDirs))
	// dirToWriteTime maps directory path to the parsed time for use in
	// evidence string formatting later.
	dirToWriteTime := make(map[string]time.Time, len(artistDirs))
	for artistID, dir := range artistDirs {
		writeStr, ok := writeTimesByArtist[artistID]
		if !ok || writeStr == "" {
			continue
		}
		parsed := dbutil.ParseTime(writeStr)
		if parsed.IsZero() {
			r.logger.Warn("mtime check: failed to parse write time for artist",
				"library", lib.Name, "artist_id", artistID, "raw", writeStr)
			continue
		}
		// When multiple artists share a directory, keep the most recent
		// write time as the baseline for mtime comparison.
		if existing, ok := lastWrittenAts[dir]; !ok || parsed.After(existing) {
			lastWrittenAts[dir] = parsed
			dirToWriteTime[dir] = parsed
		}
	}

	if len(lastWrittenAts) == 0 {
		r.logger.Debug("mtime check: no parseable write times for library, skipping",
			"library", lib.Name, "library_id", lib.ID)
		return
	}

	evidence := library.CollectMtimeEvidence(artistDirs, lastWrittenAts, r.logger)
	if len(evidence) == 0 {
		r.logger.Debug("mtime check: no external modifications detected",
			"library", lib.Name, "dirs_checked", len(artistDirs))
		return
	}

	r.logger.Debug("mtime check: external modifications detected",
		"library", lib.Name,
		"evidence_count", len(evidence))

	// Build evidence strings for the shared-FS status update, referencing
	// each artist's own baseline timestamp.
	evidenceStrings := make([]string, len(evidence))
	for i, e := range evidence {
		artistWrite := dirToWriteTime[filepath.Dir(e.Path)]
		evidenceStrings[i] = fmt.Sprintf("mtime: %s modified at %s (after last Stillwater write at %s)",
			e.Path, e.FileMtime.Format(time.RFC3339), artistWrite.Format(time.RFC3339))
	}

	evidenceJSON, marshalErr := json.Marshal(evidenceStrings)
	if marshalErr != nil {
		r.logger.Warn("mtime check: failed to marshal evidence",
			"library", lib.Name, "error", marshalErr)
		return
	}

	if setErr := r.libraryService.SetSharedFSStatus(ctx, lib.ID,
		library.SharedFSSuspected, string(evidenceJSON), lib.SharedFSPeerLibraryIDs); setErr != nil {
		r.logger.Warn("mtime check: failed to update shared-FS status",
			"library", lib.Name, "error", setErr)
	}
}
