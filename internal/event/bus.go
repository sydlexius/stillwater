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
	ArtistNew       Type = "artist.new"
	MetadataFixed   Type = "metadata.fixed"
	ReviewNeeded    Type = "review.needed"
	RuleViolation   Type = "rule.violation"
	BulkCompleted   Type = "bulk.completed"
	ScanCompleted   Type = "scan.completed"
	LidarrArtistAdd Type = "lidarr.artist.add"
	LidarrDownload  Type = "lidarr.download"
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

// Publish sends an event to the bus. Non-blocking; drops with a warning if the buffer is full.
func (b *Bus) Publish(e Event) {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	select {
	case b.ch <- e:
	default:
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
