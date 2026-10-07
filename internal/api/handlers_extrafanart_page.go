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
// operator sees comes from a real dry run on every load, so it cannot be a
// stale snapshot. The dry run takes the page's own preview guard, never the run
// singleton, so an open preview cannot make a live run answer 409. A second load
// during a preview, or any load during a run, sees the running notice.
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
	res, err := r.previewExtraFanartMigration(req.Context())
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

// previewExtraFanartMigration runs the dry run for the page under its own guard.
// It yields (errExtraFanartRunning, so the page shows the running notice) when a
// run or another preview is in progress, and it only reads, so a run that starts
// mid-preview can at worst leave a stale row; it cannot be blocked or written to.
func (r *Router) previewExtraFanartMigration(ctx context.Context) (*extraFanartRunResult, error) {
	res := &extraFanartRunResult{DryRun: true, Artists: []extraFanartArtistResult{}}
	r.extraFanartMu.Lock()
	if r.extraFanartRunning || r.extraFanartPreviewing {
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
	return r.planExtraFanartMigration(ctx, true, res)
}
