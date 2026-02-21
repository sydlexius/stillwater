# Copilot Review Fixes

## Goal

Address outstanding Copilot review comments from PRs #52, #54, #55, #57, and #60 that were merged without resolving all feedback. Covers security vulnerabilities, correctness bugs, performance issues, CI/build fixes, code quality, and missing test coverage.

## Source

Copilot PR reviews filed after merge. Each item verified against the current codebase on 2026-02-20 to confirm it is still applicable.

## Issues

| # | Title | Mode | Model | Priority |
|---|-------|------|-------|----------|
| 61 | fix(security): validate NFO snapshot belongs to requested artist | direct | sonnet | P1 security |
| 62 | fix(security): harden backup download headers | direct | haiku | P1 security |
| 63 | fix(security): rate limiter XFF spoofing, goroutine leak, dead code | direct | sonnet | P1 security |
| 64 | feat(security): add CSP and HSTS headers | plan | sonnet | P1 security |
| 65 | fix: health report pagination error handling | direct | haiku | P2 bug |
| 66 | fix: rune-aware truncation in NFO diff display | direct | haiku | P2 bug |
| 67 | fix: correct health score test comment | direct | haiku | P2 bug |
| 68 | fix: implement pagination for 10k artist limit | direct | sonnet | P3 perf |
| 69 | fix(ci): GoReleaser pre-release tagging and Dockerfile | direct | haiku | P4 ci |
| 70 | fix: log level and UI text improvements | direct | haiku | P5 quality |
| 71 | test: add coverage for backup handlers and security middleware | direct | sonnet | P6 test |

## Workflow

Each batch gets its own feature branch and PR so Copilot can review the changes before merging. Branch naming: `fix/copilot-review-batch-N` (or descriptive name for single-issue PRs).

## Implementation Order

Work through issues in priority order. Security fixes first, then bugs, then the rest. Issues within the same priority tier can be done in any order.

### Batch 1: Security (Issues #61, #62, #63, #64)

Branch: `fix/copilot-review-security`

These are the highest priority and should be done first. Issue #64 (CSP/HSTS) uses plan mode because CSP headers require careful tuning for HTMX compatibility.

### Batch 2: Bugs (Issues #65, #66, #67)

Branch: `fix/copilot-review-bugs`

Small, focused fixes. Each is a single-file change. Combined into one PR.

### Batch 3: Performance (Issue #68)

Branch: `fix/copilot-review-pagination`

Touches three files with the same pattern (replace hard-coded 10k fetch with pagination loop). Single PR.

### Batch 4: CI/Build (Issue #69)

Branch: `fix/copilot-review-ci`

Three small changes to GoReleaser config and Dockerfile. Single PR.

### Batch 5: Code Quality and Tests (Issues #70, #71)

Branch: `fix/copilot-review-quality`

Low urgency. Can be done at any time.

## Guidance per Issue

### #61 - NFO snapshot cross-artist validation

Straightforward guard clause addition. Two functions in the same file need the same fix: after `GetByID`, check `snap.ArtistID == artistID`. Also fix silent error swallowing in the diff handler by returning proper HTTP errors instead of continuing with nil data.

Suggested prompt: "Fix #61. In handlers_nfo.go, add artistID validation after GetByID in both handleNFODiff and handleNFOSnapshotRestore. Also return 404/400 errors for snapshot fetch/parse failures in handleNFODiff instead of silently ignoring them."

### #62 - Backup download headers

Two-line fix. Quote the filename in Content-Disposition and add Content-Type header.

Suggested prompt: "Fix #62. In handlers_backup.go handleBackupDownload, quote the filename in the Content-Disposition header and add an explicit Content-Type: application/octet-stream header."

### #63 - Rate limiter XFF, goroutine leak, dead code

Three related changes in ratelimit.go. The XFF fix requires a design decision: either parse the rightmost IP (simpler) or add trusted proxy config (more robust). For a self-hosted app behind a reverse proxy, rightmost IP parsing is a reasonable default. The goroutine leak fix requires passing a context from main.go.

Suggested prompt: "Fix #63. In ratelimit.go: (1) Change clientIP to parse the rightmost (last) IP from X-Forwarded-For instead of the leftmost. (2) Add a context.Context parameter to NewLoginRateLimiter and use it to stop the cleanup goroutine. Update main.go to pass the app context. (3) Remove the redundant len(xff) > 0 check in clientIP."

### #64 - CSP and HSTS headers

Needs plan mode because CSP must be compatible with HTMX (which uses `htmx.ajax`, inline event handlers like `hx-on`, etc.) and any inline styles/scripts in templates. The CSP policy needs to allow `'unsafe-inline'` for styles at minimum, and possibly `'unsafe-eval'` depending on HTMX configuration. Test in browser dev tools after adding.

Suggested prompt: "Fix #64. Add CSP and HSTS headers to the security middleware. CSP needs to be compatible with HTMX, Cropper.js, and Chart.js. HSTS should only be set when the request arrives over HTTPS (check r.TLS or X-Forwarded-Proto). Start with a permissive CSP and tighten based on what the app actually needs."

### #65 - Health report pagination error handling

Replace `break` with error logging and HTTP 500 response.

Suggested prompt: "Fix #65. In handlers_report.go, replace the silent break on artistService.List error with a logged error and HTTP 500 response."

### #66 - UTF-8 truncation

Convert truncateStr to use `[]rune` instead of byte slicing.

Suggested prompt: "Fix #66. In nfo_diff.templ, change truncateStr to use rune-aware slicing so multi-byte UTF-8 characters are not split."

### #67 - Health score test comment

Read the test and the actual health score calculation logic, correct the comment to match reality.

Suggested prompt: "Fix #67. In scanner_test.go, correct the health score calculation comment to reflect the actual number of evaluated vs skipped rules. Verify the test assertion value matches."

### #68 - 10k artist pagination

Same pattern in three files. Replace single large fetch with a pagination loop.

Suggested prompt: "Fix #68. In scanner.go, fixer.go, and bulk_executor.go, replace the hard-coded PageSize: 10000 with a pagination loop that fetches artists in batches of 1000 until all are processed."

### #69 - GoReleaser fixes

Three small config changes.

Suggested prompt: "Fix #69. In .goreleaser.yml: (1) add skip_push for prerelease to the major.minor and latest docker manifest entries, (2) change make_latest from true to auto. In build/docker/Dockerfile.goreleaser: add WORKDIR /app before the COPY directives."

### #70 - Log level and UI text

Two small changes in different files.

Suggested prompt: "Fix #70. (1) In cmd/stillwater/main.go, change the encryption key file path log from Info to Debug level. (2) In web/templates/settings.templ, change the missing API key message from 'Subscription required' to 'API key required'."

### #71 - Test coverage

Write new test files for backup handlers and security middleware. Follow the existing test patterns in the codebase (httptest.NewRecorder, table-driven tests).

Suggested prompt: "Fix #71. Create test files for: (1) internal/api/handlers_backup_test.go covering backup create, history, and download endpoints. (2) internal/api/middleware/csrf_test.go, ratelimit_test.go, and security_test.go covering token generation/validation, rate limiting behavior, and header presence. Follow the existing test patterns in the codebase."

## Progress

- [ ] #61 - fix(security): validate NFO snapshot belongs to requested artist
- [ ] #62 - fix(security): harden backup download headers
- [ ] #63 - fix(security): rate limiter XFF spoofing, goroutine leak, dead code
- [ ] #64 - feat(security): add CSP and HSTS headers
- [ ] #65 - fix: health report pagination error handling
- [ ] #66 - fix: rune-aware truncation in NFO diff display
- [ ] #67 - fix: correct health score test comment
- [ ] #68 - fix: implement pagination for 10k artist limit
- [ ] #69 - fix(ci): GoReleaser pre-release tagging and Dockerfile
- [ ] #70 - fix: log level and UI text improvements
- [ ] #71 - test: add coverage for backup handlers and security middleware
