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
		Page:     intQuery(req, "page", 1),
		PageSize: intQuery(req, "page_size", 50),
		Sort:     req.URL.Query().Get("sort"),
		Order:    req.URL.Query().Get("order"),
		Search:   req.URL.Query().Get("search"),
		Filter:   req.URL.Query().Get("filter"),
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
	id := req.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing artist id"})
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
		renderTempl(w, req, templates.LoginPage(r.assets()))
		return
	}

	params := artist.ListParams{
		Page:     intQuery(req, "page", 1),
		PageSize: intQuery(req, "page_size", 50),
		Sort:     req.URL.Query().Get("sort"),
		Order:    req.URL.Query().Get("order"),
		Search:   req.URL.Query().Get("search"),
		Filter:   req.URL.Query().Get("filter"),
	}
	params.Validate()

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
		},
		Search: params.Search,
		Sort:   params.Sort,
		Order:  params.Order,
		Filter: params.Filter,
	}

	if isHTMXRequest(req) {
		renderTempl(w, req, templates.ArtistTable(data))
		return
	}
	renderTempl(w, req, templates.ArtistsPage(r.assets(), data))
}

// handleArtistDetailPage renders the artist detail HTML page.
// GET /artists/{id}
func (r *Router) handleArtistDetailPage(w http.ResponseWriter, req *http.Request) {
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		renderTempl(w, req, templates.LoginPage(r.assets()))
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

	conns, _ := r.connectionService.List(req.Context())

	priorities, _ := r.providerSettings.GetPriorities(req.Context())
	fieldProviders := buildFieldProvidersMap(priorities)

	data := templates.ArtistDetailData{
		Artist:         *a,
		Members:        members,
		Aliases:        aliases,
		HasConnections: len(conns) > 0,
		FieldProviders: fieldProviders,
	}
	renderTempl(w, req, templates.ArtistDetailPage(r.assets(), data))
}

// handleArtistImagesPage renders the image management page.
// GET /artists/{id}/images
func (r *Router) handleArtistImagesPage(w http.ResponseWriter, req *http.Request) {
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		renderTempl(w, req, templates.LoginPage(r.assets()))
		return
	}

	id := req.PathValue("id")
	a, err := r.artistService.GetByID(req.Context(), id)
	if err != nil {
		http.Error(w, "artist not found", http.StatusNotFound)
		return
	}

	webSearchEnabled, _ := r.providerSettings.AnyWebSearchEnabled(req.Context())

	selectedType := req.URL.Query().Get("type")
	if selectedType != "" && !validImageTypes[selectedType] {
		selectedType = ""
	}

	data := templates.ImageSearchData{
		Artist:           *a,
		WebSearchEnabled: webSearchEnabled,
		SelectedType:     selectedType,
	}
	renderTempl(w, req, templates.ImageSearchPage(r.assets(), data))
}

// isHTMXRequest checks if the request was made by HTMX.
func isHTMXRequest(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true"
}
