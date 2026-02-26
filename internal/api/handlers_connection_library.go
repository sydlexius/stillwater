package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
	"github.com/sydlexius/stillwater/internal/connection/jellyfin"
	"github.com/sydlexius/stillwater/internal/connection/lidarr"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/web/templates"
)

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
		renderTempl(w, req, templates.DiscoverResults(connID, templLibs))
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

	result := populateResult{}

	switch conn.Type {
	case connection.TypeEmby:
		client := emby.New(conn.URL, conn.APIKey, r.logger)
		if popErr := r.populateFromEmby(req, client, lib, &result); popErr != nil {
			r.logger.Error("populating from emby", "error", popErr)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": popErr.Error()})
			return
		}

	case connection.TypeJellyfin:
		client := jellyfin.New(conn.URL, conn.APIKey, r.logger)
		if popErr := r.populateFromJellyfin(req, client, lib, &result); popErr != nil {
			r.logger.Error("populating from jellyfin", "error", popErr)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": popErr.Error()})
			return
		}

	case connection.TypeLidarr:
		client := lidarr.New(conn.URL, conn.APIKey, r.logger)
		if popErr := r.populateFromLidarr(req, client, lib, &result); popErr != nil {
			r.logger.Error("populating from lidarr", "error", popErr)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": popErr.Error()})
			return
		}

	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported connection type"})
		return
	}

	writeJSON(w, http.StatusOK, result)
}

func (r *Router) populateFromEmby(req *http.Request, client *emby.Client, lib *library.Library, result *populateResult) error {
	startIndex := 0
	pageSize := 100
	for {
		resp, err := client.GetArtists(req.Context(), lib.ExternalID, startIndex, pageSize)
		if err != nil {
			return fmt.Errorf("fetching artists from emby: %w", err)
		}

		for _, item := range resp.Items {
			result.Total++
			mbid := item.ProviderIDs.MusicBrainzArtist

			if mbid != "" {
				existing, lookupErr := r.artistService.GetByMBIDAndLibrary(req.Context(), mbid, lib.ID)
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
				existing, lookupErr := r.artistService.GetByNameAndLibrary(req.Context(), item.Name, lib.ID)
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
			if err := r.artistService.Create(req.Context(), a); err != nil {
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

func (r *Router) populateFromJellyfin(req *http.Request, client *jellyfin.Client, lib *library.Library, result *populateResult) error {
	startIndex := 0
	pageSize := 100
	for {
		resp, err := client.GetArtists(req.Context(), lib.ExternalID, startIndex, pageSize)
		if err != nil {
			return fmt.Errorf("fetching artists from jellyfin: %w", err)
		}

		for _, item := range resp.Items {
			result.Total++
			mbid := item.ProviderIDs.MusicBrainzArtist

			if mbid != "" {
				existing, lookupErr := r.artistService.GetByMBIDAndLibrary(req.Context(), mbid, lib.ID)
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
				existing, lookupErr := r.artistService.GetByNameAndLibrary(req.Context(), item.Name, lib.ID)
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
			if err := r.artistService.Create(req.Context(), a); err != nil {
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

func (r *Router) populateFromLidarr(req *http.Request, client *lidarr.Client, lib *library.Library, result *populateResult) error {
	artists, err := client.GetArtists(req.Context())
	if err != nil {
		return fmt.Errorf("fetching artists from lidarr: %w", err)
	}

	for _, la := range artists {
		result.Total++
		mbid := la.ForeignArtistID

		if mbid != "" {
			existing, lookupErr := r.artistService.GetByMBIDAndLibrary(req.Context(), mbid, lib.ID)
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
			existing, lookupErr := r.artistService.GetByNameAndLibrary(req.Context(), la.ArtistName, lib.ID)
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
		if err := r.artistService.Create(req.Context(), a); err != nil {
			r.logger.Warn("creating artist from lidarr", "name", la.ArtistName, "error", err)
			result.Skipped++
			continue
		}
		result.Created++
	}
	return nil
}
