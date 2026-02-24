package api

import (
	"fmt"
	"net/http"

	"github.com/sydlexius/stillwater/internal/rule"
	"github.com/sydlexius/stillwater/web/templates"
)

// handleListNotifications returns all open rule violations.
// GET /api/v1/notifications?status=open
func (r *Router) handleListNotifications(w http.ResponseWriter, req *http.Request) {
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
// POST /api/v1/notifications/{id}/dismiss
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

	// Return empty HTML for HTMX hx-swap="outerHTML" to remove the row.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
}

// handleResolveViolation marks a violation as resolved.
// POST /api/v1/notifications/{id}/resolve
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
// DELETE /api/v1/notifications/resolved
func (r *Router) handleClearResolvedViolations(w http.ResponseWriter, req *http.Request) {
	daysOld := 7

	err := r.ruleService.ClearResolvedViolations(req.Context(), daysOld)
	if err != nil {
		writeError(w, req, http.StatusInternalServerError, "failed to clear resolved violations")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "cleared"})
}

// handleNotificationsPage renders the notifications page.
// GET /notifications
func (r *Router) handleNotificationsPage(w http.ResponseWriter, req *http.Request) {
	violations, err := r.ruleService.ListViolations(req.Context(), rule.ViolationStatusOpen)
	if err != nil {
		writeError(w, req, http.StatusInternalServerError, "failed to load violations")
		return
	}

	if violations == nil {
		violations = []rule.RuleViolation{}
	}

	data := templates.NotificationsData{
		Violations: violations,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = templates.NotificationsPage(r.assets(), data).Render(req.Context(), w)
}

// handleNotificationsTable renders the notifications table for HTMX swaps.
// GET /notifications/table
func (r *Router) handleNotificationsTable(w http.ResponseWriter, req *http.Request) {
	violations, err := r.ruleService.ListViolations(req.Context(), rule.ViolationStatusOpen)
	if err != nil {
		writeError(w, req, http.StatusInternalServerError, "failed to load violations")
		return
	}

	if violations == nil {
		violations = []rule.RuleViolation{}
	}

	data := templates.NotificationsData{
		Violations: violations,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = templates.NotificationsTable(data).Render(req.Context(), w)
}
