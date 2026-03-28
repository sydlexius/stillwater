package logging

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
)

// captureHandler is a test handler that records whether Handle was called.
type captureHandler struct {
	records []slog.Record
	level   slog.Level
	attrs   []slog.Attr
	group   string
}

func (h *captureHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.records = append(h.records, r)
	return nil
}

func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newAttrs := make([]slog.Attr, len(h.attrs)+len(attrs))
	copy(newAttrs, h.attrs)
	copy(newAttrs[len(h.attrs):], attrs)
	return &captureHandler{level: h.level, attrs: newAttrs, group: h.group}
}

func (h *captureHandler) WithGroup(name string) slog.Handler {
	g := name
	if h.group != "" {
		g = h.group + "." + name
	}
	return &captureHandler{level: h.level, attrs: h.attrs, group: g}
}

// errorHandler always returns an error from Handle.
type errorHandler struct {
	captureHandler
	err error
}

func (h *errorHandler) Handle(ctx context.Context, r slog.Record) error {
	h.captureHandler.Handle(ctx, r) //nolint:errcheck
	return h.err
}

func TestMultiHandler_FansOutToAll(t *testing.T) {
	h1 := &captureHandler{level: slog.LevelDebug}
	h2 := &captureHandler{level: slog.LevelDebug}
	multi := NewMultiHandler(h1, h2)

	logger := slog.New(multi)
	logger.Info("test message")

	if len(h1.records) != 1 {
		t.Errorf("handler 1: expected 1 record, got %d", len(h1.records))
	}
	if len(h2.records) != 1 {
		t.Errorf("handler 2: expected 1 record, got %d", len(h2.records))
	}
}

func TestMultiHandler_Enabled_AnyHandler(t *testing.T) {
	infoHandler := &captureHandler{level: slog.LevelInfo}
	debugHandler := &captureHandler{level: slog.LevelDebug}
	multi := NewMultiHandler(infoHandler, debugHandler)

	// Debug is enabled because at least one handler (debugHandler) accepts it.
	if !multi.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("expected Enabled(Debug) = true when one handler accepts debug")
	}

	// Verify that a debug record reaches the debug handler but not the info handler.
	logger := slog.New(multi)
	logger.Debug("debug msg")

	if len(infoHandler.records) != 0 {
		t.Errorf("info handler should not receive debug records, got %d", len(infoHandler.records))
	}
	if len(debugHandler.records) != 1 {
		t.Errorf("debug handler should receive debug records, got %d", len(debugHandler.records))
	}
}

func TestMultiHandler_ErrorPropagation(t *testing.T) {
	failing := &errorHandler{
		captureHandler: captureHandler{level: slog.LevelDebug},
		err:            fmt.Errorf("handler error"),
	}
	passing := &captureHandler{level: slog.LevelDebug}
	multi := NewMultiHandler(failing, passing)

	logger := slog.New(multi)
	logger.Info("test")

	// The passing handler should still have received the record.
	if len(passing.records) != 1 {
		t.Errorf("passing handler should receive record even when earlier handler fails, got %d", len(passing.records))
	}
	// The failing handler should also have received it (it captures then errors).
	if len(failing.records) != 1 {
		t.Errorf("failing handler should still receive record, got %d", len(failing.records))
	}
}

func TestMultiHandler_WithAttrs(t *testing.T) {
	h1 := &captureHandler{level: slog.LevelDebug}
	h2 := &captureHandler{level: slog.LevelDebug}
	multi := NewMultiHandler(h1, h2)

	child := multi.WithAttrs([]slog.Attr{slog.String("key", "val")})
	logger := slog.New(child)
	logger.Info("test")

	// Both inner handlers should be captureHandler with attrs set.
	childMulti, ok := child.(*MultiHandler)
	if !ok {
		t.Fatal("WithAttrs should return a *MultiHandler")
	}
	for i, h := range childMulti.handlers {
		ch, ok := h.(*captureHandler)
		if !ok {
			t.Fatalf("inner handler %d: expected *captureHandler", i)
		}
		if len(ch.attrs) != 1 || ch.attrs[0].Key != "key" {
			t.Errorf("inner handler %d: expected attrs with key=val, got %v", i, ch.attrs)
		}
	}
}

func TestMultiHandler_WithGroup(t *testing.T) {
	h1 := &captureHandler{level: slog.LevelDebug}
	h2 := &captureHandler{level: slog.LevelDebug}
	multi := NewMultiHandler(h1, h2)

	child := multi.WithGroup("mygroup")
	childMulti, ok := child.(*MultiHandler)
	if !ok {
		t.Fatal("WithGroup should return a *MultiHandler")
	}
	for i, h := range childMulti.handlers {
		ch, ok := h.(*captureHandler)
		if !ok {
			t.Fatalf("inner handler %d: expected *captureHandler", i)
		}
		if ch.group != "mygroup" {
			t.Errorf("inner handler %d: expected group=mygroup, got %q", i, ch.group)
		}
	}
}
