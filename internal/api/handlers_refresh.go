package api

import (
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
		r.logger.Error("search failed", "error", err)
		writeError(w, req, http.StatusInternalServerError, "search failed")
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

	// Store the selected ID(s). This handler is only invoked from the
	// disambiguation UI where the user explicitly chose an identity, so
	// we overwrite unconditionally (supports re-identification).
	if body.MBID != "" {
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
	)

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

	// Apply fetched metadata to the artist using the shared merge helper.
	if u := artist.FetchResultToUpdate(result); u != nil {
		artist.ApplyMetadata(a, u, artist.OverwriteAttempted, artist.MergeOptions{
			AttemptedFields:   result.AttemptedFields,
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

	r.applyMemberRefresh(writeCtx, a.ID, result)

	return result, nil
}

// applyMemberRefresh upserts provider-returned members for an artist when the
// provider both attempted the "members" field and returned a non-empty list.
// An empty list is treated as incomplete data (MusicBrainz relation data is
// often sparse), not an intentional clear. Existing members are left untouched
// when the provider was not attempted or returned zero members.
func (r *Router) applyMemberRefresh(ctx context.Context, artistID string, result *provider.FetchResult) {
	if result.Metadata == nil {
		return
	}
	if slices.Contains(result.AttemptedFields, "members") && len(result.Metadata.Members) > 0 {
		members := convertProviderMembers(artistID, result.Metadata.Members)
		if err := r.artistService.UpsertMembers(ctx, artistID, members); err != nil {
			r.logger.Warn("upserting members after refresh",
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
// re-link) a MusicBrainz entry. When clear_ids=true is passed, all provider
// IDs are wiped first (the destructive "Re-identify" flow). Without that
// parameter the existing IDs are preserved (the non-destructive "Identify" flow).
// POST /api/v1/artists/{id}/reidentify
func (r *Router) handleReidentify(w http.ResponseWriter, req *http.Request) {
	artistID := req.PathValue("id")

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		writeError(w, req, http.StatusNotFound, "artist not found")
		return
	}

	// Log the action for the audit trail.
	r.logger.Info("re-identifying artist",
		slog.String("artist_id", a.ID),
		slog.String("artist_name", a.Name),
		slog.String("previous_mbid", a.MusicBrainzID),
		slog.Bool("clear_ids", req.FormValue("clear_ids") == "true"),
	)

	// Only clear provider IDs when explicitly requested (the "Re-identify"
	// flow). The "Identify" flow skips this so existing Discogs/Spotify/etc
	// IDs are preserved while the user links a MusicBrainz entry.
	if req.FormValue("clear_ids") == "true" {
		a.MusicBrainzID = ""
		a.AudioDBID = ""
		a.DiscogsID = ""
		a.WikidataID = ""
		a.DeezerID = ""
		a.SpotifyID = ""
		a.AudioDBIDFetchedAt = nil
		a.DiscogsIDFetchedAt = nil
		a.WikidataIDFetchedAt = nil
		a.LastFMFetchedAt = nil

		if err := r.artistService.Update(req.Context(), a); err != nil {
			r.logger.Warn("failed to clear provider IDs",
				"artist_id", a.ID,
				"error", err,
			)
			writeError(w, req, http.StatusInternalServerError, "failed to clear provider IDs")
			return
		}

		// Clearing provider IDs affects health scores (e.g. missing-MBID rules).
		r.InvalidateHealthCache()
	}

	if isHTMXRequest(req) {
		renderTempl(w, req, templates.RefreshDisambiguationForm(a.ID, a.Name))
		return
	}

	msg := "Search to find and link the correct artist."
	if req.FormValue("clear_ids") == "true" {
		msg = "Provider IDs cleared. " + msg
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "disambiguation_required",
		"artist":  a.Name,
		"message": msg,
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
	if strings.HasPrefix(req.Header.Get("Content-Type"), "application/json") {
		var body map[string]string
		if err := json.NewDecoder(req.Body).Decode(&body); err == nil {
			return body[name]
		}
		return ""
	}
	return req.FormValue(name)
}
