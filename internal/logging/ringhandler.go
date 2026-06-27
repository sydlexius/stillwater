package logging

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
)

// RingHandler is an slog.Handler that captures log records into a RingBuffer.
// It is intended to be used alongside the primary handler via MultiHandler.
type RingHandler struct {
	buffer    *RingBuffer
	level     slog.Leveler
	attrs     []slog.Attr
	group     string
	addSource bool
}

// NewRingHandler creates a handler that writes to the given buffer.
// The level controls the minimum severity captured. When addSource is true,
// the handler captures the caller's source file and line number.
func NewRingHandler(buffer *RingBuffer, level slog.Leveler, addSource bool) *RingHandler {
	return &RingHandler{
		buffer:    buffer,
		level:     level,
		addSource: addSource,
	}
}

// Enabled reports whether the handler captures records at the given level.
func (h *RingHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

// Handle converts a slog.Record to a LogEntry and writes it to the buffer.
func (h *RingHandler) Handle(_ context.Context, r slog.Record) error {
	h.buffer.Write(recordToEntry(r, h.attrs, h.group, h.addSource))
	return nil
}

// recordToEntry converts a slog.Record into a LogEntry, applying the same
// source-location derivation, component extraction, and RedactingReplaceAttr
// scrubbing for both the in-memory ring buffer and the live log broadcaster.
// preAttrs are the handler's accumulated WithAttrs attributes and group is its
// WithGroup prefix. Sharing this walk keeps the two log sinks byte-for-byte
// consistent (a divergence would mean the viewer and the live tail show
// different redaction or component values for the same record).
//
//nolint:gocognit // slog.Record attribute walk requires a type switch over every supported slog.Kind (string, int64, uint64, float64, bool, duration, time, group, any) so each kind can be encoded to LogEntry's typed field; this is the slog contract, not application logic, and cannot be flattened.
func recordToEntry(r slog.Record, preAttrs []slog.Attr, group string, addSource bool) LogEntry {
	entry := LogEntry{
		Time:    r.Time,
		Level:   FormatLevel(r.Level),
		Message: r.Message,
	}

	// Extract source location from the record's PC (set by slog when the
	// caller uses slog.Info/Warn/etc). AddSource in the primary handler
	// only affects that handler; we replicate the logic here.
	var pkgName string // derived package name for auto-component
	if addSource && r.PC != 0 {
		fs := runtime.CallersFrames([]uintptr{r.PC})
		f, _ := fs.Next()
		if f.File != "" {
			// Use short file name (last path component) for readability.
			short := f.File
			for i := len(short) - 1; i >= 0; i-- {
				if short[i] == '/' {
					short = short[i+1:]
					break
				}
			}
			entry.Source = fmt.Sprintf("%s:%d", short, f.Line)

			// Derive the Go package directory name for auto-component.
			// e.g. ".../internal/watcher/probe.go" -> "watcher"
			dir := f.File[:len(f.File)-len(short)]
			if len(dir) > 1 && dir[len(dir)-1] == '/' {
				dir = dir[:len(dir)-1]
			}
			for i := len(dir) - 1; i >= 0; i-- {
				if dir[i] == '/' {
					pkgName = dir[i+1:]
					break
				}
			}
		}
	}

	// Collect attributes: start with pre-stored attrs from WithAttrs,
	// then append the record's own attrs. Each attribute is passed through
	// RedactingReplaceAttr so that sensitive field values are scrubbed in
	// the ring buffer (log viewer) as well as in the primary handler.
	// addAttr recurses into slog.KindGroup so nested sensitive keys are
	// redacted rather than stored verbatim.
	attrs := make(map[string]any)
	var addAttr func(prefix string, a slog.Attr)
	addAttr = func(prefix string, a slog.Attr) {
		a.Value = a.Value.Resolve()
		if a.Value.Kind() == slog.KindGroup {
			next := a.Key
			if prefix != "" {
				next = prefix + "." + next
			}
			for _, ga := range a.Value.Group() {
				addAttr(next, ga)
			}
			return
		}
		a = RedactingReplaceAttr(nil, a)
		key := a.Key
		if prefix != "" {
			key = prefix + "." + key
		}
		if key == "component" {
			entry.Component = a.Value.String()
			return
		}
		// Serialize error values to their message string. A bare error marshals
		// to an empty JSON object ("{}") because it has no exported fields, which
		// loses the message and renders as "error:[object Object]" in the viewer
		// (#1338 X3). err.Error() preserves the human-readable message.
		v := a.Value.Any()
		if err, ok := v.(error); ok {
			attrs[key] = err.Error()
		} else {
			attrs[key] = v
		}
	}

	for _, a := range preAttrs {
		addAttr(group, a)
	}

	r.Attrs(func(a slog.Attr) bool {
		addAttr(group, a)
		return true
	})

	// If no explicit component was set, use the derived package name.
	if entry.Component == "" && pkgName != "" {
		entry.Component = pkgName
	}

	if len(attrs) > 0 {
		entry.Attrs = attrs
	}

	return entry
}

// WithAttrs returns a new RingHandler with the given attributes pre-stored.
func (h *RingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newAttrs := make([]slog.Attr, len(h.attrs), len(h.attrs)+len(attrs))
	copy(newAttrs, h.attrs)
	newAttrs = append(newAttrs, attrs...)
	return &RingHandler{
		buffer:    h.buffer,
		level:     h.level,
		attrs:     newAttrs,
		group:     h.group,
		addSource: h.addSource,
	}
}

// WithGroup returns a new RingHandler with the given group name.
func (h *RingHandler) WithGroup(name string) slog.Handler {
	newGroup := name
	if h.group != "" {
		newGroup = h.group + "." + name
	}
	newAttrs := make([]slog.Attr, len(h.attrs))
	copy(newAttrs, h.attrs)
	return &RingHandler{
		buffer:    h.buffer,
		level:     h.level,
		attrs:     newAttrs,
		group:     newGroup,
		addSource: h.addSource,
	}
}
