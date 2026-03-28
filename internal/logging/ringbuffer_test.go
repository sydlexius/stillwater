package logging

import (
	"fmt"
	"testing"
	"time"
)

func TestRingBuffer_WriteAndRead(t *testing.T) {
	rb := NewRingBuffer(5)

	now := time.Now()
	for i := 0; i < 3; i++ {
		rb.Write(LogEntry{
			Time:    now.Add(time.Duration(i) * time.Second),
			Level:   "info",
			Message: fmt.Sprintf("msg-%d", i),
		})
	}

	if rb.Len() != 3 {
		t.Fatalf("expected 3 entries, got %d", rb.Len())
	}

	entries := rb.Entries(LogFilter{Limit: 10})
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	// Entries should be newest first.
	if entries[0].Message != "msg-2" {
		t.Errorf("expected newest first, got %s", entries[0].Message)
	}
	if entries[2].Message != "msg-0" {
		t.Errorf("expected oldest last, got %s", entries[2].Message)
	}
}

func TestRingBuffer_Overflow(t *testing.T) {
	rb := NewRingBuffer(3)

	now := time.Now()
	for i := 0; i < 5; i++ {
		rb.Write(LogEntry{
			Time:    now.Add(time.Duration(i) * time.Second),
			Level:   "info",
			Message: fmt.Sprintf("msg-%d", i),
		})
	}

	// Buffer holds 3; oldest 2 should be overwritten.
	if rb.Len() != 3 {
		t.Fatalf("expected 3 entries, got %d", rb.Len())
	}

	entries := rb.Entries(LogFilter{Limit: 10})
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	if entries[0].Message != "msg-4" {
		t.Errorf("expected msg-4, got %s", entries[0].Message)
	}
	if entries[2].Message != "msg-2" {
		t.Errorf("expected msg-2, got %s", entries[2].Message)
	}
}

func TestRingBuffer_LevelFilter(t *testing.T) {
	rb := NewRingBuffer(10)

	now := time.Now()
	levels := []string{"debug", "info", "warn", "error"}
	for i, lvl := range levels {
		rb.Write(LogEntry{
			Time:    now.Add(time.Duration(i) * time.Second),
			Level:   lvl,
			Message: lvl + " message",
		})
	}

	// Filter for warn and above.
	entries := rb.Entries(LogFilter{Level: "warn", Limit: 10})
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries (warn+error), got %d", len(entries))
	}
	if entries[0].Level != "error" {
		t.Errorf("expected error first, got %s", entries[0].Level)
	}
	if entries[1].Level != "warn" {
		t.Errorf("expected warn second, got %s", entries[1].Level)
	}
}

func TestRingBuffer_SearchFilter(t *testing.T) {
	rb := NewRingBuffer(10)

	now := time.Now()
	rb.Write(LogEntry{Time: now, Level: "info", Message: "connecting to database"})
	rb.Write(LogEntry{Time: now.Add(time.Second), Level: "info", Message: "starting HTTP server"})
	rb.Write(LogEntry{Time: now.Add(2 * time.Second), Level: "info", Message: "database migration done"})

	entries := rb.Entries(LogFilter{Search: "database", Limit: 10})
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries matching 'database', got %d", len(entries))
	}
}

func TestRingBuffer_SearchFilter_CaseInsensitive(t *testing.T) {
	rb := NewRingBuffer(10)

	now := time.Now()
	rb.Write(LogEntry{Time: now, Level: "info", Message: "Database Error occurred"})

	entries := rb.Entries(LogFilter{Search: "database error", Limit: 10})
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry with case-insensitive match, got %d", len(entries))
	}
}

func TestRingBuffer_ComponentFilter(t *testing.T) {
	rb := NewRingBuffer(10)

	now := time.Now()
	rb.Write(LogEntry{Time: now, Level: "info", Message: "scanning", Component: "scanner"})
	rb.Write(LogEntry{Time: now.Add(time.Second), Level: "info", Message: "fetching", Component: "provider"})
	rb.Write(LogEntry{Time: now.Add(2 * time.Second), Level: "info", Message: "scan done", Component: "scanner"})

	entries := rb.Entries(LogFilter{Component: "scanner", Limit: 10})
	if len(entries) != 2 {
		t.Fatalf("expected 2 scanner entries, got %d", len(entries))
	}
}

func TestRingBuffer_AfterFilter(t *testing.T) {
	rb := NewRingBuffer(10)

	base := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		rb.Write(LogEntry{
			Time:    base.Add(time.Duration(i) * time.Minute),
			Level:   "info",
			Message: fmt.Sprintf("msg-%d", i),
		})
	}

	// Only entries after 12:02 should be returned (12:03, 12:04).
	after := base.Add(2 * time.Minute)
	entries := rb.Entries(LogFilter{After: after, Limit: 10})
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries after %v, got %d", after, len(entries))
	}
	if entries[0].Message != "msg-4" {
		t.Errorf("expected msg-4, got %s", entries[0].Message)
	}
}

func TestRingBuffer_LimitDefault(t *testing.T) {
	rb := NewRingBuffer(200)

	now := time.Now()
	for i := 0; i < 150; i++ {
		rb.Write(LogEntry{
			Time:    now.Add(time.Duration(i) * time.Millisecond),
			Level:   "info",
			Message: fmt.Sprintf("msg-%d", i),
		})
	}

	// Default limit is 100.
	entries := rb.Entries(LogFilter{})
	if len(entries) != 100 {
		t.Fatalf("expected default limit of 100, got %d", len(entries))
	}
}

func TestRingBuffer_LimitMax(t *testing.T) {
	rb := NewRingBuffer(1000)

	now := time.Now()
	for i := 0; i < 600; i++ {
		rb.Write(LogEntry{
			Time:    now.Add(time.Duration(i) * time.Millisecond),
			Level:   "info",
			Message: fmt.Sprintf("msg-%d", i),
		})
	}

	// Max limit is 500.
	entries := rb.Entries(LogFilter{Limit: 999})
	if len(entries) != 500 {
		t.Fatalf("expected max limit of 500, got %d", len(entries))
	}
}

func TestRingBuffer_Clear(t *testing.T) {
	rb := NewRingBuffer(10)

	now := time.Now()
	for i := 0; i < 5; i++ {
		rb.Write(LogEntry{
			Time:    now.Add(time.Duration(i) * time.Second),
			Level:   "info",
			Message: fmt.Sprintf("msg-%d", i),
		})
	}

	rb.Clear()

	if rb.Len() != 0 {
		t.Fatalf("expected 0 entries after clear, got %d", rb.Len())
	}

	entries := rb.Entries(LogFilter{Limit: 10})
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries from Entries after clear, got %d", len(entries))
	}
}

func TestRingBuffer_ClearThenWrite(t *testing.T) {
	rb := NewRingBuffer(5)

	now := time.Now()
	for i := 0; i < 3; i++ {
		rb.Write(LogEntry{Time: now, Level: "info", Message: fmt.Sprintf("old-%d", i)})
	}
	rb.Clear()

	rb.Write(LogEntry{Time: now, Level: "info", Message: "new-0"})
	if rb.Len() != 1 {
		t.Fatalf("expected 1 entry after clear+write, got %d", rb.Len())
	}

	entries := rb.Entries(LogFilter{Limit: 10})
	if len(entries) != 1 || entries[0].Message != "new-0" {
		t.Errorf("expected new-0, got %v", entries)
	}
}

func TestRingBuffer_CombinedFilters(t *testing.T) {
	rb := NewRingBuffer(20)

	base := time.Date(2025, 6, 15, 10, 0, 0, 0, time.UTC)
	rb.Write(LogEntry{Time: base, Level: "debug", Message: "debug old", Component: "api"})
	rb.Write(LogEntry{Time: base.Add(time.Minute), Level: "info", Message: "info old", Component: "api"})
	rb.Write(LogEntry{Time: base.Add(2 * time.Minute), Level: "warn", Message: "warn new api", Component: "api"})
	rb.Write(LogEntry{Time: base.Add(3 * time.Minute), Level: "error", Message: "error new scanner", Component: "scanner"})
	rb.Write(LogEntry{Time: base.Add(4 * time.Minute), Level: "warn", Message: "warn new scanner", Component: "scanner"})

	// Level >= warn, component = scanner, after 2 minutes.
	entries := rb.Entries(LogFilter{
		Level:     "warn",
		Component: "scanner",
		After:     base.Add(2 * time.Minute),
		Limit:     10,
	})
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
}

func TestRingBuffer_ZeroSize(t *testing.T) {
	// Zero or negative size should default to 2000.
	rb := NewRingBuffer(0)
	if rb.size != 2000 {
		t.Errorf("expected default size 2000, got %d", rb.size)
	}
}

func TestLevelSeverity(t *testing.T) {
	tests := []struct {
		level    string
		severity int
	}{
		{"debug", 0},
		{"info", 1},
		{"warn", 2},
		{"error", 3},
		{"DEBUG", 0},
		{"INFO", 1},
		{"unknown", 1}, // defaults to info severity
	}
	for _, tt := range tests {
		got := levelSeverity(tt.level)
		if got != tt.severity {
			t.Errorf("levelSeverity(%q) = %d, want %d", tt.level, got, tt.severity)
		}
	}
}
