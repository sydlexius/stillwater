package api

import (
	"net/http"
	"strconv"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/web/components"
	"github.com/sydlexius/stillwater/web/templates"
)

// handleListArtists returns paginated artist list as JSON.
// GET /api/v1/artists
func (r *Router) handleListArtists(w http.ResponseWriter, req *http.Request) {
	params := artist.ListParams{
		Page:      intQuery(req, "page", 1),
		PageSize:  intQuery(req, "page_size", 50),
		Sort:      req.URL.Query().Get("sort"),
		Order:     req.URL.Query().Get("order"),
		Search:    req.URL.Query().Get("search"),
		Filter:    req.URL.Query().Get("filter"),
		LibraryID: req.URL.Query().Get("library_id"),
		Filters:   parseFlyoutFilters(req),
	}
	params.Validate()

	artists, total, err := r.artistService.List(req.Context(), params)
	if err != nil {
		r.logger.Error("listing artists", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"artists":   artists,
		"total":     total,
		"page":      params.Page,
		"page_size": params.PageSize,
	})
}

// handleGetArtist returns a single artist as JSON.
// GET /api/v1/artists/{id}
func (r *Router) handleGetArtist(w http.ResponseWriter, req *http.Request) {
	id, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	a, err := r.artistService.GetByID(req.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "artist not found"})
		return
	}

	members, err := r.artistService.ListMembersByArtistID(req.Context(), id)
	if err != nil {
		r.logger.Warn("listing band members", "artist_id", id, "error", err)
		members = nil
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"artist":  a,
		"members": members,
	})
}

// intQuery extracts an integer query parameter with a default value.
func intQuery(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// handleArtistsPage renders the artist list HTML page.
// GET /artists
func (r *Router) handleArtistsPage(w http.ResponseWriter, req *http.Request) {
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		r.renderLoginPage(w, req)
		return
	}

	params := artist.ListParams{
		Page:      intQuery(req, "page", 1),
		PageSize:  intQuery(req, "page_size", 50),
		Sort:      req.URL.Query().Get("sort"),
		Order:     req.URL.Query().Get("order"),
		Search:    req.URL.Query().Get("search"),
		Filter:    req.URL.Query().Get("filter"),
		LibraryID: req.URL.Query().Get("library_id"),
		Filters:   parseFlyoutFilters(req),
	}
	params.Validate()

	view := req.URL.Query().Get("view")
	if view != "grid" && view != "table" {
		view = r.getStringSetting(req.Context(), "ui.artists_view", "table")
	}
	if view != "grid" {
		view = "table"
	}

	artists, total, err := r.artistService.List(req.Context(), params)
	if err != nil {
		r.logger.Error("listing artists for page", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	totalPages := total / params.PageSize
	if total%params.PageSize > 0 {
		totalPages++
	}

	// Collect artist IDs for batch lookups (compliance, platform presence).
	var artistIDs []string
	if len(artists) > 0 {
		artistIDs = make([]string, len(artists))
		for i, a := range artists {
			artistIDs[i] = a.ID
		}
	}

	// Fetch per-artist compliance status from active rule violations.
	// Best-effort: a failure does not prevent the page from rendering.
	var complianceMap map[string]artist.ComplianceStatus
	if r.ruleService != nil && len(artistIDs) > 0 {
		cm, err := r.ruleService.GetComplianceForArtists(req.Context(), artistIDs)
		if err != nil {
			r.logger.Error("fetching compliance for artist list", "error", err)
		} else {
			complianceMap = cm
		}
	}

	// Fetch per-artist platform presence (Emby, Jellyfin, Lidarr) from
	// artist_platform_ids joined with connections. Best-effort.
	var platformPresence map[string]artist.PlatformPresence
	if len(artistIDs) > 0 {
		pp, err := r.artistService.GetPlatformPresenceForArtists(req.Context(), artistIDs)
		if err != nil {
			r.logger.Warn("fetching platform presence for artist list", "error", err)
		} else {
			platformPresence = pp
		}
	}

	data := templates.ArtistListData{
		Artists: artists,
		Pagination: components.PaginationData{
			CurrentPage: params.Page,
			TotalPages:  totalPages,
			PageSize:    params.PageSize,
			TotalItems:  total,
			BaseURL:     "/artists",
			Sort:        params.Sort,
			Order:       params.Order,
			Search:      params.Search,
			Filter:      params.Filter,
			View:        view,
			LibraryID:   params.LibraryID,
		},
		ComplianceMap:    complianceMap,
		PlatformPresence: platformPresence,
		Search:           params.Search,
		Sort:             params.Sort,
		Order:            params.Order,
		Filter:           params.Filter,
		Filters:          params.Filters,
		LibraryID:        params.LibraryID,
		View:             view,
		ProfileName:      r.getActiveProfileName(req.Context()),
	}

	if r.libraryService != nil {
		libs, err := r.libraryService.List(req.Context())
		if err != nil {
			r.logger.Warn("listing libraries for artists page", "error", err)
		}
		data.Libraries = libs

		// Build source info map for imported libraries (non-manual).
		sources := make(map[string]templates.LibrarySourceInfo)
		connNames := map[string]string{} // cache connection ID -> name
		for _, lib := range libs {
			if lib.Source == "" || lib.Source == "manual" {
				continue
			}
			info := templates.LibrarySourceInfo{Source: lib.Source}
			if lib.ConnectionID != "" {
				if name, ok := connNames[lib.ConnectionID]; ok {
					info.ConnectionName = name
				} else {
					if conn, connErr := r.connectionService.GetByID(req.Context(), lib.ConnectionID); connErr == nil {
						info.ConnectionName = conn.Name
						connNames[lib.ConnectionID] = conn.Name
					}
				}
			}
			if info.ConnectionName == "" {
				info.ConnectionName = lib.SourceDisplayName()
			}
			sources[lib.ID] = info
		}
		if len(sources) > 0 {
			data.LibrarySources = sources
		}
	}

	if isHTMXRequest(req) {
		if view == "grid" {
			renderTempl(w, req, templates.ArtistGrid(data))
		} else {
			renderTempl(w, req, templates.ArtistTable(data))
		}
		return
	}

	renderTempl(w, req, templates.ArtistsPage(r.assetsFor(req), data))
}

// parseFlyoutFilters reads the URL query params written by the filter flyout
// component and returns a map of filter key -> "include" or "exclude".
//
// The flyout JS writes params in the form: filter_missing_meta=%2By (include)
// or filter_missing_meta=-y (exclude). Recognized keys are: missing_meta,
// missing_images, missing_mbid, excluded, locked.
func parseFlyoutFilters(req *http.Request) map[string]string {
	keys := []string{"missing_meta", "missing_images", "missing_mbid", "excluded", "locked"}
	filters := make(map[string]string, len(keys))
	for _, k := range keys {
		raw := req.URL.Query().Get("filter_" + k)
		if raw == "" {
			continue
		}
		// The flyout prefixes values with '+' (include) or '-' (exclude).
		switch {
		case len(raw) > 0 && raw[0] == '+':
			filters[k] = "include"
		case len(raw) > 0 && raw[0] == '-':
			filters[k] = "exclude"
		}
	}
	if len(filters) == 0 {
		return nil
	}
	return filters
}

// handleArtistDetailPage renders the artist detail HTML page.
// GET /artists/{id}
func (r *Router) handleArtistDetailPage(w http.ResponseWriter, req *http.Request) {
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		r.renderLoginPage(w, req)
		return
	}

	id := req.PathValue("id")
	a, err := r.artistService.GetByID(req.Context(), id)
	if err != nil {
		http.Error(w, "artist not found", http.StatusNotFound)
		return
	}

	members, err := r.artistService.ListMembersByArtistID(req.Context(), id)
	if err != nil {
		r.logger.Warn("listing band members for page", "artist_id", id, "error", err)
	}

	aliases, err := r.artistService.ListAliases(req.Context(), id)
	if err != nil {
		r.logger.Warn("listing aliases for page", "artist_id", id, "error", err)
	}

	priorities, _ := r.providerSettings.GetPriorities(req.Context())
	fieldProviders := buildFieldProvidersMap(priorities)

	var libraryName string
	var librarySource string
	if r.libraryService != nil && a.LibraryID != "" {
		if lib, err := r.libraryService.GetByID(req.Context(), a.LibraryID); err == nil {
			libraryName = lib.Name
			librarySource = lib.Source
		}
	}

	data := templates.ArtistDetailData{
		Artist:         *a,
		Members:        members,
		Aliases:        aliases,
		FieldProviders: fieldProviders,
		LibraryName:    libraryName,
		LibrarySource:  librarySource,
		ProfileName:    r.getActiveProfileName(req.Context()),
	}
	renderTempl(w, req, templates.ArtistDetailPage(r.assetsFor(req), data))
}

// handleArtistImagesPage renders the image management page.
// GET /artists/{id}/images
func (r *Router) handleArtistImagesPage(w http.ResponseWriter, req *http.Request) {
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		r.renderLoginPage(w, req)
		return
	}

	id := req.PathValue("id")
	a, err := r.artistService.GetByID(req.Context(), id)
	if err != nil {
		http.Error(w, "artist not found", http.StatusNotFound)
		return
	}

	webSearchEnabled, _ := r.providerSettings.AnyWebSearchEnabled(req.Context())
	autoFetch := r.getBoolSetting(req.Context(), "auto_fetch_images", false)
	if req.URL.Query().Get("fetch") == "1" {
		autoFetch = true
	}

	selectedType := req.URL.Query().Get("type")
	if selectedType != "" && !validImageTypes[selectedType] {
		selectedType = ""
	}

	data := templates.ImageSearchData{
		Artist:           *a,
		WebSearchEnabled: webSearchEnabled,
		AutoFetchImages:  autoFetch,
		SelectedType:     selectedType,
		ProfileName:      r.getActiveProfileName(req.Context()),
	}
	renderTempl(w, req, templates.ImageSearchPage(r.assetsFor(req), data))
}

// isHTMXRequest checks if the request was made by HTMX.
func isHTMXRequest(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true"
}
