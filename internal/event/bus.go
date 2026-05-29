package event

import (
	"log/slog"
	"sync"
	"time"
)

// Type identifies a category of event.
type Type string

// Known event types.
const (
	ArtistNew            Type = "artist.new"
	MetadataFixed        Type = "metadata.fixed"
	ReviewNeeded         Type = "review.needed"
	RuleViolation        Type = "rule.violation"
	BulkCompleted        Type = "bulk.completed"
	ScanCompleted        Type = "scan.completed"
	LidarrArtistAdd      Type = "lidarr.artist.add"
	LidarrDownload       Type = "lidarr.download"
	EmbyArtistUpdate     Type = "emby.artist.update"
	EmbyLibraryScan      Type = "emby.library.scan"
	JellyfinArtistUpdate Type = "jellyfin.artist.update"
	JellyfinLibraryScan  Type = "jellyfin.library.scan"
	ArtistUpdated        Type = "artist.updated"
	FSDirCreated         Type = "fs.dir.created"
	FSDirRemoved         Type = "fs.dir.removed"
	FSUnexpectedWrite    Type = "fs.unexpected.write"
	// ConflictChanged fires when the conflict ledger transitions between
	// states (clean / image-writeback / NFO-writeback / round-trip).
	// Subscribed by the SSE hub so the banner refetches and by any tests
	// verifying gate state changes are observable.
	ConflictChanged Type = "conflict.changed"
	// OperationProgress fires for long-running operations whose state is
	// surfaced by the global ProgressPill. Each event carries an op_id, a
	// human-readable label, processed/total counters, a status (running /
	// completed / failed / canceled), and an optional cancel_url. One pill
	// is rendered per distinct op_id; subsequent events update it in place
	// until a terminal status is observed.
	OperationProgress Type = "operation.progress"
	// ConnectionPushFailed fires when a fire-and-forget push to an external
	// platform connection (Emby/Jellyfin lock-sync, metadata push) returns
	// an error from its goroutine. The SSE hub broadcasts it as a toast
	// because the originating handler has already returned success to the
	// caller, so there is no other way for the operator to learn that the
	// platform write failed.
	ConnectionPushFailed Type = "connection.push_failed"

	// --- M55 next-channel events (catalog defined by #1341) ---

	// ActivityRecent carries a single recent-activity item for the next
	// dashboard's live activity rail. Data: {ts, kind, text, artistId?}.
	// Emission is wired by the dashboard work (#1334); the type and SSE
	// mapping live here so that issue does not have to touch the catalog.
	ActivityRecent Type = "activity.recent"
	// SettingsChanged fires after a successful preferences/settings write so
	// other open tabs can refetch or toast. Data: {sectionId, updatedBy, ts}.
	SettingsChanged Type = "settings.changed"
	// DashboardActionResolved mirrors the existing "dashboard:action-resolved"
	// HTMX trigger onto the SSE bus so the action-queue badge updates across
	// tabs, not just in the tab that resolved the action.
	DashboardActionResolved Type = "dashboard.action-resolved"
	// LogsLine and LogsThrottled are emitted by the dedicated logs stream
	// (#1338 GET /api/v1/logs/stream), NOT broadcast through the main events
	// hub (a raw log firehose must not fan out to every connected client).
	// The types are cataloged here so #1338 can rely on the envelope shapes:
	// LogsLine Data carries a structured log record; LogsThrottled Data
	// carries {dropped, window} when the server-side rate limit sheds lines.
	LogsLine      Type = "logs.line"
	LogsThrottled Type = "logs.throttled"
)

// Event represents something that happened in the system.
type Event struct {
	Type      Type           `json:"type"`
	Timestamp time.Time      `json:"timestamp"`
	Data      map[string]any `json:"data,omitempty"`
}

// Handler is a function that processes an event.
type Handler func(Event)

// Bus is an in-process event bus backed by a buffered channel.
type Bus struct {
	ch      chan Event
	mu      sync.RWMutex
	subs    map[Type][]Handler
	logger  *slog.Logger
	done    chan struct{}
	stopped bool
}

// NewBus creates a new event bus with the given buffer size.
func NewBus(logger *slog.Logger, bufSize int) *Bus {
	if bufSize <= 0 {
		bufSize = 256
	}
	return &Bus{
		ch:     make(chan Event, bufSize),
		subs:   make(map[Type][]Handler),
		logger: logger,
		done:   make(chan struct{}),
	}
}

// Subscribe registers a handler for the given event type.
func (b *Bus) Subscribe(t Type, h Handler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs[t] = append(b.subs[t], h)
}

// Publish sends an event to the bus. Non-blocking; drops with a warning
// if the buffer is full.
//
// ConnectionPushFailed is escalated to slog.Error with the full Data
// payload because it is low-volume + high-importance: a silent drop under
// backpressure means the operator misses platform write failures during
// exactly the worst time (a push storm during heavy bulk activity). The
// Data dump preserves the artist + connection context so the failure is
// recoverable from logs even when the SSE pipe was overrun.
func (b *Bus) Publish(e Event) {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	select {
	case b.ch <- e:
	default:
		if e.Type == ConnectionPushFailed {
			b.logger.Error("event bus full, dropping connection push failure",
				"type", string(e.Type),
				"data", e.Data,
			)
			return
		}
		b.logger.Warn("event bus full, dropping event", "type", string(e.Type))
	}
}

// Start begins draining the channel and dispatching events to subscribers.
// Call this in a goroutine. It blocks until Stop is called.
func (b *Bus) Start() {
	for {
		select {
		case e := <-b.ch:
			b.dispatch(e)
		case <-b.done:
			// Drain remaining events
			for {
				select {
				case e := <-b.ch:
					b.dispatch(e)
				default:
					return
				}
			}
		}
	}
}

// Stop signals the bus to stop processing events after draining the buffer.
func (b *Bus) Stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.stopped {
		b.stopped = true
		close(b.done)
	}
}

func (b *Bus) dispatch(e Event) {
	b.mu.RLock()
	handlers := b.subs[e.Type]
	b.mu.RUnlock()

	for _, h := range handlers {
		func() {
			defer func() {
				if r := recover(); r != nil {
					b.logger.Error("event handler panicked", "type", string(e.Type), "panic", r)
				}
			}()
			h(e)
		}()
	}
}
