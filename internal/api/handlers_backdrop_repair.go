// Package api -- handlers_backdrop_repair.go
//
// Admin-gated report + remediation for within-artist fanart duplication
// (#2540 PR-2). The rule-engine batch runner lives on *rule.Pipeline (Task
// 1/2: ScanFanartDuplicates, RemediateFanartDuplicates); this file reaches it
// through a narrow capability interface so the many NewPipeline /
// mergeTestRouterWithPipeline test call sites (which construct fakes
// implementing only rule.PipelineRunner) need no interface change.
//
// Route: GET {basePath}/reports/backdrop-duplicates (this file).
// Admin-only (reuses requireForeignAdmin, same gate as /reports/duplicates).
package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/sydlexius/stillwater/internal/rule"
	"github.com/sydlexius/stillwater/web/templates"
)

// fanartDuplicateRepairer is the pipeline capability this handler (and the
// Task 4 remediate endpoint) needs. Defined once here so both handlers share
// a single type assertion against r.pipeline.
type fanartDuplicateRepairer interface {
	ScanFanartDuplicates(ctx context.Context) (rule.FanartDupReport, error)
	RemediateFanartDuplicates(ctx context.Context) (rule.FanartRepairResult, error)
}

// handleBackdropDuplicatesPage renders the admin-gated within-artist fanart
// duplication report (dry-run preview; #2540 PR-2). GET
// {basePath}/reports/backdrop-duplicates.
func (r *Router) handleBackdropDuplicatesPage(w http.ResponseWriter, req *http.Request) {
	if !r.requireForeignAdmin(w, req) {
		return
	}

	repairer, ok := r.pipeline.(fanartDuplicateRepairer)
	if !ok {
		// Fail loud: the production pipeline always implements this; a miss
		// is a wiring bug, never a silent no-op (this repo forbids
		// silent-failure capability guards).
		r.logger.Error("pipeline does not implement fanartDuplicateRepairer; backdrop-duplicates report unavailable")
		http.Error(w, "backdrop duplicate report unavailable", http.StatusInternalServerError)
		return
	}

	report, err := repairer.ScanFanartDuplicates(req.Context())
	if err != nil {
		r.logger.Error("scanning fanart duplicates", slog.String("error", err.Error()))
		http.Error(w, "scan failed", http.StatusInternalServerError)
		return
	}

	// Opportunistic cache refresh (#2608): this page just paid for the full
	// from-disk scan, so hand the sidebar's Images section a fresh library
	// count for free rather than making it wait for the next periodic refresh.
	// A PARTIAL scan is skipped -- ExactRedundantSlots then covers only the
	// reachable half of the library, and caching that undercount would show a
	// confidently wrong number the operator cannot distinguish from the truth.
	if report.ScanErrors == 0 {
		r.storeLibraryDupCount(report.ExactRedundantSlots)
	}

	renderTempl(w, req, templates.BackdropDuplicatesPage(r.assetsFor(req), buildBackdropDuplicatesView(report)))
}

// handleBackdropDuplicatesRemediate collapses exact within-artist fanart
// duplicates library-wide (#2540 PR-2 Task 4). POST
// /api/v1/reports/backdrop-duplicates/remediate. Admin-gated via
// requireForeignAdmin (same gate as the report page); singleton via
// r.backdropRepairRunning, guarded by the same r.bulkActionMu the bulk-action
// singleton uses, so a concurrent request AND a concurrent bulk action
// (fetch_images/run_rules, which also writes/renumbers fanart rows) both get
// 409 rather than racing the batch collapse. Responds with JSON for API callers and sets
// HX-Refresh so the HTMX button on the report page reloads to the
// now-clean state (mirrors the HX-Refresh convention used across
// handlers_image.go / handlers_connection.go for HTMX-triggered mutations).
func (r *Router) handleBackdropDuplicatesRemediate(w http.ResponseWriter, req *http.Request) {
	if !r.requireForeignAdmin(w, req) {
		return
	}

	repairer, ok := r.pipeline.(fanartDuplicateRepairer)
	if !ok {
		// Fail loud: see handleBackdropDuplicatesPage above for the rationale.
		r.logger.Error("pipeline does not implement fanartDuplicateRepairer; remediate unavailable")
		http.Error(w, "remediation unavailable", http.StatusInternalServerError)
		return
	}

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
			"message": "a fanart duplicate repair or bulk action is already in progress",
		})
		return
	}
	r.backdropRepairRunning = true
	r.bulkActionMu.Unlock()
	defer func() {
		r.bulkActionMu.Lock()
		r.backdropRepairRunning = false
		r.bulkActionMu.Unlock()
	}()

	result, err := repairer.RemediateFanartDuplicates(req.Context())
	if err != nil {
		r.logger.Error("remediating fanart duplicates", slog.String("error", err.Error()))
		http.Error(w, "remediation failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("HX-Refresh", "true")
	writeJSON(w, http.StatusOK, map[string]any{
		"artists_processed": result.ArtistsProcessed,
		"slots_removed":     result.SlotsRemoved,
		"failures":          len(result.Failures),
	})
}

// buildBackdropDuplicatesView converts the rule-engine report into the
// template's view model. Extracted as a named function so tests can exercise
// the conversion independently of HTTP plumbing, mirroring
// buildArtistDuplicatesView's split for the near-duplicate-artist report.
func buildBackdropDuplicatesView(report rule.FanartDupReport) templates.BackdropDuplicatesPageView {
	rows := make([]templates.BackdropDupArtistRow, 0, len(report.PerArtist))
	for _, a := range report.PerArtist {
		rows = append(rows, templates.BackdropDupArtistRow{
			ArtistID:   a.ArtistID,
			Name:       a.Name,
			ExactDrops: a.ExactDrops,
		})
	}
	return templates.BackdropDuplicatesPageView{
		ArtistsAffected:     report.ArtistsAffected,
		ExactRedundantSlots: report.ExactRedundantSlots,
		ScanErrors:          report.ScanErrors,
		Rows:                rows,
	}
}
