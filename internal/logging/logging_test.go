package logging

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestNewManager_DefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	mgr, logger := NewManager(cfg)
	defer mgr.Close() //nolint:errcheck

	if logger == nil {
		t.Fatal("expected non-nil logger")
	}
	if mgr.Config().Level != "info" {
		t.Errorf("expected level info, got %s", mgr.Config().Level)
	}
	if mgr.Config().Format != "json" {
		t.Errorf("expected format json, got %s", mgr.Config().Format)
	}
}

func TestManager_LevelSwap(t *testing.T) {
	mgr, logger := NewManager(Config{Level: "info", Format: "json"})
	defer mgr.Close() //nolint:errcheck

	// The ring handler uses the same level as the configured level, so
	// Enabled() accurately reflects what will be captured.
	if !logger.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("expected info to be enabled")
	}
	if logger.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("expected debug to be disabled when configured at info")
	}

	// Reconfigure to debug -- debug should now be enabled.
	mgr.Reconfigure(Config{Level: "debug", Format: "json"})
	if !logger.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("expected debug to be enabled after reconfigure")
	}

	// Reconfigure to error -- only error should be enabled.
	mgr.Reconfigure(Config{Level: "error", Format: "json"})
	if !logger.Enabled(context.Background(), slog.LevelError) {
		t.Error("expected error to be enabled")
	}
	if logger.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("expected info to be disabled when configured at error")
	}

	// Verify that ring buffer captured entries at all levels when logging at debug.
	mgr.Reconfigure(Config{Level: "debug", Format: "json"})
	rb := mgr.RingBuffer()
	rb.Clear()
	logger.Debug("debug msg")
	logger.Info("info msg")
	logger.Error("error msg")
	entries := rb.Entries(LogFilter{Limit: 10})
	if len(entries) != 3 {
		t.Errorf("expected ring buffer to capture 3 entries, got %d", len(entries))
	}
}

func TestManager_FormatSwap(t *testing.T) {
	mgr, _ := NewManager(Config{Level: "info", Format: "json"})
	defer mgr.Close() //nolint:errcheck

	if mgr.Config().Format != "json" {
		t.Errorf("expected json, got %s", mgr.Config().Format)
	}

	mgr.Reconfigure(Config{Level: "info", Format: "text"})
	if mgr.Config().Format != "text" {
		t.Errorf("expected text after reconfigure, got %s", mgr.Config().Format)
	}
}

func TestManager_FileOutput(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "test.log")

	cfg := Config{
		Level:          "info",
		Format:         "json",
		FilePath:       logFile,
		FileMaxSizeMB:  1,
		FileMaxFiles:   1,
		FileMaxAgeDays: 1,
	}
	mgr, logger := NewManager(cfg)

	logger.Info("hello from test")

	if err := mgr.Close(); err != nil {
		t.Fatalf("closing manager: %v", err)
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("reading log file: %v", err)
	}
	if len(data) == 0 {
		t.Error("expected log file to contain data")
	}
}

func TestManager_CloseIdempotent(t *testing.T) {
	mgr, _ := NewManager(DefaultConfig())
	if err := mgr.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := mgr.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestManager_ReconfigureIdempotent(t *testing.T) {
	cfg := Config{Level: "info", Format: "json"}
	mgr, _ := NewManager(cfg)
	defer mgr.Close() //nolint:errcheck

	// Reconfigure with same config should be fine
	mgr.Reconfigure(cfg)
	mgr.Reconfigure(cfg)
	if mgr.Config().Level != "info" {
		t.Errorf("expected info, got %s", mgr.Config().Level)
	}
}

func TestValidLevel(t *testing.T) {
	for _, l := range []string{"debug", "info", "warn", "error"} {
		if !ValidLevel(l) {
			t.Errorf("expected %q to be valid", l)
		}
	}
	for _, l := range []string{"", "trace", "fatal", "DEBUG"} {
		if ValidLevel(l) {
			t.Errorf("expected %q to be invalid", l)
		}
	}
}

func TestValidFormat(t *testing.T) {
	if !ValidFormat("text") || !ValidFormat("json") {
		t.Error("text and json should be valid")
	}
	if ValidFormat("xml") || ValidFormat("") {
		t.Error("xml and empty should be invalid")
	}
}

func TestParseLevel(t *testing.T) {
	tests := []struct {
		in  string
		out slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
		{"", slog.LevelInfo},
		{"unknown", slog.LevelInfo},
	}
	for _, tt := range tests {
		if got := parseLevel(tt.in); got != tt.out {
			t.Errorf("parseLevel(%q) = %v, want %v", tt.in, got, tt.out)
		}
	}
}

func TestFormatLevel(t *testing.T) {
	tests := []struct {
		in  slog.Level
		out string
	}{
		{slog.LevelDebug, "debug"},
		{slog.LevelInfo, "info"},
		{slog.LevelWarn, "warn"},
		{slog.LevelError, "error"},
	}
	for _, tt := range tests {
		if got := FormatLevel(tt.in); got != tt.out {
			t.Errorf("FormatLevel(%v) = %q, want %q", tt.in, got, tt.out)
		}
	}
}

func TestDerivedHandler_SeesReconfigure(t *testing.T) {
	mgr, logger := NewManager(Config{Level: "debug", Format: "json"})
	defer mgr.Close() //nolint:errcheck

	// Create a derived logger with an extra attribute.
	derived := logger.With("component", "test")

	rb := mgr.RingBuffer()
	rb.Clear()

	// Write via derived logger before reconfigure.
	derived.Info("before reconfigure")

	// Reconfigure to text format (this swaps the inner handler).
	mgr.Reconfigure(Config{Level: "debug", Format: "text"})

	// Write via derived logger after reconfigure.
	derived.Info("after reconfigure")

	// Both messages should appear in the ring buffer, proving the derived
	// logger delegated through the parent and observed the swap.
	entries := rb.Entries(LogFilter{Limit: 10})
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries from derived logger, got %d", len(entries))
	}
	// Verify newest first ordering.
	if entries[0].Message != "after reconfigure" {
		t.Errorf("expected 'after reconfigure', got %q", entries[0].Message)
	}
	if entries[1].Message != "before reconfigure" {
		t.Errorf("expected 'before reconfigure', got %q", entries[1].Message)
	}
}

func TestDerivedHandler_WithGroup(t *testing.T) {
	mgr, logger := NewManager(Config{Level: "debug", Format: "json"})
	defer mgr.Close() //nolint:errcheck

	// Create a grouped derived logger.
	grouped := logger.WithGroup("mygroup")

	rb := mgr.RingBuffer()
	rb.Clear()

	grouped.Info("grouped message")

	// Reconfigure and log again -- the grouped logger should still work.
	mgr.Reconfigure(Config{Level: "debug", Format: "text"})
	grouped.Info("grouped after reconfigure")

	entries := rb.Entries(LogFilter{Limit: 10})
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
}

func TestConfig_String(t *testing.T) {
	cfg := Config{Level: "info", Format: "json"}
	s := cfg.String()
	if s != "level=info format=json" {
		t.Errorf("unexpected string: %s", s)
	}

	cfg.FilePath = "/var/log/sw.log"
	cfg.FileMaxSizeMB = 50
	cfg.FileMaxFiles = 5
	cfg.FileMaxAgeDays = 7
	s = cfg.String()
	expected := "level=info format=json file=/var/log/sw.log max_size=50MB max_files=5 max_age=7d"
	if s != expected {
		t.Errorf("got %q, want %q", s, expected)
	}
}
