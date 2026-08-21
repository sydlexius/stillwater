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
	"time"

	"github.com/sydlexius/stillwater/internal/publish"
	"github.com/sydlexius/stillwater/web/templates"
)

// handlePlatformBackdropDuplicatesPage renders the admin-gated dry-run report
// of redundant backdrops on connected platforms. GET
// {basePath}/reports/platform-backdrop-duplicates.
//
// #3092: this handler used to call ScanPlatformBackdropDuplicates -- a live
// per-artist, per-connection sweep of every connected Emby/Jellyfin --
// synchronously on every render, and block the response on it. Measured at 62s
// against a 1221-artist library with two connections; the operator's browser
// simply waits, and the server's 180s WriteTimeout is the only backstop.
//
// The fix is the one #2684 already applied to the sibling local report
// (handleBackdropDuplicatesPage): never run that sweep on the request path at
// all. The page now reads the cached publish.PlatformBackdropDupReport that
// platformDupCountsFrom already produces for the sidebar's dupimages.Cache
// platform counts -- ONE background sweep, shared by both consumers. A cold
// cache kicks the same single-flight background refresh the sidebar uses and
// answers immediately with a pending notice.
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

	report, at, computed := r.platformDupReportSnapshot()

	// Kick a background refresh when the cache is COLD or the snapshot has aged
	// out. Never blocking: TriggerRefresh returns immediately.
	//
	// The staleness half closes a "frozen forever" gap. Triggering only on
	// !computed means that once ANY sweep lands nothing ever asks for a fresher
	// one -- and maintenance.StartDuplicateImageCountRefresh, which was supposed
	// to cover that, has NO production caller, so the first sweep's numbers
	// would stand for the life of the process. An operator who connects a new
	// Emby would never see it appear.
	//
	// An AGE GATE rather than an unconditional trigger absorbed by the cache's
	// 15-minute retryCooldown: that cooldown is a RETRY FLOOR for a cold cache
	// whose sources keep failing, not a refresh cadence. Using it as one makes
	// Cache.refresh -- which runs BOTH halves, the 62s platform sweep plus the
	// 257s full-library re-hash -- eligible every 15 minutes for as long as an
	// admin leaves this page open: a ~35% duty cycle against a designed cadence
	// of 12h. The age gate decides WHETHER a refresh is warranted; the cooldown
	// stays the floor underneath it.
	if !computed || time.Since(at) > platformDupReportMaxAge {
		r.dupImageCache().TriggerRefresh()
	}

	if !computed {
		// Nothing has ever swept (first boot). Answer immediately with a
		// pending notice rather than blocking this request on a sweep that
		// runs for a minute or more, or rendering zeros -- which would read as
		// "every connected platform is clean", a claim nothing has
		// established.
		renderTempl(w, req, templates.PlatformBackdropDuplicatesPage(r.assetsFor(req), templates.PlatformBackdropDuplicatesPageView{
			Unavailable:       true,
			UnavailableReason: "pending",
		}))
		return
	}

	// Computed but possibly stale: render what we have, with its AsOf stamp,
	// while the refresh kicked above (if any) lands in the background. Stale
	// data plus an honest "as of" beats a pending notice that discards a real
	// measurement.

	view := buildPlatformBackdropDuplicatesView(report)
	view.AsOf = at
	renderTempl(w, req, templates.PlatformBackdropDuplicatesPage(r.assetsFor(req), view))
}

// platformDupReportMaxAge is how old the cached sweep may get before a page
// load asks for a fresh one in the background. It mirrors the duplicate-image
// refresh's designed cadence (maintenance.defaultDupImageCountInterval, 12h) --
// restated rather than imported because that constant is unexported, and
// because this is a ceiling on staleness, not a schedule.
//
// Far above dupimages.Cache's 15-minute retryCooldown on purpose: the two
// answer different questions. See the call site.
const platformDupReportMaxAge = 12 * time.Hour

// storePlatformDupReport records report as the cached snapshot backing GET
// /reports/platform-backdrop-duplicates (#3092). Called by
// platformDupCountsFrom -- the background sweep shared with the sidebar's
// dupimages.Cache -- and by nothing on the request path.
//
// sweepStartedAt is when the sweep that produced report BEGAN. Two guards drop
// a write; both exist because a sweep's rows describe the platforms as they
// were when it STARTED, not when it finished.
func (r *Router) storePlatformDupReport(report publish.PlatformBackdropDupReport, sweepStartedAt time.Time) {
	r.platformDupReportMu.Lock()
	defer r.platformDupReportMu.Unlock()

	// A sweep that began before a prune must not resurrect what the
	// prune deleted. Without this, an admin who prunes mid-sweep sees the
	// deleted rows reappear with a FRESH "as of" stamp and a live Prune button
	// offering to delete images that are already gone. `!After` rather than
	// `Before` so a sweep starting in the same instant as the invalidation is
	// also dropped: at equal timestamps the ordering is unknowable, and
	// discarding a good sweep costs one refresh cycle while keeping a bad one
	// costs correctness.
	if !r.platformDupReportInvalidatedAt.IsZero() && !sweepStartedAt.After(r.platformDupReportInvalidatedAt) {
		r.logger.Debug("discarding platform backdrop duplicate report from a sweep that began before the last prune",
			slog.Time("sweep_started_at", sweepStartedAt),
			slog.Time("invalidated_at", r.platformDupReportInvalidatedAt))
		return
	}

	// A TOTAL OUTAGE must not blank an established report. When a platform is
	// unreachable every per-artist query fails, so the sweep returns an EMPTY
	// PerArtist with err == nil and a high ScanErrors. Storing that over real
	// rows replaces them with "no redundant backdrops detected", and nothing
	// re-triggers until the snapshot ages out 12h later -- so the operator loses
	// the report for half a day during a transient outage. This is the
	// report-side twin of the guard platformDupCountsFrom applies to the counts
	// (ErrPartialScan, AC-5 of #3092).
	//
	// Narrow on purpose: only a sweep that saw NOTHING while reporting errors is
	// refused, and only when there is something established to protect. A
	// partial sweep that still returned rows is stored, because the page renders
	// its own partial-scan notice and can say the result is incomplete -- and a
	// first-ever sweep is stored even when total, since "an admittedly partial
	// report" beats "pending forever" while a platform is down.
	if report.ScanErrors > 0 && len(report.PerArtist) == 0 && len(r.platformDupReport.PerArtist) > 0 {
		r.logger.Warn("platform backdrop sweep saw no rows while reporting errors; keeping the established report",
			slog.Int("scan_errors", report.ScanErrors),
			slog.Int("established_rows", len(r.platformDupReport.PerArtist)))
		return
	}

	r.platformDupReport = report
	r.platformDupReportAt = time.Now()
}

// invalidatePlatformDupReport drops the cached snapshot, returning the page to
// its never-computed state so the next render shows the pending notice and
// kicks a fresh sweep. Called after a prune, whose deletions make every cached
// row a claim about images that are no longer there.
func (r *Router) invalidatePlatformDupReport() {
	r.platformDupReportMu.Lock()
	defer r.platformDupReportMu.Unlock()
	r.platformDupReport = publish.PlatformBackdropDupReport{}
	r.platformDupReportAt = time.Time{}
	// Stamped so a sweep already in flight -- which read the platforms BEFORE
	// the deletes -- cannot land afterwards and resurrect the pruned rows. See
	// storePlatformDupReport's first guard.
	r.platformDupReportInvalidatedAt = time.Now()
}

// platformDupReportSnapshot returns the cached report and when it was taken.
// ok is false until the first sweep has ever landed, which is the page's
// signal to render the pending notice instead of a table of zeros (rendering
// zeros here would read as "every connected platform is clean", a claim
// nothing has actually established).
func (r *Router) platformDupReportSnapshot() (report publish.PlatformBackdropDupReport, at time.Time, ok bool) {
	r.platformDupReportMu.RLock()
	defer r.platformDupReportMu.RUnlock()
	return r.platformDupReport, r.platformDupReportAt, !r.platformDupReportAt.IsZero()
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

	// INVALIDATE the cached report (#3092). The page renders from cache, so
	// without this the reload the prune button fires would show the STALE
	// pre-prune rows -- indistinguishable, on screen, from a prune that
	// silently did nothing.
	//
	// Invalidate rather than RE-SWEEP, unlike the sibling report's
	// post-remediation rescan (#2684). That rescan is affordable because its
	// handler first extends the write deadline to 30 minutes. This one has no
	// work deadline at all (deliberate; see platformPruneMu in router.go), and
	// a 62s sweep on top of a prune of comparable cost would push the response
	// past the server's 180s WriteTimeout -- the work completes, the write then
	// fails, and the operator gets an empty body after their artwork is already
	// deleted. That is the worst possible receipt.
	//
	// So the page falls back to pending and the operator's reload kicks a fresh
	// sweep through the cold-cache path. KNOWN CONSEQUENCE: TriggerRefresh
	// honors the cache's 15-minute lazy cooldown, so a sweep shortly before the
	// prune can leave the page pending until it lapses. That is the cooldown
	// doing its job, and "not yet measured" still beats confidently listing
	// deleted images.
	r.invalidatePlatformDupReport()

	// The sidebar's platform counts are a SEPARATE cache (dupimages.Cache) and
	// the invalidation above does not touch them, so without this they keep
	// showing the pre-prune number indefinitely: that cache's lazy trigger only
	// fires on !Computed, which stays false once any sweep has landed, and the
	// periodic refresh meant to catch it has no production caller. The two
	// surfaces would then permanently disagree -- the report says "not yet
	// measured", the sidebar pill still claims N duplicates that were deleted.
	//
	// TriggerRefresh, not Refresh: fire-and-forget, so the prune's response is
	// not held for a sweep. It is single-flight and cooldown-guarded, so at
	// worst this is a no-op -- which is why it is safe to call unconditionally
	// here even though the cooldown may defer the actual re-count.
	r.dupImageCache().TriggerRefresh()

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
