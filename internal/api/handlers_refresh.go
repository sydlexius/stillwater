package api

import (
	"context"
	"encoding/json"
	"log/slog"
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

	r.evaluateArtistHealth(req.Context(), a)

	if isHTMXRequest(req) {
		r.renderRefreshWithOOB(w, req, a.ID, sources)
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

	// Fetch artist to get filesystem path for album comparison.
	var localAlbums []string
	if a, err := r.artistService.GetByID(req.Context(), artistID); err == nil && a.Path != "" {
		localAlbums = artist.ListLocalAlbums(a.Path)
	}

	candidates := r.enrichWithAlbumComparison(req.Context(), results, localAlbums)

	if isHTMXRequest(req) {
		renderTempl(w, req, templates.DisambiguationResults(artistID, candidates))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": candidates})
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

	r.evaluateArtistHealth(req.Context(), a)

	if isHTMXRequest(req) {
		r.renderRefreshWithOOB(w, req, a.ID, sources)
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
	if m.DeezerID != "" && a.DeezerID == "" {
		a.DeezerID = m.DeezerID
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

// renderRefreshWithOOB renders the refresh result summary followed by OOB
// fragments that update the artist detail sections in-place.
func (r *Router) renderRefreshWithOOB(w http.ResponseWriter, req *http.Request, artistID string, sources []provider.FieldSource) {
	// Re-fetch the updated artist to get current field values
	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		renderTempl(w, req, templates.RefreshResultSummary(artistID, sources))
		return
	}

	members, err := r.artistService.ListMembersByArtistID(req.Context(), artistID)
	if err != nil {
		r.logger.Warn("listing members for OOB refresh", "artist_id", artistID, "error", err)
		renderTempl(w, req, templates.RefreshResultSummary(artistID, sources))
		return
	}

	priorities, _ := r.providerSettings.GetPriorities(req.Context())
	fieldProviders := buildFieldProvidersMap(priorities)

	var isDegraded bool
	if r.libraryService != nil && a.LibraryID != "" {
		if lib, err := r.libraryService.GetByID(req.Context(), a.LibraryID); err == nil {
			isDegraded = lib.IsDegraded()
		}
	}

	oobData := templates.RefreshOOBData{
		Artist:         *a,
		Members:        members,
		FieldProviders: fieldProviders,
		IsDegraded:     isDegraded,
	}

	// Write primary response then OOB fragments sequentially
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.RefreshResultSummary(a.ID, sources).Render(req.Context(), w); err != nil {
		r.logger.Error("rendering refresh summary", "artist_id", artistID, "error", err)
		return
	}
	if err := templates.RefreshOOBFragments(oobData).Render(req.Context(), w); err != nil {
		r.logger.Error("rendering OOB fragments", "artist_id", artistID, "error", err)
	}
}

// handleReidentify clears all provider IDs for an artist and returns the
// disambiguation form so the user can re-link the correct entry.
// POST /api/v1/artists/{id}/reidentify
func (r *Router) handleReidentify(w http.ResponseWriter, req *http.Request) {
	artistID := req.PathValue("id")

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		writeError(w, req, http.StatusNotFound, "artist not found")
		return
	}

	// Log previous MBID for audit trail before clearing.
	r.logger.Info("re-identifying artist",
		slog.String("artist_id", a.ID),
		slog.String("artist_name", a.Name),
		slog.String("previous_mbid", a.MusicBrainzID),
	)

	// Clear all provider IDs and their fetch timestamps so the UI shows
	// "Not set" instead of the misleading "Not found" for providers that
	// have not been re-queried yet.
	a.MusicBrainzID = ""
	a.AudioDBID = ""
	a.DiscogsID = ""
	a.WikidataID = ""
	a.DeezerID = ""
	a.AudioDBIDFetchedAt = nil
	a.DiscogsIDFetchedAt = nil
	a.WikidataIDFetchedAt = nil
	a.LastFMFetchedAt = nil

	if err := r.artistService.Update(req.Context(), a); err != nil {
		writeError(w, req, http.StatusInternalServerError, "failed to clear provider IDs")
		return
	}

	if isHTMXRequest(req) {
		renderTempl(w, req, templates.RefreshDisambiguationForm(a.ID, a.Name))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "disambiguation_required",
		"artist":  a.Name,
		"message": "Provider IDs cleared. Search to find and link the correct artist.",
	})
}

// enrichWithAlbumComparison wraps search results in DisambiguationCandidate,
// enriching the top 3 MusicBrainz results with album comparison data when
// local albums are available.
func (r *Router) enrichWithAlbumComparison(ctx context.Context, results []provider.ArtistSearchResult, localAlbums []string) []templates.DisambiguationCandidate {
	candidates := make([]templates.DisambiguationCandidate, len(results))
	for i, res := range results {
		candidates[i].Result = res
	}

	if len(localAlbums) == 0 || r.providerRegistry == nil {
		return candidates
	}

	// Type-assert MusicBrainz provider to ReleaseGroupFetcher.
	mbProvider := r.providerRegistry.Get(provider.NameMusicBrainz)
	if mbProvider == nil {
		return candidates
	}
	fetcher, ok := mbProvider.(provider.ReleaseGroupFetcher)
	if !ok {
		return candidates
	}

	// Enrich top 3 MB results that have an MBID. Track attempts (not just
	// successes) to cap the total number of API calls made during search.
	attempted := 0
	for i := range candidates {
		if attempted >= 3 {
			break
		}
		res := candidates[i].Result
		if res.MusicBrainzID == "" {
			continue
		}

		attempted++

		groups, err := fetcher.GetReleaseGroups(ctx, res.MusicBrainzID)
		if err != nil {
			r.logger.Warn("fetching release groups for disambiguation",
				slog.String("mbid", res.MusicBrainzID),
				slog.String("error", err.Error()),
			)
			continue
		}

		remoteTitles := make([]string, len(groups))
		for j, rg := range groups {
			remoteTitles[j] = rg.Title
		}

		comp := artist.CompareAlbums(localAlbums, remoteTitles)
		candidates[i].AlbumComparison = &comp
	}

	return candidates
}

// evaluateArtistHealth runs the rule engine against an artist and updates
// the stored health score. Errors are logged but not propagated (non-blocking).
// Callers should pass the artist object they already have to avoid an extra DB read.
func (r *Router) evaluateArtistHealth(ctx context.Context, a *artist.Artist) {
	if r.ruleEngine == nil {
		return
	}

	result, err := r.ruleEngine.Evaluate(ctx, a)
	if err != nil {
		r.logger.Warn("evaluating artist health", slog.String("artist_id", a.ID), slog.String("error", err.Error()))
		return
	}

	a.HealthScore = result.HealthScore
	if err := r.artistService.Update(ctx, a); err != nil {
		r.logger.Warn("saving artist health score", slog.String("artist_id", a.ID), slog.String("error", err.Error()))
	}
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
