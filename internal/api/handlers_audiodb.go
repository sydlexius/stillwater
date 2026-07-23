package api

import (
	"context"
	"net/http"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/web/templates"
)

// handleAudioDBIdentify returns the TheAudioDB "match by name" modal body: a
// disambiguation search form pre-filled with the artist name, rendered into the
// shared field-provider modal (#field-provider-modal-body, which auto-opens on
// swap). The form auto-runs its first search on load and POSTs to the AudioDB
// search endpoint. This is the next/ entry point for the per-row identify icon
// on the audiodb_id row (web/templates/artist_field.templ fieldEditActions, via
// identifyProvider). Mirrors handleDiscogsIdentify. HTML-only response.
// GET /api/v1/artists/{id}/audiodb/identify
func (r *Router) handleAudioDBIdentify(w http.ResponseWriter, req *http.Request) {
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
		Provider:   provider.NameAudioDB,
		Field:      "audiodb_id",
		ArtistName: a.Name,
		SearchURL:  "/api/v1/artists/" + a.ID + "/audiodb/search",
	}))
}

// handleAudioDBSearch searches TheAudioDB by name and returns scored candidates
// for linking, mirroring handleDiscogsSearch. Unlike Discogs/Deezer (which score
// against their own provider IDs), AudioDB search results already carry a
// MusicBrainz ID (strMusicBrainzID), so confidence is computed via the shared
// MusicBrainz cross-MBID album comparison (enrichAndScoreTier2) rather than an
// AudioDB-specific ReleaseGroupFetcher.
// POST /api/v1/artists/{id}/audiodb/search
func (r *Router) handleAudioDBSearch(w http.ResponseWriter, req *http.Request) {
	if r.artistService == nil || r.orchestrator == nil {
		writeError(w, req, http.StatusServiceUnavailable, "artist service or orchestrator not configured")
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
		req.Context(), query, []provider.ProviderName{provider.NameAudioDB})
	if err != nil {
		r.logger.Error("audiodb search failed", "error", err)
		writeError(w, req, http.StatusInternalServerError, "search failed")
		return
	}

	// An AudioDB provider error (rate limit, outage, missing API key) is distinct
	// from a clean "no matches" result: surface it so the empty list is not
	// mistaken for "no such artist on TheAudioDB".
	providerErrored := len(collectFailedProviderDisplayNames(statuses)) > 0

	var localAlbums []string
	if a.Path != "" {
		localAlbums = artist.ListLocalAlbums(a.Path)
	}

	// AudioDB results carry MusicBrainz IDs, so reuse the shared MusicBrainz
	// album-comparison scorer (keyed on res.MusicBrainzID) rather than a
	// provider-specific fetcher.
	candidates := r.enrichAudioDBCandidates(req.Context(), results, localAlbums)

	if isHTMXRequest(req) {
		providerError := ""
		if providerErrored {
			providerError = provider.NameAudioDB.DisplayName()
		}
		renderTempl(w, req, templates.AudioDBCandidates(templates.AudioDBCandidatesData{
			ArtistID:      artistID,
			Candidates:    toAudioDBTemplateCandidates(candidates),
			ProviderError: providerError,
		}))
		return
	}

	resp := map[string]any{"results": candidates}
	if providerErrored {
		resp["provider_error"] = provider.NameAudioDB.DisplayName()
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleAudioDBLink links a selected TheAudioDB candidate to the artist and runs
// a metadata refresh. Like Discogs (and unlike Deezer), AudioDB returns real
// metadata (biography, genres, styles, moods) so the refresh writes artist.nfo;
// the link is therefore guarded twice:
//
//  1. Locked-field check: if the audiodb_id field is pinned, the write is
//     refused with 409 so a user lock survives the identify flow. (audiodb_id
//     IS part of the lockable field vocabulary -- see artist.FieldAudioDBID.)
//  2. Conflict gate: the refresh may write artist.nfo, so the NFO write gate is
//     consulted and a gated write is refused with the structured 409 payload.
//
// POST /api/v1/artists/{id}/audiodb/link
func (r *Router) handleAudioDBLink(w http.ResponseWriter, req *http.Request) {
	if r.artistService == nil {
		writeError(w, req, http.StatusServiceUnavailable, "artist service not configured")
		return
	}

	artistID := req.PathValue("id")

	audiodbID := extractFormOrJSONField(req, "audiodb_id")
	if audiodbID == "" || !isAllDigits(audiodbID) {
		writeError(w, req, http.StatusBadRequest, "a numeric audiodb_id is required")
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

	// The two guards below each return a 409 with a distinct body shape
	// (a field_locked object vs. the conflict-gate ConflictWriteBlock); the
	// route's OpenAPI 409 schema is a oneOf of both (see internal/api/openapi.yaml).
	// Guard 1: respect a user pin on the audiodb_id field. A locked field must
	// not be overwritten by the identify flow.
	if r.artistService.IsFieldLocked(a, artist.FieldAudioDBID) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":  "field_locked",
			"field":  string(artist.FieldAudioDBID),
			"reason": "the TheAudioDB ID field is locked; unlock it before matching by name",
		})
		return
	}

	// Guard 2: the refresh below may write artist.nfo, so gate on the conflict
	// ledger (returns 409 when blocked).
	if !r.gateNFOWrite(w, req) {
		return
	}

	a.AudioDBID = audiodbID

	refreshSkipped, err := r.autoLinkAndRefresh(req.Context(), a)
	if err != nil {
		r.logger.Error("audiodb link: updating artist", "artist_id", a.ID, "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to link TheAudioDB ID")
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
			r.logger.Warn("audiodb link: re-fetch for OOB swap failed; using in-memory artist",
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
				r.logger.Warn("audiodb link: loading provider priorities for row render failed; rendering row without provider hints",
					"artist_id", a.ID, "error", perr)
			}
			fieldProviders = buildFieldProvidersMap(priorities)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := templates.AudioDBLinkSuccess(*fresh, fieldProviders).Render(req.Context(), w); err != nil {
			r.logger.Error("rendering audiodb link success", "artist_id", a.ID, "error", err)
		}
		return
	}

	resp := map[string]any{
		"status":     "linked",
		"artist_id":  a.ID,
		"audiodb_id": a.AudioDBID,
	}
	if refreshSkipped {
		// The TheAudioDB ID was persisted (a manual edit the lock allows) but
		// the provider refresh that normally follows was suppressed by the
		// artist-level lock.
		resp["refresh_skipped_locked"] = true
	}
	writeJSON(w, http.StatusOK, resp)
}

// enrichAudioDBCandidates scores TheAudioDB search results by album-discography
// agreement. Unlike the Discogs/Deezer siblings (which key on their own provider
// ID via a provider-specific fetcher), AudioDB results carry a MusicBrainz ID, so
// scoring delegates to the shared MusicBrainz cross-MBID scorer enrichAndScoreTier2.
//
// When there are no local album subdirectories to compare against, the album
// comparison cannot add signal: this short-circuits to name-only scoring via
// convertToScoredCandidates (reason "no album data available"), mirroring the
// len(localAlbums)==0 early-return in enrichDiscogsCandidates / enrichDeezerCandidates.
// This avoids firing pointless MusicBrainz GetReleaseGroups calls when there is
// nothing on disk to score against.
func (r *Router) enrichAudioDBCandidates(ctx context.Context, results []provider.ArtistSearchResult, localAlbums []string) []ScoredCandidate {
	if len(localAlbums) == 0 {
		return convertToScoredCandidates(results)
	}
	return r.enrichAndScoreTier2(ctx, results, localAlbums)
}

// toAudioDBTemplateCandidates adapts the api-package ScoredCandidate values to
// the templates-package view model (the templates package cannot import api).
func toAudioDBTemplateCandidates(scored []ScoredCandidate) []templates.AudioDBCandidate {
	out := make([]templates.AudioDBCandidate, len(scored))
	for i := range scored {
		out[i] = templates.AudioDBCandidate{
			Result:          scored[i].ArtistSearchResult,
			AlbumComparison: scored[i].AlbumComparison,
			Confidence:      scored[i].Confidence,
		}
	}
	return out
}
