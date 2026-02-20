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
		Enabled *bool            `json:"enabled"`
		Config  *rule.RuleConfig `json:"config"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if body.Enabled != nil {
		existing.Enabled = *body.Enabled
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
