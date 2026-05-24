package api

import (
	"log/slog"

	"github.com/sydlexius/stillwater/internal/event"
)

// publishOpProgress emits an event.OperationProgress event carrying the
// shape consumed by the global ProgressPill JS. Callers should emit one
// event when an operation starts, throttled events while it runs, and a
// final event with a terminal status (completed / failed / canceled) so
// the pill auto-dismisses (success) or stays sticky until dismissed
// (failure).
//
// Fields on the event Data map:
//
//	op_id      stable identifier; events with the same op_id coalesce
//	           into one pill in the UI
//	label      human-readable verb (e.g. the bulk-action key); rendered
//	           verbatim today, will be localized client-side in PR7
//	processed  done count
//	total      total work units (0 means indeterminate)
//	status     "running" | "completed" | "failed" | "canceled"
//	cancel_url optional API path that cancels the underlying op; the
//	           pill renders a Cancel button only when this is non-empty
//
// A nil eventBus is a no-op so test routers without a bus continue to
// work unchanged.
//
// opID is hard-coded to "bulk_action" by today's only caller; PR7 wires
// populate-progress and bulk-lock with their own stable IDs, so keeping
// the parameter avoids a churn-only signature change when those land.
//
// TODO(PR7): When populate-progress + bulk-lock add their own op_ids,
// the global cancel singleton invariant must be replaced with per-op_id
// scoping (today's cancel handler unconditionally cancels the lone
// in-flight bulk action).
func (r *Router) publishOpProgress(opID, label string, total, processed int, status, cancelURL string) {
	if r == nil || r.eventBus == nil {
		return
	}
	// Defensive validation: a malformed event would silently render a
	// broken pill (missing op_id collides with the default key; negative
	// totals confuse the renderer; processed > total inverts the bar).
	// Guard at the publisher instead of trusting every caller.
	if opID == "" {
		r.logger.Warn("publishOpProgress called with empty op_id; dropping event",
			slog.String("label", label),
			slog.String("status", status))
		return
	}
	if total < 0 {
		// A negative total is meaningless; treat as indeterminate (0).
		total = 0
	}
	if total > 0 && processed > total {
		r.logger.Debug("publishOpProgress processed > total; clamping",
			slog.String("op_id", opID),
			slog.Int("processed", processed),
			slog.Int("total", total))
		processed = total
	}
	data := map[string]any{
		"op_id":     opID,
		"label":     label,
		"processed": processed,
		"total":     total,
		"status":    status,
	}
	if cancelURL != "" {
		data["cancel_url"] = cancelURL
	}
	r.eventBus.Publish(event.Event{
		Type: event.OperationProgress,
		Data: data,
	})
}
