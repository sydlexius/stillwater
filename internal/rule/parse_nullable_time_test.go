package rule

import (
	"testing"
	"time"
)

// TestParseNullableTime_SQLiteDatetimeFallback covers the time.DateTime branch
// in parseNullableTime: the "YYYY-MM-DD HH:MM:SS" format that SQLite's
// datetime('now') default can produce.
func TestParseNullableTime_SQLiteDatetimeFallback(t *testing.T) {
	t.Parallel()
	got, ok := parseNullableTime("2026-05-09 12:34:56")
	if !ok {
		t.Fatal("parseNullableTime returned ok=false, want true")
	}
	if got.Location() != time.UTC {
		t.Errorf("parseNullableTime location = %v, want UTC", got.Location())
	}
	want := time.Date(2026, 5, 9, 12, 34, 56, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("parseNullableTime = %v, want %v", got, want)
	}
}
