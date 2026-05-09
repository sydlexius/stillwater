package artist

import (
	"testing"
	"time"
)

// TestParseHistoryTimestamp_SQLiteDatetimeFallback covers the time.DateTime
// branch in parseHistoryTimestamp: the SQLite "YYYY-MM-DD HH:MM:SS" format
// that pre-migration rows may still hold.
func TestParseHistoryTimestamp_SQLiteDatetimeFallback(t *testing.T) {
	t.Parallel()
	raw := "2026-05-09 12:34:56"
	got := parseHistoryTimestamp("test-id", raw)
	if got.IsZero() {
		t.Fatalf("parseHistoryTimestamp(%q) returned zero time", raw)
	}
	want := time.Date(2026, 5, 9, 12, 34, 56, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("parseHistoryTimestamp(%q) = %v, want %v", raw, got, want)
	}
}
