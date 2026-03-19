---
applyTo: "internal/api/**/*.go"
---

# API Handler Review

## OpenAPI spec consistency (semantic review only)

CI validates structural spec correctness (Spectral linter) and handler-spec field
consistency (AST-based Go test in `internal/api/openapi_test.go`). Focus on
*semantic* accuracy only:

- Descriptions must reflect what the code actually does. "Empty when X" is wrong if
  the code also makes the field non-empty when Y. Prefer describing the invariant.

## Error path warning completeness (always check)

When a function surfaces failures as user-visible warnings (returns `[]string`
warnings, appends to a warning slice, or emits HX-Trigger header values):

- Verify ALL error paths append a warning before returning -- not just the path
  that reaches the primary operation. Early-return paths (service lookup failure,
  missing file, unsupported type, disabled connection) must also emit a warning.
- Client-visible warning strings must be generic -- never include raw `error.Error()`
  text from database operations, internal services, or system calls. Full errors
  belong in a server-side `slog` call; the client gets a sanitized message.
- "We logged it" is not sufficient. If sync was skipped due to an internal error,
  the client should see a warning that sync was skipped.

## Concurrency

HTTP handlers run concurrently (net/http). Check for:
- Package-level variables read or written without synchronization
- Shared caches, maps, or singletons accessed from handlers
- Goroutines using `context.Background()` where `context.WithoutCancel(reqCtx)`
  should be used (gosec G118)
