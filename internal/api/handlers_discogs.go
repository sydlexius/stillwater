package api

import (
	"context"
	"net/http"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/web/templates"
)

// handleDiscogsIdentify returns the Discogs "match by name" modal body: a
// disambiguation search form pre-filled with the artist name, rendered into the
// shared field-provider modal (#field-provider-modal-body, which auto-opens on
// swap). The form auto-runs its first search on load and POSTs to the Discogs
// search endpoint. This is the next/ entry point for the per-row identify icon
// on the discogs_id row (web/templates/artist_field.templ fieldEditActions, via
// identifyProvider). Mirrors handleDeezerIdentify. HTML-only response.
// GET /api/v1/artists/{id}/discogs/identify
func (r *Router) handleDiscogsIdentify(w http.ResponseWriter, req *http.Request) {
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
		Provider:   provider.NameDiscogs,
		Field:      "discogs_id",
		ArtistName: a.Name,
		SearchURL:  "/api/v1/artists/" + a.ID + "/discogs/search",
	}))
}

// handleDiscogsSearch searches Discogs by name and returns scored candidates for
// linking, mirroring handleDeezerSearch but keyed on Discogs's own provider ID.
// Confidence reflects name plus album/discography agreement when the artist has
// local album subdirectories (the Discogs ReleaseGroupFetcher scores the top
// candidates).
// POST /api/v1/artists/{id}/discogs/search
func (r *Router) handleDiscogsSearch(w http.ResponseWriter, req *http.Request) {
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
		req.Context(), query, []provider.ProviderName{provider.NameDiscogs})
	if err != nil {
		r.logger.Error("discogs search failed", "error", err)
		writeError(w, req, http.StatusInternalServerError, "search failed")
		return
	}

	// A Discogs provider error (rate limit, outage) is distinct from a clean
	// "no matches" result: surface it so the empty list is not mistaken for
	// "no such artist on Discogs".
	providerErrored := len(collectFailedProviderDisplayNames(statuses)) > 0

	// The a.Path != "" pre-check that used to guard this call is gone: the
	// filesystem album source treats a missing path as EvidenceUnknown itself.
	localAlbums := r.localAlbumSet(req.Context(), a)

	candidates := r.enrichDiscogsCandidates(req.Context(), results, localAlbums)

	if isHTMXRequest(req) {
		providerError := ""
		if providerErrored {
			providerError = provider.NameDiscogs.DisplayName()
		}
		renderTempl(w, req, templates.DiscogsCandidates(templates.DiscogsCandidatesData{
			ArtistID:      artistID,
			Candidates:    toDiscogsTemplateCandidates(candidates),
			ProviderError: providerError,
			// Say "albums not checked" out loud rather than omitting the badge,
			// which an operator cannot tell from "matched nothing".
			AlbumsUnavailable: localAlbums.Evidence == artist.EvidenceUnknown,
		}))
		return
	}

	resp := map[string]any{"results": candidates}
	if providerErrored {
		resp["provider_error"] = provider.NameDiscogs.DisplayName()
	}
	markAlbumsUnavailable(resp, localAlbums)
	writeJSON(w, http.StatusOK, resp)
}

// handleDiscogsLink links a selected Discogs candidate to the artist and runs a
// metadata refresh. Unlike the Deezer link (Deezer returns only name + URL),
// Discogs returns real metadata (profile, members, name variations) so the
// refresh writes the artist.nfo; the link is therefore guarded twice:
//
//  1. Locked-field check: if the discogs_id field is pinned, the write is
//     refused with 423 (refuseLockedProviderIDs) so a user lock survives the
//     identify flow. Every provider ID is a lockable token and every link flow
//     refuses the same way.
//  2. Conflict gate: the refresh may write artist.nfo, so the NFO write gate is
//     consulted and a gated write is refused with the structured 409 payload.
//
// POST /api/v1/artists/{id}/discogs/link
func (r *Router) handleDiscogsLink(w http.ResponseWriter, req *http.Request) {
	if r.artistService == nil {
		writeError(w, req, http.StatusServiceUnavailable, "artist service not configured")
		return
	}

	artistID := req.PathValue("id")

	discogsID := extractFormOrJSONField(req, "discogs_id")
	if discogsID == "" || !isAllDigits(discogsID) {
		writeError(w, req, http.StatusBadRequest, "a numeric discogs_id is required")
		return
	}

	// Resolve the artist first so a link to a NON-EXISTENT artist returns 404
	// even when the conflict gate is active (the 404 check must precede the 409
	// gate; otherwise an unknown ID would be masked by a gate block).
	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		writeError(w, req, http.StatusNotFound, "artist not found")
		return
	}

	// Guard 1: respect a user pin on the discogs_id field. A locked field must
	// not be overwritten by the identify flow. Routed through
	// refuseLockedProviderIDs (see provider_id_lock_guard.go) rather than a
	// hand-rolled write so this flow answers the same 423 shape as every other
	// lock refusal in the API.
	if r.refuseLockedProviderIDs(w, a, artist.FieldDiscogsID) {
		return
	}

	// Guard 2: the refresh below may write artist.nfo, so gate on the conflict
	// ledger (returns 409 when blocked).
	if !r.gateNFOWrite(w, req) {
		return
	}

	a.DiscogsID = discogsID

	refreshSkipped, err := r.autoLinkAndRefresh(req.Context(), a, false, "")
	if err != nil {
		r.logger.Error("discogs link: updating artist", "artist_id", a.ID, "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to link Discogs ID")
		return
	}

	// Linking a provider ID and refreshing changes health-relevant fields.
	r.InvalidateHealthCache()
	r.runRulesAfterRefresh(req.Context(), a)

	if isHTMXRequest(req) {
		// Re-fetch so the OOB row swap renders the persisted value (and any
		// provider-promoted name).
		fresh, ferr := r.artistService.GetByID(req.Context(), a.ID)
		if ferr != nil {
			// Re-fetch failed; fall back to the in-memory artist so the OOB swap
			// still renders, but log it so the degraded render is debuggable.
			r.logger.Warn("discogs link: re-fetch for OOB swap failed; using in-memory artist",
				"artist_id", a.ID, "error", ferr)
			fresh = a
		}
		var fieldProviders map[string][]string
		if r.providerSettings != nil {
			priorities, perr := r.providerSettings.GetPriorities(req.Context())
			if perr != nil {
				// Non-fatal: the row still renders without per-field provider
				// hints, so degrade rather than fail the link. Log it so the
				// degraded render is debuggable.
				r.logger.Warn("discogs link: loading provider priorities for row render failed; rendering row without provider hints",
					"artist_id", a.ID, "error", perr)
			}
			fieldProviders = buildFieldProvidersMap(priorities)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := templates.DiscogsLinkSuccess(*fresh, fieldProviders).Render(req.Context(), w); err != nil {
			r.logger.Error("rendering discogs link success", "artist_id", a.ID, "error", err)
		}
		return
	}

	// Always present, never omitted when false -- see handlers_audiodb.go for
	// why key presence must not be the signal. When true, the Discogs ID was
	// persisted (a manual edit the lock allows) but the provider refresh that
	// normally follows was suppressed by the artist-level lock.
	resp := map[string]any{
		"status":                 "linked",
		"artist_id":              a.ID,
		"discogs_id":             a.DiscogsID,
		"refresh_skipped_locked": refreshSkipped,
	}
	writeJSON(w, http.StatusOK, resp)
}

// enrichDiscogsCandidates scores Discogs search results by album-discography
// agreement, mirroring enrichDeezerCandidates but keyed on the Discogs provider
// ID (res.ProviderID).
//
// It takes an evidenced artist.AlbumSet rather than a bare []string so the
// fallback can say WHICH kind of missing album data it hit: a genuinely empty
// artist folder, or one that could not be read at all.
func (r *Router) enrichDiscogsCandidates(ctx context.Context, results []provider.ArtistSearchResult, local artist.AlbumSet) []ScoredCandidate {
	fallbackReason := albumEvidenceReason(local.Evidence)
	if r.providerRegistry == nil || local.Evidence != artist.EvidenceFound {
		return convertToScoredCandidatesReason(results, fallbackReason)
	}

	dgProvider := r.providerRegistry.Get(provider.NameDiscogs)
	if dgProvider == nil {
		return convertToScoredCandidatesReason(results, fallbackReason)
	}
	// Use the broad Main-role title set (master AND release-level, deduped) for
	// the album match so release-only albums are not undercounted (#1831). This
	// is distinct from the master-only ReleaseGroupFetcher used elsewhere.
	fetcher, ok := dgProvider.(provider.MainReleaseTitleFetcher)
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

		// Cap at the first 3 candidates (matching the MusicBrainz/Deezer
		// pattern) to bound the number of release-group API calls per search.
		if attempted >= 3 || res.ProviderID == "" {
			continue
		}
		attempted++

		remoteTitles, err := fetcher.GetMainReleaseTitles(ctx, res.ProviderID)
		if err != nil {
			r.logger.Warn("discogs identify: fetching main release titles",
				"discogs_id", res.ProviderID, "error", err)
			continue
		}

		// CompareAlbumSet delegates the arithmetic to CompareAlbums and adds the
		// local side's evidence state, so a 0% match here is a real disagreement
		// rather than an unread directory.
		ev := artist.CompareAlbumSet(local, remoteTitles)
		comp := ev.AlbumComparison
		scored[i].AlbumComparison = &comp
		scored[i].Confidence = float64(comp.MatchPercent) / 100.0
	}

	return scored
}

// toDiscogsTemplateCandidates adapts the api-package ScoredCandidate values to
// the templates-package view model (the templates package cannot import api).
func toDiscogsTemplateCandidates(scored []ScoredCandidate) []templates.DiscogsCandidate {
	out := make([]templates.DiscogsCandidate, len(scored))
	for i := range scored {
		out[i] = templates.DiscogsCandidate{
			Result:          scored[i].ArtistSearchResult,
			AlbumComparison: scored[i].AlbumComparison,
			Confidence:      scored[i].Confidence,
		}
	}
	return out
}
