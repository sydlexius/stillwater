package api

import (
	"context"
	"net/http"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/web/templates"
)

// handleDeezerIdentify returns the Deezer "match by name" modal body: a
// disambiguation search form pre-filled with the artist name, rendered into the
// shared field-provider modal (#field-provider-modal-body, which auto-opens on
// swap). The form auto-runs its first search on load and POSTs to the Deezer
// search endpoint. This is the next/ entry point for the per-row identify icon
// (web/templates/artist_field.templ fieldEditActions). HTML-only response.
// GET /api/v1/artists/{id}/deezer/identify
func (r *Router) handleDeezerIdentify(w http.ResponseWriter, req *http.Request) {
	if r.artistService == nil {
		writeError(w, req, http.StatusServiceUnavailable, "artist service not configured")
		return
	}

	artistID := req.PathValue("id")

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		writeError(w, req, http.StatusNotFound, "artist not found")
		return
	}

	renderTempl(w, req, templates.ProviderIdentifyModal(templates.ProviderIdentifyModalData{
		ArtistID:   a.ID,
		Provider:   provider.NameDeezer,
		Field:      "deezer_id",
		ArtistName: a.Name,
		SearchURL:  "/api/v1/artists/" + a.ID + "/deezer/search",
	}))
}

// handleDeezerSearch searches Deezer by name and returns scored candidates for
// linking, mirroring the MusicBrainz identify flow (handleRefreshSearch) but
// keyed on Deezer's own provider ID. Confidence reflects name plus
// album/discography agreement when the artist has local album subdirectories.
// POST /api/v1/artists/{id}/deezer/search
func (r *Router) handleDeezerSearch(w http.ResponseWriter, req *http.Request) {
	if r.artistService == nil || r.orchestrator == nil {
		writeError(w, req, http.StatusServiceUnavailable, "artist service not configured")
		return
	}

	artistID := req.PathValue("id")

	query := extractFormOrJSONField(req, "query")
	if query == "" {
		writeError(w, req, http.StatusBadRequest, "search query is required")
		return
	}

	// Fetch the artist for its filesystem path (album comparison) and to 404
	// cleanly when the ID is unknown.
	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		writeError(w, req, http.StatusNotFound, "artist not found")
		return
	}

	results, statuses, err := r.orchestrator.SearchForLinking(
		req.Context(), query, []provider.ProviderName{provider.NameDeezer})
	if err != nil {
		r.logger.Error("deezer search failed", "error", err)
		writeError(w, req, http.StatusInternalServerError, "search failed")
		return
	}

	// A Deezer provider error (rate limit, outage) is distinct from a clean
	// "no matches" result: surface it so the empty list is not mistaken for
	// "no such artist on Deezer".
	providerErrored := len(collectFailedProviderDisplayNames(statuses)) > 0

	var localAlbums []string
	if a.Path != "" {
		localAlbums = artist.ListLocalAlbums(a.Path)
	}

	candidates := r.enrichDeezerCandidates(req.Context(), results, localAlbums)

	if isHTMXRequest(req) {
		providerError := ""
		if providerErrored {
			providerError = provider.NameDeezer.DisplayName()
		}
		renderTempl(w, req, templates.DeezerCandidates(templates.DeezerCandidatesData{
			ArtistID:      artistID,
			Candidates:    toDeezerTemplateCandidates(candidates),
			ProviderError: providerError,
		}))
		return
	}

	resp := map[string]any{"results": candidates}
	if providerErrored {
		resp["provider_error"] = provider.NameDeezer.DisplayName()
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleDeezerLink links a selected Deezer candidate to the artist and runs a
// metadata refresh. Deezer's GetArtist currently returns only name + URL, so the
// refresh effect is limited; the durable value is persisting the Deezer ID for
// future image/deep-link use.
// POST /api/v1/artists/{id}/deezer/link
func (r *Router) handleDeezerLink(w http.ResponseWriter, req *http.Request) {
	if r.artistService == nil {
		writeError(w, req, http.StatusServiceUnavailable, "artist service not configured")
		return
	}

	artistID := req.PathValue("id")

	deezerID := extractFormOrJSONField(req, "deezer_id")
	if deezerID == "" || !isAllDigits(deezerID) {
		writeError(w, req, http.StatusBadRequest, "a numeric deezer_id is required")
		return
	}

	// Resolve the artist first so a link to a NON-EXISTENT artist returns 404
	// even when the conflict gate is active (the 404 check must precede the
	// 409 gate; otherwise an unknown ID would be masked by a gate block).
	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		writeError(w, req, http.StatusNotFound, "artist not found")
		return
	}

	// The refresh below may write images, so gate on the conflict ledger
	// (returns 409 when blocked). deezer_id is NOT part of the lockable field
	// vocabulary (see internal/artist/fieldname.go: only metadata fields like
	// name, biography, genres are lockable), so no field-lock check is needed.
	if !r.gateImageWrite(w, req) {
		return
	}

	a.DeezerID = deezerID

	if err := r.autoLinkAndRefresh(req.Context(), a); err != nil {
		r.logger.Error("deezer link: updating artist", "artist_id", a.ID, "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to link Deezer ID")
		return
	}

	// Linking a provider ID affects health-relevant fields.
	r.InvalidateHealthCache()

	if isHTMXRequest(req) {
		// Re-fetch so the OOB row swap renders the persisted value.
		fresh, ferr := r.artistService.GetByID(req.Context(), a.ID)
		if ferr != nil {
			// Re-fetch failed; fall back to the in-memory artist so the OOB
			// swap still renders, but log it so the degraded render is debuggable.
			r.logger.Warn("deezer link: re-fetch for OOB swap failed; using in-memory artist",
				"artist_id", a.ID, "error", ferr)
			fresh = a
		}
		var fieldProviders map[string][]string
		if r.providerSettings != nil {
			priorities, _ := r.providerSettings.GetPriorities(req.Context())
			fieldProviders = buildFieldProvidersMap(priorities)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := templates.DeezerLinkSuccess(*fresh, fieldProviders).Render(req.Context(), w); err != nil {
			r.logger.Error("rendering deezer link success", "artist_id", a.ID, "error", err)
		}
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "linked",
		"artist_id": a.ID,
		"deezer_id": a.DeezerID,
	})
}

// enrichDeezerCandidates scores Deezer search results by album-discography
// agreement, mirroring enrichAndScoreTier2 but keyed on the Deezer provider ID
// (res.ProviderID). enrichAndScoreTier2 cannot be reused: it is hardwired to the
// MusicBrainz provider and res.MusicBrainzID. When album enrichment is not
// possible (no registry, no Deezer provider, or no local albums) candidates fall
// back to name-only scoring via convertToScoredCandidates.
func (r *Router) enrichDeezerCandidates(ctx context.Context, results []provider.ArtistSearchResult, localAlbums []string) []ScoredCandidate {
	if r.providerRegistry == nil || len(localAlbums) == 0 {
		return convertToScoredCandidates(results)
	}

	dzProvider := r.providerRegistry.Get(provider.NameDeezer)
	if dzProvider == nil {
		return convertToScoredCandidates(results)
	}
	fetcher, ok := dzProvider.(provider.ReleaseGroupFetcher)
	if !ok {
		return convertToScoredCandidates(results)
	}

	scored := make([]ScoredCandidate, len(results))
	attempted := 0
	for i := range results {
		res := &results[i]
		scored[i] = ScoredCandidate{
			ArtistSearchResult: *res,
			Reason:             "album comparison",
		}

		// Cap at the first 3 candidates (matching the MusicBrainz pattern) to
		// bound the number of release-group API calls per search.
		if attempted >= 3 || res.ProviderID == "" {
			continue
		}
		attempted++

		groups, err := fetcher.GetReleaseGroups(ctx, res.ProviderID)
		if err != nil {
			r.logger.Warn("deezer identify: fetching release groups",
				"deezer_id", res.ProviderID, "error", err)
			continue
		}

		remoteTitles := make([]string, len(groups))
		for j, rg := range groups {
			remoteTitles[j] = rg.Title
		}

		comp := artist.CompareAlbums(localAlbums, remoteTitles)
		scored[i].AlbumComparison = &comp
		scored[i].Confidence = float64(comp.MatchPercent) / 100.0
	}

	return scored
}

// toDeezerTemplateCandidates adapts the api-package ScoredCandidate values to the
// templates-package view model (the templates package cannot import api).
func toDeezerTemplateCandidates(scored []ScoredCandidate) []templates.DeezerCandidate {
	out := make([]templates.DeezerCandidate, len(scored))
	for i := range scored {
		out[i] = templates.DeezerCandidate{
			Result:          scored[i].ArtistSearchResult,
			AlbumComparison: scored[i].AlbumComparison,
			Confidence:      scored[i].Confidence,
		}
	}
	return out
}

// isAllDigits reports whether s is non-empty and contains only ASCII digits.
// Used to validate a Deezer ID before linking (Deezer IDs are numeric).
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
