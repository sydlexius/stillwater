package scanner

import (
	"log/slog"
	"reflect"
	"testing"
)

// TestSetters exercises the trivial setter accessors so the patch coverage
// gate includes the back-compat wiring used by the application bootstrap
// (cmd/stillwater) and the unit test harnesses. Each setter is one line of
// production code; this test pins their semantics so a future refactor that
// changes the field name fails here instead of failing in main.go startup
// (where the broken wiring would not surface until runtime).
func TestSetters(t *testing.T) {
	t.Parallel()
	logger := slog.Default()
	s := NewService(nil, nil, nil, logger, "/music", nil)

	// SetDefaultLibraryID
	s.SetDefaultLibraryID("lib-default")
	if s.defaultLibraryID != "lib-default" {
		t.Errorf("SetDefaultLibraryID: want lib-default, got %q", s.defaultLibraryID)
	}

	// SetMtimeFastPath: NewService defaults the flag to true; the setter
	// must let an operator disable the fast-path for known-broken
	// filesystems (the documented SW_SCANNER_MTIME_FAST_PATH=false path).
	if !s.mtimeFastPath.Load() {
		t.Errorf("NewService should default mtimeFastPath=true; got false")
	}
	s.SetMtimeFastPath(false)
	if s.mtimeFastPath.Load() {
		t.Errorf("SetMtimeFastPath(false) should disable; still enabled")
	}
	s.SetMtimeFastPath(true)
	if !s.mtimeFastPath.Load() {
		t.Errorf("SetMtimeFastPath(true) should re-enable; still disabled")
	}

	// MtimeFastPath() getter mirrors the underlying atomic flag.
	if !s.MtimeFastPath() {
		t.Errorf("MtimeFastPath() should report true after SetMtimeFastPath(true)")
	}
	s.SetMtimeFastPath(false)
	if s.MtimeFastPath() {
		t.Errorf("MtimeFastPath() should report false after SetMtimeFastPath(false)")
	}
}

// TestSetExclusions pins the live exclusion-update path used by the settings
// handler: the value is lowercased, whitespace-trimmed, empties are dropped,
// and the swap is visible through the Exclusions() getter.
func TestSetExclusions(t *testing.T) {
	t.Parallel()
	logger := slog.Default()

	// NewService trims whitespace and drops empties but PRESERVES the original
	// casing and input order for display (matching stays case-insensitive via
	// the lowercased lookup map asserted below).
	s := NewService(nil, nil, nil, logger, "/music", []string{"Various Artists", " VA ", ""})
	if got, want := s.Exclusions(), []string{"Various Artists", "VA"}; !reflect.DeepEqual(got, want) {
		t.Errorf("NewService exclusions = %v, want %v", got, want)
	}
	// Confirm the directory-name lookup the scanner uses matches (lowercased).
	if m := s.exclusions.Load(); m == nil || !(*m)["various artists"] {
		t.Errorf("expected lowercased lookup hit for \"various artists\"")
	}

	// SetExclusions replaces the set; the old entries must be gone. Display
	// preserves casing/order; matching is still lowercased.
	s.SetExclusions([]string{"Soundtrack", "  OST  ", "  "})
	if got, want := s.Exclusions(), []string{"Soundtrack", "OST"}; !reflect.DeepEqual(got, want) {
		t.Errorf("after SetExclusions, exclusions = %v, want %v", got, want)
	}
	if m := s.exclusions.Load(); m == nil || !(*m)["ost"] || !(*m)["soundtrack"] {
		t.Errorf("expected lowercased lookup hits for \"ost\" and \"soundtrack\"")
	}
	m := s.exclusions.Load()
	if m == nil {
		t.Fatalf("exclusions pointer is nil after SetExclusions")
	}
	if (*m)["various artists"] {
		t.Errorf("SetExclusions did not replace the prior set; stale entry present")
	}
	if _, ok := (*m)[""]; ok {
		t.Errorf("empty/whitespace token leaked into the exclusion set")
	}

	// Empty input clears the set.
	s.SetExclusions(nil)
	if got := s.Exclusions(); len(got) != 0 {
		t.Errorf("SetExclusions(nil) should clear; got %v", got)
	}
}

// TestExclusionsOriginalCaseDedup pins the display contract: tokens that
// collapse to the same lowercased key are de-duplicated (first casing wins) so
// the settings UI never shows a redundant pair, while the lowercased lookup map
// still matches every casing.
func TestExclusionsOriginalCaseDedup(t *testing.T) {
	t.Parallel()
	s := NewService(nil, nil, nil, slog.Default(), "/music", []string{"VA", "va", "Various Artists"})

	if got, want := s.Exclusions(), []string{"VA", "Various Artists"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Exclusions() = %v, want %v (first casing wins, duplicates dropped)", got, want)
	}
	// Both casings of the de-duplicated token still match (case-insensitive).
	m := s.exclusions.Load()
	if m == nil || !(*m)["va"] {
		t.Errorf("expected case-insensitive lookup hit for \"va\"")
	}
}
