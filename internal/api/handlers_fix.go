package api

import (
	"context"
	"encoding/json"
	"errors"
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
//
// Optimizations over a naive per-violation loop:
//  1. Orphan cleanup: dismiss violations for deleted artists in one SQL query
//  2. Artist grouping: load each artist once, fix all its violations together
//  3. Rule caching: Pipeline caches rule lookups across the entire run
//  4. Yield: sleep between artist groups to release the SQLite write lock
func (r *Router) startFixAll(_ context.Context, violations []rule.RuleViolation) {
	progress := &FixAllProgress{
		Status: "running",
		Total:  len(violations),
	}

	r.fixAllMu.Lock()
	r.fixAllProgress = progress
	r.fixAllMu.Unlock()

	go func() {
		ctx := context.Background()

		// Phase 1: dismiss orphaned violations in bulk (one SQL query).
		dismissed, err := r.ruleService.DismissOrphanedViolations(ctx)
		if err != nil {
			r.logger.Warn("fix-all: orphan cleanup failed", "error", err)
		} else if dismissed > 0 {
			r.logger.Info("fix-all: dismissed orphaned violations", "count", dismissed)
		}

		// Build set of dismissed orphan artist IDs for fast skip.
		orphanIDs := make(map[string]bool)

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
				orphanIDs[artistID] = true
				// Dismiss all violations for this deleted artist (may already
				// be dismissed by Phase 1, but DismissViolation is idempotent).
				for range g.violations {
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
