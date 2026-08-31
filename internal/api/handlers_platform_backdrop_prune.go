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
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
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

// platformPruneRequest is the POST body for the prune endpoint.
//
// An EMPTY body is no longer a library-wide prune (#3139). Exactly one of
// ArtistID or AllArtists is required, and the publisher enforces the same
// invariant independently, so a forgotten scope cannot become a library-wide
// delete through either door.
type platformPruneRequest struct {
	// ArtistID scopes the prune to one artist.
	ArtistID string `json:"artist_id"`
	// AllArtists must be set explicitly to prune the whole library.
	AllArtists bool `json:"all_artists"`
}

// decodePlatformPruneRequest reads the scope from either encoding the two
// callers actually use: JSON from the API, and form-encoded from the report
// page's htmx buttons (htmx posts hx-vals as a form body -- no json-enc
// extension is vendored here, and adding one to serve a two-field payload
// would be a dependency bought for nothing).
//
// The form branch parses booleans STRICTLY. An unparsable "all_artists" is a
// 400, never a false: a malformed value must not silently become a DIFFERENT
// request than the operator authorized, on a path that deletes artwork.
func decodePlatformPruneRequest(w http.ResponseWriter, req *http.Request) (platformPruneRequest, bool) {
	var body platformPruneRequest
	ct := req.Header.Get("Content-Type")
	if mt, _, err := mime.ParseMediaType(ct); err == nil && mt == "application/x-www-form-urlencoded" {
		req.Body = http.MaxBytesReader(w, req.Body, 1<<20)
		if err := req.ParseForm(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid form body"})
			return body, false
		}
		body.ArtistID = req.PostFormValue("artist_id")
		for _, f := range []struct {
			name string
			dst  *bool
		}{
			{"all_artists", &body.AllArtists},
		} {
			raw := req.PostFormValue(f.name)
			if raw == "" {
				continue
			}
			v, err := strconv.ParseBool(raw)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid boolean for " + f.name})
				return body, false
			}
			*f.dst = v
		}
		return body, true
	}
	if !decodePHashBody(w, req, &body) {
		return body, false
	}
	return body, true
}

// writePlatformPruneScopeError maps a scope-validation failure to a 400 with
// a FIXED, client-safe message, and logs the wrapped error server-side.
//
// Deliberate rather than passing the validator's own text through: that text
// is written for a maintainer reading a log and can carry internal detail,
// while a response body is client-visible. Each sentinel keeps its own message
// so a caller can still tell the 400s apart -- mirroring
// handlers_phash_repair.go, which spells its scope 400s out for the same
// reason.
func writePlatformPruneScopeError(w http.ResponseWriter, logger *slog.Logger, err error) {
	msg := "invalid prune scope"
	switch {
	case errors.Is(err, publish.ErrPruneScopeMissing):
		msg = "artist_id is required unless all_artists is set"
	case errors.Is(err, publish.ErrPruneScopeAmbiguous):
		msg = "artist_id and all_artists are mutually exclusive"
	}
	// A scope this handler could not classify is a defect in the mapping, not
	// in the request, and it must be visible: the caller still gets a generic
	// 400, but the server log names the error so the missing case is findable.
	if logger != nil {
		logger.Warn("platform backdrop prune: scope rejected", slog.String("error", err.Error()))
	}
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
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

	body, ok := decodePlatformPruneRequest(w, req)
	if !ok {
		return
	}
	scope := publish.PlatformBackdropPruneScope{
		ArtistID:   body.ArtistID,
		AllArtists: body.AllArtists,
	}
	// Reject an unscoped or contradictory request HERE with a 400, before the
	// singleton is claimed. The publisher validates the same scope again and
	// that second check is the load-bearing one -- it holds for every caller,
	// not just this handler -- but a bad request should read as a client error
	// and must not hold the prune slot while it is rejected.
	if err := scope.Validate(); err != nil {
		writePlatformPruneScopeError(w, r.logger, err)
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

	result, err := r.publisher.PrunePlatformBackdropDuplicates(req.Context(), scope)
	if err != nil {
		r.logger.Error("pruning platform backdrop duplicates", slog.String("error", err.Error()))
		// A FAILED prune is not an UNSTARTED prune, and this is the path that
		// makes the difference visible to an operator. The publisher walks the
		// library page by page and deletes as it goes, so its only hard return
		// -- a paging failure -- can fire on page 2 with page 1 already pruned;
		// it returns the partial result alongside the error rather than a zero
		// value precisely so this handler can see that. Falling through to the
		// 500 without invalidating leaves the cached report listing backdrops
		// that are no longer on the platform, which is #3092 surviving on the
		// error path: the operator retries, the page still shows the same rows,
		// and the Prune button stays armed against images that are already
		// gone.
		//
		// CONDITIONAL, not unconditional. The question the caches actually care
		// about is "did this run possibly change the platform", and a run that
		// failed before touching anything has not invalidated anything -- so it
		// should not cost an established report and a fresh 62s sweep. Both
		// terms are load-bearing:
		//   - BackdropsRemoved > 0 is the confirmed case: deletes landed.
		//   - Failures is the AMBIGUOUS case, and it must be included. A delete
		//     whose request errors (a timeout after the platform already
		//     processed it) is recorded as a failure and NOT counted in
		//     BackdropsRemoved, so a run whose single delete timed out would
		//     otherwise skip the invalidation with the platform possibly
		//     already changed. Failures also carries entries that provably
		//     deleted nothing (a platform-ID or connection lookup that failed),
		//     and invalidating for those is deliberately accepted: the cost is
		//     one unnecessary re-sweep, while the cost of the other direction
		//     is showing the operator artwork that no longer exists.
		// The success path below stays unconditional -- there the caller was
		// told the run completed, so a re-sweep is what makes the page agree
		// with the receipt it just got.
		if result.BackdropsRemoved > 0 || len(result.Failures) > 0 {
			r.logger.Warn("platform backdrop prune failed after it may have deleted; invalidating the cached report",
				slog.Int("backdrops_removed", result.BackdropsRemoved),
				slog.Int("failures", len(result.Failures)))
			// Same two caches, for the same reasons the success path spells out
			// below: the report cache is what the page renders from, and the
			// sidebar's dupimages.Cache is a SEPARATE store that the report
			// invalidation does not touch. Skipping the sidebar here would leave
			// the two surfaces permanently disagreeing after a partial prune --
			// the report pending, the pill still claiming a count that includes
			// deleted images -- because that cache's lazy trigger only fires on
			// !Computed, which stays false once any sweep has landed.
			r.invalidatePlatformDupReport()
			r.dupImageCache().TriggerRefresh()
		}
		// #3119: return the partial accounting the publisher handed back
		// alongside the error, rather than a bare "prune failed" string. The
		// success path below returns this same shape; without it, a prune
		// that deleted 40 backdrops and then hit a paging error is
		// indistinguishable, from the response, from one that deleted
		// nothing. "partial" is explicit rather than left to be inferred from
		// backdrops_removed > 0, since a run whose only failures were
		// pre-delete lookups (BackdropsRemoved == 0, Failures > 0) still
		// changed nothing but is not equivalent to never having run.
		errBody := platformPruneResponse(result)
		errBody["error"] = "prune failed"
		errBody["partial"] = result.BackdropsRemoved > 0 || len(result.Failures) > 0
		writeJSON(w, http.StatusInternalServerError, errBody)
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

	writeJSON(w, http.StatusOK, platformPruneResponse(result))
}

// platformPruneResponse renders a prune result as the endpoint's JSON body.
// Shared by the 200 and the 500 so a partial run reports the same shape as a
// complete one.
func platformPruneResponse(result publish.PlatformBackdropPruneResult) map[string]any {
	return map[string]any{
		"artists_processed": result.ArtistsProcessed,
		"backdrops_removed": result.BackdropsRemoved,
		"skipped_changed":   result.SkippedChanged,
		"failures":          len(result.Failures),
	}
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
