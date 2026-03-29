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

// levelSeverity returns a numeric severity for ordering: debug=0, info=1, warn=2, error=3.
func levelSeverity(level string) int {
	switch strings.ToLower(level) {
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

	minSeverity := 0
	if filter.Level != "" {
		minSeverity = levelSeverity(filter.Level)
	}

	searchLower := strings.ToLower(filter.Search)

	// Use a fixed-size initial allocation to satisfy CodeQL's
	// uncontrolled-allocation-size check. The slice grows dynamically
	// as needed but the initial capacity is a compile-time constant.
	const initialCap = 64
	result := make([]LogEntry, 0, initialCap)

	// Iterate backwards from newest entry.
	for i := 0; i < rb.count && len(result) < limit; i++ {
		idx := (rb.head - 1 - i + rb.size) % rb.size
		entry := rb.entries[idx]

		// Level filter: skip entries below the minimum severity.
		if levelSeverity(entry.Level) < minSeverity {
			continue
		}

		// Time filter: skip entries at or before the After timestamp.
		if !filter.After.IsZero() && !entry.Time.After(filter.After) {
			continue
		}

		// Component filter: exact match.
		if filter.Component != "" && entry.Component != filter.Component {
			continue
		}

		// Search filter: case-insensitive substring match on message.
		if searchLower != "" && !strings.Contains(strings.ToLower(entry.Message), searchLower) {
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
