package rule

import (
	"context"
	"log/slog"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/event"
)

// dirtyMarkTimeout caps how long a single dirty-mark write is allowed to
// take. The event bus dispatch loop is single-threaded; a hung write would
// stall every other subscriber. The timeout ensures we drop on the floor
// rather than block the bus indefinitely.
const dirtyMarkTimeout = 5 * time.Second

// DirtySubscriber listens for artist mutation events and stamps dirty_since
// on the affected artist so the next "Run Rules" pass picks them up. This
// is the inverse of HealthSubscriber: HealthSubscriber consumes mutations
// to recompute scores; DirtySubscriber consumes mutations to schedule
// rule re-evaluation.
//
// Unlike HealthSubscriber there is no debounce: a single SQL UPDATE per
// event is cheap, and over-stamping dirty_since (e.g. multiple events for
// the same artist) is a no-op for correctness because the timestamp only
// matters in relation to rules_evaluated_at.
type DirtySubscriber struct {
	artistService *artist.Service
	logger        *slog.Logger
}

// NewDirtySubscriber wires a subscriber that marks artists dirty when
// they receive ArtistUpdated events. If artistService is nil, all
// HandleEvent calls are no-ops to allow graceful degradation.
func NewDirtySubscriber(artistService *artist.Service, logger *slog.Logger) *DirtySubscriber {
	return &DirtySubscriber{
		artistService: artistService,
		logger:        logger.With(slog.String("component", "rule-dirty-subscriber")),
	}
}

// HandleEvent extracts the artist_id from the event's data map and stamps
// dirty_since for that artist. Errors are logged but never propagated, in
// keeping with the event bus contract. Safe to call from any goroutine.
//
// The event bus dispatches handlers serially within a single goroutine.
// Keeping HandleEvent fast (one indexed UPDATE) is therefore important;
// see dirtyMarkTimeout for the upper bound enforced if the DB is wedged.
func (d *DirtySubscriber) HandleEvent(e event.Event) {
	if d.artistService == nil {
		return
	}

	raw, ok := e.Data["artist_id"]
	if !ok {
		d.logger.Warn("dirty subscriber: event missing artist_id", "event_type", string(e.Type))
		return
	}
	artistID, ok := raw.(string)
	if !ok || artistID == "" {
		d.logger.Warn("dirty subscriber: invalid artist_id in event", "event_type", string(e.Type))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), dirtyMarkTimeout)
	defer cancel()

	if err := d.artistService.MarkDirty(ctx, artistID, time.Now().UTC()); err != nil {
		d.logger.Warn("dirty subscriber: marking artist dirty",
			"artist_id", artistID,
			"event_type", string(e.Type),
			"error", err)
	}
}
