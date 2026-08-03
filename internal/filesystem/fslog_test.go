package filesystem

import (
	"context"
	"log/slog"
	"sync"
	"testing"
)

// --- destructive-step record capture (issue #2636) ---------------------------
//
// The filesystem package emits its always-on destructive-step records through
// the package-level slog default, resolved per call by fsLog(). These helpers
// install a capturing handler for the duration of a test and expose the records
// for assertion.
//
// Tests using them must NOT call t.Parallel(): slog.SetDefault is
// process-global, and Go only starts parallel tests after every sequential test
// in the package has finished, so running sequentially is the only way to own
// the default logger exclusively.

type fsLogEntry struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

type fsLogState struct {
	mu      sync.Mutex
	entries []fsLogEntry
}

type fsLogHandler struct {
	state *fsLogState
	attrs []slog.Attr
}

func (h *fsLogHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *fsLogHandler) Handle(_ context.Context, r slog.Record) error {
	entry := fsLogEntry{level: r.Level, msg: r.Message, attrs: map[string]string{}}
	// Attributes bound via With() (component/op/path here) arrive on the
	// handler, not on the record, so both sources have to be merged or every
	// assertion on "op" would silently see an empty value.
	for _, a := range h.attrs {
		entry.attrs[a.Key] = a.Value.String()
	}
	r.Attrs(func(a slog.Attr) bool {
		entry.attrs[a.Key] = a.Value.String()
		return true
	})
	h.state.mu.Lock()
	h.state.entries = append(h.state.entries, entry)
	h.state.mu.Unlock()
	return nil
}

func (h *fsLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	merged := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	merged = append(merged, h.attrs...)
	merged = append(merged, attrs...)
	return &fsLogHandler{state: h.state, attrs: merged}
}

func (h *fsLogHandler) WithGroup(_ string) slog.Handler { return h }

// captureFSLogs redirects the default logger into a fresh state for the
// remainder of the test and restores the previous default on cleanup.
func captureFSLogs(t *testing.T) *fsLogState {
	t.Helper()
	state := &fsLogState{}
	prev := slog.Default()
	slog.SetDefault(slog.New(&fsLogHandler{state: state}))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return state
}

// matching returns every captured entry with the given message that also
// carries the given path, so an assertion cannot be satisfied by an unrelated
// record about a different file emitted in the same test.
func (s *fsLogState) matching(msg, path string) []fsLogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []fsLogEntry
	for _, e := range s.entries {
		if e.msg == msg && e.attrs["path"] == path {
			out = append(out, e)
		}
	}
	return out
}

// requireOneFSRecord asserts exactly one record with the given message and path
// exists, that it carries the component tag and the expected level and step,
// and returns it so the caller can assert the outcome attributes that matter
// for its specific failure mode.
func requireOneFSRecord(t *testing.T, s *fsLogState, msg, path string, level slog.Level, step string) fsLogEntry {
	t.Helper()
	got := s.matching(msg, path)
	if len(got) != 1 {
		s.mu.Lock()
		all := append([]fsLogEntry(nil), s.entries...)
		s.mu.Unlock()
		t.Fatalf("want exactly 1 %q record for path %s, got %d; all captured records: %+v", msg, path, len(got), all)
	}
	e := got[0]
	if e.level != level {
		t.Errorf("record %q level = %v, want %v (an Info-level destructive record is silenced by a routine level bump)", msg, e.level, level)
	}
	if e.attrs["component"] != "filesystem" {
		t.Errorf("record %q component = %q, want %q", msg, e.attrs["component"], "filesystem")
	}
	if e.attrs["step"] != step {
		t.Errorf("record %q step = %q, want %q", msg, e.attrs["step"], step)
	}
	return e
}

// TestFSLogResolvesDefaultAtCallTime pins the reason fsLog is a function rather
// than a package-level var. cmd/stillwater installs the configured handler with
// slog.SetDefault long after this package's init runs, so a captured-at-init
// logger would send every destructive record to the bootstrap default: no
// redaction, no ring buffer, no live level control, and nothing capturable by
// the tests below.
//
// The mutation that proves this test has teeth is turning fsLog into
// `var fsLogger = slog.Default().With(...)` plus `func fsLog() *slog.Logger {
// return fsLogger }`: the handler installed here is then never consulted and
// the assertion fails on zero captured entries.
func TestFSLogResolvesDefaultAtCallTime(t *testing.T) {
	state := captureFSLogs(t)

	fsLog().Warn("probe", slog.String("path", "/tmp/probe"))

	got := state.matching("probe", "/tmp/probe")
	if len(got) != 1 {
		t.Fatalf("fsLog() did not route through the default logger installed after package init: got %d records, want 1", len(got))
	}
	if got[0].attrs["component"] != "filesystem" {
		t.Errorf("component = %q, want %q", got[0].attrs["component"], "filesystem")
	}
}
