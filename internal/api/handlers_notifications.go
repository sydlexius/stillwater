package api

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	img "github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/rule"
	"github.com/sydlexius/stillwater/web/templates"
)

// handleListNotifications returns rule violations with optional filtering and sorting.
// GET /api/v1/notifications?status=open&severity=error&category=image&sort=artist_name&order=asc
func (r *Router) handleListNotifications(w http.ResponseWriter, req *http.Request) {
	p := parseNotificationParams(req)

	violations, err := r.ruleService.ListViolationsFiltered(req.Context(), p)
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

// handleNotificationsExport streams a CSV or JSON export of filtered violations.
// Respects the same filter/sort params as handleListNotifications.
// GET /api/v1/notifications/export
func (r *Router) handleNotificationsExport(w http.ResponseWriter, req *http.Request) {
	p := parseNotificationParams(req)

	violations, err := r.ruleService.ListViolationsFiltered(req.Context(), p)
	if err != nil {
		r.logger.Error("listing violations for export", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to list violations")
		return
	}

	if violations == nil {
		violations = []rule.RuleViolation{}
	}

	// JSON export when requested via query param or Accept header
	format := req.URL.Query().Get("format")
	if format == "json" || strings.Contains(req.Header.Get("Accept"), "application/json") {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", `attachment; filename="violations.json"`)
		w.WriteHeader(http.StatusOK)

		type exportRow struct {
			ArtistName  string `json:"artist_name"`
			LibraryName string `json:"library_name"`
			RuleID      string `json:"rule_id"`
			Severity    string `json:"severity"`
			Message     string `json:"message"`
			Status      string `json:"status"`
			Age         string `json:"age"`
		}
		rows := make([]exportRow, len(violations))
		now := time.Now()
		for i, v := range violations {
			rows[i] = exportRow{
				ArtistName:  v.ArtistName,
				LibraryName: v.LibraryName,
				RuleID:      v.RuleID,
				Severity:    v.Severity,
				Message:     v.Message,
				Status:      v.Status,
				Age:         violationAge(v.CreatedAt, now),
			}
		}
		if err := json.NewEncoder(w).Encode(map[string]any{
			"violations": rows,
			"count":      len(rows),
		}); err != nil {
			r.logger.Error("writing JSON export", "error", err)
		}
		return
	}

	// Default: CSV export
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", `attachment; filename="violations.csv"`)
	w.WriteHeader(http.StatusOK)

	cw := csv.NewWriter(w)
	if err := cw.Write([]string{"Artist Name", "Library", "Rule ID", "Severity", "Message", "Status", "Age"}); err != nil {
		r.logger.Error("writing CSV header", "error", err)
		return
	}

	now := time.Now()
	for _, v := range violations {
		if req.Context().Err() != nil {
			break
		}
		if err := cw.Write([]string{
			sanitizeCSV(v.ArtistName),
			sanitizeCSV(v.LibraryName),
			sanitizeCSV(v.RuleID),
			v.Severity,
			sanitizeCSV(v.Message),
			v.Status,
			violationAge(v.CreatedAt, now),
		}); err != nil {
			r.logger.Error("writing CSV row", "artist", v.ArtistName, "error", err)
			return
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		r.logger.Error("flushing CSV writer", "error", err)
	}
}

// violationAge computes a human-readable age string from a creation time.
// Uses the same scale as the UI formatAge function: minutes, hours, or days.
func violationAge(created time.Time, now time.Time) string {
	if created.IsZero() {
		return ""
	}
	ago := now.Sub(created)
	if ago < 0 {
		ago = 0
	}
	if ago < time.Hour {
		return fmt.Sprintf("%dm", int(ago.Minutes()))
	}
	if ago < 24*time.Hour {
		return fmt.Sprintf("%dh", int(ago.Hours()))
	}
	return fmt.Sprintf("%dd", int(ago.Hours()/24))
}

// handleDismissViolation dismisses a rule violation.
// POST /api/v1/notifications/{id}/dismiss
func (r *Router) handleDismissViolation(w http.ResponseWriter, req *http.Request) {
	id, ok := RequirePathParam(w, req, "id")
	if !ok {
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
	id, ok := RequirePathParam(w, req, "id")
	if !ok {
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

// validGroupByValues are the accepted group_by parameter values.
var validGroupByValues = map[string]bool{
	"artist":   true,
	"rule":     true,
	"severity": true,
	"category": true,
}

// parseNotificationParams extracts filter/sort/group params from the request.
func parseNotificationParams(req *http.Request) rule.ViolationListParams {
	q := req.URL.Query()

	status := q.Get("status")
	if status == "" {
		status = "active"
	}

	sort := q.Get("sort")
	order := q.Get("order")
	// Normalize defaults so UI sort icons match actual query behavior.
	if sort == "" {
		sort = "severity"
	}
	if order == "" {
		order = "desc"
	}

	groupBy := q.Get("group_by")
	if !validGroupByValues[groupBy] {
		groupBy = ""
	}

	return rule.ViolationListParams{
		Status:   status,
		Sort:     sort,
		Order:    order,
		Severity: q.Get("severity"),
		Category: q.Get("category"),
		RuleID:   q.Get("rule_id"),
		GroupBy:  groupBy,
	}
}

// buildNotificationsData creates the template data from params and violations.
func buildNotificationsData(p rule.ViolationListParams, violations []rule.RuleViolation) templates.NotificationsData {
	data := templates.NotificationsData{
		Violations: violations,
		Sort:       p.Sort,
		Order:      p.Order,
		Status:     p.Status,
		Severity:   p.Severity,
		Category:   p.Category,
		RuleID:     p.RuleID,
		GroupBy:    p.GroupBy,
	}

	if p.GroupBy != "" {
		data.Groups = rule.GroupViolations(violations, p.GroupBy)
		data.Grouped = true
	}

	return data
}

// handleNotificationsPage renders the notifications page.
// GET /notifications
func (r *Router) handleNotificationsPage(w http.ResponseWriter, req *http.Request) {
	p := parseNotificationParams(req)

	violations, err := r.ruleService.ListViolationsFiltered(req.Context(), p)
	if err != nil {
		writeError(w, req, http.StatusInternalServerError, "failed to load violations")
		return
	}

	if violations == nil {
		violations = []rule.RuleViolation{}
	}

	data := buildNotificationsData(p, violations)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = templates.NotificationsPage(r.assets(), data).Render(req.Context(), w)
}

// handleApplyViolationCandidate downloads and applies a chosen image candidate.
// POST /api/v1/notifications/{id}/apply-candidate
func (r *Router) handleApplyViolationCandidate(w http.ResponseWriter, req *http.Request) {
	id, ok := RequirePathParam(w, req, "id")
	if !ok {
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
	var matchedCandidate *rule.ImageCandidate
	for _, c := range v.Candidates {
		if c.URL == body.URL && c.ImageType == body.ImageType {
			matchedCandidate = &c
			break
		}
	}
	if matchedCandidate == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url does not match any stored candidate for this violation"})
		return
	}

	// Load artist
	a, err := r.artistService.GetByID(req.Context(), v.ArtistID)
	if err != nil {
		writeError(w, req, http.StatusNotFound, fmt.Sprintf("artist not found: %v", err))
		return
	}

	// Download and save the chosen image using platform-aware naming.
	// Use the candidate's provider source and the violation's rule ID
	// so provenance tracks where the image actually came from.
	candidateMeta := &img.ExifMeta{
		Source:  matchedCandidate.Source,
		Fetched: time.Now().UTC(),
		URL:     body.URL,
		Rule:    v.RuleID,
		Mode:    "manual",
	}
	naming, useSymlinks := r.getActiveNamingAndSymlinks(req.Context(), body.ImageType)
	if _, err := rule.SaveImageFromURL(req.Context(), a, body.ImageType, body.URL, naming, useSymlinks, candidateMeta, r.platformService, r.logger); err != nil {
		r.logger.Error("applying image candidate", "artist_id", a.ID, "image_type", body.ImageType, "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to apply image candidate")
		return
	}

	// Persist artist update (image flag set by SaveImageFromURL)
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

	w.Header().Set("Cache-Control", "no-store")

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
		//nolint:errcheck // Clear the mobile badge via OOB swap
		fmt.Fprint(w, `<span id="notif-badge-mobile" hx-swap-oob="innerHTML"></span>`)
		return
	}

	display := fmt.Sprintf("%d", total)
	if total > 99 {
		display = "99+"
	}
	badge := fmt.Sprintf(`<span class="absolute -top-1 -right-2 inline-flex items-center justify-center min-w-[1.25rem] h-5 px-1 text-xs font-bold text-white bg-red-500 rounded-full">%s</span>`, display)
	//nolint:errcheck,gosec // display is derived from an integer, no XSS risk
	fmt.Fprint(w, badge)
	// Update the mobile badge via OOB swap so only one poll is needed.
	//nolint:errcheck,gosec // G705: badge is derived from an integer, no XSS risk
	fmt.Fprintf(w, `<span id="notif-badge-mobile" hx-swap-oob="innerHTML">%s</span>`, badge)
}

// handleNotificationsTable renders the notifications table for HTMX swaps.
// GET /notifications/table
func (r *Router) handleNotificationsTable(w http.ResponseWriter, req *http.Request) {
	p := parseNotificationParams(req)

	violations, err := r.ruleService.ListViolationsFiltered(req.Context(), p)
	if err != nil {
		writeError(w, req, http.StatusInternalServerError, "failed to load violations")
		return
	}

	if violations == nil {
		violations = []rule.RuleViolation{}
	}

	data := buildNotificationsData(p, violations)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = templates.NotificationsTable(data).Render(req.Context(), w)
}
