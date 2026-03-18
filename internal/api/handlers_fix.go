package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"

	"github.com/sydlexius/stillwater/internal/rule"
)

// FixAllProgress tracks the state of an async fix-all operation.
type FixAllProgress struct {
	mu        sync.RWMutex
	Status    string `json:"status"`
	Total     int    `json:"total"`
	Processed int    `json:"processed"`
	Fixed     int    `json:"fixed"`
	Skipped   int    `json:"skipped"`
	Failed    int    `json:"failed"`
}

// handleFixViolation applies the recommended fix for a single violation.
// POST /api/v1/notifications/{id}/fix
func (r *Router) handleFixViolation(w http.ResponseWriter, req *http.Request) {
	id, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	fr, err := r.pipeline.FixViolation(req.Context(), id)
	if err != nil {
		r.logger.Error("fix violation failed", "id", id, "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to apply fix")
		return
	}

	status := "fixed"
	if !fr.Fixed {
		status = "failed"
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"status":  status,
		"message": fr.Message,
	})
}

// handleFixAll starts an async bulk fix for all active fixable violations.
// POST /api/v1/notifications/fix-all
func (r *Router) handleFixAll(w http.ResponseWriter, req *http.Request) {
	var body struct {
		IDs []string `json:"ids"`
	}
	_ = json.NewDecoder(req.Body).Decode(&body)

	violations, err := r.ruleService.ListViolationsFiltered(req.Context(), rule.ViolationListParams{
		Status: "active",
	})
	if err != nil {
		r.logger.Error("listing violations for fix-all", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to list violations")
		return
	}

	idSet := make(map[string]bool, len(body.IDs))
	for _, id := range body.IDs {
		idSet[id] = true
	}

	var fixable []rule.RuleViolation
	for _, v := range violations {
		if !v.Fixable {
			continue
		}
		if len(idSet) > 0 && !idSet[v.ID] {
			continue
		}
		fixable = append(fixable, v)
	}

	if len(fixable) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "completed",
			"message": "no fixable violations",
			"total":   0,
		})
		return
	}

	r.startFixAll(req.Context(), fixable)

	writeJSON(w, http.StatusAccepted, map[string]any{
		"status": "running",
		"total":  len(fixable),
	})
}

// handleFixAllStatus returns the progress of the current fix-all operation.
// GET /api/v1/notifications/fix-all/status
func (r *Router) handleFixAllStatus(w http.ResponseWriter, _ *http.Request) {
	r.fixAllMu.RLock()
	progress := r.fixAllProgress
	r.fixAllMu.RUnlock()

	if progress == nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "idle"})
		return
	}

	progress.mu.RLock()
	resp := map[string]any{
		"status":    progress.Status,
		"total":     progress.Total,
		"processed": progress.Processed,
		"fixed":     progress.Fixed,
		"skipped":   progress.Skipped,
		"failed":    progress.Failed,
	}
	progress.mu.RUnlock()

	writeJSON(w, http.StatusOK, resp)
}

// startFixAll begins fixing violations in a background goroutine.
// The parent context is detached so the operation survives the HTTP request.
func (r *Router) startFixAll(_ context.Context, violations []rule.RuleViolation) {
	progress := &FixAllProgress{
		Status: "running",
		Total:  len(violations),
	}

	r.fixAllMu.Lock()
	r.fixAllProgress = progress
	r.fixAllMu.Unlock()

	go func() {
		// Use a background context so the fix-all outlives the HTTP request.
		ctx := context.Background()

		for _, rv := range violations {
			fr, err := r.pipeline.FixViolation(ctx, rv.ID)

			progress.mu.Lock()
			progress.Processed++
			if err != nil {
				progress.Failed++
				r.logger.Warn("fix-all: violation fix failed", "id", rv.ID, "error", err)
			} else if fr.Fixed {
				progress.Fixed++
			} else {
				progress.Skipped++
			}
			progress.mu.Unlock()
		}

		progress.mu.Lock()
		progress.Status = "completed"
		progress.mu.Unlock()
	}()
}
