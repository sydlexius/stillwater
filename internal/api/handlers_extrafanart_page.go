// Package api -- handlers_extrafanart_page.go
//
// The operator page for the extrafanart/ migration (#3179):
//
//	GET {basePath}/reports/extrafanart-migration    admin; a PREVIEW only
//
// Loading the page reads what the migration would do and writes nothing. Its Run
// button posts to the endpoint in handlers_extrafanart_migration.go, which does
// the move and answers with this page's body as the receipt.
package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/sydlexius/stillwater/web/templates"
)

// handleExtraFanartMigrationPage renders the admin preview page. The plan the
// operator sees is the cached result of the last finished dry run when there is
// one (#3434), else a real dry run; a live run drops the cache, and a POST dry run
// refreshes it. Cached rows can be stale if files changed behind the server, which
// is why the live run never reads them. The dry run takes the page's own preview
// guard, never the run singleton, so an open preview cannot make a live run
// answer 409. A second load during a preview, or any load during a run, sees the
// running notice.
func (r *Router) handleExtraFanartMigrationPage(w http.ResponseWriter, req *http.Request) {
	if !r.requireForeignAdmin(w, req) {
		return
	}
	// A preview over a large library can outlast the server's WriteTimeout; extend
	// the deadline for this response only, as the POST handler does, and say so
	// when that is not possible.
	if err := http.NewResponseController(w).SetWriteDeadline(extraFanartWriteDeadline(time.Now())); err != nil {
		r.logger.Warn("extrafanart migration: could not extend the write deadline; a long preview may lose its page",
			slog.String("error", err.Error()))
	}
	assets := r.assetsFor(req)
	res, err := r.previewExtraFanartMigration(req.Context(), true)
	view := extraFanartView(res, assets.BasePath)
	switch {
	case errors.Is(err, errExtraFanartRunning):
		view.Running = true
	case err != nil:
		if errors.Is(err, context.Canceled) {
			// The operator closed the tab mid-preview: not a server fault, and nobody
			// is reading, so do not render, log errors or attempt a 500.
			r.logger.Info("extrafanart migration preview canceled by the client")
			return
		}
		r.logger.Error("extrafanart migration preview failed", slog.String("error", err.Error()))
		// view.Aborted is already set from the result (extraFanartView).
	}
	renderTempl(w, req, templates.ExtraFanartMigrationPage(assets, view))
}

// extraFanartView converts a run result into the template's view model.
func extraFanartView(res *extraFanartRunResult, basePath string) templates.ExtraFanartMigrationView {
	v := templates.ExtraFanartMigrationView{
		BasePath: basePath, SkippedMissing: res.ArtistsSkippedMissing, Aborted: res.aborted, Status: res.Status,
		ArtistsWithFiles: res.ArtistsWithFiles, Moves: res.Planned,
		SkippedIdentical: res.SkippedIdentical, Problems: res.Problems,
		Receipt: !res.DryRun, Moved: res.Moved, Failed: res.Failed,
	}
	for _, a := range res.Artists {
		if a.Error != "" {
			v.Rows = append(v.Rows, templates.ExtraFanartMigrationRow{ArtistID: a.ArtistID, Artist: a.Name, Outcome: "failed", Reason: a.Error})
		}
		for _, f := range a.Files {
			v.Rows = append(v.Rows, templates.ExtraFanartMigrationRow{
				ArtistID: a.ArtistID, Artist: a.Name, File: f.File, Dest: f.Destination, Outcome: f.Outcome, Reason: f.Reason,
			})
		}
	}
	return v
}

// previewExtraFanartMigration produces the dry-run result for the page and for the
// POST dry run, under the page's own guard (never the run singleton). It yields
// (errExtraFanartRunning, so the page shows the running notice) when a run or
// another preview is in progress, and it only reads, so a run that starts
// mid-preview can at worst leave a stale row; it cannot be blocked or written to.
//
// useCache true (the page) serves the cached preview when the convention matches;
// false (the POST dry run, i.e. Refresh) always reads the disk. Either way a
// finished clean preview is stored for the next page load. The cache is the only
// place this function keeps anything: the plans themselves are discarded.
func (r *Router) previewExtraFanartMigration(ctx context.Context, useCache bool) (*extraFanartRunResult, error) {
	res := &extraFanartRunResult{DryRun: true, Artists: []extraFanartArtistResult{}}
	// Stamp the generation and the "as of" time BEFORE the first read of anything
	// (the profile lookup below is already a read). If a live run invalidates the
	// cache at any point after this, the generation no longer matches and store()
	// refuses this result, so rows read before that run's moves can never be
	// kept. The stamp is the START time: it must not claim to be fresher than the
	// data, which began to be read now, not when the walk ends.
	gen, begin := r.extraFanartPreview.begin(r.extraFanartClock)
	res.asOf = begin

	ctx, cancel := context.WithTimeout(ctx, extraFanartRunTimeout)
	defer cancel()
	names, kodi, err := r.extraFanartConvention(ctx)
	if err != nil {
		res.aborted = true
		res.finish()
		return res, err
	}
	key := extraFanartConventionKey(names, kodi)

	r.extraFanartMu.Lock()
	if r.extraFanartRunning {
		r.extraFanartMu.Unlock()
		res.Status = "running"
		return res, errExtraFanartRunning
	}
	// A hit is served without taking the preview guard: it reads memory only. The
	// lock order is extraFanartMu then the cache's own lock, never the reverse.
	if useCache {
		if hit, ok := r.extraFanartPreview.lookup(key, r.extraFanartClock()); ok {
			r.extraFanartMu.Unlock()
			return hit, nil
		}
	}
	if r.extraFanartPreviewing {
		r.extraFanartMu.Unlock()
		res.Status = "running"
		return res, errExtraFanartRunning
	}
	r.extraFanartPreviewing = true
	r.extraFanartMu.Unlock()
	defer func() {
		r.extraFanartMu.Lock()
		r.extraFanartPreviewing = false
		r.extraFanartMu.Unlock()
	}()

	res, err = r.walkExtraFanartArtists(ctx, names, kodi, true, res)
	if err == nil {
		if why := r.extraFanartPreview.store(gen, key, begin, res); why != "" {
			r.logger.Info("extrafanart migration: preview not cached",
				slog.String("reason", why), slog.Int("rows", extraFanartRowCount(res)))
		}
	}
	return res, err
}
