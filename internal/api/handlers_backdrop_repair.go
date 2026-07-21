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
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/sydlexius/stillwater/internal/dupimages"
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
//
// #2684: this handler used to call ScanFanartDuplicates -- a full-library,
// from-disk fanart rehash -- synchronously on every render. That scan is not
// context-aware at the point it actually blocks (image.HashFile does a bare
// os.Open/io.ReadAll with no ctx param), so a bounded context around the call
// cannot save it from a stalled network-mounted read, and it contends for the
// single SQLite connection with any other write-heavy background job
// (library populate/re-sync, a bulk action, ...) regardless of which specific
// subsystem happens to be running one. Measured at 257.79s / 0 bytes against
// a 1226-artist library, against 0.044s for the JSON API serving the same
// report from that scan's cached output.
//
// The fix is to never run that scan on the request path at all. This page now
// reads the same cached rule.FanartDupReport that libraryDupCount (in
// handlers_duplicate_images_nav.go) already produces for the sidebar's
// dupimages.Cache library-duplicate count -- one background scan, shared by
// both consumers, exactly the pattern #2608 established for the sidebar
// itself ("Either one on a 60s sidebar poll would be catastrophic").
func (r *Router) handleBackdropDuplicatesPage(w http.ResponseWriter, req *http.Request) {
	if !r.requireForeignAdmin(w, req) {
		return
	}

	report, at, computed := r.backdropDupReportSnapshot()
	if !computed {
		// Cold cache: nothing has scanned yet (first boot, or the periodic
		// refresh has not ticked). Kick the same background refresh the
		// sidebar already uses -- single-flight and cooldown-guarded inside
		// dupimages.Cache, so a burst of page loads/polls triggers at most
		// one scan -- and answer immediately with a pending notice rather
		// than blocking this request on a scan that can run for minutes.
		r.dupImageCache().TriggerRefresh()
		renderTempl(w, req, templates.BackdropDuplicatesPage(r.assetsFor(req), templates.BackdropDuplicatesPageView{
			Unavailable:       true,
			UnavailableReason: "pending",
		}))
		return
	}

	view := buildBackdropDuplicatesView(report)
	view.AsOf = at
	renderTempl(w, req, templates.BackdropDuplicatesPage(r.assetsFor(req), view))
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

	// Extend this response's write deadline BEFORE the expensive work. The
	// server's 180s WriteTimeout (internal/server/listeners.go) is a deadline on
	// the CONNECTION, not a cancellation of the handler, and this POST does a
	// full remediation pass plus a rescan -- the scan alone measured 257s
	// against a 1226-artist library. Without this the work completes and only
	// then fails to write, so the operator gets an empty body after their
	// artwork has already been collapsed: the worst possible receipt.
	//
	// FINITE, not cleared, for the reason established in #2685: a zero deadline
	// removes the only bound on the final write, and a client that stops reading
	// without closing its socket would then block this handler forever -- which
	// here would also hold backdropRepairRunning, freed by the deferred unlock
	// above, and 409 every later remediation for the life of the process.
	if err := http.NewResponseController(w).
		SetWriteDeadline(time.Now().Add(backdropRemediateWriteTimeout)); err != nil {
		r.logger.Error("could not extend write deadline for fanart duplicate remediation; "+
			"a long run may complete but fail to return its report",
			slog.Any("error", err))
	}

	result, err := repairer.RemediateFanartDuplicates(req.Context())
	if err != nil {
		r.logger.Error("remediating fanart duplicates", slog.String("error", err.Error()))
		http.Error(w, "remediation failed", http.StatusInternalServerError)
		return
	}

	// Rescan and refresh the cached report (#2684). The GET page now renders
	// from cache rather than scanning, so without this the HX-Refresh reload
	// below would show the STALE pre-remediation duplicates -- indistinguishable
	// from "remediation silently did nothing". This is the deliberate
	// exception to "the page never scans on render": remediation is an
	// explicit operator-triggered POST the admin is already waiting on (the
	// same reasoning #2685 used to lift the write deadline on the
	// registry-repair route), so paying for one more scan here is acceptable
	// in a way it is not on the GET path. Best-effort: a failed rescan just
	// leaves the cache as it was, logged, and the next background refresh
	// (periodic or lazy) will catch it up.
	// Route the rescan through the dupimages cache's Refresh rather than calling
	// ScanFanartDuplicates directly. Refresh shares ONE single-flight latch with
	// the periodic and lazy background refreshes, and libraryDupCount -- the
	// function it invokes -- already performs exactly this scan and caches both
	// the full report and the sidebar count. Calling the scanner directly here
	// bypassed that latch, so an operator remediating during a periodic refresh
	// launched a SECOND concurrent full-library re-hash contending for the
	// single SQLite connection (SetMaxOpenConns(1)) -- the same doubled-I/O
	// failure beginRefresh was written to prevent.
	//
	// ErrRefreshInFlight means a scan is already running and this one dropped.
	// That scan may have STARTED before the remediation, in which case
	// storeBackdropDupReport discards its result as stale and the page keeps
	// showing pre-remediation counts until the next refresh. Surfaced at Warn
	// rather than papered over, because "your remediation worked but the page
	// has not caught up yet" is exactly what an operator needs told.
	if rerr := r.dupImageCache().Refresh(req.Context()); rerr != nil {
		if errors.Is(rerr, dupimages.ErrRefreshInFlight) {
			r.logger.Warn("post-remediation rescan dropped: a duplicate-image refresh was already running; " +
				"the report page may show pre-remediation counts until the next refresh")
		} else {
			r.logger.Warn("re-scanning fanart duplicates after remediation", slog.String("error", rerr.Error()))
		}
	}

	w.Header().Set("HX-Refresh", "true")
	writeJSON(w, http.StatusOK, map[string]any{
		"artists_processed": result.ArtistsProcessed,
		"slots_removed":     result.SlotsRemoved,
		"failures":          len(result.Failures),
	})
}

// backdropRemediateWriteTimeout is the write deadline the remediation POST
// substitutes for the server's 180s default. The remediation pass plus its
// rescan both walk the whole library from disk; the scan alone measured 257s
// against 1226 artists. Finite on purpose (see the call site): it must clear
// the slowest legitimate run while still guaranteeing the handler returns, so
// the remediation slot is always released.
const backdropRemediateWriteTimeout = 30 * time.Minute

// storeBackdropDupReport records report as the cached snapshot backing GET
// /reports/backdrop-duplicates (#2684). Called by libraryDupCount (the
// background scan shared with the sidebar's dupimages.Cache) and by
// handleBackdropDuplicatesRemediate (an explicit post-collapse rescan).
// startedAt is when the scan that produced report BEGAN. A report whose scan
// started no later than the cached one's is DROPPED: scans overlap and finish
// out of order, so accepting the last writer lets a scan that began before a
// remediation land after it and quietly restore the counts the operator just
// collapsed. The operator would then see their own remediation appear to have
// done nothing.
func (r *Router) storeBackdropDupReport(report rule.FanartDupReport, startedAt time.Time) {
	r.backdropDupReportMu.Lock()
	defer r.backdropDupReportMu.Unlock()
	if !r.backdropDupReportStartedAt.IsZero() && !startedAt.After(r.backdropDupReportStartedAt) {
		// Not an error: losing this race is the normal, correct outcome for the
		// older scan. Logged so an operator chasing a "stale" page can see that
		// a result was deliberately discarded rather than silently lost.
		r.logger.Debug("discarding fanart duplicate report from an older scan",
			slog.Time("scan_started_at", startedAt),
			slog.Time("cached_scan_started_at", r.backdropDupReportStartedAt))
		return
	}
	r.backdropDupReport = report
	r.backdropDupReportAt = time.Now()
	r.backdropDupReportStartedAt = startedAt
}

// backdropDupReportSnapshot returns the cached report and when it was taken.
// ok is false until the first scan has ever landed, which is the page's
// signal to render the pending notice instead of a table of zeros (rendering
// zeros here would read as "library is clean", a claim nothing has actually
// established yet).
func (r *Router) backdropDupReportSnapshot() (report rule.FanartDupReport, at time.Time, ok bool) {
	r.backdropDupReportMu.RLock()
	defer r.backdropDupReportMu.RUnlock()
	return r.backdropDupReport, r.backdropDupReportAt, !r.backdropDupReportAt.IsZero()
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
