package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/event"
)

// MaxBulkActionIDs caps the number of artist IDs accepted in a single bulk
// action request. This bounds both memory and run time so a single request
// cannot monopolize the singleton bulk-action slot indefinitely.
const MaxBulkActionIDs = 1000

// Allowed bulk action types.
const (
	BulkActionRunRules    = "run_rules"
	BulkActionReIdentify  = "re_identify"
	BulkActionScan        = "scan"
	BulkActionFetchImages = "fetch_images"
)

// idPattern accepts UUIDs and other plausible artist ID encodings used in the
// repository (UUIDs and ULIDs are both used in different code paths). We keep
// the character class conservative so arbitrary input cannot leak through.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// BulkActionProgress tracks the state of an in-flight bulk action. It is
// mutex-protected and shared between the request handler and the background
// goroutine processing the IDs.
type BulkActionProgress struct {
	mu          sync.RWMutex
	Action      string `json:"action"`
	Status      string `json:"status"` // "running", "completed", "failed"
	Total       int    `json:"total"`
	Processed   int    `json:"processed"`
	Succeeded   int    `json:"succeeded"`
	Skipped     int    `json:"skipped"`
	Failed      int    `json:"failed"`
	CurrentName string `json:"current_name"`
	StartedAt   time.Time
	CompletedAt time.Time
}

// snapshot copies the progress state for safe JSON marshaling.
func (p *BulkActionProgress) snapshot() map[string]any {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return map[string]any{
		"action":       p.Action,
		"status":       p.Status,
		"total":        p.Total,
		"processed":    p.Processed,
		"succeeded":    p.Succeeded,
		"skipped":      p.Skipped,
		"failed":       p.Failed,
		"current_name": p.CurrentName,
	}
}

// bulkActionRequest is the JSON body accepted by handleBulkAction.
type bulkActionRequest struct {
	Action string   `json:"action"`
	IDs    []string `json:"ids"`
}

// validActions returns whether the supplied action string is one of the
// recognized bulk-action types.
func validActions(s string) bool {
	switch s {
	case BulkActionRunRules, BulkActionReIdentify, BulkActionScan, BulkActionFetchImages:
		return true
	}
	return false
}

// handleBulkAction starts an async bulk action over an explicit list of artist
// IDs. Only one bulk action may run at a time; concurrent starts are rejected
// with 409 Conflict. Mirrors the progress-tracker pattern used by fix-all and
// bulk-identify so callers get consistent semantics.
//
// POST /api/v1/artists/bulk-actions
func (r *Router) handleBulkAction(w http.ResponseWriter, req *http.Request) {
	// Parse and validate the request body before claiming the singleton slot
	// so a malformed request cannot lock out legitimate ones.
	var body bulkActionRequest
	dec := json.NewDecoder(req.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if !validActions(body.Action) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid or missing action"})
		return
	}
	if len(body.IDs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ids must be a non-empty list"})
		return
	}
	if len(body.IDs) > MaxBulkActionIDs {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "too many ids"})
		return
	}
	// Deduplicate + validate each ID at the API boundary. Any invalid ID
	// fails the entire request; partial acceptance would make progress
	// counters ambiguous.
	seen := make(map[string]struct{}, len(body.IDs))
	ids := make([]string, 0, len(body.IDs))
	for _, id := range body.IDs {
		if !idPattern.MatchString(id) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id format"})
			return
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}

	// Claim the singleton slot atomically. Reject with 409 if another bulk
	// action is still running. The conflict check happens before the service
	// availability gate so a request that would 503 never silently displaces
	// a legitimately running job.
	progress := &BulkActionProgress{
		Action:    body.Action,
		Status:    "running",
		Total:     len(ids),
		StartedAt: time.Now().UTC(),
	}
	r.bulkActionMu.Lock()
	if r.bulkActionProgress != nil {
		r.bulkActionProgress.mu.RLock()
		running := r.bulkActionProgress.Status == "running"
		r.bulkActionProgress.mu.RUnlock()
		if running {
			r.bulkActionMu.Unlock()
			writeJSON(w, http.StatusConflict, map[string]any{
				"status":  "running",
				"message": "a bulk action is already in progress",
			})
			return
		}
	}
	r.bulkActionProgress = progress
	r.bulkActionMu.Unlock()

	// Service availability gates depend on which action we were asked to run.
	// Release the slot if a required service is missing so the next caller
	// is not blocked by a failed start.
	releaseSlot := func() {
		r.bulkActionMu.Lock()
		if r.bulkActionProgress == progress {
			r.bulkActionProgress = nil
		}
		r.bulkActionMu.Unlock()
	}
	switch body.Action {
	case BulkActionRunRules, BulkActionFetchImages:
		if r.pipeline == nil {
			releaseSlot()
			writeError(w, req, http.StatusServiceUnavailable, "rule pipeline not configured")
			return
		}
	case BulkActionReIdentify:
		if r.artistService == nil {
			releaseSlot()
			writeError(w, req, http.StatusServiceUnavailable, "artist service not configured")
			return
		}
	case BulkActionScan:
		// scan reuses the rule pipeline to refresh derived artist state,
		// so both the artist service and the pipeline must be configured.
		// Gate both upfront; otherwise applyBulkAction would silently skip
		// and the caller would see a misleading 202 + completed snapshot.
		if r.artistService == nil {
			releaseSlot()
			writeError(w, req, http.StatusServiceUnavailable, "artist service not configured")
			return
		}
		if r.pipeline == nil {
			releaseSlot()
			writeError(w, req, http.StatusServiceUnavailable, "rule pipeline not configured")
			return
		}
	}

	r.runBulkAction(req.Context(), body.Action, ids, progress)

	writeJSON(w, http.StatusAccepted, map[string]any{
		"status": "running",
		"action": body.Action,
		"total":  len(ids),
	})
}

// handleBulkActionStatus returns the current state of the in-flight or most
// recently completed bulk action.
//
// GET /api/v1/artists/bulk-actions/status
func (r *Router) handleBulkActionStatus(w http.ResponseWriter, _ *http.Request) {
	r.bulkActionMu.RLock()
	progress := r.bulkActionProgress
	r.bulkActionMu.RUnlock()

	if progress == nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "idle"})
		return
	}
	writeJSON(w, http.StatusOK, progress.snapshot())
}

// runBulkAction executes the action for each artist ID in a detached goroutine.
// Uses a mutex-protected progress struct so status polls see consistent state.
func (r *Router) runBulkAction(reqCtx context.Context, action string, ids []string, progress *BulkActionProgress) {
	go func() {
		// Detach from the request lifecycle but keep request-scoped values
		// (user, logger, etc.). fix-all uses the same pattern.
		ctx := context.WithoutCancel(reqCtx)

		// Build the connection index once for re-identify so per-artist
		// work inside the loop stays cheap.
		var connIdx *connectionIndex
		if action == BulkActionReIdentify {
			connIdx = r.buildConnectionIndex(ctx)
		}

		for _, id := range ids {
			a, err := r.artistService.GetByID(ctx, id)
			if err != nil {
				progress.mu.Lock()
				progress.Processed++
				if errors.Is(err, artist.ErrNotFound) {
					progress.Skipped++
				} else {
					progress.Failed++
				}
				progress.mu.Unlock()
				r.logger.Warn("bulk action: load artist failed", "action", action, "id", id, "error", err)
				continue
			}

			progress.mu.Lock()
			progress.CurrentName = a.Name
			progress.mu.Unlock()

			outcome := r.applyBulkAction(ctx, action, a, connIdx)

			progress.mu.Lock()
			progress.Processed++
			switch outcome {
			case bulkOutcomeSucceeded:
				progress.Succeeded++
			case bulkOutcomeSkipped:
				progress.Skipped++
			default:
				progress.Failed++
			}
			progress.mu.Unlock()

			// Yield between artists so HTTP handlers can acquire the SQLite
			// write lock. Matches the fix-all / bulk-identify cadence.
			time.Sleep(10 * time.Millisecond)
		}

		progress.mu.Lock()
		progress.Status = "completed"
		progress.CurrentName = ""
		progress.CompletedAt = time.Now().UTC()
		succeeded := progress.Succeeded
		failed := progress.Failed
		total := progress.Total
		progress.mu.Unlock()

		// Surface the completion via the event bus so the SSE hub can
		// broadcast a toast to connected clients.
		if r.eventBus != nil {
			r.eventBus.Publish(event.Event{
				Type: event.BulkCompleted,
				Data: map[string]any{
					"type":      action,
					"status":    "completed",
					"total":     total,
					"succeeded": succeeded,
					"failed":    failed,
				},
			})
		}

		// Bulk actions change artist state, which invalidates health and
		// badge caches for the dashboard.
		r.InvalidateHealthCache()
	}()
}

// bulkOutcome labels the per-artist result of a bulk action.
type bulkOutcome int

const (
	bulkOutcomeSucceeded bulkOutcome = iota
	bulkOutcomeFailed
	bulkOutcomeSkipped
)

// applyBulkAction dispatches a single artist through the requested action. All
// branches return one of the three bulkOutcome values so the caller can update
// the progress counters uniformly.
func (r *Router) applyBulkAction(ctx context.Context, action string, a *artist.Artist, connIdx *connectionIndex) bulkOutcome {
	switch action {
	case BulkActionRunRules, BulkActionFetchImages:
		// Both actions flow through the rule pipeline. run_rules evaluates
		// every enabled rule and auto-fixes those in auto mode; image rules
		// in auto mode will fetch missing art as a side effect, which is
		// the natural implementation of "fetch images".
		if _, err := r.pipeline.RunForArtist(ctx, a); err != nil {
			r.logger.Warn("bulk action: RunForArtist failed", "action", action, "artist_id", a.ID, "error", err)
			return bulkOutcomeFailed
		}
		return bulkOutcomeSucceeded

	case BulkActionReIdentify:
		// Reuse the same tiered pipeline bulk-identify already exercises so
		// behavior stays consistent across entry points.
		if r.orchestrator == nil && connIdx == nil {
			return bulkOutcomeSkipped
		}
		res := r.identifyArtist(ctx, a, connIdx)
		switch res.Outcome {
		case outcomeAutoLinked:
			return bulkOutcomeSucceeded
		case outcomeQueued, outcomeUnmatched, outcomeSkipped:
			return bulkOutcomeSkipped
		default:
			return bulkOutcomeFailed
		}

	case BulkActionScan:
		// Per-artist "scan" reparses the NFO state and refreshes derived
		// flags. Running the rule pipeline achieves this (it refreshes
		// artist fields and persists any metadata delta) without needing
		// a separate filesystem walk.
		if r.pipeline == nil {
			return bulkOutcomeSkipped
		}
		if _, err := r.pipeline.RunForArtist(ctx, a); err != nil {
			r.logger.Warn("bulk action: scan RunForArtist failed", "artist_id", a.ID, "error", err)
			return bulkOutcomeFailed
		}
		return bulkOutcomeSucceeded
	}

	return bulkOutcomeSkipped
}
