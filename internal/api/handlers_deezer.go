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

	// The a.Path != "" pre-check that used to guard this call is gone: the
	// filesystem album source treats a missing path as EvidenceUnknown itself,
	// and two places deciding the same thing is how the two drift apart.
	localAlbums := r.localAlbumSet(req.Context(), a)

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
			// Say "albums not checked" out loud rather than omitting the badge,
			// which an operator cannot tell from "matched nothing".
			AlbumsUnavailable: localAlbums.Evidence == artist.EvidenceUnknown,
		}))
		return
	}

	resp := map[string]any{"results": candidates}
	if providerErrored {
		resp["provider_error"] = provider.NameDeezer.DisplayName()
	}
	markAlbumsUnavailable(resp, localAlbums)
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

	// Respect a user pin on deezer_id. Since #3037 every provider ID is a
	// guarded lock token, so without this the write below reaches the persist
	// chokepoint, is silently reverted, and the operator gets a 200 for a link
	// that did not happen.
	if r.refuseLockedProviderIDs(w, a, artist.FieldDeezerID) {
		return
	}

	// The refresh below may write images, so gate on the conflict ledger
	// (returns 409 when blocked).
	if !r.gateImageWrite(w, req) {
		return
	}

	a.DeezerID = deezerID

	refreshSkipped, err := r.autoLinkAndRefresh(req.Context(), a, false, "")
	if err != nil {
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
			priorities, perr := r.providerSettings.GetPriorities(req.Context())
			if perr != nil {
				// Non-fatal: the row still renders without per-field provider
				// hints (the fetch-from-providers icon set), so degrade rather
				// than fail the link. Log it so the degraded render is debuggable.
				r.logger.Warn("deezer link: loading provider priorities for row render failed; rendering row without provider hints",
					"artist_id", a.ID, "error", perr)
			}
			fieldProviders = buildFieldProvidersMap(priorities)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := templates.DeezerLinkSuccess(*fresh, fieldProviders).Render(req.Context(), w); err != nil {
			r.logger.Error("rendering deezer link success", "artist_id", a.ID, "error", err)
		}
		return
	}

	// Always present, never omitted when false -- see handlers_audiodb.go for
	// why key presence must not be the signal. When true, the Deezer ID was
	// persisted (a manual edit the lock allows) but the provider refresh that
	// normally follows was suppressed by the artist-level lock.
	resp := map[string]any{
		"status":                 "linked",
		"artist_id":              a.ID,
		"deezer_id":              a.DeezerID,
		"refresh_skipped_locked": refreshSkipped,
	}
	writeJSON(w, http.StatusOK, resp)
}

// enrichDeezerCandidates scores Deezer search results by album-discography
// agreement, mirroring enrichAndScoreTier2 but keyed on the Deezer provider ID
// (res.ProviderID). enrichAndScoreTier2 cannot be reused: it is hardwired to the
// MusicBrainz provider and res.MusicBrainzID.
//
// It takes an evidenced artist.AlbumSet rather than a bare []string so the
// fallback can say WHICH kind of missing album data it hit. A set that is not
// EvidenceFound is never compared -- an Unknown set contributes no titles, and
// comparing against no titles yields a 0% match that reads as a real finding.
func (r *Router) enrichDeezerCandidates(ctx context.Context, results []provider.ArtistSearchResult, local artist.AlbumSet) []ScoredCandidate {
	fallbackReason := albumEvidenceReason(local.Evidence)
	if r.providerRegistry == nil || local.Evidence != artist.EvidenceFound {
		return convertToScoredCandidatesReason(results, fallbackReason)
	}

	dzProvider := r.providerRegistry.Get(provider.NameDeezer)
	if dzProvider == nil {
		return convertToScoredCandidatesReason(results, fallbackReason)
	}
	fetcher, ok := dzProvider.(provider.ReleaseGroupFetcher)
	if !ok {
		return convertToScoredCandidatesReason(results, fallbackReason)
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

		// CompareAlbumSet rather than CompareAlbums: the arithmetic is identical
		// (it delegates), but it carries the local side's evidence into the
		// result, so a 0% here is provably "the catalogues disagree" and not
		// "we never read the albums".
		ev := artist.CompareAlbumSet(local, remoteTitles)
		comp := ev.AlbumComparison
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
