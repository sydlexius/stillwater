package api

// handlers_artist_duplicates_ignored.go -- the manage-ignored surface for the
// duplicates report (#2219 remainder, folds #2220). #2227 shipped the ignore
// (POST .../duplicates/ignore); this file adds the read + restore side:
//
//	GET    /api/v1/artists/duplicates/ignored        -- JSON list of ignored groups
//	DELETE /api/v1/artists/duplicates/ignored/{id}    -- un-ignore (restore) one group
//	GET    /reports/duplicates/ignored                -- the manage page (HTML)
//
// The restore path deletes the signature row and invalidates duplicatesCount so
// the sidebar duplicates pill RE-INCREMENTS on the next poll (the mirror of the
// ignore path's invalidate). Restoring adds no count-specific logic: because the
// page list and the pill both read the ignored set through the same
// artist.FilterIgnoredGroups, dropping the row makes the group reappear in both
// at once. Modeled on the foreign-file allowlist manager (handlers_foreign_files.go:
// handleForeignAllowlistList / handleForeignAllowlistRemove / handleForeignAllowlistPage).

import (
	"errors"
	"net/http"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/web/templates"
)

// loadIgnoredDuplicatesView builds the manage-view model from the ledger.
// Extracted (like loadForeignAllowlistView) so tests can exercise the
// data-loading + row-construction without wiring the full page Router. Returns a
// zero view when r.db is nil so a DB-less test still renders the empty state.
func (r *Router) loadIgnoredDuplicatesView(req *http.Request) (templates.ArtistDuplicatesIgnoredPageView, error) {
	view := templates.ArtistDuplicatesIgnoredPageView{}
	if r.db == nil {
		return view, nil
	}
	groups, err := artist.LoadIgnoredGroups(req.Context(), r.db)
	if err != nil {
		return view, err
	}
	view.Rows = make([]templates.IgnoredDuplicateGroupRow, 0, len(groups))
	for i := range groups {
		g := &groups[i]
		view.Rows = append(view.Rows, templates.IgnoredDuplicateGroupRow{
			ID:          g.ID,
			GroupKey:    g.GroupKey,
			Reason:      g.Reason,
			MemberCount: g.MemberCount(),
			CreatedAt:   g.CreatedAt,
		})
	}
	return view, nil
}

// handleArtistDuplicatesIgnoredPage renders /reports/duplicates/ignored, the
// manage-ignored view. Admin-only via requireForeignAdmin (same gate as the
// duplicates page and the foreign-allowlist page). A nil DB renders the empty
// view rather than erroring, matching handleArtistDuplicatesPage.
func (r *Router) handleArtistDuplicatesIgnoredPage(w http.ResponseWriter, req *http.Request) {
	if !r.requireForeignAdmin(w, req) {
		return
	}
	view, err := r.loadIgnoredDuplicatesView(req)
	if err != nil {
		r.logger.Error("listing ignored duplicate groups for page", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	renderTempl(w, req, templates.ArtistDuplicatesIgnoredPage(r.assetsFor(req), view))
}

// handleArtistDuplicatesIgnoredList returns the JSON list of ignored groups.
// Route-level middleware.RequireAdmin gates it (mirrors handleForeignAllowlistList);
// a nil DB is a 503 so an API caller gets a structured envelope.
//
// GET /api/v1/artists/duplicates/ignored
func (r *Router) handleArtistDuplicatesIgnoredList(w http.ResponseWriter, req *http.Request) {
	if r.db == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error":   "unavailable",
			"message": "database not configured",
		})
		return
	}
	groups, err := artist.LoadIgnoredGroups(req.Context(), r.db)
	if err != nil {
		r.logger.Error("listing ignored duplicate groups", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error":   "internal",
			"message": "see server logs",
		})
		return
	}
	// Project to a stable snake_case wire shape rather than leaking the domain
	// struct's field names; member_count is derived so the caller need not parse
	// the signature. The raw signature is included for parity with the ignore
	// response (which returns the signature it persisted).
	items := make([]map[string]any, 0, len(groups))
	for i := range groups {
		g := &groups[i]
		items = append(items, map[string]any{
			"id":           g.ID,
			"signature":    g.Signature,
			"group_key":    g.GroupKey,
			"reason":       g.Reason,
			"member_count": g.MemberCount(),
			"created_at":   g.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "count": len(items)})
}

// handleArtistDuplicatesRestore un-ignores (restores) one group by its id, so it
// resurfaces on the duplicates page and in the sidebar count. Route-level
// middleware.RequireAdmin gates it; the global CSRF middleware validates the
// mutating DELETE. On success the count cache is invalidated (mirror of the
// ignore path) so the pill re-increments, and the refreshed manage table is
// returned for the HTMX outerHTML swap.
//
// DELETE /api/v1/artists/duplicates/ignored/{id}
func (r *Router) handleArtistDuplicatesRestore(w http.ResponseWriter, req *http.Request) {
	if r.db == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error":   "unavailable",
			"message": "database not configured",
		})
		return
	}
	id := req.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid_request",
			"message": "missing id",
		})
		return
	}
	if err := artist.RestoreDuplicateGroup(req.Context(), r.db, id); err != nil {
		if errors.Is(err, artist.ErrIgnoredGroupNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error":   "not_found",
				"message": "ignored duplicate group not found",
			})
			return
		}
		r.logger.Error("restoring ignored duplicate group", "id", id, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error":   "internal",
			"message": "see server logs",
		})
		return
	}

	// Un-ignoring changes the duplicate-group set; drop the sidebar's cached
	// count so the next poll re-includes the restored group and the pill
	// increments. Exact mirror of handleArtistDuplicatesIgnore's invalidate.
	duplicatesCount.invalidate()

	r.renderIgnoredDuplicatesTable(w, req, "rendering ignored-duplicates table after restore failed")
}

// renderIgnoredDuplicatesTable re-lists the ignored groups and renders the
// swappable table partial so the restore action's HTMX response updates the
// table in place, showing the empty-state copy when the last row is removed.
// Mirrors renderForeignAllowlistTable. Caller must have gated on r.db != nil.
func (r *Router) renderIgnoredDuplicatesTable(w http.ResponseWriter, req *http.Request, errLabel string) {
	view, err := r.loadIgnoredDuplicatesView(req)
	if err != nil {
		r.logger.Error("listing ignored duplicate groups for partial render", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error":   "internal",
			"message": "see server logs",
		})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if rerr := templates.ArtistDuplicatesIgnoredTable(view).Render(req.Context(), w); rerr != nil {
		r.logger.Warn(errLabel, "error", rerr)
	}
}
