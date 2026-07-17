// Package api -- handlers_phash_repair.go
//
// Admin-gated destructive back-out and restore for cross-artist backdrop
// pollution (#2564 PR-4b). The read-only detector and its JSON report live in
// handlers_phash_mismatch.go; this file drives the two mutating halves that
// report surfaces: RemediatePHashMismatches (quarantine + local removal +
// platform delete) and RestorePHashQuarantine (put the quarantined bytes back,
// locally and on the platform).
//
// Both run on *rule.Pipeline and are reached through a narrow capability
// interface, mirroring handlers_phash_mismatch.go / handlers_backdrop_repair.go,
// so the many test call sites constructing fakes that implement only
// rule.PipelineRunner need no interface change.
//
// Routes (both admin-only via requireForeignAdmin; snake_case JSON):
//
//	POST {basePath}/api/v1/reports/phash-mismatch/remediate
//	     {artist_id?, all_artists, dry_run, tolerance}
//	POST {basePath}/api/v1/reports/phash-mismatch/restore
//	     {artist_id, op_id}
//
// SINGLETON. Both endpoints claim r.backdropRepairRunning under r.bulkActionMu --
// the SAME flag the fanart-duplicate remediation uses -- so a phash back-out, a
// fanart-duplicate repair, and a fetch_images/run_rules bulk action are all
// mutually exclusive. That exclusion is load-bearing, not tidy: every one of
// them tombs/renumbers/writes an artist's fanart files on disk, so two running
// at once would race the same directory. The rule pipeline additionally holds a
// per-artist lock (Pipeline.phashArtistMu) that serializes a remediate against a
// restore of the SAME artist; this handler-level singleton is the coarser guard
// that keeps two whole runs from overlapping at all.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"

	"github.com/sydlexius/stillwater/internal/rule"
)

// pHashRemediator is the pipeline capability these handlers need: the
// destructive back-out and its restore.
type pHashRemediator interface {
	RemediatePHashMismatches(ctx context.Context, scope rule.PHashMismatchScope, opts rule.PHashRemediateOpts) (rule.PHashRemediateResult, error)
	RestorePHashQuarantine(ctx context.Context, artistID, opID string) (rule.PHashRestoreResult, error)
}

// phashRemediateRequest is the POST body for the back-out endpoint.
type phashRemediateRequest struct {
	// ArtistID scopes the back-out to one artist. Exactly one of ArtistID or
	// AllArtists is required; the pipeline rejects a run with neither so a
	// forgotten scope cannot become a library-wide delete.
	ArtistID string `json:"artist_id"`
	// AllArtists must be set explicitly to run library-wide. See
	// rule.PHashRemediateOpts.AllArtists for why the default is per-artist.
	AllArtists bool `json:"all_artists"`
	// DryRun previews without mutating anything.
	DryRun bool `json:"dry_run"`
	// Tolerance overrides the similarity cutoff. Zero (omitted) selects the
	// detector's default; a provided value must be in (0, 1].
	Tolerance float64 `json:"tolerance"`
}

// phashRestoreRequest is the POST body for the restore endpoint.
type phashRestoreRequest struct {
	ArtistID string `json:"artist_id"`
	OpID     string `json:"op_id"`
}

// claimPHashRepairSlot atomically claims the shared destructive-fanart
// singleton (r.backdropRepairRunning, guarded by r.bulkActionMu) or writes a
// 409 and returns ok=false. On success it returns a release func the caller
// MUST defer. See this file's package comment for why the flag is shared with
// the fanart-duplicate remediation and bulk actions.
func (r *Router) claimPHashRepairSlot(w http.ResponseWriter) (release func(), ok bool) {
	r.bulkActionMu.Lock()
	bulkRunning := false
	if r.bulkActionProgress != nil {
		r.bulkActionProgress.mu.RLock()
		bulkRunning = r.bulkActionProgress.Status == bulkActionRunning
		r.bulkActionProgress.mu.RUnlock()
	}
	if bulkRunning || r.backdropRepairRunning {
		r.bulkActionMu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]any{
			"status":  "running",
			"message": "a backdrop repair or bulk action is already in progress",
		})
		return nil, false
	}
	r.backdropRepairRunning = true
	r.bulkActionMu.Unlock()
	return func() {
		r.bulkActionMu.Lock()
		r.backdropRepairRunning = false
		r.bulkActionMu.Unlock()
	}, true
}

// decodePHashBody strictly decodes a single JSON object from the request body
// into dst, writing a 400 and returning ok=false on malformed input. An empty
// body is allowed (EOF) so the caller's zero-value defaults apply.
func decodePHashBody(w http.ResponseWriter, req *http.Request, dst any) bool {
	req.Body = http.MaxBytesReader(w, req.Body, 1<<20)
	dec := json.NewDecoder(req.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return false
	}
	// Reject trailing tokens after the object so `{...}{...}` cannot succeed on
	// only the first object (mirrors handleBulkAction).
	if err := dec.Decode(&struct{}{}); err == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unexpected trailing data in body"})
		return false
	} else if !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return false
	}
	return true
}

// handlePHashMismatchRemediate backs out cross-artist backdrop pollution:
// quarantines the flagged bytes, removes them locally, and deletes the mirrored
// copy from connected platforms. POST
// {basePath}/api/v1/reports/phash-mismatch/remediate. Admin-gated; singleton.
func (r *Router) handlePHashMismatchRemediate(w http.ResponseWriter, req *http.Request) {
	if !r.requireForeignAdmin(w, req) {
		return
	}
	remediator, ok := r.pipeline.(pHashRemediator)
	if !ok {
		// Fail loud: the production pipeline always implements this; a miss is
		// a wiring bug, never a silent no-op (this repo forbids silent-failure
		// capability guards).
		r.logger.Error("pipeline does not implement pHashRemediator; phash-mismatch remediate unavailable")
		http.Error(w, "phash mismatch remediate unavailable", http.StatusInternalServerError)
		return
	}

	var body phashRemediateRequest
	if !decodePHashBody(w, req, &body) {
		return
	}

	// A run must be scoped to one artist OR explicitly library-wide. Reject
	// neither here (the pipeline also enforces it) so a forgotten scope is a
	// 400, never an accidental library-wide delete.
	if body.ArtistID == "" && !body.AllArtists {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "artist_id is required unless all_artists is set",
		})
		return
	}

	scope := rule.PHashMismatchScope{ArtistID: body.ArtistID}
	if body.Tolerance != 0 {
		// math.IsNaN is load-bearing, not belt-and-braces: every IEEE-754
		// comparison against NaN is false, so `t <= 0 || t > 1` ADMITS NaN, and
		// a NaN tolerance defeats the detector's `sim >= tolerance` filter. On a
		// path that deletes files an unusable tolerance is a 400, never a
		// silent fallback to the default the operator did not ask for.
		if math.IsNaN(body.Tolerance) || body.Tolerance <= 0 || body.Tolerance > 1 {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "tolerance must be a number in (0, 1]",
			})
			return
		}
		scope.Tolerance = body.Tolerance
	}

	release, ok := r.claimPHashRepairSlot(w)
	if !ok {
		return
	}
	defer release()

	result, err := remediator.RemediatePHashMismatches(req.Context(), scope, rule.PHashRemediateOpts{
		AllArtists: body.AllArtists,
		DryRun:     body.DryRun,
	})
	if err != nil {
		r.logger.Error("remediating phash mismatches", slog.String("error", err.Error()))
		http.Error(w, "remediation failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handlePHashMismatchRestore puts a repair operation's quarantined backdrops
// back -- locally and on the platforms they were deleted from. POST
// {basePath}/api/v1/reports/phash-mismatch/restore. Admin-gated; singleton.
func (r *Router) handlePHashMismatchRestore(w http.ResponseWriter, req *http.Request) {
	if !r.requireForeignAdmin(w, req) {
		return
	}
	remediator, ok := r.pipeline.(pHashRemediator)
	if !ok {
		// Fail loud: see handlePHashMismatchRemediate above for the rationale.
		r.logger.Error("pipeline does not implement pHashRemediator; phash-mismatch restore unavailable")
		http.Error(w, "phash mismatch restore unavailable", http.StatusInternalServerError)
		return
	}

	var body phashRestoreRequest
	if !decodePHashBody(w, req, &body) {
		return
	}
	if body.ArtistID == "" || body.OpID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "artist_id and op_id are both required",
		})
		return
	}

	release, ok := r.claimPHashRepairSlot(w)
	if !ok {
		return
	}
	defer release()

	result, err := remediator.RestorePHashQuarantine(req.Context(), body.ArtistID, body.OpID)
	if err != nil {
		r.logger.Error("restoring phash quarantine", slog.String("error", err.Error()))
		http.Error(w, "restore failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
