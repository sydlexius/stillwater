// Package api -- handlers_extrafanart_migration.go
//
// Operator entry point for the extrafanart/ migration (#3179). The engine that
// moves the files lives in internal/image (PlanExtraFanartMigration and
// ApplyExtraFanartMigration); this file adds NO file-moving logic. It only
// decides when the engine runs: never as a side effect of a sync or a scan,
// only when an administrator asks.
//
// Routes (same shape as the platform backdrop prune, handlers_platform_backdrop_prune.go):
//
//	POST {basePath}/api/v1/reports/extrafanart-migration    admin; singleton; dry_run flag
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	img "github.com/sydlexius/stillwater/internal/image"
)

// extraFanartRunTimeout bounds one whole run so a wedged filesystem read cannot
// hold the singleton slot (the deferred release always runs).
const extraFanartRunTimeout = 10 * time.Minute

// errExtraFanartRunning means another run holds the singleton slot.
var errExtraFanartRunning = errors.New("an extrafanart migration is already in progress")

// Client-visible reason codes. Free text from the engine or the OS can carry
// absolute paths and internals, so a response only ever carries one of these
// fixed codes (a UI can map them to text); the full error goes to the
// server log with the artist and file. The artist and file stay in their own
// structured fields, so every failure is still named.
const (
	reasonIdenticalCopy = "identical_copy" // byte-identical to a root image; left in place
	reasonNotMovedSafe  = "not_moved_safely"
	reasonPlanFailed    = "plan_failed"
	// Live-run codes. reasonFolderGone is the mid-plan mount drop: the folder
	// existed when planned and is gone at apply time, which is a FAILURE for that
	// artist and never a skip (a skip is only decided at plan time).
	reasonFolderGone   = "folder_unavailable"
	reasonMoveFailed   = "move_failed"
	reasonIndexRefresh = "index_refresh_failed" // moved, but stored hashes could not be cleared
	reasonRunStopped   = "run_stopped"          // the run ended before this move was attempted
	// reasonFolderUnreadable: the folder could not be checked (permissions, a stalled
	// mount), which is NOT proof it vanished.
	reasonFolderUnreadable = "folder_unreadable"
	// reasonDirNotRemoved: the files moved but the emptied extrafanart/ folder stayed.
	reasonDirNotRemoved = "directory_not_removed"
)

// extraFanartInvalidateTimeout bounds the post-move hash invalidation. The engine
// runs it on a context that survives a cancel (moved files must be invalidated), so
// without its own deadline a hung database write would hold the shutdown drain open.
const extraFanartInvalidateTimeout = 30 * time.Second

// boundedInvalidator gives every invalidation call its own deadline.
type boundedInvalidator struct {
	inner   img.HashInvalidator
	timeout time.Duration
}

func (b boundedInvalidator) InvalidateImageHashes(ctx context.Context, artistID, imageType string) error {
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	return b.inner.InvalidateImageHashes(ctx, artistID, imageType)
}

func (b boundedInvalidator) InvalidateImageGeometry(ctx context.Context, artistID, imageType string) error {
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	return b.inner.InvalidateImageGeometry(ctx, artistID, imageType)
}

// extraFanartFileResult is one extrafanart/ file in a run. File and Destination
// are base names; the artist entry above them says where they live.
type extraFanartFileResult struct {
	File        string `json:"file"`
	Destination string `json:"destination,omitempty"`
	// Outcome is planned, skipped or blocked in a preview; moved, source_gone,
	// skipped, blocked or failed in a live run.
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"`
}

// extraFanartArtistResult is what happened to one artist's extrafanart/.
type extraFanartArtistResult struct {
	ArtistID string                  `json:"artist_id"`
	Name     string                  `json:"name"`
	Files    []extraFanartFileResult `json:"files"`
	Error    string                  `json:"error,omitempty"`
}

// extraFanartRunResult is the single body shape for EVERY response of the
// POST endpoint. DryRun is set when the value is constructed, before any guard
// can fail, so no early return can answer with a zero-valued dry_run (the
// defect PR #3169 shipped, issue #3179 trap 2).
type extraFanartRunResult struct {
	DryRun bool `json:"dry_run"`
	// Status: nothing_to_do | nothing_checked (nothing to do, but some artists were
	// skipped as missing, so not everything was examined) | planned | blocked |
	// failed (the run stopped early) | running. A live run reports migrated,
	// partial (some files moved, some did not) or failed instead of planned/blocked.
	Status         string `json:"status"`
	ArtistsScanned int    `json:"artists_scanned"`
	// ArtistsSkippedMissing counts artists whose folder was not found (an
	// unmounted share, a stale row). Not a problem, but never invisible: the
	// response reports it, so an unreachable library cannot read as migrated.
	ArtistsSkippedMissing int `json:"artists_skipped_missing"`
	// ArtistsWithFiles counts artists with files in extrafanart/, plus artists whose
	// plan failed (their files could not be listed, so they are counted too).
	ArtistsWithFiles int `json:"artists_with_files"`
	Planned          int `json:"planned"` // files a dry run would move
	SkippedIdentical int `json:"skipped_identical"`
	Problems         int `json:"problems"` // blocked files and planning errors
	// Moved and Failed are live-run counts (zero in a preview): files moved, and
	// files that were due to move but did not (blocked, failed, or stranded by a
	// vanished folder). Files left alone as identical stay in SkippedIdentical.
	Moved   int                       `json:"moved"`
	Failed  int                       `json:"failed"`
	Artists []extraFanartArtistResult `json:"artists"`
	Error   string                    `json:"error,omitempty"`

	// aborted is set when the run stopped early (a lookup failed or the context
	// ended) rather than finishing. Not part of the body; it only steers Status.
	aborted bool
}

// finish derives Status. A preview cannot have changed anything, so it is never
// reported partial, however many problems it found (the platform prune's #3157
// F1 lesson).
func (res *extraFanartRunResult) finish() {
	moves := res.Planned
	if !res.DryRun {
		res.finishLive()
		return
	}
	switch {
	case res.aborted && res.DryRun:
		res.Status = "failed" // never "partial": a dry run changed nothing
	case res.DryRun && moves > 0:
		res.Status = "planned"
	case res.DryRun && res.Problems > 0:
		res.Status = "blocked"
	case res.DryRun && res.ArtistsSkippedMissing > 0:
		// Nothing to do among the artists that WERE examined, but some were not:
		// an unmounted library must not read as a verified-empty one.
		res.Status = "nothing_checked"
	case moves == 0:
		res.Status = "nothing_to_do"
	}
}

// finishLive derives the Status of a live run. Anything that did not move
// cleanly is visible in the status: a run with moves AND problems is partial,
// never migrated, and a stopped run that moved files is partial, not failed.
func (res *extraFanartRunResult) finishLive() {
	switch {
	case res.aborted && res.Moved > 0, res.Problems > 0 && res.Moved > 0:
		res.Status = "partial"
	case res.aborted, res.Problems > 0:
		res.Status = "failed"
	case res.Moved > 0:
		res.Status = "migrated"
	case res.ArtistsSkippedMissing > 0:
		res.Status = "nothing_checked"
	default:
		res.Status = "nothing_to_do"
	}
}

// runExtraFanartMigration plans the migration, and applies it when dryRun is
// false, for every artist that has a filesystem path. It always returns a non-nil result
// with DryRun set, even alongside an error, so callers can answer truthfully on
// every path.
func (r *Router) runExtraFanartMigration(ctx context.Context, dryRun bool) (*extraFanartRunResult, error) {
	res := &extraFanartRunResult{DryRun: dryRun, Artists: []extraFanartArtistResult{}}

	r.extraFanartMu.Lock()
	if r.extraFanartRunning {
		r.extraFanartMu.Unlock()
		res.Status = "running"
		return res, errExtraFanartRunning
	}
	r.extraFanartRunning = true
	r.extraFanartMu.Unlock()
	defer func() {
		r.extraFanartMu.Lock()
		r.extraFanartRunning = false
		r.extraFanartMu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(ctx, extraFanartRunTimeout)
	defer cancel()

	// Strict: a failed profile lookup must not become a guessed convention that
	// plans against the wrong names (see fanartNamesStrict).
	// ONE profile lookup supplies both the candidate names and the numbering
	// style, so they cannot disagree and a failure cannot leave the run planning
	// with half a convention (isKodiNumbering swallows its own lookup error).
	names, kodi, err := r.extraFanartConvention(ctx)
	if err != nil {
		res.aborted = true
		res.finish()
		return res, err
	}

	const pageSize = 200
	for page := 1; ; page++ {
		artists, _, lerr := r.artistService.List(ctx, artist.ListParams{Page: page, PageSize: pageSize})
		if lerr != nil {
			// Earlier pages may already have been applied; keep their accounting.
			res.aborted = true
			res.finish()
			return res, lerr
		}
		for i := range artists {
			if artists[i].Path == "" {
				continue
			}
			res.ArtistsScanned++
			r.migrateOneArtist(ctx, &artists[i], names, kodi, dryRun, res)
			// Checked AFTER the artist, not before: a cancel or timeout that lands
			// while an artist is planned (including the last one) makes that plan
			// fail, and the run must then report itself stopped, never complete.
			if cerr := ctx.Err(); cerr != nil {
				res.aborted = true
				res.finish()
				return res, cerr
			}
		}
		if len(artists) < pageSize {
			break
		}
	}
	res.finish()
	return res, nil
}

// extraFanartConvention reads the active platform profile once and returns the
// fanart names to plan against and whether it uses Kodi numbering. Any lookup
// failure is returned, never replaced by a guessed default.
func (r *Router) extraFanartConvention(ctx context.Context) (names []string, kodi bool, err error) {
	profile, err := r.platformService.GetActive(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("resolving active platform profile: %w", err)
	}
	var configured []string
	if profile != nil {
		configured = profile.ImageNaming.NamesForType("fanart")
		kodi = strings.EqualFold(profile.ID, "kodi")
	}
	names, err = img.ResolveFanartNames(configured)
	return names, kodi, err
}

// artistFolderMissing reports whether a planning error just means the artist's
// own folder is gone (an unmounted share, a stale row). Such an artist has
// nothing to migrate, and counting it would make every run report a problem
// forever and never reach "nothing to do". The Stat is what separates that from
// an unreadable extrafanart/ inside a folder that EXISTS, which stays a real
// problem, as do permission errors and everything else. os.Stat follows
// symlinks, so an artist path that is a dangling link counts as missing.
//
// The stat is BOUNDED by ctx (img.StatBounded): on a hard-mounted share that has
// stopped answering, a raw os.Stat would hang past the run timeout and the
// request context. A context error from it is not ErrNotExist, so a canceled or
// timed-out check is never counted as "missing": the artist falls through to the
// plan-failure path, and the ctx check after each artist ends the run 500 failed.
func artistFolderMissing(ctx context.Context, planErr error, artistPath string) bool {
	if !errors.Is(planErr, fs.ErrNotExist) {
		return false
	}
	_, err := img.StatBounded(ctx, artistPath)
	return errors.Is(err, fs.ErrNotExist)
}

// migrateOneArtist plans one artist, folding the outcome into res, and applies
// the plan when dryRun is false. A failure is recorded and the run continues.
func (r *Router) migrateOneArtist(ctx context.Context, a *artist.Artist, names []string, kodi, dryRun bool, res *extraFanartRunResult) {
	plan, err := img.PlanExtraFanartMigration(ctx, a.Path, names, kodi)
	if err != nil {
		if artistFolderMissing(ctx, err, a.Path) {
			r.logger.Info("extrafanart migration: artist folder does not exist; skipping",
				slog.String("artist_id", a.ID), slog.String("artist", a.Name))
			res.ArtistsSkippedMissing++
			return
		}
		r.logger.Warn("extrafanart migration: planning failed", slog.String("artist_id", a.ID),
			slog.String("artist", a.Name), slog.String("error", err.Error()))
		res.ArtistsWithFiles++
		res.Problems++
		res.Artists = append(res.Artists, extraFanartArtistResult{ArtistID: a.ID, Name: a.Name, Files: []extraFanartFileResult{}, Error: reasonPlanFailed})
		return
	}
	if len(plan.Entries) == 0 {
		return // no extrafanart/ files: nothing to report for this artist
	}
	res.ArtistsWithFiles++
	out := extraFanartArtistResult{ArtistID: a.ID, Name: a.Name, Files: make([]extraFanartFileResult, 0, len(plan.Entries))}

	if dryRun {
		for _, e := range plan.Entries {
			f := extraFanartFileResult{File: filepath.Base(e.Source)}
			switch e.Disposition {
			case img.DispositionMove:
				f.Outcome, f.Destination = "planned", filepath.Base(e.Dest)
				res.Planned++
			case img.DispositionSkipIdentical:
				f.Outcome, f.Reason = "skipped", reasonIdenticalCopy
				res.SkippedIdentical++
			default:
				// The engine's blocked reason can be a hash/IO error with a path.
				f.Outcome, f.Reason = "blocked", reasonNotMovedSafe
				r.logger.Warn("extrafanart migration: file blocked in plan", slog.String("artist_id", a.ID),
					slog.String("artist", a.Name), slog.String("file", f.File), slog.String("reason", e.Reason))
				res.Problems++
			}
			out.Files = append(out.Files, f)
		}
		res.Artists = append(res.Artists, out)
		return
	}
	r.applyOneArtist(ctx, a, plan, out, res)
}

// applyOneArtist executes one artist's plan and folds the truthful per-file
// account into res. The engine moves with an atomic no-replace rename (the
// internal/filesystem contract: never write a target in place), so there is no
// partial file to clean up.
//
// Deliberately does NOT reuse artistFolderMissing: that helper decides a PLAN-time
// skip. A folder that vanishes between plan and apply (a mount that dropped
// mid-run) strands files that were meant to move, so it is reported as this
// artist failing, never skipped and never counted as success.
func (r *Router) applyOneArtist(ctx context.Context, a *artist.Artist, plan *img.ExtraFanartPlan, out extraFanartArtistResult, res *extraFanartRunResult) {
	if r.extraFanartBeforeApply != nil {
		r.extraFanartBeforeApply(a.ID) // test seam; nil in production
	}
	timeout := r.extraFanartInvalidateTimeout
	if timeout <= 0 {
		timeout = extraFanartInvalidateTimeout
	}
	// A nil invalidator stays nil so the engine refuses it, as before.
	var inv img.HashInvalidator
	if r.fanartInvalidator != nil {
		inv = boundedInvalidator{inner: r.fanartInvalidator, timeout: timeout}
	}
	ar, aerr := img.ApplyExtraFanartMigration(ctx, inv, a.ID, plan)
	if ar == nil { // refused outright (no invalidator or plan): nothing moved
		r.logger.Error("extrafanart migration: apply refused", slog.String("artist_id", a.ID), slog.String("error", aerr.Error()))
		out.Error = reasonMoveFailed
		out.Files = append(out.Files, r.unattempted(plan.Entries, reasonMoveFailed, res)...)
		res.Problems++
		res.Artists = append(res.Artists, out)
		return
	}
	// ONE bounded stat decides whether "source gone" means "already migrated" or
	// "the folder itself is gone". Bounded by a context that outlives a cancel so
	// a run stopped mid-artist still classifies what it did reach.
	folderGone, folderUnreadable := classifyApplyFolder(ctx, a.Path, ar.Results)
	for _, mr := range ar.Results {
		f := extraFanartFileResult{File: filepath.Base(mr.Entry.Source)}
		switch mr.Outcome {
		case img.OutcomeMoved:
			f.Outcome, f.Destination = "moved", filepath.Base(mr.Entry.Dest)
			res.Moved++
		case img.OutcomeSkipped:
			f.Outcome, f.Reason = "skipped", reasonIdenticalCopy
			res.SkippedIdentical++
		case img.OutcomeGone:
			if folderGone || folderUnreadable {
				f.Outcome, f.Reason = "failed", reasonFolderGone
				if folderUnreadable {
					f.Reason = reasonFolderUnreadable
				}
				out.Error = f.Reason
				res.Failed++
			} else {
				f.Outcome = "source_gone" // no longer at its old path: a repeat run, or something else removed it; never counted as moved
			}
		case img.OutcomeBlocked:
			f.Outcome, f.Reason = "blocked", reasonNotMovedSafe
			res.Failed++
			res.Problems++
		default:
			f.Outcome, f.Reason = "failed", reasonMoveFailed
			res.Failed++
			res.Problems++
		}
		if mr.Entry.Disposition == img.DispositionMove {
			res.Planned++
		}
		if mr.Err != nil {
			r.logger.Warn("extrafanart migration: file not moved", slog.String("artist_id", a.ID),
				slog.String("artist", a.Name), slog.String("file", f.File), slog.String("error", mr.Err.Error()))
		}
		out.Files = append(out.Files, f)
	}
	if folderGone || folderUnreadable {
		res.Problems++
		r.logger.Warn("extrafanart migration: artist folder gone or unreadable after planning",
			slog.String("artist_id", a.ID), slog.String("artist", a.Name), slog.Bool("unreadable", folderUnreadable))
	}
	if len(ar.Results) < len(plan.Entries) { // the run was stopped partway through this artist
		out.Files = append(out.Files, r.unattempted(plan.Entries[len(ar.Results):], reasonRunStopped, res)...)
	}
	if ar.InvalidErr != nil || ar.DirErr != nil {
		r.logger.Error("extrafanart migration: follow-up after moving failed", slog.String("artist_id", a.ID),
			slog.String("artist", a.Name), slog.Any("invalidate_error", ar.InvalidErr), slog.Any("dir_error", ar.DirErr))
	}
	if ar.InvalidErr != nil { // stored hashes may now describe different files: say so
		out.Error = reasonIndexRefresh
		res.Problems++
	} else if ar.DirErr != nil && out.Error == "" { // moved, but the emptied folder stayed
		out.Error = reasonDirNotRemoved
		res.Problems++
	}
	res.Artists = append(res.Artists, out)
}

// classifyApplyFolder decides what "source gone" means once an apply has run. Only
// fs.ErrNotExist (or a path that is no longer a directory) proves the folder
// vanished. Any other stat error (permissions, a timeout) proves nothing, so it is
// reported as unreadable rather than guessed to be a drop. The single bounded stat
// runs on a context that outlives a cancel, so a stopped run still classifies what
// it reached, and runs only when some entry was source-gone.
func classifyApplyFolder(ctx context.Context, artistPath string, results []img.MigrationResult) (gone, unreadable bool) {
	for _, mr := range results {
		if mr.Outcome != img.OutcomeGone {
			continue
		}
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		fi, err := img.StatBounded(sctx, artistPath)
		cancel()
		switch {
		case errors.Is(err, fs.ErrNotExist), err == nil && !fi.IsDir():
			return true, false
		case err != nil:
			return false, true
		}
		return false, false
	}
	return false, false
}

// unattempted reports plan entries the engine never applied, keeping each one's
// disposition: identical files stay skipped, blocked files stay blocked (and count
// as problems), and only MOVE entries become failures with the given reason.
func (r *Router) unattempted(entries []img.MigrationEntry, reason string, res *extraFanartRunResult) []extraFanartFileResult {
	out := make([]extraFanartFileResult, 0, len(entries))
	for _, e := range entries {
		f := extraFanartFileResult{File: filepath.Base(e.Source)}
		switch e.Disposition {
		case img.DispositionSkipIdentical:
			f.Outcome, f.Reason = "skipped", reasonIdenticalCopy
			res.SkippedIdentical++
		case img.DispositionMove:
			f.Outcome, f.Reason = "failed", reason
			res.Planned++
			res.Failed++
		default:
			f.Outcome, f.Reason = "blocked", reasonNotMovedSafe
			res.Failed++
			res.Problems++
		}
		out = append(out, f)
	}
	return out
}

// extraFanartRequest is the POST body. DryRun defaults to TRUE when absent: a
// request that forgot the flag gets a rehearsal, never an irreversible move.
type extraFanartRequest struct {
	// DryRun is raw so an omitted field (nil, the default) can be told apart from
	// an explicit null, which is malformed.
	DryRun json.RawMessage `json:"dry_run"`
}

// decodeExtraFanartRequest reads dry_run from JSON (API) or a form body (a
// browser form post). A malformed value is a 400, never a silent default: a
// typo must not turn a rehearsal into a real run. Nothing has run on a 400, so
// those bodies carry no dry_run field.
func decodeExtraFanartRequest(w http.ResponseWriter, req *http.Request) (dryRun, ok bool) {
	var dry *bool
	body := &extraFanartRequest{}
	if mt, _, err := mime.ParseMediaType(req.Header.Get("Content-Type")); err == nil && mt == "application/x-www-form-urlencoded" {
		req.Body = http.MaxBytesReader(w, req.Body, 1<<20)
		if err := req.ParseForm(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid form body"})
			return false, false
		}
		if _, present := req.PostForm["dry_run"]; present {
			v, err := strconv.ParseBool(req.PostForm.Get("dry_run"))
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid boolean for dry_run"})
				return false, false
			}
			dry = &v
		}
	} else if !decodePHashBody(w, req, &body) {
		return false, false
	} else if body == nil { // a top-level JSON null body
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return false, false
	} else if body.DryRun != nil {
		var v bool
		if string(body.DryRun) == "null" || json.Unmarshal(body.DryRun, &v) != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid boolean for dry_run"})
			return false, false
		}
		dry = &v
	}
	return dry == nil || *dry, true
}

// extraFanartWriteDeadline is when the response must be written by. The run is
// bounded by extraFanartRunTimeout, but its tail runs past that: two sequential
// hash invalidations (each bounded by extraFanartInvalidateTimeout, on a context
// that survives a cancel), a 10s folder check, and unbounded final directory
// work. The margin covers those bounds plus a minute of slack.
//
// Limit, stated honestly: the deadline covers the run's bounded budget plus the
// bounded post-timeout invalidations. A hard-stalled mount blocks the migration
// itself (the raw Lstat, ReadDir and Rmdir calls take no deadline), and no write
// deadline can deliver a receipt for a run that never returns. That is a
// pre-existing property of those filesystem calls and out of scope here.
func extraFanartWriteDeadline(now time.Time) time.Time {
	return now.Add(extraFanartRunTimeout + 2*extraFanartInvalidateTimeout + 10*time.Second + time.Minute)
}

// extraFanartHTTPStatus derives the response code from the run's outcome, never
// from recomputed counts: 409 while another run holds the slot, 500 when the run
// stopped early (err), 207 when a LIVE run finished but some files did not move
// (status partial or failed), and 200 otherwise. A finished preview is always
// 200, even when it reports blocked files: it changed nothing.
func extraFanartHTTPStatus(res *extraFanartRunResult, err error) int {
	switch {
	case errors.Is(err, errExtraFanartRunning):
		return http.StatusConflict
	case err != nil:
		return http.StatusInternalServerError
	case !res.DryRun && (res.Status == "partial" || res.Status == "failed"):
		return http.StatusMultiStatus
	}
	return http.StatusOK
}

// handleExtraFanartMigrationRun runs the migration. POST
// /api/v1/reports/extrafanart-migration. Admin-gated; singleton (409).
//
//nolint:contextcheck // a preview follows the request; a live run deliberately detaches onto webhookShutdownCtx (see below)
func (r *Router) handleExtraFanartMigrationRun(w http.ResponseWriter, req *http.Request) {
	if !r.requireForeignAdmin(w, req) {
		return
	}
	dryRun, ok := decodeExtraFanartRequest(w, req)
	if !ok {
		return
	}
	// A preview is read-only and follows the request. A live run must NOT: a client
	// disconnect or proxy timeout would leave a half-moved library. It runs on the
	// shutdown-scoped context (stopped only by DrainWebhooks) and webhookWg makes
	// that drain wait for it before the database closes. The singleton lock is
	// held inside runExtraFanartMigration and released on every path.
	runCtx := req.Context()
	if !dryRun {
		r.webhookWg.Add(1)
		defer r.webhookWg.Done()
		runCtx = r.webhookShutdownCtx
	}
	// The server's WriteTimeout (180s) is shorter than a run may legitimately take
	// (extraFanartRunTimeout), and a run that finishes after it would lose its
	// receipt. Extend the deadline for this response only; never fail silently.
	if err := http.NewResponseController(w).SetWriteDeadline(extraFanartWriteDeadline(time.Now())); err != nil {
		r.logger.Warn("extrafanart migration: could not extend the write deadline; a long run may lose its response",
			slog.Bool("dry_run", dryRun), slog.String("error", err.Error()))
	}
	res, err := r.runExtraFanartMigration(runCtx, dryRun)
	status := extraFanartHTTPStatus(res, err)
	switch {
	case errors.Is(err, errExtraFanartRunning):
		res.Error = "an extrafanart migration is already in progress"
	case err != nil:
		r.logger.Error("extrafanart migration failed", slog.Bool("dry_run", dryRun), slog.String("error", err.Error()))
		res.Error = "extrafanart migration failed"
	case status == http.StatusMultiStatus:
		r.logger.Warn("extrafanart migration finished with files that did not move",
			slog.String("status", res.Status), slog.Int("moved", res.Moved),
			slog.Int("failed", res.Failed), slog.Int("problems", res.Problems))
	}

	writeJSON(w, status, res)
}
