package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/rule"
	"github.com/sydlexius/stillwater/web/components"
	"github.com/sydlexius/stillwater/web/templates"
)

// handleListArtists returns paginated artist list as JSON.
// GET /api/v1/artists
func (r *Router) handleListArtists(w http.ResponseWriter, req *http.Request) {
	userID := middleware.UserIDFromContext(req.Context())
	sortKey, ok := validateSortParam(w, req, allowedArtistSort)
	if !ok {
		return
	}
	order, ok := validateOrderParam(w, req)
	if !ok {
		return
	}
	params := artist.ListParams{
		Page:      intQuery(req, "page", 1),
		PageSize:  r.getUserPageSize(req.Context(), userID, intQuery(req, "page_size", 0)),
		Sort:      sortKey,
		Order:     order,
		Search:    req.URL.Query().Get("search"),
		Filter:    req.URL.Query().Get("filter"),
		LibraryID: req.URL.Query().Get("library_id"),
		Filters:   parseFlyoutFilters(req),
		IDs:       parseIDsParam(req.URL.Query().Get("ids")),
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

// handleArtistsBadge returns an HTML fragment with the total artist count for the
// sidebar badge. Uses the dedicated Count() path to avoid fetching and hydrating
// full artist rows.
// GET /api/v1/artists/badge
func (r *Router) handleArtistsBadge(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Cache-Control", "no-store")

	total, err := r.artistService.Count(req.Context(), artist.CountParams{})
	if err != nil {
		r.logger.Error("fetching artist count for badge", "error", err)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		//nolint:errcheck // badge fragment; write errors are not actionable
		fmt.Fprint(w, `<!-- error fetching artist count -->`)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if total == 0 {
		return
	}
	//nolint:errcheck // badge fragment; write errors are not actionable
	fmt.Fprintf(w, `<span class="inline-flex items-center rounded-full bg-gray-700 dark:bg-gray-600 px-1.5 py-0.5 text-xs font-medium text-gray-200 tabular-nums">%d</span>`, total)
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

// parseIDsParam splits the `ids` query parameter into a slice of artist IDs.
// The bulk-selection "Show selected" affordance (#1227) emits the in-memory
// selection set as a comma-separated list (`?ids=a,b,c`) so the user can
// review or paginate across the cross-page selection. Whitespace and empty
// segments are trimmed; the artist service's Validate() caps the slice
// length and stays the canonical truncation point. Returns nil for an
// empty input so an absent param does not allocate.
func parseIDsParam(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	ids := make([]string, 0, len(parts))
	for _, p := range parts {
		// Match the bulk-actions canonical ID shape so malformed tokens
		// (whitespace, punctuation, oversized strings) cannot round-trip
		// through pagination chips into the SQL filter. idPattern is
		// defined in handlers_bulk_actions.go.
		p = strings.TrimSpace(p)
		if idPattern.MatchString(p) {
			ids = append(ids, p)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	return ids
}

// handleArtistsPage renders the artist list HTML page.
// GET /artists
//
// buildArtistListData assembles the ArtistListData for the artists list page
// from the request query (sort/order/flyout filters/pagination/off-page ids)
// plus best-effort compliance and platform-presence lookups. It returns
// ok=false after writing a login or error response, so callers can simply
// return. Shared by the stable handleArtistsPage and the next/ channel's
// handleNextArtistsPage so both render byte-identical data (M55 #1335).
//
//nolint:gocognit // Artists page handler (cog 50): resolves auth, parses paging/sort/filter params, validates "show selected" IDs, queries the page slice + total counts + facet counts, then renders both full-page and HTMX-partial responses. The parse/query/render stages could move into named helpers reading from a shared request-context struct without reshuffling render-time fields. Refactor tracked in #1550.
func (r *Router) buildArtistListData(w http.ResponseWriter, req *http.Request) (templates.ArtistListData, bool) {
	if !r.requireAuth(w, req) {
		return templates.ArtistListData{}, false
	}
	userID := middleware.UserIDFromContext(req.Context())

	sortKey, ok := validateSortParam(w, req, allowedArtistSort)
	if !ok {
		return templates.ArtistListData{}, false
	}
	order, ok := validateOrderParam(w, req)
	if !ok {
		return templates.ArtistListData{}, false
	}
	params := artist.ListParams{
		Page:      intQuery(req, "page", 1),
		PageSize:  r.getUserPageSize(req.Context(), userID, intQuery(req, "page_size", 0)),
		Sort:      sortKey,
		Order:     order,
		Search:    req.URL.Query().Get("search"),
		Filter:    req.URL.Query().Get("filter"),
		LibraryID: req.URL.Query().Get("library_id"),
		Filters:   parseFlyoutFilters(req),
		IDs:       parseIDsParam(req.URL.Query().Get("ids")),
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
		return templates.ArtistListData{}, false
	}

	totalPages := total / params.PageSize
	if total%params.PageSize > 0 {
		totalPages++
	}

	// Collect artist IDs for batch lookups (compliance, platform presence).
	var artistIDs []string
	if len(artists) > 0 {
		artistIDs = make([]string, len(artists))
		for i := range artists {
			artistIDs[i] = artists[i].ID
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

	// BaseURL drives pagination links, the "show all" affordance, and the shared
	// sort/selection JS fragment-swap path; it must be channel-aware so the
	// next/ page swaps the next/ fragment rather than the stable table (#1335).
	listBaseURL := "/artists"
	if middleware.UXChannelFromContext(req.Context()) == middleware.UXNext {
		listBaseURL = "/next/artists"
	}
	data := templates.ArtistListData{
		Artists: artists,
		Pagination: components.PaginationData{
			CurrentPage: params.Page,
			TotalPages:  totalPages,
			PageSize:    params.PageSize,
			TotalItems:  total,
			BaseURL:     listBaseURL,
			Sort:        params.Sort,
			Order:       params.Order,
			Search:      params.Search,
			Filter:      params.Filter,
			View:        view,
			LibraryID:   params.LibraryID,
			IDs:         params.IDs,
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
		IDs:              params.IDs,
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
		for i := range libs {
			lib := &libs[i]
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

	return data, true
}

// handleArtistsPage renders the stable artists list: the full page, or for an
// HTMX request the table/grid fragment. The data assembly lives in
// buildArtistListData so the next/ channel renders the identical data set.
func (r *Router) handleArtistsPage(w http.ResponseWriter, req *http.Request) {
	data, ok := r.buildArtistListData(w, req)
	if !ok {
		return
	}
	if isHTMXRequest(req) {
		if data.View == "grid" {
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
// or filter_missing_meta=-y (exclude). Single-value keys use exactly "+y" or
// "-y". Per-library params use filter_library_{id}=+y / -y and are stored as
// "library_{id}" -> "include"/"exclude". Recognized single-value keys span
// legacy composite filters (missing_meta, missing_images, missing_mbid,
// excluded, locked), artist-type filters (type_person, type_group,
// type_orchestra, type_other), metadata-field presence (has_biography, has_years_active,
// has_formed, has_disbanded, has_born, has_died, has_gender, has_type,
// has_country, has_genres, has_styles, has_moods, has_members,
// has_discography), image presence (has_thumb, has_fanart, has_logo,
// has_banner), platform membership (in_emby, in_jellyfin, has_lidarr), and
// rule status (has_violations). The full set is the keys slice below.
func parseFlyoutFilters(req *http.Request) map[string]string {
	keys := []string{
		// Legacy / composite filters.
		"missing_meta", "missing_images", "missing_mbid", "excluded", "locked",
		// Artist type filters (aggregated into IN/NOT IN by buildWhereClause).
		// type_other is the negation facet (everything not Person/Group/
		// Orchestra-Choir, including untyped), resolved in buildWhereClause.
		"type_person", "type_group", "type_orchestra", "type_other",
		// Metadata field presence filters.
		"has_biography", "has_years_active", "has_formed", "has_disbanded",
		"has_born", "has_died", "has_gender", "has_type", "has_country",
		"has_genres", "has_styles", "has_moods", "has_members", "has_discography",
		// Per-image-type presence filters.
		"has_thumb", "has_fanart", "has_logo", "has_banner",
		// Platform membership filters.
		"in_emby", "in_jellyfin", "has_lidarr",
		// Rule violation filter.
		"has_violations",
	}
	filters := make(map[string]string)
	for _, k := range keys {
		raw := req.URL.Query().Get("filter_" + k)
		switch raw {
		case "+y":
			filters[k] = "include"
		case "-y":
			filters[k] = "exclude"
		}
	}

	// Parse per-library filter params (filter_library_{id}=+y / -y).
	for param, vals := range req.URL.Query() {
		if !strings.HasPrefix(param, "filter_library_") {
			continue
		}
		if len(vals) == 0 || vals[0] == "" {
			continue
		}
		libID := param[len("filter_library_"):]
		if libID == "" {
			continue
		}
		switch vals[0] {
		case "+y":
			filters["library_"+libID] = "include"
		case "-y":
			filters["library_"+libID] = "exclude"
		}
	}

	// Include-mode normalization (issue #1217, revised by #1786): once any library is set to Include, explicit library excludes are redundant -- buildWhereClause ignores libExcludes in that mode.
	libraryWhitelist := false
	for key, state := range filters {
		if state == "include" && strings.HasPrefix(key, "library_") {
			libraryWhitelist = true
			break
		}
	}
	if libraryWhitelist {
		for key, state := range filters {
			if state == "exclude" && strings.HasPrefix(key, "library_") {
				delete(filters, key)
			}
		}
	}

	if len(filters) == 0 {
		return nil
	}
	return filters
}

// handleArtistMatchingIDs returns the IDs of all artists that match the
// current filter state. Used by the "select all N matching" affordance on
// /artists so the client can load the full cross-page selection without a
// server-side "select-all mode" flag.
//
// GET /api/v1/artists/matching-ids
//
// Accepts the same filter query params as GET /api/v1/artists (search,
// filter, library_id, filter_*). Page, page_size, and sort are ignored.
// Returns:
//
//	{
//	  "ids":   ["id1", "id2", ...],  // up to MaxListIDs entries
//	  "total": 1234,                 // true match count (may exceed len(ids))
//	  "capped": true                 // true when total > MaxListIDs
//	}
func (r *Router) handleArtistMatchingIDs(w http.ResponseWriter, req *http.Request) {
	// Parse the same filter params as handleArtistsPage, but without
	// page/page_size/sort since we return the full (capped) ID list.
	params := artist.CountParams{
		Search:    req.URL.Query().Get("search"),
		Filter:    req.URL.Query().Get("filter"),
		LibraryID: req.URL.Query().Get("library_id"),
		Filters:   parseFlyoutFilters(req),
	}

	ids, total, capped, err := r.artistService.ListIDs(req.Context(), params)
	if err != nil {
		r.logger.Error("listing artist IDs for matching-ids", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ids":    ids,
		"total":  total,
		"capped": capped,
	})
}

// buildArtistDetailData assembles the shared ArtistDetailData for both the
// stable handleArtistDetailPage and the next/ handleNextArtistDetailPage so the
// two channels never diverge on connections, violation counts, the field
// providers map, or library metadata. It returns the assembled data, the loaded
// artist (callers need *a for neighbor lookups), and ok=false after it has
// already written an error/login response to w.
func (r *Router) buildArtistDetailData(w http.ResponseWriter, req *http.Request) (templates.ArtistDetailData, *artist.Artist, bool) {
	if !r.requireAuth(w, req) {
		return templates.ArtistDetailData{}, nil, false
	}

	id := req.PathValue("id")
	a, err := r.artistService.GetByID(req.Context(), id)
	if err != nil {
		http.Error(w, "artist not found", http.StatusNotFound)
		return templates.ArtistDetailData{}, nil, false
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

	showPlatformDebug := r.getBoolSetting(req.Context(), "show_platform_debug", false)

	// Read the active tab from query params, defaulting to "overview".
	activeTab := req.URL.Query().Get("tab")
	switch activeTab {
	case "overview", "images", "providers", "discography", "history", "violations", "debug":
		// valid tab, keep it
	default:
		activeTab = "overview"
	}

	// Build platform connection list for "View on Platform" links.
	// Cap at len(pids); per-row connection fetches may error and skip,
	// so final length can be smaller but never larger.
	var connections []templates.ArtistDetailConnection
	hasDebugConnection := false
	if r.connectionService != nil {
		pids, pidErr := r.artistService.GetPlatformIDs(req.Context(), id)
		if pidErr != nil {
			r.logger.Warn("listing platform IDs for detail page", "artist_id", id, "error", pidErr)
		}
		connections = make([]templates.ArtistDetailConnection, 0, len(pids))
		for _, pid := range pids {
			conn, connErr := r.connectionService.GetByID(req.Context(), pid.ConnectionID)
			if connErr != nil {
				r.logger.Warn("fetching connection for detail page", "connection_id", pid.ConnectionID, "error", connErr)
				continue
			}
			if conn.Type == connection.TypeEmby || conn.Type == connection.TypeJellyfin {
				hasDebugConnection = true
			}
			extURL := buildPlatformArtistURL(conn, pid.PlatformArtistID)
			connections = append(connections, templates.ArtistDetailConnection{
				ID:      conn.ID,
				Name:    conn.Name,
				Type:    conn.Type,
				URL:     extURL,
				Managed: conn.FeatureManageServerFiles,
			})
		}
	}

	// Reject tab=debug when the feature is disabled or no debug-capable connections exist.
	if activeTab == "debug" && (!showPlatformDebug || !hasDebugConnection) {
		activeTab = "overview"
	}

	// Active violation count (tab badge) + per-severity breakdown (next/ hero).
	violationCount, violationsBySeverity := r.artistViolationCounts(req.Context(), id)

	return templates.ArtistDetailData{
		Artist:               *a,
		Members:              members,
		Aliases:              aliases,
		FieldProviders:       fieldProviders,
		LibraryName:          libraryName,
		LibrarySource:        librarySource,
		ProfileName:          r.getActiveProfileName(req.Context()),
		ActiveTab:            activeTab,
		Connections:          connections,
		ShowPlatformDebug:    showPlatformDebug,
		HasDebugConnection:   hasDebugConnection,
		ViolationCount:       violationCount,
		ViolationsBySeverity: violationsBySeverity,
	}, a, true
}

// artistViolationCounts returns the active violation total and the per-severity
// breakdown ("error"/"warning"/"info") for an artist, logging and degrading to
// zero/nil on error so the detail page still renders. Split out of
// buildArtistDetailData to keep that assembler's cognitive complexity in check.
func (r *Router) artistViolationCounts(ctx context.Context, id string) (total int, bySeverity map[string]int) {
	if r.ruleService == nil {
		return 0, nil
	}
	totalLoaded := false
	if vc, err := r.ruleService.CountActiveViolationsForArtist(ctx, id); err != nil {
		r.logger.Warn("counting violations for artist detail page", "artist_id", id, "error", err)
	} else {
		total = vc
		totalLoaded = true
	}
	if bySev, err := r.ruleService.CountActiveViolationsBySeverity(ctx, rule.ViolationListParams{ArtistID: id}); err != nil {
		r.logger.Warn("counting violations by severity for artist detail page", "artist_id", id, "error", err)
	} else {
		bySeverity = bySev
		// If the total count failed to load but the per-severity buckets did,
		// derive total from the buckets so the page never shows a contradictory
		// ViolationCount=0 alongside non-zero severity counts.
		if !totalLoaded {
			for _, n := range bySev {
				total += n
			}
		}
	}
	return total, bySeverity
}

// handleArtistDetailPage renders the artist detail HTML page.
// GET /artists/{id}
func (r *Router) handleArtistDetailPage(w http.ResponseWriter, req *http.Request) {
	data, _, ok := r.buildArtistDetailData(w, req)
	if !ok {
		return
	}
	renderTempl(w, req, templates.ArtistDetailPage(r.assetsFor(req), data))
}

// buildPlatformArtistURL constructs the external URL to view an artist on
// the given platform connection. For Emby and Jellyfin the URL includes the
// server's identity (?serverId=<id>) when it has been resolved from
// /System/Info, which is required for deep-links to land on the correct
// server in multi-server web client setups. When the server ID has not yet
// been captured (e.g. legacy connections before the first successful test),
// the parameter is omitted cleanly so the URL still works for single-server
// deployments.
func buildPlatformArtistURL(conn *connection.Connection, platformArtistID string) string {
	base := strings.TrimRight(conn.URL, "/")
	switch conn.Type {
	case connection.TypeEmby:
		u := base + "/web/index.html#!/item?id=" + url.QueryEscape(platformArtistID)
		if conn.GetPlatformServerID() != "" {
			u += "&serverId=" + url.QueryEscape(conn.GetPlatformServerID())
		}
		return u
	case connection.TypeJellyfin:
		u := base + "/web/index.html#!/details?id=" + url.QueryEscape(platformArtistID)
		if conn.GetPlatformServerID() != "" {
			u += "&serverId=" + url.QueryEscape(conn.GetPlatformServerID())
		}
		return u
	case connection.TypeLidarr:
		return base + "/artist/" + url.PathEscape(platformArtistID)
	default:
		return base
	}
}

// handleArtistImagesPage renders the image management page.
// GET /artists/{id}/images
func (r *Router) handleArtistImagesPage(w http.ResponseWriter, req *http.Request) {
	if !r.requireAuth(w, req) {
		return
	}

	id := req.PathValue("id")
	a, err := r.artistService.GetByID(req.Context(), id)
	if err != nil {
		http.Error(w, "artist not found", http.StatusNotFound)
		return
	}

	webSearchEnabled, _ := r.providerSettings.AnyWebSearchEnabled(req.Context())
	// Check user preference first; fall back to the legacy app-level setting.
	autoFetch := r.getUserBoolPreference(req.Context(), PrefAutoFetchImages, r.getBoolSetting(req.Context(), "auto_fetch_images", false))

	selectedType := req.URL.Query().Get("type")
	if selectedType != "" && !validImageTypes[selectedType] {
		selectedType = ""
	}

	autoCrop := req.URL.Query().Get("crop") == "1"
	selectedIndex := intQuery(req, "index", -1)

	data := templates.ImageSearchData{
		Artist:           *a,
		WebSearchEnabled: webSearchEnabled,
		AutoFetchImages:  autoFetch,
		SelectedType:     selectedType,
		SelectedIndex:    selectedIndex,
		ProfileName:      r.getActiveProfileName(req.Context()),
		AutoCrop:         autoCrop,
		BasePath:         r.basePath,
	}
	renderTempl(w, req, templates.ImageSearchPage(r.assetsFor(req), data))
}

// isHTMXRequest checks if the request was made by HTMX.
func isHTMXRequest(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true"
}
