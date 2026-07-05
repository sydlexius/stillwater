package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/filesystem"
	"github.com/sydlexius/stillwater/internal/foreign"
	"github.com/sydlexius/stillwater/internal/i18n"
	"github.com/sydlexius/stillwater/web/components"
	"github.com/sydlexius/stillwater/web/templates"
)

// handleForeignFilesCount returns an HTML fragment for the sidebar's Foreign
// Files child link. Admin-only (mirrors handleArtistDuplicatesCount).
//
// GET /api/v1/foreign-files/count
//
// Returns:
//   - empty body when the foreign-file count is zero (HTMX innerHTML swap
//     leaves the parent <li> empty so the child disappears from the nav);
//   - an <a> link populated with the count when count > 0.
//
// ?ch=next: caller is the promoted sidebar; the entry is static so the
// response injects only the count pill span (no href). Stable callers get a
// full <a> link to the canonical /reports/foreign-files path (M55 #1757 PR-6a:
// the foreign-files page was promoted from /settings/foreign-files to the
// Reports hub).
//
// The 403 response uses a JSON envelope (writeJSON) even though the success
// path emits text/html. This is intentional and mirrors handleArtistDuplicatesCount:
// the sidebar template only renders this placeholder for administrators (the
// admin-only gate is in the templ template), so a 403 should never occur in a
// healthy session. HTMX does not swap content on non-2xx responses by default,
// so the JSON body is never shown to the user. The JSON envelope keeps the
// error contract consistent with the rest of /api/v1/ for API clients.
func (r *Router) handleForeignFilesCount(w http.ResponseWriter, req *http.Request) {
	if middleware.RoleFromContext(req.Context()) != "administrator" {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":   "forbidden",
			"message": "administrator role required",
		})
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	count := r.foreignSummaryForBanner(req.Context())
	w.WriteHeader(http.StatusOK)
	if req.URL.Query().Get("ch") == "next" {
		// The sidebar entry is static (always visible); only inject the count pill.
		// Empty response at count==0 clears the pill span without hiding the entry.
		if count > 0 {
			tr := i18n.TFromCtx(req.Context())
			// Localized, count-bearing accessible name (fmt-style %d key, mirroring
			// the tf() interpolation convention). swForeignPillSwap folds this onto
			// the parent nav link's aria-label so screen-reader users hear the
			// number; the link keeps a static aria-label for the collapsed sidebar
			// states, which per ARIA would otherwise override a descendant pill name.
			ariaTmpl := tr.T("nav.reports.foreign.aria")
			aria := ariaTmpl
			if ariaTmpl != "nav.reports.foreign.aria" { // guard a missing key
				aria = fmt.Sprintf(ariaTmpl, count)
			}
			// data-count drives the pulse-on-increase detection; data-aria is the
			// accessible name; title is the calm hover tooltip.
			fmt.Fprintf(w, //nolint:errcheck // Best-effort HTTP write; client disconnect is not actionable
				`<span class="sw-sidebar-count-pill" data-count="%d" data-aria="%s" title="%s">%d</span>`,
				count, html.EscapeString(aria), html.EscapeString(tr.T("nav.reports.foreign.tooltip")), count)
		}
		return
	}
	if count <= 0 {
		return
	}
	label := html.EscapeString(i18n.TFromCtx(req.Context()).T("nav.reports.foreign"))
	href := html.EscapeString(r.basePath + "/reports/foreign-files")
	fmt.Fprintf(w, //nolint:errcheck // Best-effort HTTP write; client disconnect is not actionable
		`<a href="%s" class="sw-sidebar-link sw-sidebar-subnav-link" data-path="/reports/foreign-files" aria-label="%s">`+
			`<span class="sw-sidebar-label">%s</span>`+
			`<span class="sw-sidebar-badge-pill">%d</span>`+
			`</a>`,
		href, label, label, count)
}

// foreignSummaryForBanner is invoked by handleGetConflictBanner and
// handleGetConflicts so the banner state can include the foreign-file
// count without forcing the caller to wire two services together.
// Returns 0 if the repo is unavailable (e.g. DB not configured in tests)
// so the banner degrades silently to its conflict-only behavior. The
// scanner does not yet persist a last-scan timestamp; previously this
// function returned time.Now() in that slot which was misleading
// because callers ignored it anyway, so the timestamp return is dropped
// rather than fabricated.
func (r *Router) foreignSummaryForBanner(ctx context.Context) int {
	if r.foreignRepo == nil {
		return 0
	}
	n, err := r.foreignRepo.Count(ctx)
	if err != nil {
		r.logger.Warn("foreign-file banner count failed", "error", err)
		return 0
	}
	return n
}

// loadForeignFilesView builds the page view from the ledger. Returned as a
// helper (rather than inlined into the page handler) so tests can exercise
// the data-loading + view-construction logic without wiring the full Router
// (static assets, auth service, etc.) needed to render the wrapping page.
// Returns a zero view when the repo is nil so the caller can still render
// the empty-state copy in tests-without-DB scenarios.
func (r *Router) loadForeignFilesView(ctx context.Context) (templates.ForeignFilesPageView, error) {
	view := templates.ForeignFilesPageView{}
	if r.foreignRepo == nil {
		return view, nil
	}
	entries, err := r.foreignRepo.List(ctx)
	if err != nil {
		return view, err
	}
	view.Rows = make([]templates.ForeignFileRow, 0, len(entries))
	for i := range entries {
		view.Rows = append(view.Rows, foreignEntryToRow(&entries[i]))
	}
	view.Count = len(view.Rows)
	return view, nil
}

// loadForeignAllowlistView is the analog for the allowlist page.
func (r *Router) loadForeignAllowlistView(ctx context.Context) (templates.ForeignAllowlistPageView, error) {
	view := templates.ForeignAllowlistPageView{}
	if r.foreignRepo == nil {
		return view, nil
	}
	entries, err := r.foreignRepo.ListAllowlist(ctx)
	if err != nil {
		return view, err
	}
	view.Rows = make([]templates.ForeignAllowlistRow, 0, len(entries))
	for i := range entries {
		e := &entries[i]
		view.Rows = append(view.Rows, templates.ForeignAllowlistRow{
			ID:         e.ID,
			Scope:      string(e.Scope),
			ArtistID:   e.ArtistID,
			ArtistName: e.ArtistName,
			FileName:   e.FileName,
			Note:       e.Note,
			CreatedAt:  e.CreatedAt.Format(time.RFC3339),
		})
	}
	return view, nil
}

// handleForeignFilesPage renders /reports/foreign-files (M55 #1757 PR-6a:
// promoted from the next/ lane to the canonical Reports-hub path; the page is
// channel-agnostic so there is no checkNextChannel guard). Admin-only; the
// management page exposes destructive actions so we mirror the rest of the
// settings UI's RBAC.
func (r *Router) handleForeignFilesPage(w http.ResponseWriter, req *http.Request) {
	if !r.requireForeignAdmin(w, req) {
		return
	}
	view, err := r.loadForeignFilesView(req.Context())
	if err != nil {
		r.logger.Error("listing foreign files for page", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	renderTempl(w, req, templates.ForeignFilesPage(r.assetsFor(req), view))
}

// handleForeignAllowlistPage renders /reports/foreign-files/allowlist (M55
// #1757 PR-6a: promoted from the next/ lane; channel-agnostic, no
// checkNextChannel guard). Admin-only.
//
// Pagination: the handler reads "page" and "page_size" query parameters
// (respecting the user's stored page-size preference via getUserPageSize) and
// slices the full allowlist in-memory. An HTMX request -- triggered by the
// pagination Prev/Next links -- returns only the ForeignAllowlistBody fragment
// so the swap replaces just the table-plus-pager region without re-rendering
// the surrounding chrome.
func (r *Router) handleForeignAllowlistPage(w http.ResponseWriter, req *http.Request) {
	if !r.requireForeignAdmin(w, req) {
		return
	}

	userID := middleware.UserIDFromContext(req.Context())
	page := intQuery(req, "page", 1)
	pageSize := r.getUserPageSize(req.Context(), userID, intQuery(req, "page_size", 0))

	view, err := r.loadForeignAllowlistView(req.Context())
	if err != nil {
		r.logger.Error("listing foreign allowlist for page", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	totalItems := len(view.Rows)
	totalPages := (totalItems + pageSize - 1) / pageSize
	if totalPages < 1 {
		totalPages = 1
	}
	if page < 1 {
		page = 1
	}
	if page > totalPages {
		page = totalPages
	}

	start := (page - 1) * pageSize
	end := start + pageSize
	if start > totalItems {
		start = totalItems
	}
	if end > totalItems {
		end = totalItems
	}
	view.Rows = view.Rows[start:end]

	view.Pagination = components.PaginationData{
		CurrentPage: page,
		TotalPages:  totalPages,
		PageSize:    pageSize,
		TotalItems:  totalItems,
		BaseURL:     r.basePath + "/reports/foreign-files/allowlist",
		// TargetID is "foreign-allowlist-body": pagination links (rendered
		// via NextPagination in ForeignAllowlistBody) swap the whole body
		// fragment (table + pagination) with outerHTML so the keyboard
		// boundary controls remain in the DOM after page navigation.
		TargetID: "foreign-allowlist-body",
	}

	// HTMX pagination requests (Prev/Next clicks from NextPagination) swap
	// the ForeignAllowlistBody fragment (table + pagination controls) as one
	// unit; full-page navigations render the complete page shell.
	if isHTMXRequest(req) {
		renderTempl(w, req, templates.ForeignAllowlistBody(view))
		return
	}
	renderTempl(w, req, templates.ForeignAllowlistPage(r.assetsFor(req), view))
}

// handleForeignFilesList returns the JSON list. Used by API consumers and
// covered by the OpenAPI spec; the HTML pages do not consume it directly.
//
// GET /api/v1/foreign-files
func (r *Router) handleForeignFilesList(w http.ResponseWriter, req *http.Request) {
	if r.foreignRepo == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "foreign-file scanner not configured"})
		return
	}
	entries, err := r.foreignRepo.List(req.Context())
	if err != nil {
		r.logger.Error("listing foreign files", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "listing foreign files"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": entries, "count": len(entries)})
}

// handleForeignFileAllowlist allowlists one specific ledger row (artist-scoped).
// The row is removed from the ledger so the UI hides it immediately; on the
// next scan the allowlist suppresses re-detection.
//
// POST /api/v1/foreign-files/{id}/allowlist
func (r *Router) handleForeignFileAllowlist(w http.ResponseWriter, req *http.Request) {
	if r.foreignRepo == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "foreign-file scanner not configured"})
		return
	}
	id := req.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing id"})
		return
	}
	entry, err := r.foreignRepo.GetByID(req.Context(), id)
	if err != nil {
		if errors.Is(err, foreign.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "foreign-file row not found"})
			return
		}
		r.logger.Error("loading foreign-file row for allowlist", "id", id, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "loading foreign-file row"})
		return
	}
	hash, err := resolveForeignHash(&entry)
	if err != nil {
		r.logger.Error("resolving content hash for allowlist", "id", id, "path", entry.FilePath, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "resolving content hash"})
		return
	}
	if err := r.foreignRepo.AddAllowlist(req.Context(), foreign.AllowlistEntry{
		Scope:       foreign.ScopeArtist,
		ArtistID:    entry.ArtistID,
		FileName:    entry.FileName,
		ContentHash: hash,
		Note:        "added from foreign-files page",
	}); err != nil {
		r.logger.Error("writing artist-scoped allowlist", "id", id, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "writing allowlist"})
		return
	}
	if err := r.foreignRepo.DeleteByID(req.Context(), id); err != nil && !errors.Is(err, foreign.ErrNotFound) {
		// Error not Warn: the user-facing destructive action (allowlist
		// write) succeeded but the ledger cleanup failed, so the row will
		// reappear in the next render. The user is likely to click
		// Allowlist again and hit a different error path. Surface this
		// loudly so the operator notices the partial-state.
		r.logger.Error("removing allowlisted ledger row after successful allowlist write", "id", id, "error", err)
	}
	// Render the refreshed table so HTMX swaps #foreign-files-table in place.
	// Row-level hx-swap="delete" was insufficient: when the last row is
	// removed, only the <tr> disappeared and the empty-state copy plus the
	// bulk-dismiss button stayed stale (#1246 review round 2).
	r.renderForeignFilesTable(w, req, "rendering foreign-files table after allowlist failed")
}

// handleForeignFileDelete deletes the actual file from disk via the
// filesystem package's RemoveFileSafe (atomic rename-then-unlink). On
// success the ledger row is removed; the file is no longer present so it
// cannot be re-detected. Errors propagate as 500.
//
// DELETE /api/v1/foreign-files/{id}/file
func (r *Router) handleForeignFileDelete(w http.ResponseWriter, req *http.Request) {
	if r.foreignRepo == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "foreign-file scanner not configured"})
		return
	}
	id := req.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing id"})
		return
	}
	entry, err := r.foreignRepo.GetByID(req.Context(), id)
	if err != nil {
		if errors.Is(err, foreign.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "foreign-file row not found"})
			return
		}
		r.logger.Error("loading foreign-file row for delete", "id", id, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "loading foreign-file row"})
		return
	}
	if err := filesystem.RemoveFileSafe(entry.FilePath); err != nil {
		// If the file is already gone, treat as success and clean up the
		// ledger row -- the user wanted it gone. RemoveFileSafe wraps the
		// underlying os error with %w so errors.Is(os.ErrNotExist) catches
		// both Lstat-derived and Remove-derived "missing" cases.
		if !errors.Is(err, os.ErrNotExist) {
			r.logger.Error("foreign-file delete failed", "path", entry.FilePath, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "deleting file"})
			return
		}
	}
	if err := r.foreignRepo.DeleteByID(req.Context(), id); err != nil && !errors.Is(err, foreign.ErrNotFound) {
		// Error not Warn: the file is gone from disk but the ledger row
		// remains, so the next scan will not re-detect (file is absent)
		// yet the table still shows the entry. This is a partial-state
		// the operator should see promptly.
		r.logger.Error("removing ledger row after successful disk delete", "id", id, "error", err)
	}
	r.renderForeignFilesTable(w, req, "rendering foreign-files table after delete failed")
}

// handleForeignFilesDismiss bulk-allowlists every currently-active foreign
// file globally. Designed for the banner Dismiss button: one click clears
// the warning state for the rest of the install. Returns the re-rendered
// #foreign-files-table partial (HTML) so HTMX can swap the table container
// in place; HTMX consumers should target #foreign-files-table, not the
// conflict banner.
//
// POST /api/v1/foreign-files/dismiss
func (r *Router) handleForeignFilesDismiss(w http.ResponseWriter, req *http.Request) {
	if r.foreignRepo == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "foreign-file scanner not configured"})
		return
	}
	// ListRaw returns every ledger row (un-collapsed) so the dismiss loop
	// can call DeleteByPath for every (artist_id, file_path) pair. Using the
	// collapsed List here would leave sibling rows behind: when two artists
	// share the same content hash, List returns one representative row but
	// the other row's ledger entry would survive and the banner would not
	// clear until the next scan.
	entries, err := r.foreignRepo.ListRaw(req.Context())
	if err != nil {
		r.logger.Error("listing foreign files for dismiss", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "listing foreign files"})
		return
	}
	seen := map[string]bool{}
	for i := range entries {
		e := &entries[i]
		hash, herr := resolveForeignHash(e)
		if herr != nil {
			// Skip-don't-clear: an unreadable file may be transient
			// (perm flap, mid-write). Surface the failure so a chronic
			// hash failure does not silently swallow every dismiss.
			r.logger.Warn("dismiss content-hash resolve failed", "path", e.FilePath, "error", herr)
			continue
		}
		// AddAllowlist runs once per distinct hash, but DeleteByPath must
		// run for every entry: two files with identical bytes share one
		// allowlist row yet each keep their own ledger row. seen[hash] is
		// set only after AddAllowlist succeeds so a failed write is
		// retried on the next duplicate rather than silently skipped.
		if !seen[hash] {
			if err := r.foreignRepo.AddAllowlist(req.Context(), foreign.AllowlistEntry{
				Scope:       foreign.ScopeGlobal,
				FileName:    e.FileName,
				ContentHash: hash,
				Note:        "bulk dismiss from banner",
			}); err != nil {
				// Skip the ledger delete on failure: clearing the row would hide
				// the warning even though dismissal was not actually persisted,
				// and the file would only re-appear on the next scan cycle.
				r.logger.Warn("dismiss bulk allowlist failed for file", "file", e.FileName, "error", err)
				continue
			}
			seen[hash] = true
		}
		if err := r.foreignRepo.DeleteByPath(req.Context(), e.ArtistID, e.FilePath); err != nil && !errors.Is(err, foreign.ErrNotFound) {
			// Error not Warn: the global allowlist write succeeded but the
			// ledger cleanup failed, so the row stays visible. With many
			// rows in a single dismiss this can cause confusing partial
			// renders; loud logging helps the operator correlate.
			r.logger.Error("dismiss ledger cleanup failed after successful allowlist write", "id", e.ID, "error", err)
		}
	}
	// Render the actual remaining rows so HTMX can swap #foreign-files-table
	// in place. Both AddAllowlist and DeleteByPath in the loop are best-effort
	// (their errors only log a Warn), so the post-loop state is whatever
	// survived. Returning an empty view unconditionally would hide surviving
	// detections until the next refresh and make a partial dismiss look
	// successful (#1246 review round 2).
	r.renderForeignFilesTable(w, req, "rendering foreign-files table after dismiss failed")
}

// renderForeignFilesTable lists the current foreign-files ledger and renders
// the swappable #foreign-files-table partial. Centralized so the row actions
// (allowlist / delete) and the bulk dismiss share the same render path; the
// table fragment is the canonical post-mutation response so HTMX consumers
// see empty-state copy when the last row is removed and the bulk-dismiss
// button disappears at the same moment. Caller is required to have already
// gated on r.foreignRepo != nil; this helper does not re-check.
func (r *Router) renderForeignFilesTable(w http.ResponseWriter, req *http.Request, errLabel string) {
	view, err := r.loadForeignFilesView(req.Context())
	if err != nil {
		r.logger.Error("listing foreign files for partial render", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "listing foreign files"})
		return
	}
	// QOL (M55 #1773): every caller of this helper is a successful mutation
	// (per-row allowlist, per-row delete, bulk dismiss), so the detected-file
	// count just changed. Emit an HX-Trigger so the next/ sidebar count pill
	// (#sidebar-foreign-pill, hx-trigger "... swFFCountChanged from:body")
	// refreshes immediately instead of waiting up to 60s for its poll. htmx
	// dispatches this as a body event on the client; the stable channel has no
	// listener for it, so this is inert there (no stable behavior change). The
	// camelCase event name matches the existing header-driven precedent in this
	// codebase (clobberRecheck / oobeConflictRefresh / logRefresh).
	w.Header().Set("HX-Trigger", "swFFCountChanged")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if rerr := templates.ForeignFilesTable(view).Render(req.Context(), w); rerr != nil {
		r.logger.Warn(errLabel, "error", rerr)
	}
}

// renderForeignAllowlistTable is the analog for the allowlist page; the
// per-row Remove action now also returns a refreshed table partial so the
// empty-state copy appears on last-row removal. Caller must have already
// gated on r.foreignRepo != nil.
func (r *Router) renderForeignAllowlistTable(w http.ResponseWriter, req *http.Request, errLabel string) {
	view, err := r.loadForeignAllowlistView(req.Context())
	if err != nil {
		r.logger.Error("listing foreign allowlist for partial render", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "listing allowlist"})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if rerr := templates.ForeignAllowlistTable(view).Render(req.Context(), w); rerr != nil {
		r.logger.Warn(errLabel, "error", rerr)
	}
}

// handleForeignAllowlistList returns the JSON list of allowlist rows.
//
// GET /api/v1/foreign-file-allowlist
func (r *Router) handleForeignAllowlistList(w http.ResponseWriter, req *http.Request) {
	if r.foreignRepo == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "foreign-file scanner not configured"})
		return
	}
	entries, err := r.foreignRepo.ListAllowlist(req.Context())
	if err != nil {
		r.logger.Error("listing foreign allowlist", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "listing allowlist"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": entries, "count": len(entries)})
}

// handleForeignAllowlistRemove removes a single allowlist row, re-enabling
// detection for the matching files on the next scan.
//
// DELETE /api/v1/foreign-file-allowlist/{id}
func (r *Router) handleForeignAllowlistRemove(w http.ResponseWriter, req *http.Request) {
	if r.foreignRepo == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "foreign-file scanner not configured"})
		return
	}
	id := req.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing id"})
		return
	}
	if err := r.foreignRepo.RemoveAllowlist(req.Context(), id); err != nil {
		if errors.Is(err, foreign.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "allowlist entry not found"})
			return
		}
		r.logger.Error("removing foreign allowlist entry", "id", id, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "removing allowlist entry"})
		return
	}
	r.renderForeignAllowlistTable(w, req, "rendering foreign-allowlist table after remove failed")
}

// requireForeignAdmin renders an inline 403 for non-admin viewers and
// renders the login page for unauthenticated viewers. Mirrors the gate in
// handleSettingsPage so the foreign-files pages stay admin-only.
func (r *Router) requireForeignAdmin(w http.ResponseWriter, req *http.Request) bool {
	if !r.requireAuth(w, req) {
		return false
	}
	if middleware.RoleFromContext(req.Context()) != "administrator" {
		http.Error(w, "Forbidden: administrator role required", http.StatusForbidden)
		return false
	}
	return true
}

// foreignEntryToRow converts a domain entry to its template row shape.
// Pointer receiver keeps copy cost minimal under gocritic's rangeValCopy
// budget after Entry grew to include content_hash and DuplicateCount.
func foreignEntryToRow(e *foreign.Entry) templates.ForeignFileRow {
	return templates.ForeignFileRow{
		ID:             e.ID,
		ArtistID:       e.ArtistID,
		ArtistName:     e.ArtistName,
		FilePath:       e.FilePath,
		FileName:       e.FileName,
		SizeBytes:      e.SizeBytes,
		DetectedAt:     e.DetectedAt.Format(time.RFC3339),
		DuplicateCount: e.DuplicateCount,
	}
}

// resolveForeignHash returns the entry's stored content hash, or computes
// it from disk when the row predates migration 008 and the column is empty.
// Allowlist writes key on this value, so an empty hash would silently
// produce duplicate rows under the partial-index WHERE clause and break
// dedupe; rehashing on demand keeps the dismiss/allowlist paths correct
// for legacy rows until the next scan refreshes the column. Pointer
// receiver avoids the rangeValCopy lint hit on the dismiss loop.
func resolveForeignHash(e *foreign.Entry) (string, error) {
	if e.ContentHash != "" {
		return e.ContentHash, nil
	}
	return hashFile(e.FilePath)
}

// hashFile streams the file at path through sha256 and returns the
// lowercase hex digest. Mirrors the scanner's helper so the handler
// package does not need to import internal/foreign just for the same
// computation. Streaming avoids loading large files into memory.
func hashFile(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: path comes from a server-managed ledger row, not user input
	if err != nil {
		return "", fmt.Errorf("hash open: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only handle
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash read: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
