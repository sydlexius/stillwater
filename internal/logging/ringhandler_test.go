package logging

import (
	"context"
	"log/slog"
	"testing"
)

func TestRingHandler_BasicCapture(t *testing.T) {
	rb := NewRingBuffer(10)
	h := NewRingHandler(rb, slog.LevelDebug)
	logger := slog.New(h)

	logger.Info("hello world")

	entries := rb.Entries(LogFilter{Limit: 10})
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Message != "hello world" {
		t.Errorf("expected 'hello world', got %q", entries[0].Message)
	}
	if entries[0].Level != "info" {
		t.Errorf("expected level 'info', got %q", entries[0].Level)
	}
}

func TestRingHandler_ComponentExtraction(t *testing.T) {
	rb := NewRingBuffer(10)
	h := NewRingHandler(rb, slog.LevelDebug)
	logger := slog.New(h)

	logger.Info("scanning library", "component", "scanner", "path", "/music")

	entries := rb.Entries(LogFilter{Limit: 10})
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Component != "scanner" {
		t.Errorf("expected component 'scanner', got %q", entries[0].Component)
	}
	// "component" should not appear in attrs since it was extracted.
	if _, ok := entries[0].Attrs["component"]; ok {
		t.Error("component should not be in attrs map")
	}
	// "path" should be in attrs.
	if v, ok := entries[0].Attrs["path"]; !ok || v != "/music" {
		t.Errorf("expected path=/music in attrs, got %v", entries[0].Attrs)
	}
}

func TestRingHandler_LevelFiltering(t *testing.T) {
	rb := NewRingBuffer(10)
	// Only capture warn and above.
	h := NewRingHandler(rb, slog.LevelWarn)
	logger := slog.New(h)

	logger.Debug("debug msg")
	logger.Info("info msg")
	logger.Warn("warn msg")
	logger.Error("error msg")

	entries := rb.Entries(LogFilter{Limit: 10})
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries (warn+error), got %d", len(entries))
	}
}

func TestRingHandler_Enabled(t *testing.T) {
	rb := NewRingBuffer(10)
	h := NewRingHandler(rb, slog.LevelWarn)

	if h.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("debug should not be enabled when level is warn")
	}
	if h.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("info should not be enabled when level is warn")
	}
	if !h.Enabled(context.Background(), slog.LevelWarn) {
		t.Error("warn should be enabled")
	}
	if !h.Enabled(context.Background(), slog.LevelError) {
		t.Error("error should be enabled")
	}
}

func TestRingHandler_WithAttrs(t *testing.T) {
	rb := NewRingBuffer(10)
	h := NewRingHandler(rb, slog.LevelDebug)

	// Create a child handler with pre-stored attrs.
	child := h.WithAttrs([]slog.Attr{slog.String("service", "api")})
	logger := slog.New(child)
	logger.Info("request handled")

	entries := rb.Entries(LogFilter{Limit: 10})
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if v, ok := entries[0].Attrs["service"]; !ok || v != "api" {
		t.Errorf("expected service=api in attrs, got %v", entries[0].Attrs)
	}
}

func TestRingHandler_WithGroup(t *testing.T) {
	rb := NewRingBuffer(10)
	h := NewRingHandler(rb, slog.LevelDebug)

	// Create a grouped handler.
	child := h.WithGroup("http")
	logger := slog.New(child)
	logger.Info("request", "method", "GET", "path", "/api")

	entries := rb.Entries(LogFilter{Limit: 10})
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if v, ok := entries[0].Attrs["http.method"]; !ok || v != "GET" {
		t.Errorf("expected http.method=GET in attrs, got %v", entries[0].Attrs)
	}
}

func TestRingHandler_WithAttrsAndComponent(t *testing.T) {
	rb := NewRingBuffer(10)
	h := NewRingHandler(rb, slog.LevelDebug)

	// Pre-store a component attr via WithAttrs.
	child := h.WithAttrs([]slog.Attr{slog.String("component", "scanner")})
	logger := slog.New(child)
	logger.Info("scan started")

	entries := rb.Entries(LogFilter{Limit: 10})
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	// Component from WithAttrs should be extracted into the Component field,
	// the same as record-level attrs.
	if entries[0].Component != "scanner" {
		t.Errorf("expected Component = scanner, got %q", entries[0].Component)
	}
	if _, ok := entries[0].Attrs["component"]; ok {
		t.Error("component should be extracted, not in attrs map")
	}
}

func TestRingHandler_MultipleLevels(t *testing.T) {
	rb := NewRingBuffer(10)
	h := NewRingHandler(rb, slog.LevelDebug)
	logger := slog.New(h)

	logger.Debug("debug msg")
	logger.Info("info msg")
	logger.Warn("warn msg")
	logger.Error("error msg")

	entries := rb.Entries(LogFilter{Limit: 10})
	if len(entries) != 4 {
		t.Fatalf("expected 4 entries, got %d", len(entries))
	}

	// Newest first.
	expected := []string{"error", "warn", "info", "debug"}
	for i, e := range entries {
		if e.Level != expected[i] {
			t.Errorf("entry[%d] level = %q, want %q", i, e.Level, expected[i])
		}
	}
}
