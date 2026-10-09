// Package api -- handlers_phash_runs.go
//
// Read-only listing of an artist's cross-artist backdrop back-outs (#2882), so
// an operator can find the op_id that POST .../phash-mismatch/restore needs after
// the remediate response is gone.
//
//	GET {basePath}/api/v1/artists/{id}/backdrop-repairs
//	    -> {"runs": [{op_id, created_at, entries: [...]}, ...]}   (newest first)
//
// Admin-only via requireForeignAdmin, like the remediate/restore routes. It only
// reads manifests, so it does NOT claim the destructive-fanart singleton.
package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/sydlexius/stillwater/internal/rule"
)

// pHashRunLister is the pipeline capability this handler needs. Kept separate
// from pHashRemediator so existing fakes that implement only the two mutating
// methods keep compiling.
type pHashRunLister interface {
	ListPHashRepairRuns(ctx context.Context, artistID string) ([]rule.PHashRepairRun, error)
}

// handlePHashRepairRuns lists one artist's quarantined back-out runs.
func (r *Router) handlePHashRepairRuns(w http.ResponseWriter, req *http.Request) {
	if !r.requireForeignAdmin(w, req) {
		return
	}
	lister, ok := r.pipeline.(pHashRunLister)
	if !ok {
		// Fail loud: the production pipeline always implements this.
		r.logger.Error("pipeline does not implement pHashRunLister; backdrop-repairs list unavailable")
		http.Error(w, "backdrop repair listing unavailable", http.StatusInternalServerError)
		return
	}

	artistID := req.PathValue("id")
	runs, err := lister.ListPHashRepairRuns(req.Context(), artistID)
	if err != nil {
		if rule.IsArtistNotFound(err) {
			http.Error(w, "artist not found", http.StatusNotFound)
			return
		}
		r.logger.Error("listing phash repair runs",
			slog.String("artist_id", artistID), slog.String("error", err.Error()))
		http.Error(w, "listing back-outs failed", http.StatusInternalServerError)
		return
	}
	if runs == nil {
		// Always an array on the wire, never null.
		runs = []rule.PHashRepairRun{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}
