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
)

// extraFanartFileResult is one extrafanart/ file in a run. File and Destination
// are base names; the artist entry above them says where they live.
type extraFanartFileResult struct {
	File        string `json:"file"`
	Destination string `json:"destination,omitempty"`
	// Outcome is planned, skipped or blocked.
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
	// Status: nothing_to_do | planned | blocked | failed (the run stopped early) | running.
	Status           string                    `json:"status"`
	ArtistsScanned   int                       `json:"artists_scanned"`
	ArtistsWithFiles int                       `json:"artists_with_files"`
	Planned          int                       `json:"planned"` // files a dry run would move
	SkippedIdentical int                       `json:"skipped_identical"`
	Problems         int                       `json:"problems"` // blocked files and planning errors
	Artists          []extraFanartArtistResult `json:"artists"`
	Error            string                    `json:"error,omitempty"`

	// aborted is set when the run stopped early (a lookup failed or the context
	// ended) rather than finishing. Not part of the body; it only steers Status.
	aborted bool
}

// finish derives Status. A preview cannot have changed anything, so it is never
// reported partial, however many problems it found (the platform prune's #3157
// F1 lesson).
func (res *extraFanartRunResult) finish() {
	moves := res.Planned
	switch {
	case res.aborted && res.DryRun:
		res.Status = "failed" // never "partial": a dry run changed nothing
	case res.DryRun && moves > 0:
		res.Status = "planned"
	case res.DryRun && res.Problems > 0:
		res.Status = "blocked"
	case moves == 0:
		res.Status = "nothing_to_do"
	}
}

// runExtraFanartMigration plans the migration (preview only in this version) for
// every artist that has a filesystem path. It always returns a non-nil result
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

// migrateOneArtist plans one artist, folding the outcome into res. Only the
// preview path exists in this version; dryRun is kept for the live run. A failure is recorded and the run continues.
func (r *Router) migrateOneArtist(ctx context.Context, a *artist.Artist, names []string, kodi, dryRun bool, res *extraFanartRunResult) {
	plan, err := img.PlanExtraFanartMigration(ctx, a.Path, names, kodi)
	if err != nil {
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

// handleExtraFanartMigrationRun runs the migration. POST
// /api/v1/reports/extrafanart-migration. Admin-gated; singleton (409).
func (r *Router) handleExtraFanartMigrationRun(w http.ResponseWriter, req *http.Request) {
	if !r.requireForeignAdmin(w, req) {
		return
	}
	dryRun, ok := decodeExtraFanartRequest(w, req)
	if !ok {
		return
	}
	// Only the preview exists in this version. A live request is refused BEFORE
	// any work: no singleton, no filesystem access. dry_run is echoed truthfully.
	if !dryRun {
		writeJSON(w, http.StatusNotImplemented, extraFanartRunResult{
			DryRun: false, Status: "failed", Artists: []extraFanartArtistResult{},
			Error: "running the extrafanart migration is not available in this version; use dry_run=true to preview",
		})
		return
	}
	runCtx := req.Context()
	res, err := r.runExtraFanartMigration(runCtx, dryRun)
	status := http.StatusOK
	switch {
	case errors.Is(err, errExtraFanartRunning):
		status = http.StatusConflict
		res.Error = "an extrafanart migration is already in progress"
	case err != nil:
		r.logger.Error("extrafanart migration failed", slog.Bool("dry_run", dryRun), slog.String("error", err.Error()))
		status = http.StatusInternalServerError
		res.Error = "extrafanart migration failed"
	}

	writeJSON(w, status, res)
}
