package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/web/templates"
)

// handleArtistRefresh triggers a full metadata refresh for a single artist.
// If the artist has no MusicBrainz ID, returns the disambiguation search UI
// so the user can link the correct entry first.
// POST /api/v1/artists/{id}/refresh
func (r *Router) handleArtistRefresh(w http.ResponseWriter, req *http.Request) {
	artistID := req.PathValue("id")

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		writeError(w, req, http.StatusNotFound, "artist not found")
		return
	}

	if a.MusicBrainzID == "" {
		// No MBID -- show disambiguation UI
		if isHTMXRequest(req) {
			renderTempl(w, req, templates.RefreshDisambiguationForm(a.ID, a.Name))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "disambiguation_required",
			"artist":  a.Name,
			"message": "MusicBrainz ID is required. Search to find and link the correct artist.",
		})
		return
	}

	// MBID available -- run full refresh
	sources, err := r.executeRefresh(req, a)
	if err != nil {
		writeError(w, req, http.StatusInternalServerError, "metadata refresh failed")
		return
	}

	if isHTMXRequest(req) {
		renderTempl(w, req, templates.RefreshResultSummary(a.ID, sources))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "refreshed",
		"sources": sources,
	})
}

// handleRefreshSearch searches MusicBrainz and Discogs by name for disambiguation.
// POST /api/v1/artists/{id}/refresh/search
func (r *Router) handleRefreshSearch(w http.ResponseWriter, req *http.Request) {
	artistID := req.PathValue("id")

	query := extractFormOrJSONField(req, "query")
	if query == "" {
		writeError(w, req, http.StatusBadRequest, "search query is required")
		return
	}

	// Search only MusicBrainz and Discogs for disambiguation
	linkProviders := []provider.ProviderName{
		provider.NameMusicBrainz,
		provider.NameDiscogs,
	}

	results, err := r.orchestrator.SearchForLinking(req.Context(), query, linkProviders)
	if err != nil {
		writeError(w, req, http.StatusInternalServerError, "search failed: "+err.Error())
		return
	}

	if isHTMXRequest(req) {
		renderTempl(w, req, templates.DisambiguationResults(artistID, results))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// handleRefreshLink stores the selected provider ID from disambiguation,
// then continues with the full metadata refresh.
// POST /api/v1/artists/{id}/refresh/link
func (r *Router) handleRefreshLink(w http.ResponseWriter, req *http.Request) {
	artistID := req.PathValue("id")

	var body struct {
		MBID      string `json:"mbid"`
		DiscogsID string `json:"discogs_id"`
		Source    string `json:"source"`
	}
	if strings.HasPrefix(req.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeError(w, req, http.StatusBadRequest, "invalid request body")
			return
		}
	} else {
		body.MBID = req.FormValue("mbid")
		body.DiscogsID = req.FormValue("discogs_id")
		body.Source = req.FormValue("source")
	}

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		writeError(w, req, http.StatusNotFound, "artist not found")
		return
	}

	// Store the selected ID(s)
	if body.MBID != "" && a.MusicBrainzID == "" {
		a.MusicBrainzID = body.MBID
	}
	if body.DiscogsID != "" && a.DiscogsID == "" {
		a.DiscogsID = body.DiscogsID
	}

	if err := r.artistService.Update(req.Context(), a); err != nil {
		writeError(w, req, http.StatusInternalServerError, "failed to store provider ID")
		return
	}

	// Now run the full refresh with the linked MBID
	sources, err := r.executeRefresh(req, a)
	if err != nil {
		writeError(w, req, http.StatusInternalServerError, "metadata refresh failed")
		return
	}

	if isHTMXRequest(req) {
		renderTempl(w, req, templates.RefreshResultSummary(a.ID, sources))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "linked_and_refreshed",
		"sources": sources,
	})
}

// executeRefresh runs the orchestrator's FetchMetadata and applies results to the artist.
func (r *Router) executeRefresh(req *http.Request, a *artist.Artist) ([]provider.FieldSource, error) {
	result, err := r.orchestrator.FetchMetadata(req.Context(), a.MusicBrainzID, a.Name)
	if err != nil {
		r.logger.Error("metadata refresh failed",
			"artist_id", a.ID,
			"error", err)
		return nil, err
	}

	// Apply fetched metadata to the artist
	applyRefreshResult(a, result)

	if err := r.artistService.Update(req.Context(), a); err != nil {
		r.logger.Error("saving refreshed metadata failed",
			"artist_id", a.ID,
			"error", err)
		return nil, err
	}

	// Update per-provider fetch timestamps so the UI can show "Not found" vs "Not set".
	for _, prov := range result.AttemptedProviders {
		if err := r.artistService.UpdateProviderFetchedAt(req.Context(), a.ID, string(prov)); err != nil {
			r.logger.Warn("updating provider fetched_at",
				"artist_id", a.ID,
				"provider", prov,
				"error", err)
		}
	}

	// Update members if provided
	if result.Metadata != nil && len(result.Metadata.Members) > 0 {
		members := convertProviderMembers(a.ID, result.Metadata.Members)
		if err := r.artistService.UpsertMembers(req.Context(), a.ID, members); err != nil {
			r.logger.Warn("upserting members after refresh",
				"artist_id", a.ID,
				"error", err)
		}
	}

	return result.Sources, nil
}

// applyRefreshResult merges a FetchResult into an artist record.
func applyRefreshResult(a *artist.Artist, result *provider.FetchResult) {
	if result.Metadata == nil {
		return
	}
	m := result.Metadata

	if m.Biography != "" {
		a.Biography = m.Biography
	}
	if len(m.Genres) > 0 {
		a.Genres = m.Genres
	}
	if len(m.Styles) > 0 {
		a.Styles = m.Styles
	}
	if len(m.Moods) > 0 {
		a.Moods = m.Moods
	}
	if m.Formed != "" {
		a.Formed = m.Formed
	}
	if m.Born != "" {
		a.Born = m.Born
	}
	if m.Died != "" {
		a.Died = m.Died
	}
	if m.Disbanded != "" {
		a.Disbanded = m.Disbanded
	}
	if m.YearsActive != "" {
		a.YearsActive = m.YearsActive
	}
	if m.Type != "" {
		a.Type = m.Type
	}
	if m.Gender != "" {
		a.Gender = m.Gender
	}

	// Merge provider IDs
	if m.MusicBrainzID != "" && a.MusicBrainzID == "" {
		a.MusicBrainzID = m.MusicBrainzID
	}
	if m.AudioDBID != "" && a.AudioDBID == "" {
		a.AudioDBID = m.AudioDBID
	}
	if m.DiscogsID != "" && a.DiscogsID == "" {
		a.DiscogsID = m.DiscogsID
	}
	if m.WikidataID != "" && a.WikidataID == "" {
		a.WikidataID = m.WikidataID
	}

	// Update metadata sources
	if a.MetadataSources == nil {
		a.MetadataSources = make(map[string]string)
	}
	for _, src := range result.Sources {
		a.MetadataSources[src.Field] = string(src.Provider)
	}
}

// convertProviderMembers converts provider MemberInfo to artist BandMember models.
func convertProviderMembers(artistID string, members []provider.MemberInfo) []artist.BandMember {
	result := make([]artist.BandMember, len(members))
	for i, m := range members {
		result[i] = artist.BandMember{
			ArtistID:         artistID,
			MemberName:       m.Name,
			MemberMBID:       m.MBID,
			Instruments:      m.Instruments,
			VocalType:        m.VocalType,
			DateJoined:       m.DateJoined,
			DateLeft:         m.DateLeft,
			IsOriginalMember: false,
			SortOrder:        i,
		}
	}
	return result
}

// extractFormOrJSONField reads a named value from either a JSON body or form data.
func extractFormOrJSONField(req *http.Request, name string) string {
	if strings.HasPrefix(req.Header.Get("Content-Type"), "application/json") {
		var body map[string]string
		if err := json.NewDecoder(req.Body).Decode(&body); err == nil {
			return body[name]
		}
		return ""
	}
	return req.FormValue(name)
}
