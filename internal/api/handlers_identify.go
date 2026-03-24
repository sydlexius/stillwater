package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/rule"
)

// IdentifyProgress tracks the state of a bulk-identify operation.
type IdentifyProgress struct {
	mu          sync.RWMutex
	Status      string              `json:"status"` // "running", "completed", "canceled"
	Total       int                 `json:"total"`
	Processed   int                 `json:"processed"`
	AutoLinked  int                 `json:"auto_linked"`
	Queued      int                 `json:"queued"`
	Unmatched   int                 `json:"unmatched"`
	Failed      int                 `json:"failed"`
	CurrentName string              `json:"current_name"`
	ReviewQueue []IdentifyCandidate `json:"review_queue,omitempty"`
	cancelFn    context.CancelFunc
}

// IdentifyCandidate represents an artist that needs manual review for linking.
type IdentifyCandidate struct {
	ArtistID   string            `json:"artist_id"`
	ArtistName string            `json:"artist_name"`
	ArtistPath string            `json:"artist_path"`
	Tier       string            `json:"tier"` // "connection", "album", "name"
	Candidates []ScoredCandidate `json:"candidates"`
}

// ScoredCandidate wraps a provider search result with confidence scoring.
type ScoredCandidate struct {
	provider.ArtistSearchResult
	AlbumComparison *artist.AlbumComparison `json:"album_comparison,omitempty"`
	Confidence      float64                 `json:"confidence"`
	Reason          string                  `json:"reason"`
}

// identifyOutcome represents the result of processing a single artist.
type identifyOutcome int

const (
	outcomeAutoLinked identifyOutcome = iota
	outcomeQueued
	outcomeUnmatched
	outcomeFailed
	outcomeSkipped
)

// identifyResult holds the outcome and optional review candidate for a single artist.
type identifyResult struct {
	Outcome   identifyOutcome
	Candidate *IdentifyCandidate // only set for outcomeQueued
}

// connectionIndex maps normalized artist names to connection-library entries that
// already have a MusicBrainz ID, enabling fast Tier 1 lookups.
type connectionIndex struct {
	byName map[string][]connEntry // normalized name -> entries
}

// connEntry represents a single connection-library artist with provider IDs.
type connEntry struct {
	Name          string
	MusicBrainzID string
	DiscogsID     string
}

// lookup returns all connection entries matching the given artist name (normalized).
func (idx *connectionIndex) lookup(name string) []connEntry {
	return idx.byName[strings.ToLower(strings.TrimSpace(name))]
}

// handleBulkIdentify starts a bulk identification job for unidentified artists.
// Rejects concurrent starts with 409 Conflict (same pattern as fix-all).
// POST /api/v1/artists/bulk-identify
func (r *Router) handleBulkIdentify(w http.ResponseWriter, req *http.Request) {
	if r.artistService == nil {
		writeError(w, req, http.StatusServiceUnavailable, "artist service not configured")
		return
	}

	// Parse optional library_id filter before claiming the slot.
	var body struct {
		LibraryID string `json:"library_id"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}

	// Atomic check-and-set: reject if already running, otherwise claim the slot.
	progress := &IdentifyProgress{Status: "running"}
	r.identifyMu.Lock()
	if r.identifyProgress != nil {
		r.identifyProgress.mu.RLock()
		running := r.identifyProgress.Status == "running"
		r.identifyProgress.mu.RUnlock()
		if running {
			r.identifyMu.Unlock()
			writeJSON(w, http.StatusConflict, map[string]any{
				"status":  "running",
				"message": "bulk identify already in progress",
			})
			return
		}
	}
	r.identifyProgress = progress
	r.identifyMu.Unlock()

	// releaseProgress clears the slot if this request still owns it.
	releaseProgress := func() {
		r.identifyMu.Lock()
		if r.identifyProgress == progress {
			r.identifyProgress = nil
		}
		r.identifyMu.Unlock()
	}

	// Page through all unidentified, non-excluded artists.
	var allArtists []artist.Artist
	page := 1
	const pageSize = 200
	for {
		params := artist.ListParams{
			Page:     page,
			PageSize: pageSize,
			Filter:   "missing_mbid",
			Sort:     "name",
			Order:    "asc",
		}
		if body.LibraryID != "" {
			params.LibraryID = body.LibraryID
		}

		artists, total, err := r.artistService.List(req.Context(), params)
		if err != nil {
			releaseProgress()
			r.logger.Error("listing unidentified artists", "error", err)
			writeError(w, req, http.StatusInternalServerError, "failed to list artists")
			return
		}

		// Filter out excluded artists in-memory (the missing_mbid filter
		// does not exclude them by default).
		for i := range artists {
			if !artists[i].IsExcluded {
				allArtists = append(allArtists, artists[i])
			}
		}

		if page*pageSize >= total {
			break
		}
		page++
	}

	if len(allArtists) == 0 {
		releaseProgress()
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "completed",
			"message": "no unidentified artists found",
			"total":   0,
		})
		return
	}

	progress.mu.Lock()
	progress.Total = len(allArtists)
	progress.mu.Unlock()

	// Create cancellable context before launching the goroutine so cancelFn
	// is always set when status is "running" (prevents nil panic in cancel handler).
	// The cancel function is stored on progress.cancelFn and called by both the
	// background goroutine (defer) and the cancel handler.
	bgCtx := context.WithoutCancel(req.Context())
	cancelCtx, cancel := context.WithCancel(bgCtx) //nolint:gosec // cancel stored in progress.cancelFn, deferred in goroutine
	progress.cancelFn = cancel

	r.runBulkIdentify(cancelCtx, allArtists, progress)

	writeJSON(w, http.StatusAccepted, map[string]any{
		"status": "running",
		"total":  len(allArtists),
	})
}

// handleBulkIdentifyProgress returns the current state of the bulk identify job.
// GET /api/v1/artists/bulk-identify
func (r *Router) handleBulkIdentifyProgress(w http.ResponseWriter, _ *http.Request) {
	r.identifyMu.RLock()
	progress := r.identifyProgress
	r.identifyMu.RUnlock()

	if progress == nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "idle"})
		return
	}

	progress.mu.RLock()
	// Deep-copy the review queue under the lock to prevent a data race:
	// the background goroutine may append to ReviewQueue after the lock is
	// released but before JSON encoding reads the slice contents.
	rq := make([]IdentifyCandidate, len(progress.ReviewQueue))
	copy(rq, progress.ReviewQueue)
	resp := map[string]any{
		"status":       progress.Status,
		"total":        progress.Total,
		"processed":    progress.Processed,
		"auto_linked":  progress.AutoLinked,
		"queued":       progress.Queued,
		"unmatched":    progress.Unmatched,
		"failed":       progress.Failed,
		"current_name": progress.CurrentName,
		"review_queue": rq,
	}
	progress.mu.RUnlock()

	writeJSON(w, http.StatusOK, resp)
}

// handleBulkIdentifyCancel cancels a running bulk identify job.
// DELETE /api/v1/artists/bulk-identify
func (r *Router) handleBulkIdentifyCancel(w http.ResponseWriter, _ *http.Request) {
	r.identifyMu.RLock()
	progress := r.identifyProgress
	r.identifyMu.RUnlock()

	if progress == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "idle",
			"message": "no bulk identify running",
		})
		return
	}

	progress.mu.RLock()
	cancel := progress.cancelFn
	running := progress.Status == "running"
	actualStatus := progress.Status
	progress.mu.RUnlock()

	if !running {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  actualStatus,
			"message": "bulk identify already finished",
		})
		return
	}

	if cancel != nil {
		cancel()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "canceling",
		"message": "bulk identify cancellation requested",
	})
}

// handleBulkIdentifyLink links an artist from the review queue to a provider ID
// and runs a full metadata refresh.
// POST /api/v1/artists/bulk-identify/link
func (r *Router) handleBulkIdentifyLink(w http.ResponseWriter, req *http.Request) {
	if r.artistService == nil {
		writeError(w, req, http.StatusServiceUnavailable, "artist service not configured")
		return
	}

	var body struct {
		ArtistID  string `json:"artist_id"`
		MBID      string `json:"mbid"`
		DiscogsID string `json:"discogs_id"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeError(w, req, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.ArtistID == "" || body.MBID == "" {
		writeError(w, req, http.StatusBadRequest, "artist_id and mbid are required")
		return
	}

	a, err := r.artistService.GetByID(req.Context(), body.ArtistID)
	if err != nil {
		if errors.Is(err, artist.ErrNotFound) {
			writeError(w, req, http.StatusNotFound, "artist not found")
		} else {
			r.logger.Error("bulk-identify link: failed to get artist", "artist_id", body.ArtistID, "error", err)
			writeError(w, req, http.StatusInternalServerError, "failed to retrieve artist")
		}
		return
	}

	a.MusicBrainzID = body.MBID
	if body.DiscogsID != "" {
		a.DiscogsID = body.DiscogsID
	}

	if err := r.autoLinkAndRefresh(req.Context(), a); err != nil {
		r.logger.Error("bulk-identify link: updating artist", "artist_id", a.ID, "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to update artist")
		return
	}

	// Remove from review queue if progress is still in memory.
	r.identifyMu.RLock()
	progress := r.identifyProgress
	r.identifyMu.RUnlock()
	if progress != nil {
		progress.mu.Lock()
		for i, c := range progress.ReviewQueue {
			if c.ArtistID == body.ArtistID {
				progress.ReviewQueue = append(progress.ReviewQueue[:i], progress.ReviewQueue[i+1:]...)
				break
			}
		}
		progress.mu.Unlock()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "linked",
		"artist_id": a.ID,
		"mbid":      a.MusicBrainzID,
	})
}

// runBulkIdentify processes unidentified artists through the 3-tier pipeline
// in a background goroutine. The caller must set r.identifyProgress and cancelFn
// before calling. The ctx passed in is already cancellable and detached from the
// request lifecycle.
func (r *Router) runBulkIdentify(ctx context.Context, artists []artist.Artist, progress *IdentifyProgress) {
	go func() {
		// Ensure the cancellable context is cleaned up when the goroutine exits,
		// regardless of whether it completed normally or was canceled.
		defer progress.cancelFn()

		// Build connection index for Tier 1.
		connIdx := r.buildConnectionIndex(ctx)

		for i := range artists {
			// Check for cancellation.
			if ctx.Err() != nil {
				progress.mu.Lock()
				progress.Status = "canceled"
				progress.CurrentName = ""
				progress.mu.Unlock()
				return
			}

			a := &artists[i]

			progress.mu.Lock()
			progress.CurrentName = a.Name
			progress.mu.Unlock()

			result := r.identifyArtist(ctx, a, connIdx)

			progress.mu.Lock()
			progress.Processed++
			switch result.Outcome {
			case outcomeAutoLinked:
				progress.AutoLinked++
			case outcomeQueued:
				progress.Queued++
				if result.Candidate != nil {
					progress.ReviewQueue = append(progress.ReviewQueue, *result.Candidate)
				}
			case outcomeUnmatched:
				progress.Unmatched++
			case outcomeFailed:
				progress.Failed++
			case outcomeSkipped:
				// Skipped artists (locked) do not increment any counter besides Processed.
			}
			progress.mu.Unlock()

			// Yield between artists to release the SQLite write lock.
			time.Sleep(10 * time.Millisecond)
		}

		progress.mu.Lock()
		progress.Status = "completed"
		progress.CurrentName = ""
		progress.mu.Unlock()
	}()
}

// identifyArtist runs the 3-tier identification pipeline for a single artist.
func (r *Router) identifyArtist(ctx context.Context, a *artist.Artist, connIdx *connectionIndex) identifyResult {
	// Skip locked artists -- they should not be auto-modified.
	if a.Locked {
		return identifyResult{Outcome: outcomeSkipped}
	}

	// Tier 1: Connection-based matching.
	// Only auto-link when all connection entries for this name agree on the
	// same MBID. If multiple entries exist with different MBIDs, skip auto-link
	// and fall through to Tier 2 for disambiguation.
	if connIdx != nil {
		entries := connIdx.lookup(a.Name)
		if len(entries) > 0 {
			mbid := entries[0].MusicBrainzID
			unanimous := mbid != ""
			discogsID := entries[0].DiscogsID
			for _, entry := range entries[1:] {
				if entry.MusicBrainzID != mbid {
					unanimous = false
					break
				}
			}
			if unanimous {
				a.MusicBrainzID = mbid
				if discogsID != "" {
					a.DiscogsID = discogsID
				}
				if err := r.autoLinkAndRefresh(ctx, a); err != nil {
					return identifyResult{Outcome: outcomeFailed}
				}
				return identifyResult{Outcome: outcomeAutoLinked}
			}
		}
	}

	// Tier 2 and 3 require the orchestrator for provider searches.
	if r.orchestrator == nil {
		return identifyResult{Outcome: outcomeUnmatched}
	}

	// Tier 2: Album comparison (only if artist has local album subdirectories).
	localAlbums := artist.ListLocalAlbums(a.Path)
	if len(localAlbums) > 0 {
		searchName := filepath.Base(a.Path)
		results, err := r.orchestrator.SearchForLinking(ctx, searchName, []provider.ProviderName{provider.NameMusicBrainz})
		if err != nil {
			r.logger.Warn("bulk-identify: Tier 2 search failed",
				"artist", a.Name, "error", err)
			// Fall through to Tier 3 on search failure.
		} else if len(results) > 0 {
			scored := r.enrichAndScoreTier2(ctx, results, localAlbums)
			tier2Result := r.evaluateTier2(ctx, a, scored)
			// evaluateTier2 returns outcomeUnmatched when all candidates < 30%;
			// in that case fall through to Tier 3 instead of returning.
			if tier2Result.Outcome != outcomeUnmatched {
				return tier2Result
			}
		}
	}

	// Tier 3: Name-only search.
	results, err := r.orchestrator.SearchForLinking(ctx, a.Name, []provider.ProviderName{provider.NameMusicBrainz})
	if err != nil {
		r.logger.Warn("bulk-identify: Tier 3 search failed",
			"artist", a.Name, "error", err)
		return identifyResult{Outcome: outcomeFailed}
	}

	if len(results) == 0 {
		return identifyResult{Outcome: outcomeUnmatched}
	}

	// Single high-confidence result: auto-link.
	if len(results) == 1 && results[0].Score >= 90 {
		a.MusicBrainzID = results[0].MusicBrainzID
		if err := r.autoLinkAndRefresh(ctx, a); err != nil {
			return identifyResult{Outcome: outcomeFailed}
		}
		return identifyResult{Outcome: outcomeAutoLinked}
	}

	// Multiple results or low score: review queue.
	scored := make([]ScoredCandidate, len(results))
	for i, res := range results {
		confidence := float64(res.Score) / 200.0 // name-only tops at 0.5
		scored[i] = ScoredCandidate{
			ArtistSearchResult: res,
			Confidence:         confidence,
			Reason:             "name match",
		}
	}

	return identifyResult{
		Outcome: outcomeQueued,
		Candidate: &IdentifyCandidate{
			ArtistID:   a.ID,
			ArtistName: a.Name,
			ArtistPath: a.Path,
			Tier:       "name",
			Candidates: scored,
		},
	}
}

// enrichAndScoreTier2 enriches search results with album comparison data and
// computes confidence scores for Tier 2 candidates.
func (r *Router) enrichAndScoreTier2(ctx context.Context, results []provider.ArtistSearchResult, localAlbums []string) []ScoredCandidate {
	// Reuse the existing album-enrichment logic (same as disambiguation).
	mbProvider := r.providerRegistry.Get(provider.NameMusicBrainz)
	if mbProvider == nil {
		return convertToScoredCandidates(results)
	}
	fetcher, ok := mbProvider.(provider.ReleaseGroupFetcher)
	if !ok {
		return convertToScoredCandidates(results)
	}

	scored := make([]ScoredCandidate, len(results))
	attempted := 0
	for i, res := range results {
		scored[i] = ScoredCandidate{
			ArtistSearchResult: res,
			Reason:             "album comparison",
		}

		if attempted >= 3 || res.MusicBrainzID == "" {
			continue
		}
		attempted++

		groups, err := fetcher.GetReleaseGroups(ctx, res.MusicBrainzID)
		if err != nil {
			r.logger.Warn("bulk-identify: fetching release groups",
				"mbid", res.MusicBrainzID, "error", err)
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

// evaluateTier2 evaluates Tier 2 candidates and returns the appropriate outcome.
func (r *Router) evaluateTier2(ctx context.Context, a *artist.Artist, scored []ScoredCandidate) identifyResult {
	// Count candidates meeting thresholds.
	var above70 []ScoredCandidate
	var above30 []ScoredCandidate
	for _, s := range scored {
		if s.AlbumComparison != nil {
			if s.AlbumComparison.MatchPercent >= 70 {
				above70 = append(above70, s)
			}
			if s.AlbumComparison.MatchPercent >= 30 {
				above30 = append(above30, s)
			}
		}
	}

	// Exactly 1 candidate with >= 70%: auto-link.
	if len(above70) == 1 {
		a.MusicBrainzID = above70[0].MusicBrainzID
		if err := r.autoLinkAndRefresh(ctx, a); err != nil {
			return identifyResult{Outcome: outcomeFailed}
		}
		return identifyResult{Outcome: outcomeAutoLinked}
	}

	// Any candidates with >= 30%: review queue.
	if len(above30) > 0 {
		return identifyResult{
			Outcome: outcomeQueued,
			Candidate: &IdentifyCandidate{
				ArtistID:   a.ID,
				ArtistName: a.Name,
				ArtistPath: a.Path,
				Tier:       "album",
				Candidates: scored,
			},
		}
	}

	// All < 30%: fall through (caller will try Tier 3).
	return identifyResult{Outcome: outcomeUnmatched}
}

// autoLinkAndRefresh sets the MBID on the artist, persists it, runs a full
// metadata refresh, and evaluates health. Returns an error only if the
// initial Update fails (refresh failures are logged but not fatal).
func (r *Router) autoLinkAndRefresh(ctx context.Context, a *artist.Artist) error {
	if err := r.artistService.Update(ctx, a); err != nil {
		r.logger.Warn("bulk-identify: update failed", "artist", a.Name, "error", err)
		return err
	}
	if r.orchestrator != nil {
		if _, err := r.executeRefreshCtx(ctx, a); err != nil {
			r.logger.Warn("bulk-identify: refresh failed after linking",
				"artist", a.Name, "error", err)
		}
	}
	rule.EvaluateAndPersistHealth(ctx, r.ruleEngine, r.artistService, a, r.logger)
	return nil
}

// buildConnectionIndex builds an in-memory index of artists from connection
// libraries (non-manual) that already have MusicBrainz IDs.
func (r *Router) buildConnectionIndex(ctx context.Context) *connectionIndex {
	if r.libraryService == nil {
		return nil
	}

	libs, err := r.libraryService.List(ctx)
	if err != nil {
		r.logger.Warn("bulk-identify: listing libraries for connection index", "error", err)
		return nil
	}

	idx := &connectionIndex{
		byName: make(map[string][]connEntry),
	}

	for _, lib := range libs {
		// Only index connection libraries (non-manual sources).
		if lib.Source == library.SourceManual {
			continue
		}

		// List all artists in this library.
		page := 1
		const pageSize = 200
		for {
			params := artist.ListParams{
				Page:      page,
				PageSize:  pageSize,
				LibraryID: lib.ID,
				Sort:      "name",
				Order:     "asc",
			}
			artists, total, listErr := r.artistService.List(ctx, params)
			if listErr != nil {
				r.logger.Warn("bulk-identify: listing artists for connection index",
					"library_id", lib.ID, "error", listErr)
				break
			}

			for _, a := range artists {
				if a.MusicBrainzID == "" {
					continue
				}
				key := strings.ToLower(strings.TrimSpace(a.Name))
				idx.byName[key] = append(idx.byName[key], connEntry{
					Name:          a.Name,
					MusicBrainzID: a.MusicBrainzID,
					DiscogsID:     a.DiscogsID,
				})
			}

			if page*pageSize >= total {
				break
			}
			page++
		}
	}

	return idx
}

// convertToScoredCandidates wraps raw search results as ScoredCandidates with
// zero confidence (used when album enrichment is not possible).
func convertToScoredCandidates(results []provider.ArtistSearchResult) []ScoredCandidate {
	scored := make([]ScoredCandidate, len(results))
	for i, res := range results {
		scored[i] = ScoredCandidate{
			ArtistSearchResult: res,
			Confidence:         0,
			Reason:             "no album data available",
		}
	}
	return scored
}
