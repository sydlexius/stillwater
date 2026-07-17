// Package api -- handlers_phash_mismatch.go
//
// Admin-gated, read-only cross-artist backdrop pollution report (#2564 PR-2).
// The detector lives on *rule.Pipeline (ScanPHashMismatches); this file
// reaches it through a narrow capability interface, mirroring
// handlers_backdrop_repair.go, so the many test call sites constructing fakes
// that implement only rule.PipelineRunner need no interface change.
//
// Route: GET {basePath}/api/v1/reports/phash-mismatch
//
//	?artist_id= scopes the probe to one artist (the registry is still
//	            library-wide; see rule.PHashMismatchScope).
//	?tolerance= overrides the similarity cutoff for this scan.
//
// JSON only, by deliberate scope choice: this PR ships the detector and a
// machine-readable report. The operator-facing page belongs with the repair
// UI it drives (the destructive back-out PR), and folding it in here would
// push the diff past the repo's size gate for no detection value.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/sydlexius/stillwater/internal/rule"
)

// pHashMismatchDetector is the pipeline capability this handler needs.
type pHashMismatchDetector interface {
	ScanPHashMismatches(ctx context.Context, scope rule.PHashMismatchScope) (rule.PHashMismatchReport, error)
}

// handlePHashMismatchReport runs the read-only cross-artist pollution scan and
// returns it as JSON. GET {basePath}/api/v1/reports/phash-mismatch.
func (r *Router) handlePHashMismatchReport(w http.ResponseWriter, req *http.Request) {
	if !r.requireForeignAdmin(w, req) {
		return
	}

	detector, ok := r.pipeline.(pHashMismatchDetector)
	if !ok {
		// Fail loud: the production pipeline always implements this; a miss
		// is a wiring bug, never a silent no-op (this repo forbids
		// silent-failure capability guards).
		r.logger.Error("pipeline does not implement pHashMismatchDetector; phash-mismatch report unavailable")
		http.Error(w, "phash mismatch report unavailable", http.StatusInternalServerError)
		return
	}

	scope := rule.PHashMismatchScope{ArtistID: req.URL.Query().Get("artist_id")}
	if raw := req.URL.Query().Get("tolerance"); raw != "" {
		tol, err := strconv.ParseFloat(raw, 64)
		if err != nil || tol <= 0 || tol > 1 {
			// Rejected rather than clamped: a caller who asked for a
			// specific cutoff must not silently get a different one on a
			// report that decides what gets deleted.
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "tolerance must be a number in (0, 1]",
			})
			return
		}
		scope.Tolerance = tol
	}

	report, err := detector.ScanPHashMismatches(req.Context(), scope)
	if err != nil {
		r.logger.Error("scanning phash mismatches", slog.String("error", err.Error()))
		http.Error(w, "scan failed", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, report)
}
