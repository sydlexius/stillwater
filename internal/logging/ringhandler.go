package logging

import (
	"context"
	"log/slog"
)

// RingHandler is an slog.Handler that captures log records into a RingBuffer.
// It is intended to be used alongside the primary handler via MultiHandler.
type RingHandler struct {
	buffer *RingBuffer
	level  slog.Leveler
	attrs  []slog.Attr
	group  string
}

// NewRingHandler creates a handler that writes to the given buffer.
// The level controls the minimum severity captured.
func NewRingHandler(buffer *RingBuffer, level slog.Leveler) *RingHandler {
	return &RingHandler{
		buffer: buffer,
		level:  level,
	}
}

// Enabled reports whether the handler captures records at the given level.
func (h *RingHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

// Handle converts a slog.Record to a LogEntry and writes it to the buffer.
func (h *RingHandler) Handle(_ context.Context, r slog.Record) error {
	entry := LogEntry{
		Time:    r.Time,
		Level:   FormatLevel(r.Level),
		Message: r.Message,
	}

	// Collect attributes: start with pre-stored attrs from WithAttrs,
	// then append the record's own attrs.
	attrs := make(map[string]any)
	for _, a := range h.attrs {
		key := a.Key
		if h.group != "" {
			key = h.group + "." + key
		}
		if key == "component" {
			entry.Component = a.Value.String()
		} else {
			attrs[key] = a.Value.Any()
		}
	}

	r.Attrs(func(a slog.Attr) bool {
		key := a.Key
		if h.group != "" {
			key = h.group + "." + key
		}
		if key == "component" {
			entry.Component = a.Value.String()
		} else {
			attrs[key] = a.Value.Any()
		}
		return true
	})

	if len(attrs) > 0 {
		entry.Attrs = attrs
	}

	h.buffer.Write(entry)
	return nil
}

// WithAttrs returns a new RingHandler with the given attributes pre-stored.
func (h *RingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newAttrs := make([]slog.Attr, len(h.attrs), len(h.attrs)+len(attrs))
	copy(newAttrs, h.attrs)
	newAttrs = append(newAttrs, attrs...)
	return &RingHandler{
		buffer: h.buffer,
		level:  h.level,
		attrs:  newAttrs,
		group:  h.group,
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
		buffer: h.buffer,
		level:  h.level,
		attrs:  newAttrs,
		group:  newGroup,
	}
}
