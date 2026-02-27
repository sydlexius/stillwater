package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
	"github.com/sydlexius/stillwater/internal/connection/jellyfin"
	"github.com/sydlexius/stillwater/internal/connection/lidarr"
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
}

// handleDiscoverLibraries lists music libraries available on a connection.
// GET /api/v1/connections/{id}/libraries
func (r *Router) handleDiscoverLibraries(w http.ResponseWriter, req *http.Request) {
	connID := req.PathValue("id")
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
		client := emby.New(conn.URL, conn.APIKey, r.logger)
		folders, libErr := client.GetMusicLibraries(req.Context())
		if libErr != nil {
			r.logger.Error("discovering emby libraries", "error", libErr)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "failed to discover libraries: " + libErr.Error()})
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
		client := jellyfin.New(conn.URL, conn.APIKey, r.logger)
		folders, libErr := client.GetMusicLibraries(req.Context())
		if libErr != nil {
			r.logger.Error("discovering jellyfin libraries", "error", libErr)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "failed to discover libraries: " + libErr.Error()})
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
	connID := req.PathValue("id")
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
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
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
}

// handlePopulateLibrary populates artists from a connection into an imported library.
// POST /api/v1/connections/{id}/libraries/{libId}/populate
func (r *Router) handlePopulateLibrary(w http.ResponseWriter, req *http.Request) {
	connID := req.PathValue("id")
	libID := req.PathValue("libId")

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

	go r.runPopulate(context.WithoutCancel(req.Context()), conn, lib, op)

	writeJSON(w, http.StatusAccepted, op)
}

// runPopulate executes the populate operation in a background goroutine.
func (r *Router) runPopulate(ctx context.Context, conn *connection.Connection, lib *library.Library, op *LibraryOpResult) {
	result := populateResult{}
	var popErr error

	switch conn.Type {
	case connection.TypeEmby:
		client := emby.New(conn.URL, conn.APIKey, r.logger)
		popErr = r.populateFromEmbyCtx(ctx, client, lib, &result)

	case connection.TypeJellyfin:
		client := jellyfin.New(conn.URL, conn.APIKey, r.logger)
		popErr = r.populateFromJellyfinCtx(ctx, client, lib, &result)

	case connection.TypeLidarr:
		client := lidarr.New(conn.URL, conn.APIKey, r.logger)
		popErr = r.populateFromLidarrCtx(ctx, client, lib, &result)
	}

	r.libraryOpsMu.Lock()
	now := time.Now().UTC()
	op.CompletedAt = &now
	if popErr != nil {
		op.Status = "failed"
		op.Message = popErr.Error()
		r.logger.Error("populate failed", "library", lib.Name, "error", popErr)
	} else {
		op.Status = "completed"
		op.Message = fmt.Sprintf("Populated %d artists from %s", result.Created, lib.Name)
	}
	r.libraryOpsMu.Unlock()
}

// handleScanLibrary triggers an async API scan that checks metadata/image state.
// POST /api/v1/connections/{id}/libraries/{libId}/scan
func (r *Router) handleScanLibrary(w http.ResponseWriter, req *http.Request) {
	connID := req.PathValue("id")
	libID := req.PathValue("libId")

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

	go r.runLibraryScan(context.WithoutCancel(req.Context()), conn, lib, op)

	writeJSON(w, http.StatusAccepted, op)
}

// runLibraryScan queries the platform API and updates *_exists flags for artists.
func (r *Router) runLibraryScan(ctx context.Context, conn *connection.Connection, lib *library.Library, op *LibraryOpResult) {
	var updated int
	var scanErr error

	switch conn.Type {
	case connection.TypeEmby:
		client := emby.New(conn.URL, conn.APIKey, r.logger)
		updated, scanErr = r.scanFromEmby(ctx, client, lib)

	case connection.TypeJellyfin:
		client := jellyfin.New(conn.URL, conn.APIKey, r.logger)
		updated, scanErr = r.scanFromJellyfin(ctx, client, lib)

	case connection.TypeLidarr:
		client := lidarr.New(conn.URL, conn.APIKey, r.logger)
		updated, scanErr = r.scanFromLidarr(ctx, client, lib)
	}

	r.libraryOpsMu.Lock()
	now := time.Now().UTC()
	op.CompletedAt = &now
	if scanErr != nil {
		op.Status = "failed"
		op.Message = scanErr.Error()
		r.logger.Error("library scan failed", "library", lib.Name, "error", scanErr)
	} else {
		op.Status = "completed"
		op.Message = fmt.Sprintf("Scan complete: %d artists updated in %s", updated, lib.Name)
	}
	r.libraryOpsMu.Unlock()
}

// handleLibraryOpStatus returns the current operation status for a library.
// GET /api/v1/libraries/{libId}/operation/status
func (r *Router) handleLibraryOpStatus(w http.ResponseWriter, req *http.Request) {
	libID := req.PathValue("libId")

	r.libraryOpsMu.Lock()
	op, ok := r.libraryOps[libID]
	r.libraryOpsMu.Unlock()

	if !ok {
		writeJSON(w, http.StatusOK, map[string]string{"status": "idle"})
		return
	}

	r.libraryOpsMu.Lock()
	snapshot := *op
	r.libraryOpsMu.Unlock()

	writeJSON(w, http.StatusOK, snapshot)
}

func (r *Router) populateFromEmbyCtx(ctx context.Context, client *emby.Client, lib *library.Library, result *populateResult) error {
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

			if mbid != "" {
				existing, lookupErr := r.artistService.GetByMBIDAndLibrary(ctx, mbid, lib.ID)
				if lookupErr != nil {
					r.logger.Warn("dedup lookup by mbid", "mbid", mbid, "error", lookupErr)
					result.Skipped++
					continue
				}
				if existing != nil {
					result.Skipped++
					continue
				}
			} else {
				existing, lookupErr := r.artistService.GetByNameAndLibrary(ctx, item.Name, lib.ID)
				if lookupErr != nil {
					r.logger.Warn("dedup lookup by name", "name", item.Name, "error", lookupErr)
					result.Skipped++
					continue
				}
				if existing != nil {
					result.Skipped++
					continue
				}
			}

			a := &artist.Artist{
				Name:          item.Name,
				SortName:      item.Name,
				MusicBrainzID: mbid,
				LibraryID:     lib.ID,
			}
			if err := r.artistService.Create(ctx, a); err != nil {
				r.logger.Warn("creating artist from emby", "name", item.Name, "error", err)
				result.Skipped++
				continue
			}
			result.Created++
		}

		startIndex += pageSize
		if startIndex >= resp.TotalRecordCount {
			break
		}
	}
	return nil
}

func (r *Router) populateFromJellyfinCtx(ctx context.Context, client *jellyfin.Client, lib *library.Library, result *populateResult) error {
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

			if mbid != "" {
				existing, lookupErr := r.artistService.GetByMBIDAndLibrary(ctx, mbid, lib.ID)
				if lookupErr != nil {
					r.logger.Warn("dedup lookup by mbid", "mbid", mbid, "error", lookupErr)
					result.Skipped++
					continue
				}
				if existing != nil {
					result.Skipped++
					continue
				}
			} else {
				existing, lookupErr := r.artistService.GetByNameAndLibrary(ctx, item.Name, lib.ID)
				if lookupErr != nil {
					r.logger.Warn("dedup lookup by name", "name", item.Name, "error", lookupErr)
					result.Skipped++
					continue
				}
				if existing != nil {
					result.Skipped++
					continue
				}
			}

			a := &artist.Artist{
				Name:          item.Name,
				SortName:      item.Name,
				MusicBrainzID: mbid,
				LibraryID:     lib.ID,
			}
			if err := r.artistService.Create(ctx, a); err != nil {
				r.logger.Warn("creating artist from jellyfin", "name", item.Name, "error", err)
				result.Skipped++
				continue
			}
			result.Created++
		}

		startIndex += pageSize
		if startIndex >= resp.TotalRecordCount {
			break
		}
	}
	return nil
}

func (r *Router) populateFromLidarrCtx(ctx context.Context, client *lidarr.Client, lib *library.Library, result *populateResult) error {
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
	}
	return nil
}

// scanFromEmby pages through Emby artists and updates image existence flags.
func (r *Router) scanFromEmby(ctx context.Context, client *emby.Client, lib *library.Library) (int, error) {
	updated := 0
	startIndex := 0
	pageSize := 100
	for {
		resp, err := client.GetArtists(ctx, lib.ExternalID, startIndex, pageSize)
		if err != nil {
			return updated, fmt.Errorf("fetching artists from emby: %w", err)
		}

		for _, item := range resp.Items {
			a, lookupErr := r.artistService.GetByNameAndLibrary(ctx, item.Name, lib.ID)
			if lookupErr != nil || a == nil {
				continue
			}

			thumbExists := item.ImageTags["Primary"] != ""
			fanartExists := item.ImageTags["Backdrop"] != ""
			logoExists := item.ImageTags["Logo"] != ""
			bannerExists := item.ImageTags["Banner"] != ""

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
	updated := 0
	startIndex := 0
	pageSize := 100
	for {
		resp, err := client.GetArtists(ctx, lib.ExternalID, startIndex, pageSize)
		if err != nil {
			return updated, fmt.Errorf("fetching artists from jellyfin: %w", err)
		}

		for _, item := range resp.Items {
			a, lookupErr := r.artistService.GetByNameAndLibrary(ctx, item.Name, lib.ID)
			if lookupErr != nil || a == nil {
				continue
			}

			thumbExists := item.ImageTags["Primary"] != ""
			fanartExists := item.ImageTags["Backdrop"] != ""
			logoExists := item.ImageTags["Logo"] != ""
			bannerExists := item.ImageTags["Banner"] != ""

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
	artists, err := client.GetArtists(ctx)
	if err != nil {
		return 0, fmt.Errorf("fetching artists from lidarr: %w", err)
	}

	updated := 0
	for _, la := range artists {
		name := la.ArtistName
		a, lookupErr := r.artistService.GetByNameAndLibrary(ctx, name, lib.ID)
		if lookupErr != nil || a == nil {
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
