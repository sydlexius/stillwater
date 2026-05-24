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
// cannot monopolize the singleton bulk-action slot indefinitely. Sourced
// from artist.MaxListIDs so the API request cap and the domain-layer
// IDs-filter cap stay in lockstep without a comment-only contract.
const MaxBulkActionIDs = artist.MaxListIDs

// Allowed bulk action types.
//
// BulkActionReIdentify is the legacy alias retained so existing callers keep
// working; it is normalized to BulkActionReIdentifyAuto on entry. The review
// variant (re_identify_review string in the UI) is dispatched separately
// through the wizard endpoints in handlers_reidentify_wizard.go and never
// flows through this handler, so no Go constant is defined for it here.
const (
	BulkActionRunRules       = "run_rules"
	BulkActionReIdentify     = "re_identify"      // legacy alias for re_identify_auto
	BulkActionReIdentifyAuto = "re_identify_auto" // silent auto-link + queue path
	BulkActionScan           = "scan"
	BulkActionFetchImages    = "fetch_images"
	// Lock / unlock are the bulk-equivalent of POST/DELETE /api/v1/artists/{id}/lock.
	// Both set source="user" via artist.Service.Lock / Unlock; idempotent (a
	// no-op call counts the artist as Skipped). They never invoke the rule
	// pipeline so they require only the artist service to be configured.
	BulkActionLock   = "lock"
	BulkActionUnlock = "unlock"
)

// idPattern accepts UUIDs and other plausible artist ID encodings used in the
// repository (UUIDs and ULIDs are both used in different code paths). We keep
// the character class conservative so arbitrary input cannot leak through.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// bulkActionStatus enumerates the terminal and in-flight lifecycle states a
// BulkActionProgress can occupy. The underlying string type preserves the
// existing JSON wire format ("running", "completed", "failed", "canceled")
// while preventing typo-driven drift at call sites.
type bulkActionStatus string

const (
	bulkActionRunning   bulkActionStatus = "running"
	bulkActionCompleted bulkActionStatus = "completed"
	bulkActionFailed    bulkActionStatus = "failed"
	bulkActionCanceled  bulkActionStatus = "canceled"
)

// BulkActionProgress tracks the state of an in-flight bulk action. It is
// mutex-protected and shared between the request handler and the background
// goroutine processing the IDs.
type BulkActionProgress struct {
	mu          sync.RWMutex
	Action      string           `json:"action"`
	Status      bulkActionStatus `json:"status"`
	Total       int              `json:"total"`
	Processed   int              `json:"processed"`
	Succeeded   int              `json:"succeeded"`
	Skipped     int              `json:"skipped"`
	Failed      int              `json:"failed"`
	CurrentName string           `json:"current_name"`
	// Re-identify-specific breakdown. These remain zero for other actions,
	// so the bulk-completion toast can detect the re_identify_auto path by
	// checking whether any of them are non-zero. Populated in addition to
	// the generic succeeded/skipped/failed counters so existing consumers
	// keep working unchanged.
	AutoLinked  int `json:"auto_linked"`
	Queued      int `json:"queued"`
	NoMatch     int `json:"no_match"`
	StartedAt   time.Time
	CompletedAt time.Time
	// cancelFn cancels the detached goroutine running this action. Stored on
	// the progress so the cancel handler can reach it without a separate
	// registry. nil for terminal snapshots.
	cancelFn context.CancelFunc
}

// snapshot copies the progress state for safe JSON marshaling. Status is
// down-cast to its underlying string so external consumers (HTTP clients,
// test assertions reading the map) compare against plain string literals
// without needing to know the bulkActionStatus type.
func (p *BulkActionProgress) snapshot() map[string]any {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return map[string]any{
		"action":       p.Action,
		"status":       string(p.Status),
		"total":        p.Total,
		"processed":    p.Processed,
		"succeeded":    p.Succeeded,
		"skipped":      p.Skipped,
		"failed":       p.Failed,
		"current_name": p.CurrentName,
		"auto_linked":  p.AutoLinked,
		"queued":       p.Queued,
		"no_match":     p.NoMatch,
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
	case BulkActionRunRules,
		BulkActionReIdentify,
		BulkActionReIdentifyAuto,
		BulkActionScan,
		BulkActionFetchImages,
		BulkActionLock,
		BulkActionUnlock:
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
// requireServicesForBulkAction returns the operator-facing message that
// belongs in a 503 response when the router is not configured to run
// the given action, or "" when every required dependency is wired.
//
// Lives outside handleBulkAction so the per-action switch is not folded
// into the request handler's cognitive complexity budget -- the inline
// version of this gate pushed handleBulkAction over the gocognit cap
// after BulkActionLock / BulkActionUnlock were added in PR7.
//
// Action gate semantics:
//
//   - run_rules, fetch_images: pipeline + artist service (runBulkAction
//     reads each artist via GetByID before dispatch; a missing pipeline
//     would silently skip every artist).
//   - re_identify_auto: artist service only (identify flow rehydrates
//     each candidate; orchestrator presence is checked deeper down).
//   - lock, unlock: artist service only (no rule pipeline hop).
//   - scan: artist service + pipeline (scan reuses RunForArtist).
//
// Unknown actions return "" because validActions() upstream already
// rejected them with a 400 before this gate runs.
func (r *Router) requireServicesForBulkAction(action string) string {
	const (
		msgArtistMissing   = "artist service not configured"
		msgPipelineMissing = "rule pipeline not configured"
	)
	switch action {
	case BulkActionRunRules, BulkActionFetchImages:
		if r.pipeline == nil {
			return msgPipelineMissing
		}
		if r.artistService == nil {
			return msgArtistMissing
		}
	case BulkActionReIdentifyAuto:
		if r.artistService == nil {
			return msgArtistMissing
		}
	case BulkActionLock, BulkActionUnlock:
		if r.artistService == nil {
			return msgArtistMissing
		}
	case BulkActionScan:
		if r.artistService == nil {
			return msgArtistMissing
		}
		if r.pipeline == nil {
			return msgPipelineMissing
		}
	}
	return ""
}

func (r *Router) handleBulkAction(w http.ResponseWriter, req *http.Request) {
	// Parse and validate the request body before claiming the singleton slot
	// so a malformed request cannot lock out legitimate ones. The 1 MiB cap
	// on the raw body is enforced before JSON decoding so a hostile caller
	// cannot force a large allocation to reach the downstream per-request
	// MaxBulkActionIDs cap.
	req.Body = http.MaxBytesReader(w, req.Body, 1<<20)
	var body bulkActionRequest
	dec := json.NewDecoder(req.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	// Reject bodies that contain extra JSON tokens after the object so
	// `{...}{...}` smuggling or accidental concatenation does not silently
	// succeed on only the first object.
	if err := dec.Decode(&struct{}{}); err == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unexpected trailing data in body"})
		return
	} else if !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if !validActions(body.Action) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid or missing action"})
		return
	}
	// Normalize the legacy alias. Everything downstream treats the auto and
	// alias keys identically; carrying one canonical value keeps snapshots
	// and switch statements simple.
	if body.Action == BulkActionReIdentify {
		body.Action = BulkActionReIdentifyAuto
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
		Status:    bulkActionRunning,
		Total:     len(ids),
		StartedAt: time.Now().UTC(),
	}
	r.bulkActionMu.Lock()
	if r.bulkActionProgress != nil {
		r.bulkActionProgress.mu.RLock()
		running := r.bulkActionProgress.Status == bulkActionRunning
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

	// Release the slot if a required service is missing so the next caller
	// is not blocked by a failed start.
	releaseSlot := func() {
		r.bulkActionMu.Lock()
		if r.bulkActionProgress == progress {
			r.bulkActionProgress = nil
		}
		r.bulkActionMu.Unlock()
	}
	if msg := r.requireServicesForBulkAction(body.Action); msg != "" {
		releaseSlot()
		writeError(w, req, http.StatusServiceUnavailable, msg)
		return
	}

	bulkCtx := r.injectMetadataLanguages(req.Context())
	r.runBulkAction(bulkCtx, body.Action, ids, progress)

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

// handleBulkActionCancel signals the in-flight bulk action (if any) to stop.
// The background goroutine observes ctx.Err() on the next loop iteration and
// finalizes the progress with status="canceled".
//
// POST /api/v1/artists/bulk-actions/cancel
func (r *Router) handleBulkActionCancel(w http.ResponseWriter, _ *http.Request) {
	r.bulkActionMu.RLock()
	progress := r.bulkActionProgress
	r.bulkActionMu.RUnlock()
	if progress == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "no bulk action in progress"})
		return
	}
	progress.mu.Lock()
	running := progress.Status == bulkActionRunning
	cancel := progress.cancelFn
	progress.mu.Unlock()
	if !running || cancel == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "no bulk action in progress"})
		return
	}
	cancel()
	writeJSON(w, http.StatusOK, map[string]string{"status": "canceling"})
}

// runBulkAction executes the action for each artist ID in a detached goroutine.
// Uses a mutex-protected progress struct so status polls see consistent state.
//
//nolint:gocognit // Action-dispatch worker: per-action branches (lock/unlock/delete/refresh) each have distinct prerequisites and outcome accounting, all wrapped in the same cancel-aware loop with mutex-protected progress; the dispatch must stay in one function for the cancel observer to see consistent state transitions.
func (r *Router) runBulkAction(reqCtx context.Context, action string, ids []string, progress *BulkActionProgress) {
	// Detach from the request lifecycle but keep request-scoped values
	// (user, logger, etc.). fix-all uses the same pattern. Wrap with a
	// cancel so the cancel handler can stop the loop mid-run.
	ctx, cancel := context.WithCancel(context.WithoutCancel(reqCtx))
	progress.mu.Lock()
	progress.cancelFn = cancel
	progress.mu.Unlock()

	// opID is the stable identifier used by the ProgressPill to coalesce
	// repeated progress events into a single pill. Bulk actions are
	// singleton (the handler 409s on concurrent starts), so a per-action
	// constant works without collision risk.
	const opID = "bulk_action"
	// Emit a "running" event up-front so the pill appears immediately
	// even before the first per-artist tick lands.
	r.publishOpProgress(opID, action, progress.Total, 0, "running", "/api/v1/artists/bulk-actions/cancel")

	go func() {
		defer cancel()

		// Build the connection index once for re-identify so per-artist
		// work inside the loop stays cheap.
		var connIdx *connectionIndex
		if action == BulkActionReIdentifyAuto {
			connIdx = r.buildConnectionIndex(ctx)
		}

		// Pre-load every artist in a single batch query instead of issuing
		// one GetByID per loop iteration (#1410). At MaxBulkActionIDs=1000
		// the legacy path issued 1000 + 3000 hydrate round-trips; the
		// batched form is 1 + 3 regardless of input size. ErrNotFound is
		// represented by a missing map entry below so the original
		// "not-found is a skip" outcome is preserved unchanged.
		artistsByID, loadErr := r.artistService.GetByIDsBatch(ctx, ids)
		if loadErr != nil {
			// A batch-wide load failure is logged once and then every ID
			// counts as Failed in the loop below so the progress totals
			// stay consistent with the per-ID error path the loop already
			// understands.
			r.logger.Warn("bulk action: batch load failed", "action", action, "ids", len(ids), "error", loadErr)
			artistsByID = map[string]*artist.Artist{}
		}

		for _, id := range ids {
			// Cancellation check. Break out of the loop and flag the
			// progress as canceled so the completion path surfaces a
			// distinct status rather than a successful completion on a
			// short-circuited run.
			if ctx.Err() != nil {
				break
			}
			a, ok := artistsByID[id]
			if !ok {
				progress.mu.Lock()
				progress.Processed++
				if loadErr != nil {
					// Whole batch failed; surface every ID as Failed so
					// the operator sees the real outcome instead of a
					// silent "everything skipped" run.
					progress.Failed++
				} else {
					// ID was in the request but absent from the result
					// set: same semantics as the legacy GetByID returning
					// artist.ErrNotFound (treat as Skipped).
					progress.Skipped++
				}
				progress.mu.Unlock()
				continue
			}

			progress.mu.Lock()
			progress.CurrentName = a.Name
			progress.mu.Unlock()

			// The re-identify auto path needs a finer-grained outcome
			// (auto-linked vs queued vs no-match) than the generic
			// succeeded/skipped/failed triad exposes. Handle it inline so
			// those counters land on the progress snapshot for the
			// completion toast.
			if action == BulkActionReIdentifyAuto {
				res := r.identifyArtist(ctx, a, connIdx)
				// For outcomeQueued, identifyArtist returns a review
				// candidate that must land in the bulk-identify review
				// queue so the user can decide later. Previously the
				// bulk-action path dropped res.Candidate on the floor,
				// silently losing every ambiguous match.
				if res.Outcome == outcomeQueued && res.Candidate != nil {
					r.identifyMu.Lock()
					if r.identifyProgress == nil {
						r.identifyProgress = &IdentifyProgress{Status: "completed"}
					}
					rp := r.identifyProgress
					r.identifyMu.Unlock()
					rp.mu.Lock()
					rp.ReviewQueue = append(rp.ReviewQueue, *res.Candidate)
					rp.mu.Unlock()
				}
				progress.mu.Lock()
				progress.Processed++
				switch res.Outcome {
				case outcomeAutoLinked:
					progress.AutoLinked++
					progress.Succeeded++
				case outcomeQueued:
					progress.Queued++
					progress.Skipped++
				case outcomeUnmatched:
					progress.NoMatch++
					progress.Skipped++
				case outcomeSkipped:
					progress.Skipped++
				case outcomeFailed:
					progress.Failed++
				}
				processed := progress.Processed
				total := progress.Total
				progress.mu.Unlock()
				step := total / 20
				if step < 1 {
					step = 1
				}
				if processed%step == 0 || processed == total {
					r.publishOpProgress(opID, action, total, processed, "running", "/api/v1/artists/bulk-actions/cancel")
				}
				time.Sleep(10 * time.Millisecond)
				continue
			}

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
			processed := progress.Processed
			total := progress.Total
			progress.mu.Unlock()

			// Throttle ProgressPill updates: one event per artist is fine
			// at 10ms cadence (100/s) for small runs, but big batches
			// would flood the SSE hub. Emit every event for the first
			// few then every 5% (rounded up) once the run is large.
			step := total / 20
			if step < 1 {
				step = 1
			}
			if processed%step == 0 || processed == total {
				r.publishOpProgress(opID, action, total, processed, "running", "/api/v1/artists/bulk-actions/cancel")
			}

			// Yield between artists so HTTP handlers can acquire the SQLite
			// write lock. Matches the fix-all / bulk-identify cadence.
			time.Sleep(10 * time.Millisecond)
		}

		progress.mu.Lock()
		// A cancel POST can land after the last artist has already been
		// processed but before this epilogue runs. In that race ctx.Err()
		// is non-nil yet every item is complete; reporting "canceled" here
		// lies to /status and the completion event. Gate on remaining work.
		finalStatus := bulkActionCompleted
		if ctx.Err() != nil && progress.Processed < progress.Total {
			finalStatus = bulkActionCanceled
		}
		progress.Status = finalStatus
		progress.CurrentName = ""
		progress.CompletedAt = time.Now().UTC()
		progress.cancelFn = nil
		succeeded := progress.Succeeded
		failed := progress.Failed
		total := progress.Total
		progress.mu.Unlock()

		// Surface the completion via the event bus so the SSE hub can
		// broadcast a toast to connected clients. Canceled runs emit the
		// same event type with status="canceled" so the client can show a
		// distinct toast.
		if r.eventBus != nil {
			r.eventBus.Publish(event.Event{
				Type: event.BulkCompleted,
				Data: map[string]any{
					"type":      action,
					"status":    finalStatus,
					"total":     total,
					"succeeded": succeeded,
					"failed":    failed,
				},
			})
		}
		// Terminal ProgressPill event. ProgressPill JS auto-dismisses
		// completed pills and keeps failed pills sticky until dismissed,
		// so we just relay the final status string.
		pillStatus := "completed"
		if failed > 0 {
			pillStatus = "failed"
		}
		if finalStatus == bulkActionCanceled {
			pillStatus = "canceled"
		}
		r.publishOpProgress(opID, action, total, total, pillStatus, "")

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
	// Rule pipeline short-circuits locked/excluded artists to a nil no-op.
	// Mirror that here so the bulk summary counts them as skipped rather
	// than inflating Succeeded for actions that didn't actually run.
	if (action == BulkActionRunRules || action == BulkActionFetchImages || action == BulkActionScan) && (a.IsExcluded || a.Locked) {
		return bulkOutcomeSkipped
	}
	switch action {
	case BulkActionRunRules:
		// run_rules evaluates every enabled rule and auto-fixes those in
		// auto mode.
		if _, err := r.pipeline.RunForArtist(ctx, a); err != nil {
			r.logger.Warn("bulk action: RunForArtist failed", "action", action, "artist_id", a.ID, "error", err)
			return bulkOutcomeFailed
		}
		return bulkOutcomeSucceeded

	case BulkActionFetchImages:
		// fetch_images must be scoped to image-category rules so it cannot
		// mutate NFO/metadata as a side effect of auto-mode fixers firing
		// for other categories.
		if _, err := r.pipeline.RunImageRulesForArtist(ctx, a); err != nil {
			r.logger.Warn("bulk action: RunImageRulesForArtist failed", "artist_id", a.ID, "error", err)
			return bulkOutcomeFailed
		}
		return bulkOutcomeSucceeded

	case BulkActionReIdentifyAuto:
		// Normally handled inline in runBulkAction so the auto/queued/
		// no-match breakdown lands on the progress snapshot. Retained here
		// as a defensive fallback so any future caller that dispatches
		// through applyBulkAction still gets consistent succeeded/skipped
		// semantics instead of silently falling through to skipped.
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
		// a separate filesystem walk. Pipeline availability is guaranteed
		// by the upfront service gate in handleBulkAction.
		if _, err := r.pipeline.RunForArtist(ctx, a); err != nil {
			r.logger.Warn("bulk action: scan RunForArtist failed", "artist_id", a.ID, "error", err)
			return bulkOutcomeFailed
		}
		return bulkOutcomeSucceeded

	case BulkActionLock:
		// Idempotent: already-locked counts as Skipped so the operator
		// sees a faithful "N changed / M skipped" summary instead of an
		// inflated success count. source="user" mirrors the per-artist
		// POST /api/v1/artists/{id}/lock handler.
		if a.Locked {
			return bulkOutcomeSkipped
		}
		if err := r.artistService.Lock(ctx, a.ID, "user"); err != nil {
			// a.Locked is a snapshot; the artist's lock state can flip
			// between the snapshot read and this write. Treat that race
			// as Skipped (the bulk action's contract is "make it so",
			// not "make it so by my hand") rather than inflating the
			// failure count.
			if errors.Is(err, artist.ErrAlreadyLocked) {
				return bulkOutcomeSkipped
			}
			r.logger.Warn("bulk action: Lock failed", "artist_id", a.ID, "error", err)
			return bulkOutcomeFailed
		}
		return bulkOutcomeSucceeded

	case BulkActionUnlock:
		if !a.Locked {
			return bulkOutcomeSkipped
		}
		if err := r.artistService.Unlock(ctx, a.ID); err != nil {
			// Same race-Skipped treatment as Lock above.
			if errors.Is(err, artist.ErrNotLocked) {
				return bulkOutcomeSkipped
			}
			r.logger.Warn("bulk action: Unlock failed", "artist_id", a.ID, "error", err)
			return bulkOutcomeFailed
		}
		return bulkOutcomeSucceeded
	}

	return bulkOutcomeSkipped
}
