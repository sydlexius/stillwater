package logging

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
)

// defaultLogSubBuffer is the per-subscriber channel capacity for the live log
// stream. The general SSE hub uses 16, but a raw log feed is far burstier than
// the hub's coalesced domain events, so a deeper buffer absorbs short spikes
// (a noisy scan, a burst of warnings) without shedding lines. When the buffer
// does fill, the broadcaster drops the line and raises a throttle signal rather
// than blocking the logging goroutine.
const defaultLogSubBuffer = 256

// Subscription is a live feed of log entries matching a LogFilter, handed back
// by LogBroadcaster.Subscribe. The caller ranges over Lines() for matching
// records and watches Throttle() to learn when the per-subscriber buffer
// overflowed (calling DrainDropped to read and reset the dropped count). The
// caller MUST call Close exactly once (typically via defer) to release the
// subscription; Close is idempotent and safe to call concurrently with the
// broadcaster's fan-out.
type Subscription struct {
	filter   LogFilter
	lines    chan LogEntry
	throttle chan struct{}
	dropped  atomic.Int64
	hub      *logHub
}

// Lines returns the channel of live log entries matching the subscription's
// filter. It is closed when the subscription is closed.
func (s *Subscription) Lines() <-chan LogEntry { return s.lines }

// Throttle returns a channel that receives a (coalesced) signal whenever the
// subscriber's buffer overflows and one or more lines are dropped. The signal
// carries no payload; call DrainDropped to read the accumulated dropped count.
// It is closed when the subscription is closed.
func (s *Subscription) Throttle() <-chan struct{} { return s.throttle }

// DrainDropped atomically returns the number of lines dropped since the last
// call and resets the counter to zero.
func (s *Subscription) DrainDropped() int { return int(s.dropped.Swap(0)) }

// Close removes the subscription from the broadcaster and closes its channels.
// It is idempotent.
func (s *Subscription) Close() { s.hub.unsubscribe(s) }

// logHub is the shared subscriber registry behind a LogBroadcaster and all of
// its WithAttrs/WithGroup derivations. Splitting it out (mirroring how every
// derived RingHandler shares one *RingBuffer) means a derived logger publishes
// to the same set of subscribers as its parent.
type logHub struct {
	mu   sync.RWMutex
	subs map[*Subscription]struct{}
	buf  int // per-subscriber channel capacity
}

func (h *logHub) subscribe(filter LogFilter) *Subscription {
	s := &Subscription{
		filter:   filter,
		lines:    make(chan LogEntry, h.buf),
		throttle: make(chan struct{}, 1),
		hub:      h,
	}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	return s
}

func (h *logHub) unsubscribe(s *Subscription) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[s]; !ok {
		return // already removed; Close is idempotent
	}
	delete(h.subs, s)
	close(s.lines)
	close(s.throttle)
}

// publish fans an entry out to every subscriber whose filter matches. Sends are
// non-blocking: a full subscriber buffer drops the line and raises a coalesced
// throttle signal so a slow client can never stall the logging goroutine. It
// takes the read lock; unsubscribe takes the write lock, so a channel is never
// closed while publish may still send to it.
func (h *logHub) publish(entry LogEntry) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for s := range h.subs {
		if !s.filter.Matches(entry) {
			continue
		}
		select {
		case s.lines <- entry:
		default:
			// Buffer full: drop the line, accumulate the count, and raise a
			// coalesced throttle signal (cap-1 channel, non-blocking send).
			s.dropped.Add(1)
			select {
			case s.throttle <- struct{}{}:
			default:
			}
		}
	}
}

func (h *logHub) count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs)
}

// LogBroadcaster is an slog.Handler that fans live log records out to live
// subscribers (the next/ logs viewer). It is meant to sit alongside the primary
// and ring handlers in the MultiHandler. Unlike the general SSE hub, it never
// touches the event bus: a raw log firehose must not be broadcast to every
// connected client, so it is delivered only to clients that explicitly open the
// dedicated /api/v1/logs/stream endpoint.
//
// It is safe for concurrent use. WithAttrs/WithGroup derivations share the same
// subscriber registry as the parent.
type LogBroadcaster struct {
	hub       *logHub
	level     slog.Leveler
	addSource bool
	attrs     []slog.Attr
	group     string
}

// NewLogBroadcaster creates a broadcaster capturing at the given level. When
// addSource is true the source file:line and derived component are populated,
// matching the ring handler so the live tail and the buffered view agree.
func NewLogBroadcaster(level slog.Leveler, addSource bool) *LogBroadcaster {
	return newLogBroadcasterWithBuffer(level, addSource, defaultLogSubBuffer)
}

// newLogBroadcasterWithBuffer is the buffer-size-injectable constructor used by
// tests to exercise the buffer-full throttle path with a small buffer.
func newLogBroadcasterWithBuffer(level slog.Leveler, addSource bool, buf int) *LogBroadcaster {
	if buf <= 0 {
		buf = defaultLogSubBuffer
	}
	return &LogBroadcaster{
		hub: &logHub{
			subs: make(map[*Subscription]struct{}),
			buf:  buf,
		},
		level:     level,
		addSource: addSource,
	}
}

// Subscribe registers a new live subscriber filtered by filter and returns its
// Subscription. The caller must Close the subscription when done. The filter's
// Limit and After fields are ignored for the live tail (they govern backfill
// from the ring buffer, not the forward stream).
func (b *LogBroadcaster) Subscribe(filter LogFilter) *Subscription {
	// Strip the backfill-only fields so the live matcher only applies the
	// forward-looking predicates (level/component/search).
	live := LogFilter{
		Level:     filter.Level,
		Search:    filter.Search,
		Component: filter.Component,
	}
	return b.hub.subscribe(live)
}

// SubscriberCount returns the number of active subscribers.
func (b *LogBroadcaster) SubscriberCount() int { return b.hub.count() }

// Enabled reports whether the handler captures records at the given level.
func (b *LogBroadcaster) Enabled(_ context.Context, level slog.Level) bool {
	return level >= b.level.Level()
}

// Handle converts the record to a LogEntry and publishes it to subscribers.
// When there are no subscribers it skips the (allocating) conversion entirely,
// so the broadcaster costs almost nothing while no log viewer is open.
func (b *LogBroadcaster) Handle(_ context.Context, r slog.Record) error {
	if b.hub.count() == 0 {
		return nil
	}
	b.hub.publish(recordToEntry(r, b.attrs, b.group, b.addSource))
	return nil
}

// WithAttrs returns a derived broadcaster with the given attributes pre-stored,
// sharing the parent's subscriber registry.
func (b *LogBroadcaster) WithAttrs(attrs []slog.Attr) slog.Handler {
	newAttrs := make([]slog.Attr, len(b.attrs), len(b.attrs)+len(attrs))
	copy(newAttrs, b.attrs)
	newAttrs = append(newAttrs, attrs...)
	return &LogBroadcaster{
		hub:       b.hub,
		level:     b.level,
		addSource: b.addSource,
		attrs:     newAttrs,
		group:     b.group,
	}
}

// WithGroup returns a derived broadcaster with the given group name appended,
// sharing the parent's subscriber registry.
func (b *LogBroadcaster) WithGroup(name string) slog.Handler {
	newGroup := name
	if b.group != "" {
		newGroup = b.group + "." + name
	}
	newAttrs := make([]slog.Attr, len(b.attrs))
	copy(newAttrs, b.attrs)
	return &LogBroadcaster{
		hub:       b.hub,
		level:     b.level,
		addSource: b.addSource,
		attrs:     newAttrs,
		group:     newGroup,
	}
}
