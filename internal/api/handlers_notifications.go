package api

import (
	"encoding/json"
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

// handleBulkDismissViolations dismisses all active violations, or a specific subset.
// POST /api/v1/notifications/bulk-dismiss
// Body (optional): {"ids": ["id1", "id2"]} -- omit or send empty ids to dismiss all active
func (r *Router) handleBulkDismissViolations(w http.ResponseWriter, req *http.Request) {
	var body struct {
		IDs []string `json:"ids"`
	}
	// Ignore decode error -- empty body means dismiss all
	_ = json.NewDecoder(req.Body).Decode(&body)

	n, err := r.ruleService.BulkDismissViolations(req.Context(), body.IDs)
	if err != nil {
		writeError(w, req, http.StatusInternalServerError, fmt.Sprintf("bulk dismiss: %v", err))
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "%d", n) //nolint:errcheck,gosec // G705: n is int, no XSS risk
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
	violations, err := r.ruleService.ListViolations(req.Context(), "active")
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

// handleApplyViolationCandidate downloads and applies a chosen image candidate.
// POST /api/v1/notifications/{id}/apply-candidate
func (r *Router) handleApplyViolationCandidate(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "violation id required"})
		return
	}

	var body struct {
		URL       string `json:"url"`
		ImageType string `json:"image_type"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if body.URL == "" || body.ImageType == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url and image_type are required"})
		return
	}

	// Load violation to get artist_id and validate the candidate
	v, err := r.ruleService.GetViolationByID(req.Context(), id)
	if err != nil {
		writeError(w, req, http.StatusNotFound, fmt.Sprintf("violation not found: %v", err))
		return
	}

	if v.Status != rule.ViolationStatusPendingChoice {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "violation is not pending candidate selection"})
		return
	}

	// Validate the URL+imageType against stored candidates (prevents SSRF with arbitrary URLs)
	var matched bool
	for _, c := range v.Candidates {
		if c.URL == body.URL && c.ImageType == body.ImageType {
			matched = true
			break
		}
	}
	if !matched {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url does not match any stored candidate for this violation"})
		return
	}

	// Load artist
	a, err := r.artistService.GetByID(req.Context(), v.ArtistID)
	if err != nil {
		writeError(w, req, http.StatusNotFound, fmt.Sprintf("artist not found: %v", err))
		return
	}

	// Download and save the chosen image
	naming := r.getActiveNamingConfig(req.Context(), body.ImageType)
	if err := rule.ApplyImageCandidate(req.Context(), a, body.ImageType, body.URL, naming, r.logger); err != nil {
		writeError(w, req, http.StatusInternalServerError, fmt.Sprintf("applying candidate: %v", err))
		return
	}

	// Persist artist update (image flag set by ApplyImageCandidate)
	if err := r.artistService.Update(req.Context(), a); err != nil {
		writeError(w, req, http.StatusInternalServerError, fmt.Sprintf("updating artist after apply-candidate: %v", err))
		return
	}

	// Mark violation resolved
	if err := r.ruleService.ResolveViolation(req.Context(), id); err != nil {
		writeError(w, req, http.StatusInternalServerError, fmt.Sprintf("resolving violation after apply-candidate: %v", err))
		return
	}

	// Re-evaluate and persist health score
	if eval, err := r.ruleEngine.Evaluate(req.Context(), a); err == nil {
		a.HealthScore = eval.HealthScore
		if err := r.artistService.Update(req.Context(), a); err != nil {
			r.logger.Warn("persisting health score after apply-candidate", "artist", a.Name, "error", err)
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
}

// handleNotificationCounts returns active violation counts by severity.
// GET /api/v1/notifications/counts
func (r *Router) handleNotificationCounts(w http.ResponseWriter, req *http.Request) {
	counts, err := r.ruleService.CountActiveViolationsBySeverity(req.Context())
	if err != nil {
		writeError(w, req, http.StatusInternalServerError, "failed to count violations")
		return
	}

	total := counts["error"] + counts["warning"] + counts["info"]

	writeJSON(w, http.StatusOK, map[string]int{
		"error":   counts["error"],
		"warning": counts["warning"],
		"info":    counts["info"],
		"total":   total,
	})
}

// handleNotificationBadge returns an HTML fragment for the navbar badge.
// GET /api/v1/notifications/badge
func (r *Router) handleNotificationBadge(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	if !r.getBoolSetting(ctx, "notif_badge_enabled", true) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		return
	}

	counts, err := r.ruleService.CountActiveViolationsBySeverity(ctx)
	if err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		return
	}

	total := 0
	if r.getBoolSetting(ctx, "notif_badge_severity_error", true) {
		total += counts["error"]
	}
	if r.getBoolSetting(ctx, "notif_badge_severity_warning", true) {
		total += counts["warning"]
	}
	if r.getBoolSetting(ctx, "notif_badge_severity_info", false) {
		total += counts["info"]
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if total == 0 {
		return
	}

	display := fmt.Sprintf("%d", total)
	if total > 99 {
		display = "99+"
	}
	//nolint:errcheck,gosec // G705: display is derived from an integer, no XSS risk
	fmt.Fprintf(w, `<span class="absolute -top-1 -right-2 inline-flex items-center justify-center min-w-[1.25rem] h-5 px-1 text-xs font-bold text-white bg-red-500 rounded-full">%s</span>`, display)
}

// handleNotificationsTable renders the notifications table for HTMX swaps.
// GET /notifications/table
func (r *Router) handleNotificationsTable(w http.ResponseWriter, req *http.Request) {
	violations, err := r.ruleService.ListViolations(req.Context(), "active")
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
