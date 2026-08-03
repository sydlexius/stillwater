package filesystem

import "log/slog"

// fsLog returns the component-scoped logger used for this package's
// always-on destructive-step records (issue #2636).
//
// This is deliberately a FUNCTION rather than a package-level var. cmd/stillwater
// calls slog.SetDefault only after the configured handler is built, which happens
// long after this package's init runs. A `var fsLogger = slog.Default().With(...)`
// would capture the bootstrap default forever, so every record here would bypass
// the configured handler: no redaction, no ring buffer, no live level changes,
// and no capture in a test that installs its own default. Resolving the default
// per call costs one atomic load plus a two-attr clone on a path that runs only
// when a file is actually being written or destroyed.
//
// These records sit ALONGSIDE TraceFSWrite in trace.go, they do not replace it.
// TraceFSWrite is an env-gated (STILLWATER_TRACE_FS=1) debug tracer that answers
// "who wrote this path" with a caller stack and is off in production. The records
// emitted through fsLog are always on and answer a different question: what did
// this writer actually do to the file, and what state did it leave behind when a
// step failed. The attribute vocabulary is shared with the tracer where it
// overlaps (op, path) so both are greppable together.
func fsLog() *slog.Logger {
	return slog.Default().With(slog.String("component", "filesystem"))
}
