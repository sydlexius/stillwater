package api

import (
	"context"
	"errors"
	"html"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/rule"
)

// ruleRunStatus tracks the state of an async run-all-rules operation.
type ruleRunStatus struct {
	Running          bool      `json:"running"`
	Status           string    `json:"status"` // idle, running, completed, failed
	ArtistsProcessed int       `json:"artists_processed"`
	ViolationsFound  int       `json:"violations_found"`
	FixesAttempted   int       `json:"fixes_attempted"`
	FixesSucceeded   int       `json:"fixes_succeeded"`
	StartedAt        time.Time `json:"started_at,omitempty"`
	CompletedAt      time.Time `json:"completed_at,omitempty"`
	Error            string    `json:"error,omitempty"`
}

// handleListRules returns all rules as JSON.
// GET /api/v1/rules
func (r *Router) handleListRules(w http.ResponseWriter, req *http.Request) {
	rules, err := r.ruleService.List(req.Context())
	if err != nil {
		r.logger.Error("listing rules", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to list rules")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"rules": rules})
}

// handleUpdateRule updates a rule's enabled state and config.
// PUT /api/v1/rules/{id}
func (r *Router) handleUpdateRule(w http.ResponseWriter, req *http.Request) {
	ruleID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	existing, err := r.ruleService.GetByID(req.Context(), ruleID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "rule not found"})
		return
	}

	var body struct {
		Enabled        *bool            `json:"enabled"`
		AutomationMode *string          `json:"automation_mode"`
		Config         *rule.RuleConfig `json:"config"`
	}
	if !DecodeJSON(w, req, &body) {
		return
	}

	if body.Enabled != nil {
		existing.Enabled = *body.Enabled
	}
	if body.AutomationMode != nil {
		switch *body.AutomationMode {
		case rule.AutomationModeAuto, rule.AutomationModeManual:
			existing.AutomationMode = *body.AutomationMode
		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid automation_mode"})
			return
		}
	}
	if body.Config != nil {
		existing.Config = *body.Config
	}

	if err := r.ruleService.Update(req.Context(), existing); err != nil {
		r.logger.Error("updating rule", "rule_id", ruleID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to update rule"})
		return
	}

	writeJSON(w, http.StatusOK, existing)
}

// handleEvaluateArtist runs all enabled rules against an artist and returns the results.
// GET /api/v1/artists/{id}/health
func (r *Router) handleEvaluateArtist(w http.ResponseWriter, req *http.Request) {
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "artist not found"})
		return
	}

	result, err := r.ruleEngine.Evaluate(req.Context(), a)
	if err != nil {
		r.logger.Error("evaluating artist health", "artist_id", artistID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to evaluate artist"})
		return
	}

	// Update the artist's health score in the database
	a.HealthScore = result.HealthScore
	if err := r.artistService.Update(req.Context(), a); err != nil {
		r.logger.Warn("updating artist health score", "artist_id", artistID, "error", err)
	}

	writeJSON(w, http.StatusOK, result)
}

// handleRunRule runs a single rule against all artists and attempts to fix violations.
// POST /api/v1/rules/{id}/run
func (r *Router) handleRunRule(w http.ResponseWriter, req *http.Request) {
	ruleID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	if _, err := r.ruleService.GetByID(req.Context(), ruleID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "rule not found"})
		return
	}

	if r.pipeline == nil {
		r.logger.Error("run-rule: pipeline not configured", "rule_id", ruleID)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "rule pipeline not configured"})
		return
	}

	result, err := r.pipeline.RunRule(req.Context(), ruleID)
	if err != nil {
		r.logger.Error("running rule", "rule_id", ruleID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to run rule"})
		return
	}

	writeJSON(w, http.StatusOK, result)
}

// handleRunArtistRules runs all enabled rules scoped to a single artist.
// POST /api/v1/artists/{id}/run-rules
func (r *Router) handleRunArtistRules(w http.ResponseWriter, req *http.Request) {
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		if errors.Is(err, artist.ErrNotFound) {
			if req.Header.Get("HX-Request") == "true" {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				if _, werr := io.WriteString(w, `<div class="text-sm text-red-600 dark:text-red-400">Artist not found.</div>`); werr != nil {
					r.logger.Warn("writing HTMX artist-not-found fragment", "artist_id", artistID, "error", werr)
				}
				return
			}
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "artist not found"})
			return
		}
		r.logger.Error("looking up artist for run-rules", "artist_id", artistID, "error", err)
		if req.Header.Get("HX-Request") == "true" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if _, werr := io.WriteString(w, `<div class="text-sm text-red-600 dark:text-red-400">Failed to look up artist. Please try again.</div>`); werr != nil {
				r.logger.Warn("writing HTMX artist-lookup-error fragment", "artist_id", artistID, "error", werr)
			}
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to look up artist"})
		return
	}

	if r.pipeline == nil {
		r.logger.Error("run-artist-rules: pipeline not configured", "artist_id", artistID)
		if req.Header.Get("HX-Request") == "true" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if _, werr := io.WriteString(w, `<div class="text-sm text-red-600 dark:text-red-400">Rule pipeline not configured.</div>`); werr != nil {
				r.logger.Warn("writing HTMX pipeline-nil fragment", "artist_id", artistID, "error", werr)
			}
			return
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "rule pipeline not configured"})
		return
	}

	result, err := r.pipeline.RunForArtist(req.Context(), a)
	if err != nil {
		r.logger.Error("running rules for artist", "artist_id", artistID, "error", err)
		if req.Header.Get("HX-Request") == "true" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if _, werr := io.WriteString(w, `<div class="text-sm text-red-600 dark:text-red-400">Failed to run rules. Please try again.</div>`); werr != nil {
				r.logger.Warn("writing HTMX error fragment", "artist_id", artistID, "error", werr)
			}
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to run rules"})
		return
	}

	notificationsURL := r.basePath + "/notifications"

	if req.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		var fragment string
		if result.ViolationsFound == 0 {
			fragment = `<div class="text-sm text-green-600 dark:text-green-400">No violations found.</div>`
		} else {
			fragment = `<div class="text-sm text-gray-700 dark:text-gray-300">Found ` +
				strconv.Itoa(result.ViolationsFound) +
				` violation(s). <a href="` + html.EscapeString(notificationsURL) + `" class="text-blue-600 dark:text-blue-400 underline hover:text-blue-800">View in Notifications</a></div>`
		}
		if _, werr := io.WriteString(w, fragment); werr != nil { //nolint:gosec // G203: notificationsURL is HTML-escaped; fragment contains only static strings and strconv.Itoa output
			r.logger.Warn("writing HTMX run-rules fragment", "artist_id", artistID, "error", werr)
		}
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"violations_found":  result.ViolationsFound,
		"notifications_url": notificationsURL,
	})
}

// handleRunAllRules starts an async run of all enabled rules against all artists.
// Returns 202 Accepted immediately with status polling via GET /api/v1/rules/run-all/status.
// Returns 409 Conflict if a run is already in progress.
// POST /api/v1/rules/run-all
func (r *Router) handleRunAllRules(w http.ResponseWriter, req *http.Request) {
	if r.pipeline == nil {
		r.logger.Error("run-all-rules: pipeline not configured")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "rule pipeline not configured"})
		return
	}

	r.ruleRunMu.Lock()
	if r.ruleRun != nil && r.ruleRun.Running {
		r.ruleRunMu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]string{"error": "rules already running"})
		return
	}
	r.ruleRun = &ruleRunStatus{
		Running:   true,
		Status:    "running",
		StartedAt: time.Now().UTC(),
	}
	r.ruleRunMu.Unlock()

	ctx := context.WithoutCancel(req.Context())
	go func() {
		defer func() {
			if rv := recover(); rv != nil {
				r.ruleRunMu.Lock()
				r.ruleRun.Running = false
				r.ruleRun.Status = "failed"
				r.ruleRun.Error = "rule evaluation failed unexpectedly"
				r.ruleRun.CompletedAt = time.Now().UTC()
				r.ruleRunMu.Unlock()
				r.logger.Error("panic in rule run", "recover", rv)
			}
		}()

		result, err := r.pipeline.RunAll(ctx)

		// Compute violation count outside the mutex to avoid blocking status polls.
		violationsFound := result.ViolationsFound
		if err == nil {
			counts, dbErr := r.ruleService.CountActiveViolationsBySeverity(ctx)
			if dbErr != nil {
				r.logger.Warn("querying active violations for toast count", "error", dbErr)
			} else {
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
				violationsFound = total
			}
		}

		r.ruleRunMu.Lock()
		r.ruleRun.Running = false
		r.ruleRun.CompletedAt = time.Now().UTC()

		if err != nil {
			r.ruleRun.Status = "failed"
			r.ruleRun.Error = "rule evaluation failed"
			r.ruleRunMu.Unlock()
			r.logger.Error("running all rules", "error", err)
			return
		}

		r.ruleRun.Status = "completed"
		r.ruleRun.ArtistsProcessed = result.ArtistsProcessed
		r.ruleRun.FixesAttempted = result.FixesAttempted
		r.ruleRun.FixesSucceeded = result.FixesSucceeded
		r.ruleRun.ViolationsFound = violationsFound
		r.ruleRunMu.Unlock()
	}()

	r.ruleRunMu.Lock()
	status := *r.ruleRun
	r.ruleRunMu.Unlock()
	writeJSON(w, http.StatusAccepted, &status)
}

// handleRunAllRulesStatus returns the current status of the async run-all-rules operation.
// GET /api/v1/rules/run-all/status
func (r *Router) handleRunAllRulesStatus(w http.ResponseWriter, req *http.Request) {
	r.ruleRunMu.Lock()
	if r.ruleRun == nil {
		r.ruleRunMu.Unlock()
		writeJSON(w, http.StatusOK, &ruleRunStatus{Status: "idle"})
		return
	}
	status := *r.ruleRun // value copy
	r.ruleRunMu.Unlock()
	writeJSON(w, http.StatusOK, &status)
}

// handleGetClassicalMode returns the current classical music evaluation mode.
// GET /api/v1/rules/classical-mode
func (r *Router) handleGetClassicalMode(w http.ResponseWriter, req *http.Request) {
	mode := rule.GetClassicalMode(req.Context(), r.db)
	writeJSON(w, http.StatusOK, map[string]string{"mode": mode})
}

// handleSetClassicalMode updates the classical music evaluation mode.
// PUT /api/v1/rules/classical-mode
func (r *Router) handleSetClassicalMode(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Mode string `json:"mode"`
	}
	if !DecodeJSON(w, req, &body) {
		return
	}

	switch body.Mode {
	case rule.ClassicalModeSkip, rule.ClassicalModeComposer, rule.ClassicalModePerformer:
		// valid
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid mode, must be one of: skip, composer, performer",
		})
		return
	}

	if err := rule.SetClassicalMode(req.Context(), r.db, body.Mode); err != nil {
		r.logger.Error("setting classical mode", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to set classical mode"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"mode": body.Mode})
}

// handleBulkFetchMetadata starts a bulk metadata fetch job.
// POST /api/v1/bulk/fetch-metadata
func (r *Router) handleBulkFetchMetadata(w http.ResponseWriter, req *http.Request) {
	r.startBulkJob(w, req, rule.BulkTypeFetchMetadata)
}

// handleBulkFetchImages starts a bulk image fetch job.
// POST /api/v1/bulk/fetch-images
func (r *Router) handleBulkFetchImages(w http.ResponseWriter, req *http.Request) {
	r.startBulkJob(w, req, rule.BulkTypeFetchImages)
}

func (r *Router) startBulkJob(w http.ResponseWriter, req *http.Request, jobType string) {
	var body rule.BulkRequest
	if !DecodeJSON(w, req, &body) {
		return
	}

	mode := body.Mode
	if mode == "" {
		mode = rule.BulkModePromptNoMatch
	}

	// Pass 0 as initial total_items; the executor sets the accurate count
	// after resolving and filtering the actual artist list.
	job, err := r.bulkService.CreateJob(req.Context(), jobType, mode, 0)
	if err != nil {
		r.logger.Error("creating bulk job", "type", jobType, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create job"})
		return
	}
	job.ArtistIDs = body.ArtistIDs

	if err := r.bulkExecutor.Start(req.Context(), job); err != nil {
		r.logger.Error("starting bulk job", "type", jobType, "job_id", job.ID, "error", err)
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a bulk job is already running"})
		return
	}

	writeJSON(w, http.StatusAccepted, job)
}

// handleBulkJobList returns recent bulk jobs.
// GET /api/v1/bulk/jobs
func (r *Router) handleBulkJobList(w http.ResponseWriter, req *http.Request) {
	jobs, err := r.bulkService.ListJobs(req.Context(), 20)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to list jobs"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
}

// handleBulkJobStatus returns a single bulk job with its items.
// GET /api/v1/bulk/jobs/{id}
func (r *Router) handleBulkJobStatus(w http.ResponseWriter, req *http.Request) {
	jobID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	job, err := r.bulkService.GetJob(req.Context(), jobID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "job not found"})
		return
	}

	items, err := r.bulkService.ListItems(req.Context(), jobID)
	if err != nil {
		r.logger.Warn("listing job items", "job_id", jobID, "error", err)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"job":   job,
		"items": items,
	})
}

// handleBulkJobCancel cancels a running bulk job.
// POST /api/v1/bulk/jobs/{id}/cancel
func (r *Router) handleBulkJobCancel(w http.ResponseWriter, req *http.Request) {
	if err := r.bulkExecutor.Cancel(); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "canceling"})
}
