package api

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/event"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/provider/musicbrainz"
	"github.com/sydlexius/stillwater/internal/rule"
	"github.com/sydlexius/stillwater/web/templates"
)

// respondRefreshSkippedLocked answers a refresh request that was suppressed by
// the artist-level lock. It is the single statement of that response shape for
// every interactive refresh entry point.
//
// The status is 200, not 409, on purpose. HTMX only swaps a response body into
// hx-target on a 2xx; the Refresh button targets #refresh-panel, so a 4xx would
// fire the generic htmx:responseError toast and skip the swap, leaving the
// operator with a vague failure instead of a clear explanation. The handler
// already has a non-error non-success precedent in exactly this shape: the
// no-MBID branch returns 200 with status "disambiguation_required".
//
// This helper itself publishes no event and sets no sync-warning trigger: it
// only writes the response. Publishing is the CALLER's job, because only the
// caller knows whether anything changed before the lock intervened.
// handleArtistRefresh gates before it touches the artist, so nothing changed
// and it publishes nothing. handleRefreshLink gates AFTER persisting the
// user's chosen provider ID, so it publishes event.ArtistUpdated before
// calling this -- the same split autoLinkAndRefresh makes for the deezer /
// discogs / audiodb / bulk-identify link paths.
func (r *Router) respondRefreshSkippedLocked(w http.ResponseWriter, req *http.Request, a *artist.Artist) {
	// Info, not Warn: this is the lock contract working, not a fault.
	r.logger.Info("refresh skipped: artist is locked", "artist_id", a.ID)

	if isHTMXRequest(req) {
		renderTempl(w, req, templates.RefreshSkippedLocked())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "skipped_locked",
		"artist":  a.Name,
		"message": "This artist is locked. Unlock it to refresh metadata from providers.",
	})
}

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

	// Artist-level lock gate. This sits ahead of the disambiguation branch so a
	// locked artist is never invited to link an MBID that would then refresh
	// nothing. Gate on a.Locked ONLY: per-field locks (a.LockedFields) are an
	// independent layer enforced inside artist.ApplyMetadata, and a fully
	// field-pinned artist is still refreshable.
	if a.Locked {
		r.respondRefreshSkippedLocked(w, req, a)
		return
	}

	if a.MusicBrainzID == "" {
		// No MBID -- show disambiguation UI. Non-destructive by construction:
		// there is no identity here to discard, so clearIDs is false.
		if isHTMXRequest(req) {
			renderTempl(w, req, templates.RefreshDisambiguationForm(a.ID, a.Name, false))
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
	result, err := r.executeRefresh(req, a)
	if err != nil {
		writeError(w, req, http.StatusInternalServerError, "metadata refresh failed")
		return
	}

	// Apply language-promoted name/sort-name from MusicBrainz. When the
	// user's metadata language preference yields a localized alias, the
	// provider returns the promoted name. Update the artist record so the
	// UI reflects it.
	nameUpdateFailed := r.applyProviderName(req.Context(), a, result.Metadata)

	if r.eventBus != nil {
		r.eventBus.Publish(event.Event{
			Type: event.ArtistUpdated,
			Data: map[string]any{"artist_id": a.ID},
		})
	}

	// Metadata refresh changes artist fields that affect health scores.
	r.InvalidateHealthCache()

	// Auto-resolve rule violations after refresh so the artist's health
	// score reflects the newly fetched metadata and images immediately.
	r.runRulesAfterRefresh(req.Context(), a)

	if isHTMXRequest(req) {
		if nameUpdateFailed {
			setSyncWarningTrigger(w, []string{"metadata refreshed but name update could not be saved"})
		}
		r.renderRefreshWithOOB(w, req, a.ID, result.Sources)
		return
	}
	resp := map[string]any{
		"status":  "refreshed",
		"sources": result.Sources,
	}
	if nameUpdateFailed {
		resp["warning"] = "metadata refreshed but name update could not be saved"
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleRefreshSearch searches MusicBrainz and Discogs by name for disambiguation.
// POST /api/v1/artists/{id}/refresh/search
func (r *Router) handleRefreshSearch(w http.ResponseWriter, req *http.Request) {
	artistID := req.PathValue("id")

	// Both fields come off ONE body read: a JSON body is not re-readable, so
	// two separate extractFormOrJSONField calls would silently lose clear_ids.
	fields := extractFormOrJSONFields(req, "query", "clear_ids")
	query := fields["query"]
	if query == "" {
		writeError(w, req, http.StatusBadRequest, "search query is required")
		return
	}
	// Carry the destructive intent from the re-identify entry point onto each
	// candidate card, so the wipe travels with the flow rather than being
	// persisted up front (#2714).
	clearIDs := fields["clear_ids"] == "true"

	// Search only MusicBrainz and Discogs for disambiguation
	linkProviders := []provider.ProviderName{
		provider.NameMusicBrainz,
		provider.NameDiscogs,
	}

	results, statuses, err := r.orchestrator.SearchForLinking(req.Context(), query, linkProviders)
	if err != nil {
		r.logger.Error("search failed", "error", err)
		writeError(w, req, http.StatusInternalServerError, "search failed")
		return
	}

	// Fetch artist to get filesystem path for album comparison. A load failure
	// leaves the zero AlbumSet, which is EvidenceUnknown -- correct, because
	// nobody looked. Unlike the other four surfaces this one does NOT 404 on an
	// unknown ID, so the artist may legitimately be absent here.
	var localAlbums artist.AlbumSet
	if a, err := r.artistService.GetByID(req.Context(), artistID); err == nil {
		localAlbums = r.localAlbumSet(req.Context(), a)
	} else {
		// Pass the error VALUE, not its string form: slog renders it, and no
		// raw error text ever enters a string-building path this way.
		r.logger.Warn("album evidence unknown: could not load artist for album comparison",
			"artist_id", artistID, "reason", err)
	}

	candidates := r.enrichWithAlbumComparison(req.Context(), query, results, localAlbums)
	failedProviders := collectFailedProviderDisplayNames(statuses)

	if isHTMXRequest(req) {
		renderTempl(w, req, templates.DisambiguationResults(templates.DisambiguationResultsData{
			ArtistID:        artistID,
			Candidates:      candidates,
			FailedProviders: failedProviders,
			ClearIDs:        clearIDs,
		}))
		return
	}
	resp := map[string]any{"results": candidates}
	if len(failedProviders) > 0 {
		resp["failed_providers"] = failedProviders
	}
	markAlbumsUnavailable(resp, localAlbums)
	writeJSON(w, http.StatusOK, resp)
}

// collectFailedProviderDisplayNames returns the human-readable provider names
// (DisplayName) for any provider whose SearchForLinking status reports an
// error. Returns nil when no providers errored so callers can use a simple
// len() check (and the JSON path can omit the key entirely).
func collectFailedProviderDisplayNames(statuses []provider.ProviderSearchStatus) []string {
	var failed []string
	for _, s := range statuses {
		if s.Errored {
			failed = append(failed, s.Provider.DisplayName())
		}
	}
	return failed
}

// discardRepudiatedProviderIDs clears the provider IDs that belonged to the
// entity a re-identify just repudiated, keeping any the current request
// supplied. It reports whether anything actually changed.
//
// WHY THIS EXISTS (#2894). A surviving stale secondary ID does not sit there
// inert: it STEERS the follow-up refresh. FetchProviderResult prefers a
// provider-specific ID over the MBID, so the refresh re-fetches the artist the
// operator just declared wrong. AudioDB is the #2 origin provider and Discogs
// outranks MusicBrainz for biography, and MusicBrainz is LAST in the priority
// order for origin, biography and years_active alike -- so the repudiated
// entity's values win before the corrected identity is ever consulted. The
// result is #2894 exactly: identity corrected, refresh visibly succeeds, wrong
// metadata survives, nothing on screen says so.
//
// WHY THIS NARROWS #2714/#2725, deliberately. Those issues reasoned about
// repairing a bad MusicBrainz ID on an artist that was otherwise correctly
// identified, and there "a wrong MBID does not make a correct Discogs ID
// wrong" holds. A re-identify is a different claim: the operator is declaring
// the whole ENTITY wrong, and these IDs were harvested from that entity's own
// provider responses in the first place (EnrichProviderIDs). They are not
// independent facts about a correct artist; they are more of the same wrong
// answer. Every NON-re-identify path keeps the old preserve-everything
// behavior -- callers gate on their own re-identify intent before calling.
//
// WHY IT IS A FUNCTION rather than inline in the handler. There are two
// re-identify entry points -- the single-artist disambiguation
// (handleRefreshLink) and the bulk wizard (handleReIdentifyWizardAccept) --
// and the first cut of this fix lived inside handleRefreshLink, which scoped
// it by HANDLER when the correct scope is INTENT. The wizard therefore kept
// every stale ID and reproduced #2894 in full on the common bulk path. One
// implementation, both callers.
//
// CALL IT ONLY IMMEDIATELY BEFORE A REFRESH THAT WILL RUN. The clear is only
// safe when the refresh that re-derives the IDs actually follows it. Callers
// must have already passed their artist-level lock gate, because a locked
// artist skips the refresh entirely and would keep the clear permanently. See
// the recovery asymmetry below for why that is not survivable.
//
// RECOVERY IS NOT SYMMETRIC, and this is the part a future reader must not
// re-derive from scratch:
//
//   - Discogs, Deezer, Wikidata, AllMusic and Spotify are re-derived from URLs
//     in the corrected MusicBrainz response by EnrichProviderIDs, so for those
//     this is a round trip.
//   - AudioDB is NOT. EnrichProviderIDs has no AudioDB branch. It comes back
//     only opportunistically -- when a refresh queries AudioDB for a field it
//     does not populate, applyField falls through to the ID merge and
//     modeFillEmpty accepts it. Until that happens the ID stays cleared and
//     AudioDB is resolved by MBID instead.
//
// That asymmetry is accepted rather than worked around: a wrong AudioDB ID is
// precisely what poisons origin, so clearing it is the point, and when it does
// return it returns resolved from the CORRECTED identity. An ID the operator
// can re-link beats metadata that is silently wrong.
//
// Per-field locks are untouched: this is provider-ID plumbing, not a field
// merge, and ApplyMetadata still reads a.LockedFields on every call.
func discardRepudiatedProviderIDs(a *artist.Artist, keepDiscogsID string) bool {
	changed := a.AudioDBID != "" || a.WikidataID != "" || a.DeezerID != "" || a.SpotifyID != ""
	a.AudioDBID = ""
	a.WikidataID = ""
	a.DeezerID = ""
	a.SpotifyID = ""
	// Only when this request did not supply a replacement: a Discogs pick is
	// the operator's own choice for THIS identity and must survive.
	if keepDiscogsID == "" {
		changed = changed || a.DiscogsID != ""
		a.DiscogsID = ""
	}
	return changed
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
		ClearIDs  string `json:"clear_ids"`
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
		body.ClearIDs = req.FormValue("clear_ids")
	}

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		writeError(w, req, http.StatusNotFound, "artist not found")
		return
	}

	// The destructive half of re-identify (#2714). Two conditions must BOTH
	// hold, and both are positive assertions -- an allow-list, never a negated
	// safe-list, because this decides a DELETE:
	//
	//  1. the request carries the re-identify intent (clear_ids == "true"),
	//     forwarded from handleReidentify through the search form and the
	//     candidate card; and
	//  2. a replacement identity is actually present in THIS request -- either
	//     a MusicBrainz ID or a Discogs ID.
	//
	// Condition 2 is what stops the MBID being stranded. The old code cleared
	// in handleReidentify, before any candidate existed, so an abandoned flow
	// left nothing behind. Here the MBID discard and its replacement are applied
	// to the same in-memory artist and committed by the single Update below, so
	// the row never observes an identity-less state -- and if the operator
	// walks away, no write happened at all.
	//
	// SCOPE: this argument covers the MusicBrainz IDENTITY only. It does NOT
	// extend to the secondary provider IDs, which a re-identify also discards
	// (#2894) but which are deliberately NOT touched in this write. They are
	// cleared further down, after the lock gate and immediately before the
	// refresh, precisely because this reasoning does not protect them: an
	// artist can survive with the right MBID and still lose an AudioDB ID that
	// nothing re-derives. See discardRepudiatedProviderIDs and its call site.
	//
	// Anything unrecognized (a missing field, "1", "TRUE", an empty body, a
	// body with neither ID) falls through to the non-destructive path and
	// preserves the existing IDs, which is the safe direction for an ambiguous
	// signal.
	reidentify := body.ClearIDs == "true" && (body.MBID != "" || body.DiscogsID != "")

	// The discard is keyed to what the replacement actually supplies: an ID
	// carried by THIS request is kept, every ID belonging to the repudiated
	// entity goes.
	//
	// A MusicBrainz pick supplies its own replacement MBID, which the
	// overwrite below applies. A Discogs pick supplies none, so the repudiated
	// MBID has to be cleared explicitly here -- otherwise the operator says
	// "this is someone else", picks a Discogs candidate, and the artist keeps
	// the known-wrong MusicBrainz ID alongside its new Discogs one.
	// Respect a user pin on the identity fields this request would write, BEFORE
	// any of them are mutated. The operator is choosing a new identity here,
	// which is what a lock on the identity field says not to do -- so refuse
	// visibly rather than let the persist chokepoint revert it behind a 200.
	//
	// musicbrainz_id is guarded when the request REPLACES it (body.MBID) or
	// REPUDIATES it (the reidentify discard below). The discard carries no body
	// value, so keying the check on body.MBID alone would skip the field on
	// exactly the path that clears it.
	writesMBID := body.MBID != "" || (reidentify && a.MusicBrainzID != "")
	var mbidField artist.FieldName
	if writesMBID {
		mbidField = artist.FieldMusicBrainzID
	}
	if r.refuseLockedProviderIDs(w, a, mbidField,
		providerIDFieldIf(body.DiscogsID, artist.FieldDiscogsID)) {
		return
	}

	if reidentify && body.MBID == "" {
		r.logger.Info("re-identify: discarding repudiated MusicBrainz identity",
			slog.String("artist_id", a.ID),
			slog.String("previous_mbid", a.MusicBrainzID),
			slog.String("replacement_discogs_id", body.DiscogsID),
		)
		a.MusicBrainzID = ""
	}

	// Store the selected ID(s). This handler is only invoked from the
	// disambiguation UI where the user explicitly chose an identity, so
	// we overwrite unconditionally (supports re-identification).
	if body.MBID != "" {
		if reidentify {
			r.logger.Info("re-identify: replacing MusicBrainz identity",
				slog.String("artist_id", a.ID),
				slog.String("previous_mbid", a.MusicBrainzID),
				slog.String("replacement_mbid", body.MBID),
			)
		}
		a.MusicBrainzID = body.MBID
	}
	if body.DiscogsID != "" {
		a.DiscogsID = body.DiscogsID
	}

	if err := r.artistService.Update(req.Context(), a); err != nil {
		r.logger.Warn("failed to store provider ID",
			"artist_id", a.ID,
			"error", err,
		)
		writeError(w, req, http.StatusInternalServerError, "failed to store provider ID")
		return
	}

	r.logger.Debug("linked provider IDs after disambiguation",
		slog.String("artist_id", a.ID),
		slog.String("artist_name", a.Name),
		slog.String("mbid", a.MusicBrainzID),
		slog.String("discogs_id", a.DiscogsID),
		slog.String("source", body.Source),
		slog.Bool("reidentify", reidentify),
	)

	// Artist-level lock gate, placed AFTER the provider-ID persist above: the
	// user explicitly choosing an identity is a manual edit, which the lock
	// allows. The automated fetch/merge that normally follows is the exact
	// "automated change" the lock promises to skip.
	//
	// The gate sitting after the Update is why this branch still publishes:
	// the provider ID on the row GENUINELY CHANGED, so invalidating the health
	// cache alone would leave nothing to recompute it and rules keyed on
	// local state (MBID presence, for one) would keep reporting the
	// pre-link violation until some unrelated trigger fired. Publishing
	// event.ArtistUpdated is what drives that recompute, via the
	// HealthSubscriber, which evaluates and scores. Evaluation applies no
	// automated change to the artist: it calls engine.Evaluate and writes
	// only health/violation bookkeeping rows, never a fixer. That is the
	// lock's actual promise, and it is exactly what autoLinkAndRefresh
	// (handlers_identify.go) already does on its own locked-skip path for the
	// deezer / discogs / audiodb / bulk-identify link handlers.
	//
	// runRulesAfterRefresh is deliberately NOT called here: it delegates to
	// Pipeline.RunForArtist, which returns immediately for a locked artist by
	// design (rule.runForArtistFiltered's IsExcluded/Locked guard), so the
	// call would be a no-op that only implied otherwise.
	if a.Locked {
		if r.eventBus != nil {
			r.eventBus.Publish(event.Event{
				Type: event.ArtistUpdated,
				Data: map[string]any{"artist_id": a.ID},
			})
		}
		r.InvalidateHealthCache()
		r.respondRefreshSkippedLocked(w, req, a)
		return
	}

	// Discard the repudiated entity's provider IDs, then refresh (#2894).
	//
	// ORDER IS THE WHOLE SAFETY ARGUMENT, so do not move this above the Update
	// or the lock gate. An earlier cut cleared the IDs in the SAME write that
	// stored the new identity, which committed the clear before either the lock
	// gate or the refresh had run, and produced two ways to destroy operator
	// data with no undo:
	//
	//   - a LOCKED artist returned at the gate above, so the refresh that
	//     re-derives the IDs never ran and the clear was permanent; and
	//   - a FAILED refresh (provider outage, rate limit, network) returned 500
	//     with the clear already committed, leaving the operator FEWER provider
	//     IDs and the SAME wrong metadata -- strictly worse than the bug.
	//
	// Clearing here makes both impossible by construction rather than by
	// remembering to unwind: the lock gate has already returned, and the clear
	// is only ever persisted by executeRefresh's own successful write. If the
	// refresh fails below, nothing was saved and the artist keeps every ID it
	// had. That is why the in-memory mutation and the fetch are adjacent, and
	// why there is no separate Update call between them.
	previousAudioDBID, previousDeezerID := a.AudioDBID, a.DeezerID
	if reidentify && discardRepudiatedProviderIDs(a, body.DiscogsID) {
		// Read from the snapshot above: by this point the fields are empty, so
		// logging them directly would record nothing. AudioDB is named first
		// because it is the one that does not reliably come back.
		r.logger.Info("re-identify: discarding the repudiated entity's provider IDs",
			slog.String("artist_id", a.ID),
			slog.String("previous_audiodb_id", previousAudioDBID),
			slog.String("previous_deezer_id", previousDeezerID),
		)
	}

	// Now run the full refresh with the linked MBID
	result, err := r.executeRefresh(req, a)
	if err != nil {
		writeError(w, req, http.StatusInternalServerError, "metadata refresh failed")
		return
	}

	// Re-identify is an explicit "this artist is someone else" action, so
	// update the display name and sort name from provider data. The artist
	// is only mutated after a successful DB update to avoid the UI or NFO
	// showing a name that was never persisted.
	nameUpdateFailed := r.applyProviderName(req.Context(), a, result.Metadata)

	if r.eventBus != nil {
		r.eventBus.Publish(event.Event{
			Type: event.ArtistUpdated,
			Data: map[string]any{"artist_id": a.ID},
		})
	}

	// Linking a provider ID and refreshing changes health-relevant fields.
	r.InvalidateHealthCache()

	// Auto-resolve rule violations after re-identification so the artist's
	// health score reflects the new provider data immediately.
	r.runRulesAfterRefresh(req.Context(), a)

	if isHTMXRequest(req) {
		if nameUpdateFailed {
			setSyncWarningTrigger(w, []string{"re-identify completed but name update could not be saved"})
		}
		r.renderRefreshWithOOB(w, req, a.ID, result.Sources)
		return
	}
	resp := map[string]any{
		"status":  "linked_and_refreshed",
		"sources": result.Sources,
	}
	if nameUpdateFailed {
		resp["warning"] = "re-identify completed but name update could not be saved"
	}
	writeJSON(w, http.StatusOK, resp)
}

// refreshArtistForBulk performs the bulk-action equivalent of
// handleArtistRefresh for a single artist (#2283) and reports the outcome as a
// bulkOutcome. It lives here, beside executeRefreshCtx and the follow-up
// helpers it reuses, so the two refresh paths stay visibly in sync.
//
// Differences from the interactive handler, all forced by the absence of a
// user to answer questions:
//
//   - No MusicBrainz ID: the handler answers with the disambiguation search UI.
//     Bulk has nowhere to render it and must not guess a link, so the artist is
//     counted as Skipped and the run continues.
//   - A failed provider-name write is a warning in the handler (HX-Trigger
//     toast) on an otherwise successful refresh. Here it is logged and the
//     artist still counts as Succeeded, because the metadata refresh itself
//     committed.
//   - InvalidateHealthCache is NOT called per artist; runBulkAction invalidates
//     once when the whole run finishes.
//
// The lock/exclusion gate lives in applyBulkAction, not here, so every
// pipeline-touching bulk action shares one statement of that rule.
func (r *Router) refreshArtistForBulk(ctx context.Context, a *artist.Artist) bulkOutcome {
	if a.MusicBrainzID == "" {
		return bulkOutcomeSkipped
	}

	// executeRefreshCtx re-runs injectMetadataLanguages on this context even
	// though runBulkAction already injected once. The second call is a
	// cheap idempotent overwrite of the same values; forking the shared
	// helper to skip it would put the two refresh paths out of sync for no
	// meaningful saving.
	result, err := r.executeRefreshCtx(ctx, a)
	if err != nil {
		// executeRefreshCtx already logged the underlying cause.
		return bulkOutcomeFailed
	}

	// result.Metadata carries any language-promoted name; applyProviderName
	// is a no-op on nil metadata, which is the correct behavior when the
	// providers returned nothing to promote.
	if r.applyProviderName(ctx, a, result.Metadata) {
		r.logger.Warn("bulk refresh: provider name update failed", "artist_id", a.ID)
	}

	if r.eventBus != nil {
		r.eventBus.Publish(event.Event{
			Type: event.ArtistUpdated,
			Data: map[string]any{"artist_id": a.ID},
		})
	}

	r.runRulesAfterRefresh(ctx, a)

	return bulkOutcomeSucceeded
}

// executeRefresh runs the orchestrator's FetchMetadata and applies results to the artist.
// It is a thin wrapper around executeRefreshCtx that extracts the context from the request.
func (r *Router) executeRefresh(req *http.Request, a *artist.Artist) (*provider.FetchResult, error) {
	return r.executeRefreshCtx(req.Context(), a)
}

// executeRefreshCtx runs the orchestrator's FetchMetadata and applies results to the artist.
// It accepts a bare context so it can be called from both HTTP handlers and background goroutines.
// When a user ID is present in the context, the user's metadata language preferences
// are loaded and injected into the context for use by individual providers.
func (r *Router) executeRefreshCtx(ctx context.Context, a *artist.Artist) (*provider.FetchResult, error) {
	ctx = r.injectMetadataLanguages(ctx)
	result, err := r.orchestrator.FetchMetadata(ctx, a.MusicBrainzID, a.Name, a.ProviderIDMap())
	if err != nil {
		r.logger.Error("metadata refresh failed",
			"artist_id", a.ID,
			"error", err)
		return nil, err
	}
	if result == nil {
		return nil, fmt.Errorf("fetch metadata returned nil result for %s", a.ID)
	}

	// Split-on-ingest: if the local artist Name is the concatenation
	// "Canonical (disambiguation)" and the provider confirms both halves,
	// promote the parenthesised suffix into Disambiguation before merging
	// provider metadata. This runs before ApplyMetadata so the downstream
	// merge sees the split values and does not re-combine them.
	if artist.SplitNameDisambiguation(a, result.Metadata) {
		r.logger.Info("promoted parenthesised suffix into disambiguation",
			"artist_id", a.ID,
			"name", a.Name,
			"disambiguation", a.Disambiguation)
	}

	// Apply fetched metadata to the artist using the shared merge helper.
	// a.LockedFields is deliberately not passed: ApplyMetadata reads the
	// artist's per-field locks off the artist itself on every path, so passing
	// them here would be a no-op duplicate and would re-teach the "each caller
	// must remember" pattern that caused issue #2749.
	if u := artist.FetchResultToUpdate(result); u != nil {
		artist.ApplyMetadata(a, u, artist.OverwriteAttempted, artist.MergeOptions{
			AttemptedFields:   result.AttemptedFields,
			PopulatedFields:   result.PopulatedFields,
			FilterDatesByType: true,
			Sources:           result.Sources,
		})
	}

	// Shield write phase from cancellation to prevent half-applied metadata.
	// FetchMetadata above is cancelable, but once we have the data, the
	// Update/Publish/Upsert sequence must run to completion.
	writeCtx := context.WithoutCancel(ctx)

	// Capture MusicBrainz-sourced field values as snapshots for contribution diffs.
	if result.Metadata != nil {
		if snaps := musicbrainz.ExtractMBFieldValues(result.Metadata, result.Sources); len(snaps) > 0 {
			if err := r.artistService.UpsertMBSnapshots(writeCtx, a.ID, snaps); err != nil {
				r.logger.Warn("failed to upsert MB snapshots",
					"artist_id", a.ID,
					"error", err)
			}
		}
	}

	if err := r.artistService.Update(writeCtx, a); err != nil {
		r.logger.Error("saving refreshed metadata failed",
			"artist_id", a.ID,
			"error", err)
		return nil, err
	}

	r.publisher.PublishMetadata(writeCtx, a)

	rule.UpdateProviderFetchTimestamps(writeCtx, r.artistService, a.ID, result.AttemptedProviders, r.logger)

	r.applyMemberRefresh(writeCtx, a.ID, result, a.LockedFields)

	return result, nil
}

// applyMemberRefresh upserts provider-returned members for an artist when the
// provider both attempted the "members" field and either returned a non-empty
// list or the result carries MembersAuthoritative=true (indicating the provider
// asserted a complete roster that happens to be empty -- i.e., the artist has
// no members -- so existing rows should be cleared).
//
// An empty list without MembersAuthoritative is treated as incomplete data
// (MusicBrainz relation data is often sparse) and leaves existing members
// untouched. Existing members are also left untouched when the provider did
// not attempt the field at all, when metadata is nil, or when the field is
// locked by the user.
func (r *Router) applyMemberRefresh(ctx context.Context, artistID string, result *provider.FetchResult, locked []string) {
	if result.Metadata == nil {
		return
	}
	for _, f := range locked {
		if strings.EqualFold(f, "members") {
			return
		}
	}
	if slices.Contains(result.AttemptedFields, "members") && (len(result.Metadata.Members) > 0 || result.MembersAuthoritative) {
		members := convertProviderMembers(artistID, result.Metadata.Members)
		if err := r.artistService.UpsertMembers(ctx, artistID, members); err != nil {
			// A failed member upsert is a real persistence defect: the core
			// metadata was already committed, but the member roster may now be
			// stale. Log at Error (not Warn) so monitoring surfaces it.
			r.logger.Error("upserting members after refresh",
				"artist_id", artistID,
				"error", err)
		}
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

	oobData := templates.RefreshOOBData{
		Artist:         *a,
		Members:        members,
		FieldProviders: fieldProviders,
		ProfileName:    r.getActiveProfileName(req.Context()),
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

// handleReidentify returns the disambiguation form so the user can link (or
// re-link) a MusicBrainz or Discogs entry. When clear_ids=true is passed, this
// is the destructive "Re-identify" flow; without it, the non-destructive
// "Identify" flow.
//
// This handler PERSISTS NOTHING (#2714). It used to wipe the artist's provider
// IDs and commit that wipe before the operator had chosen a replacement, so
// abandoning the flow -- closing the tab, a failed lookup, no acceptable
// candidate -- left the artist with no identity at all, strictly worse than
// where it started and with no path back short of a re-scan. The wipe is now
// deferred to handleRefreshLink, where a replacement is in hand and the discard
// plus the replacement commit as one Update. Nothing between here and there
// touches the database, so abandonment at any point is a no-op.
//
// The clear_ids intent travels with the flow instead of with the row: it is
// echoed into the search form as a hidden field, carried through the search
// response onto each candidate card's hx-vals, and read back by
// handleRefreshLink. That keeps the intent request-scoped, so a second operator
// (or a second tab) doing a plain non-destructive Identify on the same artist
// cannot inherit this one's destructive intent.
// POST /api/v1/artists/{id}/reidentify
func (r *Router) handleReidentify(w http.ResponseWriter, req *http.Request) {
	artistID := req.PathValue("id")

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		writeError(w, req, http.StatusNotFound, "artist not found")
		return
	}

	clearIDs := req.FormValue("clear_ids") == "true"

	// Log the action for the audit trail. "requested", not "cleared": no write
	// happens on this path.
	r.logger.Info("re-identify requested",
		slog.String("artist_id", a.ID),
		slog.String("artist_name", a.Name),
		slog.String("previous_mbid", a.MusicBrainzID),
		slog.Bool("clear_ids", clearIDs),
	)

	if isHTMXRequest(req) {
		renderTempl(w, req, templates.RefreshDisambiguationForm(a.ID, a.Name, clearIDs))
		return
	}

	msg := "Search to find and link the correct artist."
	if clearIDs {
		msg = "Existing provider IDs are kept until you choose a replacement. " + msg
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "disambiguation_required",
		"artist":  a.Name,
		"message": msg,
	})
}

// albumEvidenceBudget caps how many candidates receive an album-overlap
// comparison in one disambiguation request.
//
// The cap is a LATENCY budget, not a correctness knob. Each comparison is a
// live GetReleaseGroups call: MusicBrainz is limited to one request per second
// through a shared limiter, and the fetch paginates sequentially in pages of
// 100 up to 500 release groups. So every additional candidate costs at least a
// second of wall-clock, and a prolific artist costs a second per page, all
// inside a synchronous HTTP request. Scoring a typical ~20-candidate result
// set would run 20-60s and invite the severed-idle-connection failure that
// renders the panel as an indistinguishable "no matches" (#2818).
//
// Nothing coalesces these fetches on this path. The release-group coalescer in
// internal/rule is keyed to the rule engine's per-artist EvaluationContext and
// is not reachable from here, so the budget is the only thing bounding them.
const albumEvidenceBudget = 8

// rankScore is the network-free ranking signal for a disambiguation candidate:
// how well its name matches what the operator searched for, blended with the
// provider's own confidence.
//
// Name similarity dominates (weighted 2:1) because it answers the operator's
// actual question -- is this the artist I typed? -- while the provider score
// reflects that provider's internal ranking and is not comparable across
// providers. Both inputs are already on ArtistSearchResult and cost nothing to
// compute, which is what lets every candidate be ranked before any of them
// spends the album-evidence budget.
//
// SortName is scored alongside Name and the better of the two wins, so an
// artist listed as "Beatles, The" is not penalized against a query of "The
// Beatles". Aliases would need a separate fetch and are out of scope here.
func rankScore(query string, res provider.ArtistSearchResult) int {
	// Name and sort-name are the two the view model carries, so they are the
	// two this can measure directly. Aliases are NOT missing by oversight:
	// ArtistSearchResult has no alias field, and the alias signal reaches this
	// function through res.Score instead -- the MusicBrainz adapter scores the
	// query against every alias and sort-name at search time and keeps the
	// best (#2820). So an alias-only match arrives here as a raised provider
	// score rather than as a raised name similarity.
	name := provider.BestNameSimilarity(query, res.Name, res.SortName)
	return name*2 + res.Score
}

// enrichWithAlbumComparison wraps search results in DisambiguationCandidate,
// ranks them, and spends the album-evidence budget on the strongest.
//
// Ordering matters and is the fix for #2885. Previously the first three
// candidates in provider order were enriched, which meant the three that got
// the strongest available evidence were chosen arbitrarily -- results are
// appended per provider with no global sort (#2819), so a correct match could
// sit outside the window and render with no album evidence at all while worse
// candidates carried badges. The pass now:
//
//  1. ranks every candidate by the free signals (name similarity + provider
//     score), so position reflects match quality rather than provider order;
//  2. spends the album-evidence budget on the top albumEvidenceBudget rows;
//  3. re-sorts so a measured album overlap outranks the name-only estimate,
//     since overlap compares what the operator actually has on disk and is the
//     strongest signal available;
//  4. marks every candidate that ends without a comparison, for ANY reason --
//     ranked below the budget, no MusicBrainz ID to look up (all Discogs
//     results), a failed fetch, or an early return before any fetch runs --
//     so such a row reads as "not compared" rather than as a candidate that
//     was measured and scored zero.
//
// Only EvidenceFound proceeds to step 2. An Unknown set would compare against
// no titles and stamp every candidate with a 0% match badge -- a false claim.
func (r *Router) enrichWithAlbumComparison(ctx context.Context, query string, results []provider.ArtistSearchResult, local artist.AlbumSet) []templates.DisambiguationCandidate {
	candidates := make([]templates.DisambiguationCandidate, len(results))
	for i := range results {
		candidates[i].Result = results[i]
		// Surface the "could not look" state per candidate so the template can
		// say so rather than silently omitting the badge, which is what a
		// genuinely empty artist also does.
		candidates[i].AlbumsUnavailable = local.Evidence == artist.EvidenceUnknown
	}

	// Rank before anything else. Even when album evidence is unavailable this
	// leaves the operator with candidates ordered by match quality instead of
	// grouped by provider, which is a strict improvement on its own (#2819).
	sortCandidatesByRank(query, candidates)

	// Mark every candidate as not-compared UP FRONT, then clear the flag as
	// each one is actually compared. The inverse -- marking on the way out --
	// has to be repeated at every early return below, and missing one leaves
	// a row rendering no badge at all, which an operator reads as a measured
	// 0%. Defaulting to "not compared" makes the safe state the one you get
	// by doing nothing, so a future early return cannot reintroduce the bug.
	//
	// AlbumsUnavailable stays separate and wins in the template: it says the
	// local folder could not be read, which is a fault worth surfacing, while
	// this flag only says no comparison was made.
	// Skip a candidate already flagged AlbumsUnavailable: that says the local
	// folder could not be read, which is the more specific and more actionable
	// statement, and the template checks it first. Setting both would leave
	// two contradictory-looking flags on the same row for any API client or
	// future UI reading the struct rather than the rendered badge.
	for i := range candidates {
		if !candidates[i].AlbumsUnavailable {
			candidates[i].AlbumsNotScored = true
		}
	}

	if local.Evidence != artist.EvidenceFound || r.providerRegistry == nil {
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

	// Spend the budget on the highest-ranked candidates. Attempts are counted
	// rather than successes so a provider erroring on every call cannot walk
	// the whole candidate list one failed request at a time.
	attempted := 0
	for i := range candidates {
		res := candidates[i].Result
		// Each candidate below is already flagged not-compared by default, so
		// every `continue` here simply leaves that flag in place. Only a
		// successful comparison clears it.
		if res.MusicBrainzID == "" {
			// No MBID, so there is nothing to look up. The check stays AHEAD of
			// the budget so an unscoreable candidate never consumes it. Discogs
			// results never carry an MBID, and Discogs is one of the two
			// providers this screen searches, so this is the largest population
			// of not-compared rows.
			continue
		}
		if attempted >= albumEvidenceBudget {
			continue
		}

		attempted++

		groups, err := fetcher.GetReleaseGroups(ctx, res.MusicBrainzID)
		if err != nil {
			// The budget was consumed and no comparison was produced, so the
			// candidate keeps its not-compared flag.
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

		// CompareAlbumSet delegates the arithmetic to CompareAlbums and carries
		// the local evidence state through, so the percentage cannot be read as
		// a finding when there was none.
		ev := artist.CompareAlbumSet(local, remoteTitles)
		comp := ev.AlbumComparison
		candidates[i].AlbumComparison = &comp
		// Compared: clear the default flag so the row shows its match badge.
		candidates[i].AlbumsNotScored = false
	}

	// Re-sort now that measured evidence exists: a candidate with real album
	// overlap outranks one carrying only a name-similarity estimate.
	sortCandidatesByRank(query, candidates)

	return candidates
}

// sortCandidatesByRank orders candidates best-first, preferring measured album
// overlap over the network-free estimate.
//
// A candidate whose album overlap was actually computed sorts above one that
// was not, because the overlap is evidence about the operator's own library
// rather than a guess about a name. Among scored candidates the higher match
// percentage wins; among unscored ones the rank estimate breaks the tie. A
// candidate scored at 0% overlap deliberately still outranks an unscored one:
// it was measured against the library and found not to match, which is a
// finding, whereas the unscored candidate is simply unknown.
//
// The sort is stable so candidates that tie on every signal keep the order the
// providers returned them in, rather than shuffling between identical requests.
func sortCandidatesByRank(query string, candidates []templates.DisambiguationCandidate) {
	slices.SortStableFunc(candidates, func(a, b templates.DisambiguationCandidate) int {
		aScored := a.HasMeasuredOverlap()
		bScored := b.HasMeasuredOverlap()
		if aScored != bScored {
			if aScored {
				return -1
			}
			return 1
		}
		if aScored {
			if c := cmp.Compare(b.AlbumComparison.MatchPercent, a.AlbumComparison.MatchPercent); c != 0 {
				return c
			}
		}
		return cmp.Compare(rankScore(query, b.Result), rankScore(query, a.Result))
	})
}

// applyProviderName updates the artist's Name and SortName from provider
// metadata when the provider returned a different (e.g. language-promoted)
// name. Returns true if the DB write failed and the caller should warn.
// Uses context.WithoutCancel so the write completes even if the HTTP client
// disconnects.
func (r *Router) applyProviderName(ctx context.Context, a *artist.Artist, meta *provider.ArtistMetadata) bool {
	if meta == nil {
		return false
	}
	newName, newSort := meta.Name, meta.SortName
	// Respect per-field locks: a user pin on Name or SortName must survive
	// a provider refresh. ApplyMetadata skips these intentionally so the
	// user's display name is not clobbered mid-refresh, but applyProviderName
	// runs on a separate path and must enforce the same rule.
	nameLocked := r.artistService.IsFieldLocked(a, artist.FieldArtistName)
	sortLocked := r.artistService.IsFieldLocked(a, artist.FieldSortName)
	if nameLocked {
		newName = ""
	}
	if sortLocked {
		newSort = ""
	}
	nameChanged := (newName != "" && newName != a.Name) ||
		(newSort != "" && newSort != a.SortName)
	if !nameChanged {
		return false
	}

	origName, origSort := a.Name, a.SortName
	if newName != "" {
		a.Name = newName
	}
	if newSort != "" {
		a.SortName = newSort
	}

	writeCtx := context.WithoutCancel(ctx)
	if err := r.artistService.Update(writeCtx, a); err != nil {
		r.logger.Error("updating artist name from provider",
			"artist_id", a.ID,
			"error", err)
		a.Name, a.SortName = origName, origSort
		return true
	}
	r.logger.Info("artist name updated from provider",
		"artist_id", a.ID,
		"old_name", origName,
		"new_name", a.Name)
	r.publisher.PublishMetadata(writeCtx, a)
	return false
}

// runRulesAfterRefresh evaluates and auto-fixes rule violations for a single
// artist after a metadata refresh. Errors are logged but do not propagate to
// the caller because the refresh itself already succeeded and the rule
// evaluation is a best-effort follow-up.
func (r *Router) runRulesAfterRefresh(ctx context.Context, a *artist.Artist) {
	if r.pipeline == nil {
		return
	}

	// Detach from the request-scoped context so client disconnects do not
	// cancel the rule evaluation, then apply a hard deadline to prevent
	// unbounded execution. This matches the pattern used elsewhere in this
	// file (see applyProviderName and executeRefreshCtx).
	ruleCtx := context.WithoutCancel(ctx)
	ruleCtx, cancel := context.WithTimeout(ruleCtx, 30*time.Second)
	defer cancel()

	// Re-fetch the artist so rule evaluation sees the persisted state
	// (the caller may have applied provider names or other changes).
	fresh, err := r.artistService.GetByID(ruleCtx, a.ID)
	if err != nil {
		r.logger.Warn("re-fetching artist for post-refresh rule evaluation",
			slog.String("artist_id", a.ID),
			slog.Any("error", err))
		return
	}

	if _, err := r.pipeline.RunForArtist(ruleCtx, fresh); err != nil {
		r.logger.Warn("auto-evaluating rules after refresh",
			slog.String("artist_id", a.ID),
			slog.Any("error", err))
	}
}

// extractFormOrJSONField reads a named value from either a JSON body or form data.
func extractFormOrJSONField(req *http.Request, name string) string {
	return extractFormOrJSONFields(req, name)[name]
}

// extractFormOrJSONFields reads several named values in ONE pass over the body.
// A JSON request body can only be decoded once, so calling
// extractFormOrJSONField twice on the same request would return "" for the
// second field. Callers that need more than one value must use this.
func extractFormOrJSONFields(req *http.Request, names ...string) map[string]string {
	out := make(map[string]string, len(names))
	if strings.HasPrefix(req.Header.Get("Content-Type"), "application/json") {
		var body map[string]string
		if err := json.NewDecoder(req.Body).Decode(&body); err == nil {
			for _, n := range names {
				out[n] = body[n]
			}
		}
		return out
	}
	for _, n := range names {
		out[n] = req.FormValue(n)
	}
	return out
}
