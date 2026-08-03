package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/event"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/internal/provider"
)

// IdentifyProgress tracks the state of a bulk-identify operation.
type IdentifyProgress struct {
	mu sync.RWMutex
	// Status is one of "running", "completed", "failed", or "canceled".
	// "failed" covers a run in which every processed artist failed, which is
	// what a provider outage or a panicking adapter looks like from here.
	Status      string              `json:"status"`
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

	// releaseCount and releasesKnown record what the scorer learned about the
	// CANDIDATE's own catalogue, so the album-evidence gate does not have to
	// re-fetch data enrichAndScoreTier2 already had in hand (#2828).
	//
	// They are unexported because they are internal gate plumbing rather than
	// API surface: adding them to the JSON would commit us to a wire contract
	// for a number whose only consumer is the write decision.
	//
	// The two fields are separate for the same reason artist.AlbumEvidence is a
	// tri-state: a release-group fetch that FAILED and a candidate that
	// genuinely has no releases both leave releaseCount at 0, and only one of
	// those is a determination. releasesKnown false means "not determined",
	// which the gate declines on.
	releaseCount  int
	releasesKnown bool
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

	// Create progress and its cancellable context together so that cancelFn
	// is always non-nil when Status is "running". This prevents a race where
	// a concurrent DELETE observes Status=="running" but cancelFn==nil.
	bgCtx := context.WithoutCancel(req.Context())
	cancelCtx, cancel := context.WithCancel(bgCtx)
	progress := &IdentifyProgress{
		Status:   "running",
		cancelFn: cancel,
	}

	// Atomic check-and-set: reject if already running, otherwise claim the slot.
	r.identifyMu.Lock()
	if r.identifyProgress != nil {
		r.identifyProgress.mu.RLock()
		running := r.identifyProgress.Status == "running"
		r.identifyProgress.mu.RUnlock()
		if running {
			r.identifyMu.Unlock()
			cancel() // clean up the unused context
			writeJSON(w, http.StatusConflict, map[string]any{
				"status":  "running",
				"message": "bulk identify already in progress",
			})
			return
		}
	}
	r.identifyProgress = progress
	r.identifyMu.Unlock()

	// releaseProgress clears the slot if this request still owns it,
	// and cancels the context to free resources.
	releaseProgress := func() {
		cancel()
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
	// Shape-check the operator's MBID HERE so a malformed one is a 400 about the
	// request, not the 500 that applyIdentity's errIdentityInvalidMBID would map
	// to below. The chokepoint still enforces validity for every other caller;
	// this is the same rule stated at the API boundary, where a bad input belongs.
	if !artist.IsValidMBID(normalizeMBID(body.MBID)) {
		writeError(w, req, http.StatusBadRequest, "mbid must be a valid MusicBrainz identifier")
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

	// The ONLY call site in the tree that sets AllowReplace, which is what makes
	// it grep-assertable that nothing automated can replace a stored identity.
	// A human picked this candidate out of the review queue, so it is both
	// authorized to replace and recorded as operator-confirmed -- and attributed
	// to "manual" rather than a provider source, so the blast-radius report does
	// not file a human decision as machine damage.
	refreshSkipped, err := r.applyIdentity(req.Context(), a, identityWrite{
		MBID:         body.MBID,
		DiscogsID:    body.DiscogsID,
		Source:       artist.IdentifySourceOperator,
		Provenance:   artist.SourceOperatorConfirmed,
		AllowReplace: true,
	})
	if err != nil {
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

	// Always present, never omitted when false -- see handlers_audiodb.go for
	// why key presence must not be the signal. When true, the provider IDs were
	// persisted (a manual edit the lock allows) but the provider refresh that
	// normally follows was suppressed by the artist-level lock.
	resp := map[string]any{
		"status":                 "linked",
		"artist_id":              a.ID,
		"mbid":                   a.MusicBrainzID,
		"refresh_skipped_locked": refreshSkipped,
	}
	writeJSON(w, http.StatusOK, resp)
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

		// Recover from any panic inside the identify pipeline so a single bad
		// artist (or a misbehaving dependency such as the orchestrator) cannot
		// crash the server-wide background goroutine. The slot is released by
		// transitioning Status to "failed" so subsequent POSTs are not blocked
		// by a phantom "running" job. We log with a stable message so operators
		// can correlate against the structured-log assertion in tests.
		defer func() {
			if rec := recover(); rec != nil {
				r.logger.Error("bulk-identify panic recovered",
					"panic", rec)
				progress.mu.Lock()
				progress.Status = "failed"
				progress.CurrentName = ""
				progress.mu.Unlock()
			}
		}()

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
		// A run in which EVERY artist failed did not complete in any sense the
		// operator would recognize: the usual cause is the provider being down
		// or panicking, so reporting "completed" over a total washout tells
		// them the job succeeded and there is simply nothing to review.
		//
		// This is deliberately "all failed", not "any failed". A handful of
		// individual failures within a large run is ordinary and already
		// visible in the Failed counter; only a wholesale failure is a job
		// outcome rather than a per-artist one.
		//
		// It also replaces what a propagating panic used to signal. Provider
		// panics are now contained at the orchestrator boundary (so one bad
		// adapter cannot kill unrelated in-flight requests) and surface as an
		// errored provider status, which the tier handlers already translate
		// into outcomeFailed -- so the signal arrives here as a count rather
		// than as a stack unwind.
		if progress.Processed > 0 && progress.Failed == progress.Processed {
			progress.Status = "failed"
		} else {
			progress.Status = "completed"
		}
		progress.CurrentName = ""
		progress.mu.Unlock()
	}()
}

// identifyArtist runs the 3-tier identification pipeline for a single artist.
//
// The gocognit complexity waiver this comment used to carry was deleted rather
// than retained: extracting the Tier 1 decision into evaluateTier1 dropped this
// function back under the threshold, so the waiver suppressed nothing and the
// linter flagged it as dead. A waiver the code no longer needs is worse than
// none -- it tells a later reader this function is irreducibly complex when it
// is not.
func (r *Router) identifyArtist(ctx context.Context, a *artist.Artist, connIdx *connectionIndex) identifyResult {
	// Skip locked artists -- they should not be auto-modified.
	if a.Locked {
		return identifyResult{Outcome: outcomeSkipped}
	}

	// Tier 1: Connection-based matching.
	//
	// DELIBERATELY UNGATED BY THE ALBUM-EVIDENCE GATE, and the ordering makes
	// that structural rather than accidental: localAlbums is resolved below, so
	// Tier 1 could not consult the gate here even if it wanted to. Do not read
	// that ordering as an oversight to be "fixed" by hoisting the resolution --
	// hoisting it would silently START gating Tier 1, which is a behavior change
	// this PR did not make and did not test.
	//
	// The reasoning is in the WHICH WRITE PATHS APPLY IT block at the top of
	// internal/artist/albumgate.go, and the short form is: Tier 1 makes no
	// provider call, so gating it would require a configured MusicBrainz
	// provider before a connection-only install could auto-link anything, and it
	// would refuse the EvidenceUnknown majority that Tier 1 mostly serves. Its
	// risk shape is also different -- a connected platform's own record for an
	// artist it holds, filling a blank only (#2856), rather than a name search
	// landing on an empty stub. Smaller, not zero; tracked separately.
	if connIdx != nil {
		if res, handled := r.evaluateTier1(ctx, a, connIdx); handled {
			return res
		}
	}

	// Tier 2 and 3 require the orchestrator for provider searches.
	if r.orchestrator == nil {
		return identifyResult{Outcome: outcomeUnmatched}
	}

	// Album evidence is resolved ONCE per artist and drives both tiers below
	// (#2828). Resolving it here rather than inside Tier 2 is the fix: the old
	// code called artist.ListLocalAlbums and branched on `len(...) > 0`, which
	// cannot tell "this artist has no albums" from "I could not read the album
	// directory". Both produced an empty slice, both skipped Tier 2, and both
	// therefore fell through to Tier 3 -- the ONLY tier that performs no
	// catalogue check at all. So the absence of evidence did not merely fail to
	// object to a candidate, it steered the artist to the tier that could not
	// object, and in the production snapshot 43% of artists have no path and
	// take that route by default.
	localAlbums := r.localAlbumSet(ctx, a)

	// One cache per artist, shared by both tiers, so a candidate appearing in
	// both searches costs one GetReleaseGroups call rather than two.
	rgCache := r.newReleaseGroupCache()

	// Tier 2: Album comparison. Reachable only on a POSITIVE determination that
	// the artist has albums -- EvidenceFound. EvidenceNone (a genuinely empty
	// folder) and EvidenceUnknown (nothing was read) both have nothing to
	// compare, and both now carry that fact into Tier 3's gate instead of
	// silently arriving there as "no objection raised".
	if localAlbums.Evidence == artist.EvidenceFound {
		searchName := filepath.Base(a.Path)
		results, statuses, err := r.orchestrator.SearchForLinking(ctx, searchName, []provider.ProviderName{provider.NameMusicBrainz})
		switch {
		case err != nil:
			r.logger.Warn("bulk-identify: Tier 2 search failed",
				"artist", a.Name, "error", err)
			// Fall through to Tier 3 on search failure only.
		case len(collectFailedProviderDisplayNames(statuses)) > 0:
			// A provider returned an error (e.g. MusicBrainz outage). Treat
			// as a search failure rather than "no matches": empty results
			// here are not evidence of absence -- the lookup did not run.
			// Fall through to Tier 3, which applies the same statuses
			// check and will route to outcomeFailed instead of
			// outcomeUnmatched.
			r.logger.Warn("bulk-identify: Tier 2 provider search failed",
				"artist", a.Name,
				"failed_providers", collectFailedProviderDisplayNames(statuses))
		case len(results) > 0:
			scored := r.enrichAndScoreTier2(ctx, results, localAlbums, rgCache)
			tier2Result := r.evaluateTier2(ctx, a, localAlbums, scored)
			// If Tier 2 ran album comparison and found no match (< 30%),
			// do NOT fall through to Tier 3. The album evidence is more
			// reliable than a name-only search, so respect its verdict.
			return tier2Result
		}
	}

	// Tier 3: Name-only search (only for artists without album subdirectories,
	// or when Tier 2 search failed due to an error).
	results, statuses, err := r.orchestrator.SearchForLinking(ctx, a.Name, []provider.ProviderName{provider.NameMusicBrainz})
	if err != nil {
		r.logger.Warn("bulk-identify: Tier 3 search failed",
			"artist", a.Name, "error", err)
		return identifyResult{Outcome: outcomeFailed}
	}
	if failed := collectFailedProviderDisplayNames(statuses); len(failed) > 0 {
		// Same rationale as Tier 2: provider outage is not "no match".
		// Surface as outcomeFailed so the bulk pipeline reports the
		// artist as something the user needs to retry, not as confirmed-
		// unmatched (which would suppress future auto-retry attempts).
		r.logger.Warn("bulk-identify: Tier 3 provider search failed",
			"artist", a.Name, "failed_providers", failed)
		return identifyResult{Outcome: outcomeFailed}
	}

	if len(results) == 0 {
		return identifyResult{Outcome: outcomeUnmatched}
	}

	// Auto-link only when the SHARED confidence gate accepts the top candidate
	// (#2827). What this replaces was `len(results) == 1 && Score >= 90`, which
	// measured result SCARCITY, not match confidence: a single high-relevance
	// hit whose name barely resembles the artist auto-linked, while a
	// well-corroborated match that happened to return two rows never could.
	//
	// The gate supplies the name-similarity floor and the ambiguity margin from
	// artist.MBIDMinNameSimilarity / artist.MBIDAmbiguityMargin, so the
	// thresholds here are the same ones the nfo_has_mbid rule fixer applies --
	// shared, not duplicated. It also rejects a candidate whose MBID is not a
	// valid UUID, which the scarcity clause never checked.
	//
	// Worth stating because it reads as load-bearing and is not:
	// BestMBIDCandidates restricts RUNNER-UP eligibility to MusicBrainz-sourced
	// hits, and both search calls in this function already request MusicBrainz
	// exclusively, so that filter is a no-op at this site. It is kept because it
	// is correct and free, and because the provider list is a parameter that
	// could widen later.
	best, runnerUp := artist.BestMBIDCandidates(results)
	switch rej := artist.EvaluateMBIDCandidate(a.Name, best, runnerUp); {
	case rej != nil:
		// Info, matching the rule fixer's decline logging. Fall through to the
		// review-queue construction below.
		r.logger.Info("identify: Tier 3 candidate declined by the confidence gate",
			"artist", a.Name, "reason", rej.Reason)
	case r.identityWouldReplace(a, best.MusicBrainzID):
		// Cleared the gate but disagrees with a stored identity: never replace
		// (#2826). Fall through to the review queue so a human decides.
		r.logger.Info("identify: Tier 3 candidate disagrees with the stored MusicBrainz ID, queueing for review",
			"artist", a.Name, "stored_mbid", a.MusicBrainzID, "proposed_mbid", best.MusicBrainzID)
	case r.tier3AlbumGateDeclines(ctx, a, localAlbums, results, best, rgCache):
		// The name gates passed and the ALBUM gate did not (#2828). This is the
		// case the name gates structurally cannot catch: an empty MusicBrainz
		// stub matches a name at 100% and owns no catalogue that could
		// contradict it, which is what all 18 of the measured wrong adoptions
		// looked like. tier3AlbumGateDeclines logs the specific reason. Fall
		// through to the review queue -- the candidate is still shown, just not
		// written unattended.
	default:
		// Lock already handled upstream (see identifyArtist's a.Locked guard).
		if _, err := r.applyIdentity(ctx, a, identityWrite{
			MBID:       best.MusicBrainzID,
			Source:     artist.IdentifySourceName,
			Provenance: artist.SourceMachinePicked,
		}); err != nil {
			return identifyResult{Outcome: outcomeFailed}
		}
		return identifyResult{Outcome: outcomeAutoLinked}
	}

	// Queue only candidates with confidence >= 0.3 to avoid flooding the review
	// queue with low-confidence noise.
	//
	// The Score/200 formula below is NOT a second confidence scale competing
	// with the gate above (#2827). The gate is now the sole authority for the
	// auto-link decision; this arithmetic only decides which candidates are
	// worth SHOWING a human and in what order. It is deliberately left
	// unchanged: re-expressing a review-queue display filter in the gate's
	// correctness constants would re-tune what the operator sees, which is a
	// display change riding along on an overwrite fix.
	var reviewable []ScoredCandidate
	for i := range results {
		res := &results[i]
		confidence := float64(res.Score) / 200.0 // name-only tops at 0.5
		if confidence < 0.3 {
			continue
		}
		reviewable = append(reviewable, ScoredCandidate{
			ArtistSearchResult: *res,
			Confidence:         confidence,
			Reason:             "name match",
		})
	}

	if len(reviewable) == 0 {
		return identifyResult{Outcome: outcomeUnmatched}
	}

	return identifyResult{
		Outcome: outcomeQueued,
		Candidate: &IdentifyCandidate{
			ArtistID:   a.ID,
			ArtistName: a.Name,
			ArtistPath: a.Path,
			Tier:       "name",
			Candidates: reviewable,
		},
	}
}

// evaluateTier1 decides what the connected-platform index can say about this
// artist's identity. handled=false means Tier 1 reached no verdict and the
// caller should continue to Tier 2/3.
//
// THE CRUX (issue #2826, #2827). The shared confidence gate
// artist.EvaluateMBIDCandidate reads a provider.ArtistSearchResult's Score, and
// a connEntry has no score to read -- a connection entry is not a scored search
// hit, it is a claim by a third-party system about an artist it also holds.
// Wrapping an entry in a synthetic ArtistSearchResult{Score: 100} and calling
// the gate would launder a fabricated number through it: the score floor would
// be inert at this site while APPEARING in the diff to have been applied, which
// is worse than not calling the gate at all because it defeats review.
//
// So the gate is applied LEG BY LEG here, reusing its exported constants so
// there is exactly one definition of each threshold in the tree:
//
//   - artist.MBIDMinNameSimilarity applies DIRECTLY. It is the one leg computed
//     locally rather than read off a provider, so it transfers unchanged.
//   - artist.MBIDAmbiguityMargin is re-expressed EXACTLY, not re-tuned. Its
//     meaning is "the evidence does not discriminate between two identities".
//     With no scores to subtract, the honest form of that is: if the surviving
//     entries carry more than one distinct MBID, the index cannot discriminate.
//     That is strictly stricter than a 10-point margin and introduces no new
//     threshold.
//   - artist.MBIDMinProviderScore is INAPPLICABLE and deliberately not faked.
//     There is no relevance rank to floor. Its protection is replaced
//     STRUCTURALLY: this path may only ever fill a blank (applyIdentity
//     enforces that), so the worst case of a wrong adoption is a blank becoming
//     wrong, never a correct identity being destroyed.
//
// Plus one leg the previous code omitted entirely: syntactic validity.
// buildConnectionIndex filters only on MusicBrainzID != "", so a
// platform-sourced ID was never checked for being a UUID at all.
//
// The old "unanimous" guard this replaces was a loop over entries[1:], which
// never executes for a single entry -- so one platform record whose NAME
// happened to match overwrote a stored MBID with no confidence evidence
// whatsoever. That is #2826.
func (r *Router) evaluateTier1(ctx context.Context, a *artist.Artist, connIdx *connectionIndex) (identifyResult, bool) {
	entries := connIdx.lookup(a.Name)
	if len(entries) == 0 {
		return identifyResult{}, false
	}

	// Survivors clear BOTH the validity leg and the name-similarity leg.
	// Similarity is not automatically 100: lookup normalizes case and
	// whitespace only, so entries whose name merely normalizes equal can still
	// differ materially.
	type survivor struct {
		entry      connEntry
		mbid       string // normalized
		similarity int
	}
	var surviving []survivor
	distinct := make(map[string]struct{})
	for _, entry := range entries {
		mbid := normalizeMBID(entry.MusicBrainzID)
		if !artist.IsValidMBID(mbid) {
			continue
		}
		sim := provider.NameSimilarity(a.Name, entry.Name)
		if sim < artist.MBIDMinNameSimilarity {
			continue
		}
		surviving = append(surviving, survivor{entry: entry, mbid: mbid, similarity: sim})
		distinct[mbid] = struct{}{}
	}

	if len(surviving) == 0 {
		// Info, matching the nfo_has_mbid fixer's decline logging: a declined
		// candidate is an operator-visible event, not a fault.
		r.logger.Info("identify: no connection entry cleared the Tier 1 gates",
			"artist", a.Name, "entries", len(entries))
		return identifyResult{}, false
	}

	if a.MusicBrainzID != "" {
		stored := normalizeMBID(a.MusicBrainzID)
		if _, only := distinct[stored]; only && len(distinct) == 1 {
			// CORROBORATION: the platform agrees with what is already stored.
			// Still route through applyIdentity so the DiscogsID blank-fill and
			// the metadata refresh happen, preserving the pre-existing
			// user-visible behavior and the outcomeAutoLinked counter.
			// AllowReplace stays false and is never exercised: the IDs match, so
			// this is not a replacement. HistoryService.Record suppresses the
			// row itself (old == new).
			if _, err := r.applyIdentity(ctx, a, identityWrite{
				MBID:       stored,
				DiscogsID:  surviving[0].entry.DiscogsID,
				Source:     artist.IdentifySourceConnection,
				Provenance: a.MetadataSources[artist.SourceKeyMusicBrainzID],
			}); err != nil {
				return identifyResult{Outcome: outcomeFailed}, true
			}
			return identifyResult{Outcome: outcomeAutoLinked}, true
		}

		// The platform offers an identity that DISAGREES with the stored one.
		// Never write it: queue for a human instead (#2826). Do not fall
		// through -- Tier 2/3 could not write either under the never-replace
		// invariant, so falling through would spend provider calls to reach the
		// same refusal.
		candidates := make([]ScoredCandidate, 0, len(surviving))
		for _, s := range surviving {
			candidates = append(candidates, ScoredCandidate{
				ArtistSearchResult: provider.ArtistSearchResult{
					Name:          s.entry.Name,
					MusicBrainzID: s.mbid,
					Source:        "connection",
				},
				Confidence: float64(s.similarity) / 100.0,
				Reason:     "connection library match",
			})
		}
		r.logger.Info("identify: connection entry disagrees with the stored MusicBrainz ID, queueing for review",
			"artist", a.Name, "stored_mbid", a.MusicBrainzID, "candidates", len(candidates))
		return identifyResult{
			Outcome: outcomeQueued,
			Candidate: &IdentifyCandidate{
				ArtistID:   a.ID,
				ArtistName: a.Name,
				ArtistPath: a.Path,
				// "connection" is already the documented first value of the
				// Tier field; it was simply never emitted before.
				Tier:       "connection",
				Candidates: candidates,
			},
		}, true
	}

	// Blank MBID. One distinct surviving identity is a fill; more than one is
	// the ambiguity leg firing.
	if len(distinct) == 1 {
		// The lock was already handled upstream: identifyArtist returns
		// outcomeSkipped for a locked artist, so the skip flag cannot be true.
		if _, err := r.applyIdentity(ctx, a, identityWrite{
			MBID:       surviving[0].mbid,
			DiscogsID:  surviving[0].entry.DiscogsID,
			Source:     artist.IdentifySourceConnection,
			Provenance: artist.SourceMachinePicked,
		}); err != nil {
			return identifyResult{Outcome: outcomeFailed}, true
		}
		return identifyResult{Outcome: outcomeAutoLinked}, true
	}

	// Ambiguous and nothing to protect: FALL THROUGH rather than queue. Album
	// comparison is better evidence than the platform index and can
	// discriminate where the index cannot, so only queue if Tier 2/3 also
	// decline. Note there is deliberately no ">= 2 agreeing entries" quorum:
	// buildConnectionIndex indexes per library, so a single-connection install
	// genuinely holds exactly one entry per artist and a quorum would stop
	// Tier 1 linking anything at all.
	r.logger.Info("identify: connection entries carry distinct MusicBrainz IDs, deferring to later tiers",
		"artist", a.Name, "distinct_mbids", len(distinct))
	return identifyResult{}, false
}

// enrichAndScoreTier2 enriches search results with album comparison data and
// computes confidence scores for Tier 2 candidates.
//
// It takes an artist.AlbumSet rather than a []string (#2828). The slice form
// could not express "the album list is not a determination", so a caller that
// had failed to read the artist's folder handed in an empty slice and every
// candidate scored 0% -- indistinguishable from a candidate whose catalogue
// genuinely shares nothing with the artist's. The AlbumSet carries that
// difference through to the gate.
//
// cache may be nil: the candidate-side release counts then read as "not
// determined", which the gate declines on.
func (r *Router) enrichAndScoreTier2(ctx context.Context, results []provider.ArtistSearchResult, local artist.AlbumSet, cache *releaseGroupCache) []ScoredCandidate {
	// No release-group fetcher available (no registry, no MusicBrainz adapter,
	// or an adapter that does not implement the optional interface): fall back
	// to name-only candidates. They carry releasesKnown false, so the gate will
	// not authorize an unattended write off them.
	if cache == nil {
		return convertToScoredCandidatesReason(results, albumEvidenceReason(local.Evidence))
	}

	// A set that is not a determination has no titles to compare against, so
	// scoring it would produce a fabricated 0% for every candidate. Return the
	// reason instead, exactly as enrichAndScoreTier2Set does for the display
	// surfaces (#2852).
	if local.Evidence != artist.EvidenceFound {
		return convertToScoredCandidatesReason(results, albumEvidenceReason(local.Evidence))
	}

	scored := make([]ScoredCandidate, len(results))
	attempted := 0
	for i := range results {
		res := &results[i]
		scored[i] = ScoredCandidate{
			ArtistSearchResult: *res,
			Reason:             "album comparison",
		}

		// The 3-candidate cap on provider calls is pre-existing behavior kept
		// unchanged: it bounds the round-trips a single artist can cost. A
		// candidate past the cap keeps releasesKnown false, so it is never
		// auto-linked -- the cap costs certainty, never manufactures it.
		if attempted >= 3 || res.MusicBrainzID == "" {
			continue
		}
		attempted++

		// titles logs the CAUSE (#2862). The bare line here named the operation
		// but not what went wrong, so a broken adapter read like an empty
		// catalogue -- the conflation this series exists to remove.
		remoteTitles, known := cache.titles(ctx, res.MusicBrainzID)
		if !known {
			continue
		}
		scored[i].releasesKnown = true
		scored[i].releaseCount = len(remoteTitles)

		comp := artist.CompareAlbumSet(local, remoteTitles)
		scored[i].AlbumComparison = &comp.AlbumComparison
		scored[i].Confidence = float64(comp.MatchPercent) / 100.0
	}

	return scored
}

// evaluateTier2 evaluates Tier 2 candidates and returns the appropriate outcome.
//
// local is threaded in so the shared album-evidence gate can read it (#2828).
// Tier 2 previously read only the computed MatchPercent, which is 0 both when
// the catalogues genuinely disagree and when there was no local catalogue to
// compare -- and 0 was safe here only because Tier 2 was unreachable without
// albums. That routing is exactly what changed, so the evidence state now
// travels with the scores rather than being implied by the call site.
func (r *Router) evaluateTier2(ctx context.Context, a *artist.Artist, local artist.AlbumSet, scored []ScoredCandidate) identifyResult {
	// Count candidates meeting thresholds. The floors are the shared constants
	// rather than bare literals so the same numbers govern here and inside
	// artist.EvaluateAlbumGate; two copies of a threshold is two thresholds.
	var above70 []ScoredCandidate
	var above30 []ScoredCandidate
	for i := range scored {
		s := &scored[i]
		if s.AlbumComparison != nil {
			if s.AlbumComparison.MatchPercent >= artist.AlbumOverlapAutoLinkFloor {
				above70 = append(above70, *s)
			}
			if s.AlbumComparison.MatchPercent >= artist.AlbumOverlapReviewFloor {
				above30 = append(above30, *s)
			}
		}
	}

	// Exactly 1 candidate at or above artist.AlbumOverlapAutoLinkFloor:
	// auto-link, unless that would REPLACE a
	// stored MusicBrainz ID (#2826).
	//
	// This guard is not redundant with applyIdentity's invariant, it is what
	// keeps the invariant from firing here. Tier 2 holds the BEST evidence of
	// any tier, so it must not be the tier that trips the bug backstop: without
	// this check a legitimate Tier 2 disagreement would surface as an ERROR log
	// and outcomeFailed instead of the review-queue entry the operator needs.
	// The validity condition is here as well as in applyIdentity for the same
	// reason the never-replace condition is: applyIdentity treats an EMPTY MBID as
	// "leave alone", so a blank candidate would sail through the chokepoint and
	// still be reported as outcomeAutoLinked -- a link that linked nothing. The
	// call site is the only place that can tell the difference between "nothing to
	// write" and "written", so the outcome decision belongs here. Falling through
	// puts the candidate in the review queue (it also cleared 30%), which is the
	// honest answer.
	//
	// The album-evidence gate (#2828) is the fourth condition. It restates the
	// 70% floor in shared form and adds the two checks this site never made:
	// that the local album list was a DETERMINATION rather than an empty
	// placeholder, and that the CANDIDATE carries a catalogue of its own. An
	// entity with zero release groups scores 0% here and so could never have
	// reached above70 -- but only because a 0% score also fails the floor for
	// the ordinary reason, which is a coincidence rather than a check. Asking
	// the gate makes the refusal deliberate and keeps both tiers refusing for
	// the same stated reason.
	if len(above70) == 1 &&
		artist.IsValidMBID(normalizeMBID(above70[0].MusicBrainzID)) &&
		!r.identityWouldReplace(a, above70[0].MusicBrainzID) &&
		r.tier2GatePermits(a, local, &above70[0]) {
		// Lock already handled upstream (see identifyArtist's a.Locked guard).
		if _, err := r.applyIdentity(ctx, a, identityWrite{
			MBID:       above70[0].MusicBrainzID,
			Source:     artist.IdentifySourceAlbum,
			Provenance: artist.SourceMachinePicked,
		}); err != nil {
			return identifyResult{Outcome: outcomeFailed}
		}
		return identifyResult{Outcome: outcomeAutoLinked}
	}

	// Any candidates at or above artist.AlbumOverlapReviewFloor: review queue.
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

// normalizeMBID renders a MusicBrainz ID in the form the tree stores and
// compares: trimmed and lowercased, matching the nfo_has_mbid fixer and
// bulk_executor.fetchImages.
//
// It exists so the identity comparison and the identity WRITE cannot disagree
// about what counts as "the same ID". They did once: the write normalized while
// the comparison read the raw stored string, so a padded stored value read as a
// replacement of itself and the never-replace invariant refused a corroboration
// that changed nothing.
func normalizeMBID(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// identityWouldReplace reports whether adopting proposed would REPLACE a
// different MusicBrainz ID already stored on the artist. Tiers call it to route
// to the review queue themselves rather than letting applyIdentity's backstop
// turn a legitimate disagreement into an error. It is the same comparison
// applyIdentity makes, in one place, so the two cannot drift apart.
func (r *Router) identityWouldReplace(a *artist.Artist, proposed string) bool {
	return a.MusicBrainzID != "" && proposed != "" &&
		!strings.EqualFold(normalizeMBID(a.MusicBrainzID), normalizeMBID(proposed))
}

// errIdentityReplaceRefused is returned by applyIdentity when a caller proposed
// an MBID that would REPLACE a different stored one without operator consent.
// It is a bug backstop rather than a routine decline: every tier is expected to
// make that decision itself and queue for review, so reaching this sentinel
// means a writer forgot to (issue #2826).
var errIdentityReplaceRefused = errors.New("refusing to replace a stored MusicBrainz ID without operator consent")

// errIdentityInvalidMBID is returned by applyIdentity when a caller proposed a
// NON-EMPTY MusicBrainz ID that is not a syntactically valid UUID.
//
// Like errIdentityReplaceRefused this is the second leg of the chokepoint, not a
// routine decline: validity used to be checked at Tier 1 and Tier 3 but not at
// Tier 2 nor at the operator link handler, which is exactly the per-caller drift
// applyIdentity exists to eliminate. An EMPTY MBID is not an error -- see
// identityWrite.MBID.
var errIdentityInvalidMBID = errors.New("refusing to write a syntactically invalid MusicBrainz ID")

// identityWrite describes a proposed identity assignment for applyIdentity.
//
// The zero value refuses to replace anything, which is the whole point: a
// future writer that forgets to think about AllowReplace gets the safe
// behavior rather than the destructive one. A destructive action must be
// authorized by a positive allow-list, never by the absence of a veto.
type identityWrite struct {
	// MBID is the proposed MusicBrainz ID. Empty means "leave the artist's
	// MusicBrainzID alone" -- used by the non-identity provider-link paths.
	MBID string

	// DiscogsID is applied when non-empty, matching the pre-existing behavior
	// of the connection and review-queue link paths. The never-replace
	// invariant deliberately covers the MusicBrainz ID only: #2826 is about the
	// identity every downstream consumer treats as fact.
	DiscogsID string

	// Source is the metadata_changes attribution: one of the
	// artist.IdentifySource* values.
	Source string

	// Provenance is stamped into MetadataSources["musicbrainz_id"]:
	// artist.SourceMachinePicked or artist.SourceOperatorConfirmed. Only
	// written when MBID is non-empty, so a Discogs-only link cannot relabel
	// the provenance of an MBID it did not touch.
	Provenance string

	// AllowReplace authorizes replacing a DIFFERENT stored MusicBrainz ID.
	// ZERO VALUE = REFUSE. Only an operator-driven path may set it.
	AllowReplace bool
}

// applyIdentity is the single chokepoint for every MusicBrainz-ID write in the
// identify pipeline, and it enforces two invariants:
//
//   - NEVER-REPLACE: an automated writer may FILL A BLANK MBID or AGREE with the
//     stored one, but it may never REPLACE one (issue #2826).
//   - VALIDITY: a non-empty MBID must be a syntactically valid UUID. It was
//     previously applied at Tier 1 and Tier 3 only, so Tier 2 and the operator
//     link could each write an unchecked value.
//
// Enforcing that here rather than inside each tier is the point. Four writers
// reach this function -- Tier 1 (connection index), Tier 2 (album comparison),
// Tier 3 (name search), and the operator's review-queue link -- and a guard
// repeated in three of them is three chances to drift, plus nothing at all
// protecting a future Tier 4. One invariant here is grep-assertable: the only
// assignment to a.MusicBrainzID on the identify path is the one below, and the
// only caller that sets AllowReplace is the operator link.
//
// Taking ownership of the assignment (rather than letting callers mutate the
// artist first) is also what makes the audit row possible: by the time a
// caller-mutated artist arrived here the previous ID was already gone, so
// there was nothing to compare against and nothing to record as the replaced
// value (issue #2845).
func (r *Router) applyIdentity(ctx context.Context, a *artist.Artist, w identityWrite) (refreshSkipped bool, err error) {
	old := a.MusicBrainzID

	// VALIDITY LEG. Checked here rather than per-tier so it is as grep-assertable
	// as the never-replace leg below and no future caller can forget it.
	//
	// Only a NON-EMPTY, invalid value is a rejection. An empty MBID legitimately
	// means "leave the artist's MusicBrainzID alone" (see identityWrite.MBID) and
	// is how the Discogs-only link paths reach this function, so treating empty as
	// invalid would break every one of them.
	if w.MBID != "" && !artist.IsValidMBID(normalizeMBID(w.MBID)) {
		r.logger.Error("identify: refused a syntactically invalid MusicBrainz ID",
			"artist_id", a.ID, "artist", a.Name,
			"proposed_mbid", w.MBID, "source", w.Source)
		return false, errIdentityInvalidMBID
	}

	// Compare NORMALIZED forms. Trimming matters as much as case-folding here:
	// a stored value that arrived padded (a hand-edited NFO, a platform payload
	// with stray whitespace) is the SAME identity as its trimmed twin, and
	// comparing the raw strings made a genuine corroboration read as a
	// replacement -- which the invariant below then refused, turning a
	// no-op agreement into outcomeFailed plus a spurious ERROR log.
	//
	// EqualFold rather than ==: MBIDs are stored lowercased but providers and
	// platforms return either case, and an ID differing only in case is the same
	// identity. Treating either difference as a replacement would both refuse a
	// legitimate corroboration and record a phantom change.
	replacing := old != "" && w.MBID != "" &&
		!strings.EqualFold(normalizeMBID(old), normalizeMBID(w.MBID))
	if replacing && !w.AllowReplace {
		// ERROR, not Info: a tier that reaches this line has already failed to
		// make its own never-replace decision, so this is a defect to
		// investigate, not a routine "not confident enough" outcome.
		r.logger.Error("identify: refused to replace a stored MusicBrainz ID",
			"artist_id", a.ID, "artist", a.Name,
			"stored_mbid", old, "proposed_mbid", w.MBID, "source", w.Source)
		return false, errIdentityReplaceRefused
	}

	if w.MBID != "" {
		// Normalize on write, matching the nfo_has_mbid fixer and
		// bulk_executor.fetchImages: MBIDs are used as case-insensitive lookup
		// keys elsewhere, so storing whatever case a source returned would
		// silently break an exact-match lookup keyed on the lowercased form.
		a.MusicBrainzID = normalizeMBID(w.MBID)
		if w.Provenance != "" {
			if a.MetadataSources == nil {
				a.MetadataSources = make(map[string]string)
			}
			a.MetadataSources[artist.SourceKeyMusicBrainzID] = w.Provenance
		}
	}
	if w.DiscogsID != "" {
		a.DiscogsID = w.DiscogsID
	}

	refreshSkipped, err = r.autoLinkAndRefresh(ctx, a, false, "")
	if err != nil {
		return refreshSkipped, err
	}

	// Best-effort audit row, AFTER the write it records succeeded (issue
	// #2845). Contract copied from Pipeline.recordRuleFixHistory and
	// BulkExecutor.recordBulkMBIDHistory: a failed audit write is a
	// diagnostics gap and must never fail an operation that already
	// completed.
	r.recordIdentityHistory(ctx, a, old, w.Source)

	return refreshSkipped, nil
}

// recordIdentityHistory writes the metadata_changes row for an identity write.
//
// oldValue carries the REPLACED MBID, which is the entire point of #2845: the
// existing rule-path recorders hardcode "" because their callers are
// blank-fill-only, so nothing in the tree could tell an operator what an
// automated pass destroyed. HistoryService.Record already suppresses a row when
// oldValue is non-empty and equal to newValue, so a pure corroboration (Tier 1
// agreeing with the stored ID) writes nothing -- do NOT add a second guard for
// that here, it is already handled one layer down.
func (r *Router) recordIdentityHistory(ctx context.Context, a *artist.Artist, oldMBID, source string) {
	if r.historyService == nil || source == "" {
		return
	}
	if err := r.historyService.Record(ctx, a.ID, artist.SourceKeyMusicBrainzID,
		oldMBID, a.MusicBrainzID, source); err != nil {
		r.logger.Warn("identify: recording MusicBrainz ID history",
			"artist_id", a.ID, "source", source, "error", err)
	}
}

// autoLinkAndRefresh sets the provider ID on the artist, persists it, runs a
// full metadata refresh, and evaluates health. Returns an error only if the
// initial Update fails (refresh failures are logged but not fatal).
//
// Callers that assign a MusicBrainz ID must go through applyIdentity instead,
// which owns the never-replace invariant and the audit row. This function
// remains the persist-and-refresh primitive for the provider-link handlers
// that mutate a NON-identity field (Discogs, TheAudioDB, Deezer) and so have
// no MBID to protect.
//
// Returns refreshSkipped=true when the artist-level lock suppressed the
// provider refresh: the caller-chosen provider ID is a manual edit and is
// still persisted, but the automated fetch/merge that normally follows is the
// exact "automated change" the artist lock promises to skip (see
// applyBulkAction and Pipeline.runForArtistFiltered for the same rule).
//
// reidentify tells this function the caller is performing a RE-IDENTIFY -- the
// operator declaring the artist to be a different entity -- rather than
// linking one provider ID onto an artist whose identity is not in question.
// Only then are the repudiated entity's secondary provider IDs discarded
// (#2894). It is a parameter rather than something inferred here because the
// Deezer / Discogs / TheAudioDB link handlers share this primitive and must
// keep preserving those IDs; the distinction is the CALLER's intent and only
// the caller knows it.
// keepDiscogsID is the Discogs ID the current request supplied, if any. It is
// preserved through the discard because it is the operator's own choice for the
// NEW identity, not a leftover from the repudiated one. Ignored unless
// reidentify is true.
func (r *Router) autoLinkAndRefresh(ctx context.Context, a *artist.Artist, reidentify bool, keepDiscogsID string) (refreshSkipped bool, err error) {
	if err := r.artistService.Update(ctx, a); err != nil {
		r.logger.Warn("bulk-identify: update failed", "artist", a.Name, "error", err)
		return false, err
	}
	if a.Locked {
		// Info, not Warn: the lock contract working, not a fault. The provider
		// ID above genuinely changed, so the event below still publishes.
		//
		// The secondary-ID discard is deliberately NOT reached on this branch:
		// the refresh that would re-derive those IDs never runs for a locked
		// artist, so clearing them here would destroy them permanently (AudioDB
		// especially -- see discardRepudiatedProviderIDs on why it does not
		// reliably come back).
		r.logger.Info("link: refresh skipped, artist is locked", "artist_id", a.ID)
		refreshSkipped = true
	} else if r.orchestrator != nil {
		// Discard immediately before the fetch, never as part of the Update
		// above. The clear lives only in memory until executeRefreshCtx's own
		// successful write persists it, so a failed refresh below leaves the
		// artist with every ID it started with rather than stranding it with
		// fewer IDs and the same wrong metadata.
		if reidentify {
			previousAudioDBID := a.AudioDBID
			if discardRepudiatedProviderIDs(a, keepDiscogsID) {
				r.logger.Info("re-identify: discarding the repudiated entity's provider IDs",
					slog.String("artist_id", a.ID),
					slog.String("previous_audiodb_id", previousAudioDBID),
				)
			}
		}
		if _, err := r.executeRefreshCtx(ctx, a); err != nil {
			r.logger.Warn("bulk-identify: refresh failed after linking",
				"artist", a.Name, "error", err)
		}
	}
	if r.eventBus != nil {
		r.eventBus.Publish(event.Event{
			Type: event.ArtistUpdated,
			Data: map[string]any{"artist_id": a.ID},
		})
	}
	return refreshSkipped, nil
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

	for li := range libs {
		lib := &libs[li]
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

			for ai := range artists {
				a := &artists[ai]
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
