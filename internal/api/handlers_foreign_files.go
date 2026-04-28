package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/filesystem"
	"github.com/sydlexius/stillwater/internal/foreign"
	"github.com/sydlexius/stillwater/web/templates"
)

// foreignSummaryForBanner is invoked by handleGetConflictBanner so the
// banner state can include the foreign-file count without forcing the
// caller to wire two services together. Returns a zero summary if the
// repo is unavailable (e.g. DB not configured in tests) so the banner
// degrades silently to its conflict-only behavior.
func (r *Router) foreignSummaryForBanner(ctx context.Context) (int, time.Time) {
	if r.foreignRepo == nil {
		return 0, time.Time{}
	}
	n, err := r.foreignRepo.Count(ctx)
	if err != nil {
		r.logger.Warn("foreign-file banner count failed", "error", err)
		return 0, time.Time{}
	}
	return n, time.Now().UTC()
}

// handleForeignFilesPage renders /settings/foreign-files. Admin-only; the
// management page exposes destructive actions so we mirror the rest of the
// settings UI's RBAC.
func (r *Router) handleForeignFilesPage(w http.ResponseWriter, req *http.Request) {
	if !r.requireForeignAdmin(w, req) {
		return
	}
	view := templates.ForeignFilesPageView{}
	if r.foreignRepo != nil {
		entries, err := r.foreignRepo.List(req.Context())
		if err != nil {
			r.logger.Error("listing foreign files for page", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		view.Rows = make([]templates.ForeignFileRow, 0, len(entries))
		for _, e := range entries {
			view.Rows = append(view.Rows, foreignEntryToRow(e))
		}
		view.Count = len(view.Rows)
	}
	renderTempl(w, req, templates.ForeignFilesPage(r.assetsFor(req), view))
}

// handleForeignAllowlistPage renders /settings/foreign-files/allowlist.
// Admin-only.
func (r *Router) handleForeignAllowlistPage(w http.ResponseWriter, req *http.Request) {
	if !r.requireForeignAdmin(w, req) {
		return
	}
	view := templates.ForeignAllowlistPageView{}
	if r.foreignRepo != nil {
		entries, err := r.foreignRepo.ListAllowlist(req.Context())
		if err != nil {
			r.logger.Error("listing foreign allowlist for page", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		view.Rows = make([]templates.ForeignAllowlistRow, 0, len(entries))
		for _, e := range entries {
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "loading foreign-file row"})
		return
	}
	if err := r.foreignRepo.AddAllowlist(req.Context(), foreign.AllowlistEntry{
		Scope:    foreign.ScopeArtist,
		ArtistID: entry.ArtistID,
		FileName: entry.FileName,
		Note:     "added from foreign-files page",
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "writing allowlist"})
		return
	}
	if err := r.foreignRepo.DeleteByID(req.Context(), id); err != nil && !errors.Is(err, foreign.ErrNotFound) {
		r.logger.Warn("removing allowlisted ledger row", "id", id, "error", err)
	}
	w.WriteHeader(http.StatusOK)
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
		r.logger.Warn("removing ledger row after disk delete", "id", id, "error", err)
	}
	w.WriteHeader(http.StatusOK)
}

// handleForeignFilesDismiss bulk-allowlists every currently-active foreign
// file globally. Designed for the banner Dismiss button: one click clears
// the warning state for the rest of the install. Returns the re-rendered
// banner so HTMX can swap it in place.
//
// POST /api/v1/foreign-files/dismiss
func (r *Router) handleForeignFilesDismiss(w http.ResponseWriter, req *http.Request) {
	if r.foreignRepo == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "foreign-file scanner not configured"})
		return
	}
	entries, err := r.foreignRepo.List(req.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "listing foreign files"})
		return
	}
	seen := map[string]bool{}
	for _, e := range entries {
		key := strings.ToLower(e.FileName)
		if seen[key] {
			continue
		}
		seen[key] = true
		if err := r.foreignRepo.AddAllowlist(req.Context(), foreign.AllowlistEntry{
			Scope:    foreign.ScopeGlobal,
			FileName: e.FileName,
			Note:     "bulk dismiss from banner",
		}); err != nil {
			r.logger.Warn("dismiss bulk allowlist failed for file", "file", e.FileName, "error", err)
		}
		if err := r.foreignRepo.DeleteByPath(req.Context(), e.ArtistID, e.FilePath); err != nil && !errors.Is(err, foreign.ErrNotFound) {
			r.logger.Warn("dismiss ledger cleanup failed", "id", e.ID, "error", err)
		}
	}
	// Render the refreshed banner content so HTMX can swap it. The conflict
	// detector is independent of the foreign repo so we go through the same
	// code path as GET /api/v1/config/conflict-banner.
	r.handleGetConflictBanner(w, req)
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "removing allowlist entry"})
		return
	}
	w.WriteHeader(http.StatusOK)
}

// requireForeignAdmin renders an inline 403 for non-admin viewers and
// renders the login page for unauthenticated viewers. Mirrors the gate in
// handleSettingsPage so the foreign-files pages stay admin-only.
func (r *Router) requireForeignAdmin(w http.ResponseWriter, req *http.Request) bool {
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		r.renderLoginPage(w, req)
		return false
	}
	if middleware.RoleFromContext(req.Context()) != "administrator" {
		http.Error(w, "Forbidden: administrator role required", http.StatusForbidden)
		return false
	}
	return true
}

// foreignEntryToRow converts a domain entry to its template row shape.
func foreignEntryToRow(e foreign.Entry) templates.ForeignFileRow {
	return templates.ForeignFileRow{
		ID:         e.ID,
		ArtistID:   e.ArtistID,
		ArtistName: e.ArtistName,
		FilePath:   e.FilePath,
		FileName:   e.FileName,
		SizeBytes:  e.SizeBytes,
		DetectedAt: e.DetectedAt.Format(time.RFC3339),
	}
}
