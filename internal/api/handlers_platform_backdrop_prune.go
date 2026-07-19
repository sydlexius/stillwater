// Package api -- handlers_platform_backdrop_prune.go
//
// Admin-gated report (dry-run) for byte-identical duplicate backdrops on
// connected platforms (#2540 remote prune). Distinct from the local
// backdrop-duplicates report (handlers_backdrop_repair.go), which collapses
// within-artist fanart duplication on disk: this one reports redundant
// copies already pushed out to the operator's Emby/Jellyfin connections, in
// preparation for the prune endpoint (Task 7).
//
// Route: GET {basePath}/reports/platform-backdrop-duplicates (this file).
// Admin-only (reuses requireForeignAdmin, same gate as /reports/duplicates
// and /reports/backdrop-duplicates).
//
// Route: POST {basePath}/api/v1/reports/platform-backdrop-duplicates/prune
// (this file). Executes the prune described by the report above; admin-only
// and singleton (409 while a prune is already running), guarded by
// r.platformPruneMu/r.platformPruneRunning.
package api

import (
	"log/slog"
	"net/http"

	"github.com/sydlexius/stillwater/internal/publish"
	"github.com/sydlexius/stillwater/web/templates"
)

// handlePlatformBackdropDuplicatesPage renders the admin-gated dry-run report
// of redundant backdrops on connected platforms. GET
// {basePath}/reports/platform-backdrop-duplicates.
func (r *Router) handlePlatformBackdropDuplicatesPage(w http.ResponseWriter, req *http.Request) {
	if !r.requireForeignAdmin(w, req) {
		return
	}
	if r.publisher == nil {
		// Fail loud: the production router always wires a Publisher; a miss
		// is a wiring bug, never a silent no-op (this repo forbids
		// silent-failure capability guards).
		r.logger.Error("publisher not wired; platform backdrop report unavailable")
		http.Error(w, "platform backdrop report unavailable", http.StatusInternalServerError)
		return
	}

	report, err := r.publisher.ScanPlatformBackdropDuplicates(req.Context())
	if err != nil {
		r.logger.Error("scanning platform backdrop duplicates", slog.String("error", err.Error()))
		http.Error(w, "scan failed", http.StatusInternalServerError)
		return
	}

	// Opportunistic cache refresh (#2608): same rationale as the local
	// backdrop-duplicates page. A partial sweep is skipped -- when a platform
	// is unreachable every per-artist query fails and PerArtist comes back
	// empty with err == nil, which would CLEAR the platform rows and erase a
	// real, still-present duplicate count during a transient outage.
	if report.ScanErrors == 0 {
		r.storePlatformDupCounts(r.bucketByPlatformType(req.Context(), report))
	}

	renderTempl(w, req, templates.PlatformBackdropDuplicatesPage(r.assetsFor(req), buildPlatformBackdropDuplicatesView(report)))
}

// handlePlatformBackdropDuplicatesPrune deletes byte-identical duplicate
// backdrops on connected platforms, high-index-first. POST
// /api/v1/reports/platform-backdrop-duplicates/prune. Admin-gated; singleton
// (409 while a prune is already running).
func (r *Router) handlePlatformBackdropDuplicatesPrune(w http.ResponseWriter, req *http.Request) {
	if !r.requireForeignAdmin(w, req) {
		return
	}
	if r.publisher == nil {
		// Fail loud: see handlePlatformBackdropDuplicatesPage above for the
		// rationale.
		r.logger.Error("publisher not wired; platform backdrop prune unavailable")
		http.Error(w, "prune unavailable", http.StatusInternalServerError)
		return
	}

	r.platformPruneMu.Lock()
	if r.platformPruneRunning {
		r.platformPruneMu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]any{"status": "running", "message": "a platform backdrop prune is already in progress"})
		return
	}
	r.platformPruneRunning = true
	r.platformPruneMu.Unlock()
	defer func() {
		r.platformPruneMu.Lock()
		r.platformPruneRunning = false
		r.platformPruneMu.Unlock()
	}()

	result, err := r.publisher.PrunePlatformBackdropDuplicates(req.Context())
	if err != nil {
		r.logger.Error("pruning platform backdrop duplicates", slog.String("error", err.Error()))
		http.Error(w, "prune failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"artists_processed": result.ArtistsProcessed,
		"backdrops_removed": result.BackdropsRemoved,
		"skipped_changed":   result.SkippedChanged,
		"failures":          len(result.Failures),
	})
}

// buildPlatformBackdropDuplicatesView converts the publish-package scan
// report into the template's view model. Extracted as a named function so
// tests can exercise the conversion independently of HTTP plumbing,
// mirroring buildBackdropDuplicatesView's split for the local report.
func buildPlatformBackdropDuplicatesView(report publish.PlatformBackdropDupReport) templates.PlatformBackdropDuplicatesPageView {
	rows := make([]templates.PlatformBackdropDupRow, 0, len(report.PerArtist))
	for _, a := range report.PerArtist {
		rows = append(rows, templates.PlatformBackdropDupRow{
			ArtistID:   a.ArtistID,
			Name:       a.Name,
			Connection: a.Connection,
			Backdrops:  a.Backdrops,
			Redundant:  a.Redundant,
		})
	}
	return templates.PlatformBackdropDuplicatesPageView{
		ConnectionsAffected: report.ConnectionsAffected,
		ArtistsAffected:     report.ArtistsAffected,
		RedundantBackdrops:  report.RedundantBackdrops,
		ScanErrors:          report.ScanErrors,
		Rows:                rows,
	}
}
