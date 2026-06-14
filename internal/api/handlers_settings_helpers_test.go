package api

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// TestGetStringSetting_AbsentKey verifies that a missing settings row
// returns the fallback without emitting a log line.
func TestGetStringSetting_AbsentKey(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	var buf bytes.Buffer
	r.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	got := r.getStringSetting(context.Background(), "nonexistent.key", "default-val")
	if got != "default-val" {
		t.Errorf("getStringSetting absent key: got %q, want %q", got, "default-val")
	}
	if buf.Len() != 0 {
		t.Errorf("getStringSetting absent key: expected no log output, got %q", buf.String())
	}
}

// TestGetStringSetting_DBError verifies that a real DB error (closed connection)
// returns the fallback AND emits a Warn log line.
func TestGetStringSetting_DBError(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	var buf bytes.Buffer
	r.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// Force every subsequent DB call to fail.
	if err := r.db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}

	got := r.getStringSetting(context.Background(), "some.key", "fallback")
	if got != "fallback" {
		t.Errorf("getStringSetting DB error: got %q, want %q", got, "fallback")
	}
	logged := buf.String()
	if !strings.Contains(logged, "reading string setting") {
		t.Errorf("getStringSetting DB error: expected warn log, got %q", logged)
	}
	if !strings.Contains(logged, "level=WARN") {
		t.Errorf("getStringSetting DB error: expected WARN level, got %q", logged)
	}
}

// TestGetBoolSetting_AbsentKey verifies that a missing settings row
// returns the fallback without emitting a log line.
func TestGetBoolSetting_AbsentKey(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	var buf bytes.Buffer
	r.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	got := r.getBoolSetting(context.Background(), "nonexistent.bool", true)
	if !got {
		t.Errorf("getBoolSetting absent key: got %v, want true (fallback)", got)
	}
	if buf.Len() != 0 {
		t.Errorf("getBoolSetting absent key: expected no log output, got %q", buf.String())
	}
}

// TestGetBoolSetting_DBError verifies that a real DB error returns the fallback
// and emits a Warn log line.
func TestGetBoolSetting_DBError(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	var buf bytes.Buffer
	r.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	if err := r.db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}

	got := r.getBoolSetting(context.Background(), "some.bool", false)
	if got != false {
		t.Errorf("getBoolSetting DB error: got %v, want false (fallback)", got)
	}
	logged := buf.String()
	if !strings.Contains(logged, "reading bool setting") {
		t.Errorf("getBoolSetting DB error: expected warn log, got %q", logged)
	}
	if !strings.Contains(logged, "level=WARN") {
		t.Errorf("getBoolSetting DB error: expected WARN level, got %q", logged)
	}
}

// TestGetIntSetting_AbsentKey verifies that a missing settings row
// returns the fallback without emitting a log line.
func TestGetIntSetting_AbsentKey(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	var buf bytes.Buffer
	r.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	got := r.getIntSetting(context.Background(), "nonexistent.int", 42)
	if got != 42 {
		t.Errorf("getIntSetting absent key: got %d, want 42 (fallback)", got)
	}
	if buf.Len() != 0 {
		t.Errorf("getIntSetting absent key: expected no log output, got %q", buf.String())
	}
}

// TestGetIntSetting_DBError verifies that a real DB error returns the fallback
// and emits a Warn log line.
func TestGetIntSetting_DBError(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	var buf bytes.Buffer
	r.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	if err := r.db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}

	got := r.getIntSetting(context.Background(), "some.int", 99)
	if got != 99 {
		t.Errorf("getIntSetting DB error: got %d, want 99 (fallback)", got)
	}
	logged := buf.String()
	if !strings.Contains(logged, "reading int setting") {
		t.Errorf("getIntSetting DB error: expected warn log, got %q", logged)
	}
	if !strings.Contains(logged, "level=WARN") {
		t.Errorf("getIntSetting DB error: expected WARN level, got %q", logged)
	}
}
