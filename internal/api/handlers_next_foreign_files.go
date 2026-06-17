package api

import (
	"net/http"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/web/components"
	"github.com/sydlexius/stillwater/web/templates/next"
)

// handleNextForeignFilesPage serves the next/ channel foreign-files page
// (M55 #1773).
//
// In stable mode (SW_UX=stable) the UX middleware 404s any /next/* request
// before this handler runs (decision 12 in architecture-decisions.md). The
// in-handler channel guard below is therefore only reachable when the lane IS
// enabled (next/dual mode) and the resolved channel is not "next" -- triggered
// by an explicit X-Stillwater-UX: stable header. In that edge case it returns
// 404 (decision 12: all handleNext* handlers return 404 on an explicit /next/
// path with the stable opt-out; the path does not serve stable content).
func (r *Router) handleNextForeignFilesPage(w http.ResponseWriter, req *http.Request) {
	if !checkNextChannel(w, req) {
		return
	}
	if !r.requireForeignAdmin(w, req) {
		return
	}
	view, err := r.loadForeignFilesView(req.Context())
	if err != nil {
		r.logger.Error("listing foreign files for next page", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	renderTempl(w, req, next.ForeignFilesPageNext(r.assetsFor(req), view))
}

// handleNextForeignAllowlistPage serves the next/ channel foreign-file
// allowlist page (M55 #1773) with server-side pagination.
//
// In stable mode (SW_UX=stable) the UX middleware 404s any /next/* request
// before this handler runs (decision 12 in architecture-decisions.md). The
// in-handler channel guard below is therefore only reachable when the lane IS
// enabled (next/dual mode) and the resolved channel is not "next" -- triggered
// by an explicit X-Stillwater-UX: stable header. In that edge case it returns
// 404 (decision 12: all handleNext* handlers return 404 on an explicit /next/
// path with the stable opt-out; the path does not serve stable content).
//
// Pagination: the handler reads "page" and "page_size" query parameters
// (respecting the user's stored page-size preference via getUserPageSize) and
// slices the full allowlist in-memory. An HTMX request -- triggered by the
// pagination Prev/Next links -- returns only the ForeignAllowlistTable fragment
// so the swap replaces just the table-plus-pager region without re-rendering
// the surrounding next/ chrome.
func (r *Router) handleNextForeignAllowlistPage(w http.ResponseWriter, req *http.Request) {
	if !checkNextChannel(w, req) {
		return
	}
	if !r.requireForeignAdmin(w, req) {
		return
	}

	userID := middleware.UserIDFromContext(req.Context())
	page := intQuery(req, "page", 1)
	pageSize := r.getUserPageSize(req.Context(), userID, intQuery(req, "page_size", 0))

	view, err := r.loadForeignAllowlistView(req.Context())
	if err != nil {
		r.logger.Error("listing foreign allowlist for next page", "error", err)
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
		BaseURL:     r.basePath + "/next/reports/foreign-files/allowlist",
		// TargetID is "foreign-allowlist-body": pagination links (rendered
		// via NextPagination in ForeignAllowlistBodyNext) swap the whole body
		// fragment (table + pagination) with outerHTML so the keyboard
		// boundary controls remain in the DOM after page navigation.
		TargetID: "foreign-allowlist-body",
	}

	// HTMX pagination requests (Prev/Next clicks from NextPagination) swap
	// the ForeignAllowlistBodyNext fragment (table + pagination controls) as
	// one unit; full-page navigations render the complete next/ shell.
	if isHTMXRequest(req) {
		renderTempl(w, req, next.ForeignAllowlistBodyNext(view))
		return
	}
	renderTempl(w, req, next.ForeignAllowlistPageNext(r.assetsFor(req), view))
}
