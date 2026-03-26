package rule

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/event"
)

const (
	// debounceWindow is the delay between receiving an event and evaluating
	// the artist's health score. Events arriving within this window for the
	// same artist coalesce into a single evaluation.
	debounceWindow = 2 * time.Second

	// tickInterval is how often the background goroutine checks for pending
	// evaluations whose debounce window has expired.
	tickInterval = 500 * time.Millisecond

	// bootstrapBatchSize is the number of artists processed per batch during
	// bootstrap before sleeping to avoid starving other work.
	bootstrapBatchSize = 25

	// bootstrapBatchSleep is the pause between bootstrap batches.
	bootstrapBatchSleep = 100 * time.Millisecond
)

// HealthSubscriber listens for ArtistUpdated events and re-evaluates the
// artist's health score after a debounce window. This decouples mutation
// handlers from the rule engine while keeping stored scores fresh.
type HealthSubscriber struct {
	engine        *Engine
	artistService *artist.Service
	logger        *slog.Logger

	mu      sync.Mutex
	pending map[string]time.Time // artist ID -> earliest evaluation time

	done chan struct{}
	wg   sync.WaitGroup
}

// NewHealthSubscriber creates a subscriber that re-evaluates artist health
// scores when ArtistUpdated events are received. If engine is nil, all
// operations are no-ops for graceful degradation.
func NewHealthSubscriber(engine *Engine, artistService *artist.Service, logger *slog.Logger) *HealthSubscriber {
	return &HealthSubscriber{
		engine:        engine,
		artistService: artistService,
		logger:        logger,
		pending:       make(map[string]time.Time),
		done:          make(chan struct{}),
	}
}

// HandleEvent processes an ArtistUpdated event by scheduling a debounced
// health re-evaluation for the artist. Safe to call from any goroutine.
func (h *HealthSubscriber) HandleEvent(e event.Event) {
	if h.engine == nil {
		return
	}

	raw, exists := e.Data["artist_id"]
	if !exists {
		h.logger.Warn("health subscriber: event missing artist_id", "event_type", string(e.Type))
		return
	}
	artistID, ok := raw.(string)
	if !ok || artistID == "" {
		h.logger.Warn("health subscriber: invalid artist_id in event", "event_type", string(e.Type))
		return
	}

	evalAt := time.Now().Add(debounceWindow)

	h.mu.Lock()
	h.pending[artistID] = evalAt
	h.mu.Unlock()
}

// Start begins the background ticker that processes pending evaluations.
// Blocks until Stop is called or ctx is canceled.
func (h *HealthSubscriber) Start(ctx context.Context) {
	if h.engine == nil {
		return
	}

	h.wg.Add(1)
	defer h.wg.Done()

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			h.processPending(ctx)
		case <-h.done:
			// Drain any remaining pending evaluations
			h.processPending(ctx)
			return
		case <-ctx.Done():
			return
		}
	}
}

// Stop signals the background goroutine to finish and waits for it.
func (h *HealthSubscriber) Stop() {
	select {
	case <-h.done:
		// Already stopped
	default:
		close(h.done)
	}
	h.wg.Wait()
}

// Bootstrap re-evaluates health scores for artists that have a zero score,
// which typically means they were created before the event-based health
// system was active. Runs in batches to avoid starving other work.
func (h *HealthSubscriber) Bootstrap(ctx context.Context) {
	if h.engine == nil {
		return
	}

	ids, err := h.artistService.ListZeroHealthIDs(ctx)
	if err != nil {
		h.logger.Error("bootstrapping health scores: listing zero-score artists", "error", err)
		return
	}

	if len(ids) == 0 {
		return
	}

	h.logger.Info("bootstrapping health scores", "count", len(ids))

	done := 0
	for i, id := range ids {
		if ctx.Err() != nil {
			h.logger.Info("bootstrap canceled", "completed", done, "total", len(ids))
			return
		}

		h.evaluateArtist(ctx, id)
		done++

		// Sleep between batches
		if (i+1)%bootstrapBatchSize == 0 && i+1 < len(ids) {
			h.logger.Debug("bootstrapping health scores", "progress", done, "total", len(ids))
			time.Sleep(bootstrapBatchSleep)
		}
	}

	h.logger.Info("bootstrap complete", "evaluated", done)
}

// processPending evaluates all artists whose debounce window has expired.
func (h *HealthSubscriber) processPending(ctx context.Context) {
	now := time.Now()

	h.mu.Lock()
	var ready []string
	for id, evalAt := range h.pending {
		if now.After(evalAt) || now.Equal(evalAt) {
			ready = append(ready, id)
			delete(h.pending, id)
		}
	}
	h.mu.Unlock()

	for _, id := range ready {
		if ctx.Err() != nil {
			return
		}
		h.evaluateArtist(ctx, id)
	}
}

// evaluateArtist loads an artist by ID, runs the rule engine, and persists
// the updated health score. Errors are logged but not propagated.
func (h *HealthSubscriber) evaluateArtist(ctx context.Context, artistID string) {
	a, err := h.artistService.GetByID(ctx, artistID)
	if err != nil {
		h.logger.Warn("health subscriber: loading artist", "artist_id", artistID, "error", err)
		return
	}

	result, err := h.engine.Evaluate(ctx, a)
	if err != nil {
		h.logger.Warn("health subscriber: evaluating artist", "artist_id", artistID, "artist", a.Name, "error", err)
		return
	}

	a.HealthScore = result.HealthScore
	if err := h.artistService.Update(ctx, a); err != nil {
		h.logger.Warn("health subscriber: persisting health score", "artist_id", artistID, "artist", a.Name, "error", err)
	}
}
