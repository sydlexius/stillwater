package api

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// insertSetting is a test helper that writes a key/value pair into the
// settings table of the test router's DB.
func insertSetting(t *testing.T, r *Router, key, value string) {
	t.Helper()
	_, err := r.db.ExecContext(context.Background(),
		`INSERT INTO settings (key, value) VALUES (?, ?)`, key, value)
	if err != nil {
		t.Fatalf("insertSetting(%q=%q): %v", key, value, err)
	}
}

// -- getStringSetting ---------------------------------------------------------

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

// TestGetStringSetting_StoredValue verifies that a present, non-empty stored
// value is returned directly (happy path).
func TestGetStringSetting_StoredValue(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	insertSetting(t, r, "test.str", "hello")

	got := r.getStringSetting(context.Background(), "test.str", "fallback")
	if got != "hello" {
		t.Errorf("getStringSetting stored value: got %q, want %q", got, "hello")
	}
}

// TestGetStringSetting_EmptyStoredValue verifies that a stored empty string
// returns the fallback (empty string is treated as absent).
func TestGetStringSetting_EmptyStoredValue(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	insertSetting(t, r, "test.empty", "")

	got := r.getStringSetting(context.Background(), "test.empty", "fallback")
	if got != "fallback" {
		t.Errorf("getStringSetting empty stored value: got %q, want %q", got, "fallback")
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

// -- getBoolSetting -----------------------------------------------------------

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

// TestGetBoolSetting_True verifies that stored "true" and "1" both parse to true.
func TestGetBoolSetting_True(t *testing.T) {
	t.Parallel()
	for _, stored := range []string{"true", "1"} {
		stored := stored
		t.Run(stored, func(t *testing.T) {
			t.Parallel()
			r, _ := testRouter(t)
			insertSetting(t, r, "test.bool.on", stored)
			got := r.getBoolSetting(context.Background(), "test.bool.on", false)
			if !got {
				t.Errorf("getBoolSetting(%q): got false, want true", stored)
			}
		})
	}
}

// TestGetBoolSetting_False verifies that stored "false" parses to false.
func TestGetBoolSetting_False(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	insertSetting(t, r, "test.bool.off", "false")
	got := r.getBoolSetting(context.Background(), "test.bool.off", true)
	if got {
		t.Errorf("getBoolSetting(\"false\"): got true, want false")
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

// -- getIntSetting ------------------------------------------------------------

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

// TestGetIntSetting_StoredValue verifies that a stored valid integer is
// returned correctly (happy path, previously untested).
func TestGetIntSetting_StoredValue(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	insertSetting(t, r, "test.int", "42")

	got := r.getIntSetting(context.Background(), "test.int", 0)
	if got != 42 {
		t.Errorf("getIntSetting stored value: got %d, want 42", got)
	}
}

// TestGetIntSetting_EmptyStoredValue verifies that a stored empty string
// returns the fallback (empty is treated as absent).
func TestGetIntSetting_EmptyStoredValue(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	insertSetting(t, r, "test.int.empty", "")

	got := r.getIntSetting(context.Background(), "test.int.empty", 7)
	if got != 7 {
		t.Errorf("getIntSetting empty stored value: got %d, want 7 (fallback)", got)
	}
}

// TestGetIntSetting_InvalidValue verifies that a stored non-integer returns
// the fallback AND emits a Warn log line (Item 2 observability).
func TestGetIntSetting_InvalidValue(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	insertSetting(t, r, "test.int.bad", "bad")

	var buf bytes.Buffer
	r.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	got := r.getIntSetting(context.Background(), "test.int.bad", 99)
	if got != 99 {
		t.Errorf("getIntSetting invalid value: got %d, want 99 (fallback)", got)
	}
	logged := buf.String()
	if !strings.Contains(logged, "int setting value is not a valid integer") {
		t.Errorf("getIntSetting invalid value: expected warn log, got %q", logged)
	}
	if !strings.Contains(logged, "level=WARN") {
		t.Errorf("getIntSetting invalid value: expected WARN level, got %q", logged)
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
