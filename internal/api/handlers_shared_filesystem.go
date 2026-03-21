package api

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"

	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/web/templates"
)

// SharedFilesystemStatus holds the current shared-filesystem detection state
// returned by the status endpoint and consumed by the notification bar template.
type SharedFilesystemStatus struct {
	HasOverlaps bool                    `json:"has_overlaps"`
	Libraries   []SharedFilesystemEntry `json:"libraries"`
	Dismissed   bool                    `json:"dismissed"` // user chose "don't show again"
}

// SharedFilesystemEntry describes one library with a shared-filesystem overlap.
type SharedFilesystemEntry struct {
	LibraryID   string `json:"library_id"`
	LibraryName string `json:"library_name"`
	Path        string `json:"path"`
	OverlapWith string `json:"overlap_with"`
}

// handleSharedFilesystemStatus returns the current shared-filesystem overlap state.
// For HTMX requests, returns the notification bar HTML partial.
// For API requests, returns JSON.
// GET /api/v1/shared-filesystem/status
func (r *Router) handleSharedFilesystemStatus(w http.ResponseWriter, req *http.Request) {
	status, err := r.buildSharedFilesystemStatus(req.Context())
	if err != nil {
		r.logger.Error("checking shared filesystem status", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// For HTMX requests, render the notification bar partial.
	if isHTMXRequest(req) {
		data := templates.SharedFSBarData{
			HasOverlaps: status.HasOverlaps,
			Dismissed:   status.Dismissed,
		}
		for _, lib := range status.Libraries {
			data.Libraries = append(data.Libraries, templates.SharedFSBarLib{
				Name: lib.LibraryName,
				Path: lib.Path,
			})
		}
		renderTempl(w, req, templates.SharedFilesystemBarContent(data))
		return
	}

	writeJSON(w, http.StatusOK, status)
}

// handleSharedFilesystemDismiss stores a persistent "dismissed" preference
// so the notification bar is not shown again. For HTMX requests, returns
// an empty response so the bar element is removed from the DOM.
// POST /api/v1/shared-filesystem/dismiss
func (r *Router) handleSharedFilesystemDismiss(w http.ResponseWriter, req *http.Request) {
	_, err := r.db.ExecContext(req.Context(), //nolint:gosec // G701: static query, no user input
		`INSERT INTO settings (key, value, updated_at) VALUES ('shared_filesystem.dismissed', 'true', datetime('now'))
		 ON CONFLICT(key) DO UPDATE SET value = 'true', updated_at = datetime('now')`)
	if err != nil {
		r.logger.Error("dismissing shared filesystem warning", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// For HTMX requests, return empty body so hx-swap="outerHTML" removes the bar.
	if isHTMXRequest(req) {
		w.WriteHeader(http.StatusOK)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "dismissed"})
}

// handleSharedFilesystemRecheck forces a re-evaluation of shared-filesystem
// overlaps across all libraries and updates the flags. Useful when library
// paths or connections have changed.
// POST /api/v1/shared-filesystem/recheck
func (r *Router) handleSharedFilesystemRecheck(w http.ResponseWriter, req *http.Request) {
	count, err := r.recheckSharedFilesystem(req.Context())
	if err != nil {
		r.logger.Error("rechecking shared filesystem overlaps", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"overlaps_found": count,
	})
}

// recheckSharedFilesystem re-evaluates all library paths for overlaps and
// updates the shared_filesystem flag on each library. Returns the count
// of libraries with overlaps detected.
func (r *Router) recheckSharedFilesystem(ctx context.Context) (int, error) {
	return library.RecheckOverlaps(ctx, r.libraryService, r.logger)
}

// buildSharedFilesystemStatus assembles the current status by reading library
// flags and the dismiss preference. It calls DetectOverlaps on the full library
// list so that the OverlapWith field is populated for each entry.
func (r *Router) buildSharedFilesystemStatus(ctx context.Context) (*SharedFilesystemStatus, error) {
	sharedLibs, err := r.libraryService.ListSharedFilesystem(ctx)
	if err != nil {
		return nil, err
	}

	status := &SharedFilesystemStatus{
		HasOverlaps: len(sharedLibs) > 0,
		Libraries:   []SharedFilesystemEntry{},
	}

	if len(sharedLibs) > 0 {
		// Run overlap detection on the full library list to obtain the
		// OverlapWith descriptions. ListSharedFilesystem only returns flagged
		// libraries; we need all libraries so that DetectOverlaps can identify
		// which specific library each flagged one conflicts with.
		allLibs, allErr := r.libraryService.List(ctx)
		if allErr != nil {
			return nil, allErr
		}
		overlaps := library.DetectOverlaps(allLibs)

		// Build a lookup from library ID to its OverlapWith description.
		overlapDesc := make(map[string]string, len(overlaps))
		for _, o := range overlaps {
			overlapDesc[o.LibraryID] = o.OverlapWith
		}

		for _, lib := range sharedLibs {
			status.Libraries = append(status.Libraries, SharedFilesystemEntry{
				LibraryID:   lib.ID,
				LibraryName: lib.Name,
				Path:        lib.Path,
				OverlapWith: overlapDesc[lib.ID],
			})
		}
	}

	// Check the dismiss preference. sql.ErrNoRows means the setting has not
	// been stored yet (not dismissed). Any other DB error is logged and treated
	// as "not dismissed" (show the bar) as the safe default.
	var dismissed string
	err = r.db.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = 'shared_filesystem.dismissed'`).Scan(&dismissed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		r.logger.Warn("reading shared_filesystem.dismissed setting", "error", err)
	}
	if err == nil && dismissed == "true" {
		status.Dismissed = true
	}

	return status, nil
}

// recheckSharedFilesystemBackground runs a shared-filesystem recheck in a
// background goroutine so that library/connection mutations do not block on
// the overlap scan. Uses context.WithoutCancel to preserve request-scoped
// values (e.g. auth) while decoupling from the request lifecycle.
func (r *Router) recheckSharedFilesystemBackground(reqCtx context.Context) {
	bgCtx := context.WithoutCancel(reqCtx)
	go func() {
		if _, err := r.recheckSharedFilesystem(bgCtx); err != nil {
			r.logger.Warn("background shared-filesystem recheck failed",
				slog.String("error", err.Error()))
		}
	}()
}
