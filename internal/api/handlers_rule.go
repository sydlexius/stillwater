package api

import (
	"encoding/json"
	"net/http"

	"github.com/sydlexius/stillwater/internal/rule"
)

// handleListRules returns all rules as JSON.
// GET /api/v1/rules
func (r *Router) handleListRules(w http.ResponseWriter, req *http.Request) {
	rules, err := r.ruleService.List(req.Context())
	if err != nil {
		writeError(w, req, http.StatusInternalServerError, "failed to list rules")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"rules": rules})
}

// handleUpdateRule updates a rule's enabled state and config.
// PUT /api/v1/rules/{id}
func (r *Router) handleUpdateRule(w http.ResponseWriter, req *http.Request) {
	ruleID := req.PathValue("id")
	if ruleID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing rule id"})
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
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if body.Enabled != nil {
		existing.Enabled = *body.Enabled
	}
	if body.AutomationMode != nil {
		switch *body.AutomationMode {
		case rule.AutomationModeAuto, rule.AutomationModeNotify, rule.AutomationModeDisabled:
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to update rule"})
		return
	}

	writeJSON(w, http.StatusOK, existing)
}

// handleEvaluateArtist runs all enabled rules against an artist and returns the results.
// GET /api/v1/artists/{id}/health
func (r *Router) handleEvaluateArtist(w http.ResponseWriter, req *http.Request) {
	artistID := req.PathValue("id")
	if artistID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing artist id"})
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
	ruleID := req.PathValue("id")
	if ruleID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing rule id"})
		return
	}

	if _, err := r.ruleService.GetByID(req.Context(), ruleID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "rule not found"})
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

// handleRunAllRules runs all enabled rules against all artists and attempts fixes.
// POST /api/v1/rules/run-all
func (r *Router) handleRunAllRules(w http.ResponseWriter, req *http.Request) {
	result, err := r.pipeline.RunAll(req.Context())
	if err != nil {
		r.logger.Error("running all rules", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to run rules"})
		return
	}

	writeJSON(w, http.StatusOK, result)
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
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
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
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	mode := body.Mode
	if mode == "" {
		mode = rule.BulkModePromptNoMatch
	}

	job, err := r.bulkService.CreateJob(req.Context(), jobType, mode, len(body.ArtistIDs))
	if err != nil {
		r.logger.Error("creating bulk job", "type", jobType, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create job"})
		return
	}
	job.ArtistIDs = body.ArtistIDs

	if err := r.bulkExecutor.Start(req.Context(), job); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
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
	jobID := req.PathValue("id")
	if jobID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing job id"})
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
