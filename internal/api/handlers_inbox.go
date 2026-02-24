package api

import (
	"fmt"
	"net/http"

	"github.com/sydlexius/stillwater/internal/rule"
	"github.com/sydlexius/stillwater/web/templates"
)

// handleListInbox returns all open rule violations.
// GET /api/v1/inbox?status=open
func (r *Router) handleListInbox(w http.ResponseWriter, req *http.Request) {
	status := req.URL.Query().Get("status")
	if status == "" {
		status = rule.ViolationStatusOpen
	}

	violations, err := r.ruleService.ListViolations(req.Context(), status)
	if err != nil {
		writeError(w, req, http.StatusInternalServerError, "failed to list violations")
		return
	}

	if violations == nil {
		violations = []rule.RuleViolation{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"violations": violations,
		"count":      len(violations),
	})
}

// handleDismissViolation dismisses a rule violation.
// POST /api/v1/inbox/{id}/dismiss
func (r *Router) handleDismissViolation(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "violation id required"})
		return
	}

	err := r.ruleService.DismissViolation(req.Context(), id)
	if err != nil {
		writeError(w, req, http.StatusInternalServerError, fmt.Sprintf("failed to dismiss violation: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "dismissed"})
}

// handleResolveViolation marks a violation as resolved.
// POST /api/v1/inbox/{id}/resolve
func (r *Router) handleResolveViolation(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "violation id required"})
		return
	}

	err := r.ruleService.ResolveViolation(req.Context(), id)
	if err != nil {
		writeError(w, req, http.StatusInternalServerError, fmt.Sprintf("failed to resolve violation: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "resolved"})
}

// handleClearResolvedViolations deletes resolved violations older than 7 days.
// DELETE /api/v1/inbox/resolved
func (r *Router) handleClearResolvedViolations(w http.ResponseWriter, req *http.Request) {
	daysOld := 7

	err := r.ruleService.ClearResolvedViolations(req.Context(), daysOld)
	if err != nil {
		writeError(w, req, http.StatusInternalServerError, "failed to clear resolved violations")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "cleared"})
}

// handleInboxPage renders the inbox page.
// GET /inbox
func (r *Router) handleInboxPage(w http.ResponseWriter, req *http.Request) {
	violations, err := r.ruleService.ListViolations(req.Context(), rule.ViolationStatusOpen)
	if err != nil {
		writeError(w, req, http.StatusInternalServerError, "failed to load violations")
		return
	}

	if violations == nil {
		violations = []rule.RuleViolation{}
	}

	data := templates.InboxData{
		Violations: violations,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = templates.InboxPage(r.assets(), data).Render(req.Context(), w)
}

// handleInboxTable renders the inbox table for HTMX swaps.
// GET /inbox/table
func (r *Router) handleInboxTable(w http.ResponseWriter, req *http.Request) {
	violations, err := r.ruleService.ListViolations(req.Context(), rule.ViolationStatusOpen)
	if err != nil {
		writeError(w, req, http.StatusInternalServerError, "failed to load violations")
		return
	}

	if violations == nil {
		violations = []rule.RuleViolation{}
	}

	data := templates.InboxData{
		Violations: violations,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = templates.InboxTable(data).Render(req.Context(), w)
}
