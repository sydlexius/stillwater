package scanner

import (
	"log/slog"
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
	if !s.mtimeFastPath {
		t.Errorf("NewService should default mtimeFastPath=true; got false")
	}
	s.SetMtimeFastPath(false)
	if s.mtimeFastPath {
		t.Errorf("SetMtimeFastPath(false) should disable; still enabled")
	}
	s.SetMtimeFastPath(true)
	if !s.mtimeFastPath {
		t.Errorf("SetMtimeFastPath(true) should re-enable; still disabled")
	}
}
