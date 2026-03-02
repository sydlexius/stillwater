# Issue #323 -- Code Analysis, Test Coverage, and Quality Improvements

## Overview

Document the current state of linting, test coverage, and testing infrastructure.
Identify coverage gaps and propose targeted improvements. Evaluate whether testing
helpers or third-party libraries would benefit the project.

## Linting Status

golangci-lint with 13 linters reports **0 issues**. The codebase is lint-clean.

Active linters: errcheck, govet, staticcheck, unused, bodyclose, gosec, noctx,
sqlclosecheck, unconvert, unparam, wastedassign, misspell, revive.

No additional linters recommended at this time. The current set covers the most
impactful categories (correctness, security, resource leaks, style).

## Test Coverage Summary

### Excellent (80%+)
- `internal/event` (93.5%), `internal/logging` (88.8%), `internal/nfo` (87.7%)
- `internal/platform` (83.4%), `internal/connection` (82.7%)
- `internal/provider/duckduckgo` (82.6%)

### Good (70-80%)
- `internal/scanner` (78.1%), `internal/provider/wikidata` (77.3%)
- `internal/webhook` (76.3%), `internal/provider/audiodb` (76.0%)
- `internal/connection/lidarr` (76.2%), `internal/provider/deezer` (75.6%)
- `internal/artist` (75.3%), `internal/image` (75.5%)
- `internal/provider/musicbrainz` (75.0%), `internal/settingsio` (74.2%)
- `internal/maintenance` (71.4%)

### Acceptable (60-70%)
- `internal/provider/lastfm` (69.9%), `internal/provider` (67.1%)
- `internal/provider/discogs` (66.3%), `internal/watcher` (66.0%)
- `internal/library` (64.8%), `internal/connection/emby` (63.9%)
- `internal/backup` (62.1%), `internal/rule` (61.9%)
- `internal/connection/jellyfin` (61.5%), `internal/provider/fanarttv` (60.0%)

### Low (<60%)
- `internal/filesystem` (53.1%), `internal/api/middleware` (44.7%)
- `internal/scraper` (33.9%)

### Critical (0% or near-zero)
- **`internal/api` (12.5%)** -- all HTTP handlers
- `internal/auth` (0.0%), `internal/config` (0.0%)
- `internal/database` (0.0%), `internal/encryption` (0.0%)

## Coverage Gap Analysis

### Priority 1: internal/api (12.5%)

This is the most critical gap. The `internal/api` package contains all HTTP
handlers -- the entire surface area of the application.

**Existing test infrastructure**: `handlers_backup_test.go` has a `testRouter`
helper that creates an in-memory SQLite database and wires up all services. This
pattern should be reused for all handler tests.

**Priority handlers to test**:
1. Field update/clear (`handleFieldUpdate`, `handleFieldClear`)
2. Image fetch/crop/delete
3. Artist CRUD (list, detail, delete)
4. Rule execution and management
5. Settings read/write
6. Backup/restore (partially covered)

**Target**: 50%+ coverage

### Priority 2: internal/auth (0.0%)

Session management is security-critical code with zero test coverage.

**Key functions to test**:
- Session creation and storage
- Session validation (valid, expired, revoked)
- CSRF token generation and validation
- Password hashing and verification
- Login/logout handlers

**Target**: 80%+ coverage

### Priority 3: internal/encryption (0.0%)

Encryption is security-critical code with zero test coverage.

**Key functions to test**:
- AES-256-GCM encrypt/decrypt round-trip
- Key derivation
- Error cases (wrong key, corrupted ciphertext, empty input)

**Target**: 90%+ coverage

### Priority 4: internal/config (0.0%)

Configuration parsing affects application startup reliability.

**Key functions to test**:
- YAML file parsing
- Environment variable overrides
- Default values for missing config
- Invalid config handling (missing required fields, bad types)

**Target**: 70%+ coverage

### Priority 5: internal/scraper (33.9%)

**Key functions to test**:
- HTML parsing with golden file responses (recorded HTTP responses)
- Error handling for malformed HTML
- Rate limiting compliance

**Target**: 60%+ coverage

### Priority 6: internal/filesystem (53.1%)

**Key functions to test**:
- Atomic write (tmp/bak/rename pattern)
- Cross-mount copy fallback
- .bak file cleanup
- Permission handling
- Path edge cases

**Target**: 70%+ coverage

## Testing Infrastructure Evaluation

### Current state

The project uses only Go's standard `testing` package with `net/http/httptest`
for HTTP handler tests. No assertion library, no mock generation.

### Evaluation: Assertion helpers

**Option A: Project-internal `internal/testutil`**

```go
package testutil

func Equal(t *testing.T, got, want any) {
    t.Helper()
    if got != want {
        t.Errorf("got %v, want %v", got, want)
    }
}

func NoError(t *testing.T, err error) {
    t.Helper()
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
}
```

Pros: No external dependency, lightweight, consistent with project philosophy.
Cons: Reinventing the wheel, limited functionality.

**Option B: `testify/assert` + `testify/require`**

Pros: Well-tested, rich assertion set, widely used.
Cons: External dependency, project intentionally avoids third-party test deps.

**Option C: `go-cmp`**

Pros: Excellent for deep struct comparison (useful for Artist, NFO structs).
Cons: External dependency, though it is a Google-maintained stdlib-adjacent lib.

**Recommendation**: Start with Option A for simple assertions. Consider `go-cmp`
only if struct comparison tests become unwieldy. Avoid testify.

### Evaluation: HTTP test helpers

A `PatchJSON` / `GetJSON` / `PostJSON` helper set in `internal/testutil/http.go`
would reduce boilerplate in API handler tests:

```go
func PatchJSON(t *testing.T, handler http.HandlerFunc, path string,
    pathValues map[string]string, body any) *httptest.ResponseRecorder {
    t.Helper()
    b, _ := json.Marshal(body)
    req := httptest.NewRequest(http.MethodPatch, path, bytes.NewReader(b))
    req.Header.Set("Content-Type", "application/json")
    for k, v := range pathValues {
        req.SetPathValue(k, v)
    }
    rec := httptest.NewRecorder()
    handler(rec, req)
    return rec
}
```

**Recommendation**: Add this after the first batch of API handler tests reveals
the actual boilerplate patterns. Premature abstraction risk otherwise.

### Evaluation: Mock generation

**gomock / moq**: Useful for testing handlers in isolation from database. However,
the existing `testRouter` pattern uses real in-memory SQLite, which provides
better integration coverage. Mock generation is not recommended at this time.

### Evaluation: goleak

**goleak**: Detects goroutine leaks. Useful for testing background job lifecycle
(scanner, watcher, maintenance). Worth adding as a lightweight dependency.

**Recommendation**: Defer until background job tests are written.

## Sub-issues to Create

1. **API handler test coverage** -- target 50%+ from 12.5%
2. **Auth/encryption test coverage** -- target 80%+ from 0%
3. **Config test coverage** -- target 70%+ from 0%
4. **Scraper test coverage** -- target 60%+ from 33.9%
5. **Filesystem test coverage** -- target 70%+ from 53.1%
6. **Evaluate and implement testutil helpers** -- based on patterns from above

## Acceptance Criteria

- [ ] This plan file committed with all findings documented
- [ ] Sub-issues created for each coverage gap area
- [ ] Each sub-issue includes example test code and target coverage percentage
- [ ] Testing framework evaluation documented with recommendations
