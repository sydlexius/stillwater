package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
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

	if r.pipeline == nil {
		r.logger.Error("fix-violation: pipeline not configured", "id", id)
		writeError(w, req, http.StatusServiceUnavailable, "rule pipeline not configured")
		return
	}

	fr, err := r.pipeline.FixViolation(req.Context(), id)
	if err != nil {
		r.logger.Error("fix violation failed", "id", id, "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to apply fix")
		return
	}

	var status string
	switch {
	case fr.Fixed:
		status = "fixed"
	case fr.Dismissed:
		status = "dismissed"
	default:
		status = "failed"
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"status":  status,
		"message": fr.Message,
	})
}

// handleFixAll starts an async bulk fix for all open fixable violations.
// Rejects concurrent starts with 409 Conflict.
// POST /api/v1/notifications/fix-all
func (r *Router) handleFixAll(w http.ResponseWriter, req *http.Request) {
	if r.pipeline == nil {
		r.logger.Error("fix-all: pipeline not configured")
		writeError(w, req, http.StatusServiceUnavailable, "rule pipeline not configured")
		return
	}

	// Atomic check-and-set: reject if already running, otherwise claim the slot
	// immediately so concurrent requests cannot both pass the check.
	progress := &FixAllProgress{Status: "running"}
	r.fixAllMu.Lock()
	if r.fixAllProgress != nil {
		r.fixAllProgress.mu.RLock()
		running := r.fixAllProgress.Status == "running"
		r.fixAllProgress.mu.RUnlock()
		if running {
			r.fixAllMu.Unlock()
			writeJSON(w, http.StatusConflict, map[string]any{
				"status":  "running",
				"message": "fix-all already in progress",
			})
			return
		}
	}
	r.fixAllProgress = progress
	r.fixAllMu.Unlock()

	// releaseProgress clears the slot if this request still owns it, allowing
	// a subsequent request to start a new fix-all.
	releaseProgress := func() {
		r.fixAllMu.Lock()
		if r.fixAllProgress == progress {
			r.fixAllProgress = nil
		}
		r.fixAllMu.Unlock()
	}

	// Parse optional ID filter. Return 400 for malformed JSON (but allow
	// empty body / EOF to mean "fix all").
	var body struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		releaseProgress()
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}

	violations, err := r.ruleService.ListViolationsFiltered(req.Context(), rule.ViolationListParams{
		Status: "active",
	})
	if err != nil {
		releaseProgress()
		r.logger.Error("listing violations for fix-all", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to list violations")
		return
	}

	idSet := make(map[string]bool, len(body.IDs))
	for _, id := range body.IDs {
		idSet[id] = true
	}

	// Only include open fixable violations; skip pending_choice (requires
	// user candidate selection) and non-fixable violations.
	var fixable []rule.RuleViolation
	for _, v := range violations {
		if !v.Fixable || v.Status != rule.ViolationStatusOpen {
			continue
		}
		if len(idSet) > 0 && !idSet[v.ID] {
			continue
		}
		fixable = append(fixable, v)
	}

	if len(fixable) == 0 {
		releaseProgress()
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "completed",
			"message": "no fixable violations",
			"total":   0,
		})
		return
	}

	// Set the total now that we know the count.
	scoped := len(idSet) > 0
	progress.mu.Lock()
	progress.Total = len(fixable)
	progress.mu.Unlock()

	r.runFixAll(req.Context(), fixable, scoped, progress)

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

// runFixAll begins fixing violations in a background goroutine.
// The caller must set r.fixAllProgress before calling this method.
//
// Optimizations over a naive per-violation loop:
//  1. Orphan cleanup: dismiss violations for deleted artists in one SQL query
//     (skipped when the request is scoped to specific IDs)
//  2. Artist grouping: check each artist once, skip orphaned groups
//  3. Rule caching: Pipeline caches rule lookups across the entire run
//  4. Yield: sleep between artist groups to release the SQLite write lock
func (r *Router) runFixAll(reqCtx context.Context, violations []rule.RuleViolation, scoped bool, progress *FixAllProgress) {
	go func() {
		// Detach from request lifecycle but preserve request-scoped values.
		ctx := context.WithoutCancel(reqCtx)

		// Phase 1: dismiss orphaned violations in bulk (one SQL query).
		// Skip when the request targets specific IDs to avoid side effects
		// on unrelated violations.
		if !scoped {
			dismissed, err := r.ruleService.DismissOrphanedViolations(ctx)
			if err != nil {
				r.logger.Warn("fix-all: orphan cleanup failed", "error", err)
			} else if dismissed > 0 {
				r.logger.Info("fix-all: dismissed orphaned violations", "count", dismissed)
			}
		}

		// Phase 2: group violations by artist.
		type artistGroup struct {
			artistID   string
			violations []rule.RuleViolation
		}
		groupOrder := []string{}
		byArtist := map[string]*artistGroup{}
		for _, rv := range violations {
			g, ok := byArtist[rv.ArtistID]
			if !ok {
				g = &artistGroup{artistID: rv.ArtistID}
				byArtist[rv.ArtistID] = g
				groupOrder = append(groupOrder, rv.ArtistID)
			}
			g.violations = append(g.violations, rv)
		}

		// Phase 3: process artist groups with caching and yield.
		for _, artistID := range groupOrder {
			g := byArtist[artistID]

			// Check artist existence once per group.
			_, aErr := r.artistService.GetByID(ctx, artistID)
			if aErr != nil && errors.Is(aErr, artist.ErrNotFound) {
				// Explicitly dismiss each violation for this deleted artist.
				for _, rv := range g.violations {
					if dErr := r.ruleService.DismissViolation(ctx, rv.ID); dErr != nil {
						r.logger.Warn("fix-all: failed to dismiss orphan violation", "id", rv.ID, "error", dErr)
					}
					progress.mu.Lock()
					progress.Processed++
					progress.Skipped++
					progress.mu.Unlock()
				}
				continue
			}

			// Fix each violation for this artist.
			for _, rv := range g.violations {
				fr, fixErr := r.pipeline.FixViolation(ctx, rv.ID)

				progress.mu.Lock()
				progress.Processed++
				if fixErr != nil {
					progress.Failed++
					r.logger.Warn("fix-all: violation fix failed", "id", rv.ID, "error", fixErr)
				} else if fr.Fixed {
					progress.Fixed++
				} else {
					progress.Skipped++
				}
				progress.mu.Unlock()
			}

			// Yield between artist groups to let HTTP handlers acquire the
			// SQLite write lock, keeping the UI responsive.
			time.Sleep(10 * time.Millisecond)
		}

		progress.mu.Lock()
		progress.Status = "completed"
		progress.mu.Unlock()
	}()
}
