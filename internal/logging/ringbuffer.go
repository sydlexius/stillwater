package logging

import (
	"strings"
	"sync"
	"time"
)

// LogEntry represents a single captured log record.
type LogEntry struct {
	Time      time.Time      `json:"time"`
	Level     string         `json:"level"`
	Message   string         `json:"message"`
	Component string         `json:"component,omitempty"`
	Source    string         `json:"source,omitempty"`
	Attrs     map[string]any `json:"attrs,omitempty"`
}

// LogFilter controls which entries are returned from the ring buffer.
type LogFilter struct {
	Level     string    // minimum level: "debug", "info", "warn", "error"
	Search    string    // case-insensitive substring match on message
	Component string    // exact match on component attr
	After     time.Time // only entries after this time
	Limit     int       // max entries to return (default 100, max 500)
}

// Matches reports whether entry satisfies the filter. It is the single source
// of truth for level/component/search/after matching, shared by the ring
// buffer's Entries scan and the live LogBroadcaster fan-out so the backfill and
// the live tail never diverge on what a filter selects. Limit is a pagination
// concern, not a per-entry predicate, so it is not considered here.
func (f LogFilter) Matches(entry LogEntry) bool {
	if f.Level != "" && levelSeverity(entry.Level) < levelSeverity(f.Level) {
		return false
	}
	if !f.After.IsZero() && !entry.Time.After(f.After) {
		return false
	}
	if f.Component != "" && entry.Component != f.Component {
		return false
	}
	if f.Search != "" && !strings.Contains(strings.ToLower(entry.Message), strings.ToLower(f.Search)) {
		return false
	}
	return true
}

// levelSeverity returns a numeric severity for ordering: trace=-1, debug=0, info=1, warn=2, error=3.
func levelSeverity(level string) int {
	switch strings.ToLower(level) {
	case "trace":
		return -1
	case "debug":
		return 0
	case "info":
		return 1
	case "warn":
		return 2
	case "error":
		return 3
	default:
		return 1
	}
}

// RingBuffer is a fixed-size circular buffer of log entries.
// It is safe for concurrent use.
type RingBuffer struct {
	mu      sync.RWMutex
	entries []LogEntry
	size    int
	head    int // next write position
	count   int // number of entries currently stored
}

// NewRingBuffer creates a ring buffer that holds up to size entries.
func NewRingBuffer(size int) *RingBuffer {
	if size <= 0 {
		size = DefaultRingBufferSize
	}
	return &RingBuffer{
		entries: make([]LogEntry, size),
		size:    size,
	}
}

// Write adds an entry to the buffer, overwriting the oldest if full.
func (rb *RingBuffer) Write(entry LogEntry) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	rb.entries[rb.head] = entry
	rb.head = (rb.head + 1) % rb.size
	if rb.count < rb.size {
		rb.count++
	}
}

// Entries returns log entries matching the filter, newest first.
// If filter.Limit is zero, defaults to 100. Maximum is 500.
func (rb *RingBuffer) Entries(filter LogFilter) []LogEntry {
	rb.mu.RLock()
	defer rb.mu.RUnlock()

	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}

	// Use a fixed-size initial allocation to satisfy CodeQL's
	// uncontrolled-allocation-size check. The slice grows dynamically
	// as needed but the initial capacity is a compile-time constant.
	const initialCap = 64
	result := make([]LogEntry, 0, initialCap)

	// Iterate backwards from newest entry. Per-entry matching is delegated to
	// filter.Matches so the ring buffer and the live broadcaster apply
	// identical level/component/search/after semantics.
	for i := 0; i < rb.count && len(result) < limit; i++ {
		idx := (rb.head - 1 - i + rb.size) % rb.size
		entry := rb.entries[idx]
		if !filter.Matches(entry) {
			continue
		}
		result = append(result, entry)
	}

	return result
}

// Len returns the number of entries currently stored.
func (rb *RingBuffer) Len() int {
	rb.mu.RLock()
	defer rb.mu.RUnlock()
	return rb.count
}

// Clear removes all entries from the buffer.
func (rb *RingBuffer) Clear() {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	rb.head = 0
	rb.count = 0
	// Zero out entries to allow GC of referenced data.
	for i := range rb.entries {
		rb.entries[i] = LogEntry{}
	}
}
