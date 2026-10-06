// Package api -- handlers_extrafanart_page.go
//
// The operator page for the extrafanart/ migration (#3179):
//
//	GET {basePath}/reports/extrafanart-migration    admin; a PREVIEW only
//
// The page reads what the migration would do and writes nothing. The move
// itself stays on the POST endpoint in handlers_extrafanart_migration.go.
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
// stale snapshot. The dry run holds the migration's singleton slot while it
// runs, so a second load during it (or during a live run) sees the running notice.
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
	res, err := r.runExtraFanartMigration(req.Context(), true)
	view := extraFanartView(res, r.assetsFor(req).BasePath)
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
	renderTempl(w, req, templates.ExtraFanartMigrationPage(r.assetsFor(req), view))
}

// extraFanartView converts a run result into the template's view model.
func extraFanartView(res *extraFanartRunResult, basePath string) templates.ExtraFanartMigrationView {
	v := templates.ExtraFanartMigrationView{
		BasePath: basePath, SkippedMissing: res.ArtistsSkippedMissing, Aborted: res.aborted, Status: res.Status,
		ArtistsWithFiles: res.ArtistsWithFiles, Moves: res.Planned,
		SkippedIdentical: res.SkippedIdentical, Problems: res.Problems,
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
