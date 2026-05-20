# Stillwater - Claude Code Project Instructions

## Project Overview

Stillwater is a containerized, self-hosted web application for managing artist/composer metadata (NFO files) and images across media streaming platforms (Emby, Jellyfin, Kodi). Built with Go, HTMX, Templ, and Tailwind CSS.

## Style and Conventions

- **No emoji** in code, commits, comments, or documentation
- **No em-dashes** in any output
- Go 1.26+ with `net/http` stdlib routing (no third-party router needed)
- Structured logging via `log/slog`
- Pure Go SQLite via `modernc.org/sqlite` (no CGO)
- API-first design: all features accessible via REST API at `/api/v1/`
- Web UI consumes the same API via HTMX
- Minimal JS dependencies: only vendored libs (HTMX, Cropper.js, Chart.js)
- Follow coding standards in `.github/instructions/` for error handling, test quality, and concurrency

## Architecture

```
cmd/stillwater/       - Main entry point
cmd/gen-*/            - Doc generators (env-reference, provider-matrix, rules-catalogue, settings-reference)
internal/api/         - HTTP handlers, middleware, and SSE hub
internal/artist/      - Artist domain model, service, and repository interfaces
internal/auth/        - Authentication (session-based)
internal/backup/      - Database backup service
internal/config/      - Configuration loading (env + YAML)
internal/conflict/    - Conflict detection and gating (coalesce, ledger)
internal/connection/  - External platform connections (Emby, Jellyfin, Lidarr)
internal/database/    - SQLite database and migrations
internal/dbutil/      - Shared database helpers (type conversions, nullable handling)
internal/encryption/  - AES-256-GCM encryption for secrets
internal/event/       - Channel-based event bus
internal/filesystem/  - Atomic file writes (tmp/bak/rename pattern)
internal/foreign/     - Foreign artist scanner and model
internal/i18n/        - Internationalization support
internal/image/       - Image processing (fetch, crop, compare)
internal/imagebridge/ - Resolves artist IDs to platform-specific image URLs
internal/langpref/    - Language preferences (per-user/per-locale)
internal/library/     - Music library management
internal/logging/     - Log manager (levels, rotation, ring buffer)
internal/maintenance/ - Scheduled maintenance tasks
internal/nfo/         - NFO file parser and writer
internal/platform/    - Platform profiles
internal/provider/    - Metadata source adapters (MusicBrainz, Fanart.tv, etc.)
internal/publish/     - Publisher for NFO and platform writes
internal/rule/        - Rule engine (Bliss-inspired)
internal/scanner/     - Filesystem and API library scanners
internal/scraper/     - Configurable web scraping
internal/server/      - HTTPS / HTTP/3 listeners, ACME cert manager, TLS BYO
internal/settingsio/  - Application settings persistence
internal/updater/     - Self-updater with channel + semver gating
internal/version/     - Build version injection via ldflags
internal/watcher/     - Filesystem watcher for library directories
internal/webhook/     - Webhook dispatcher
web/components/       - Reusable templ components (badges, modals, toasts, icons)
web/templates/        - Templ templates
web/static/           - CSS, vendored JS
api/bruno/            - Bruno API test collections
build/docker/         - Dockerfile, entrypoint
build/swag/           - LSIO SWAG reverse proxy configs
```

## Common Commands

```bash
make build          # Build binary (runs templ generate + tailwind first)
make run            # Build and run locally with debug logging
make dev            # Hot reload with air
make test           # Run all tests with race detector
make test-race      # Race detector explicitly (matches `go test -race ./...`)
make generate       # Regenerate templ + tailwind (umbrella)
make lint           # Run golangci-lint
make hadolint       # Lint Dockerfile(s)
make fmt            # Format Go + Templ files
make hooks          # Install git pre-commit hook
make migrate        # Apply database migrations
make check-openapi  # Validate OpenAPI spec
make clean          # Remove build artifacts
make scan           # Build Docker image (no cache) and scan for CVEs
make docker-build   # Build Docker image
make docker-run     # Start via docker compose
```

## Running Long Tests

A test run's output is a deterministic artifact: capture it once, grep it
many times. Never re-run a long suite (race tests especially) just to
re-filter the output. Pipe it to a file, then search the file:

```bash
. scripts/lib/run-paths.sh   # provides $SW_RUN_DIR (per-worktree, ephemeral)
go test -race -count=1 ./internal/<pkg>/ 2>&1 | tee "$SW_RUN_DIR/race.log"
grep -nE 'WARNING: DATA RACE|--- FAIL' "$SW_RUN_DIR/race.log"
```

Do not run the full `./...` race suite as a pre-PR check -- that is the
pre-push gate's job and the pre-push git hook runs it automatically. The
capture rule is for targeted runs while debugging. When dispatching a
subagent that runs tests, paste this rule into its prompt; subagents do not
load project memory. The `capture-race-test-output` hookify rule blocks
uncaptured `go test -race` invocations.

## GitHub Issue Hints

When working on a GitHub issue, look for these tags in the issue body:

- **`[mode: plan]`** / **`[mode: direct]`** - Plan Mode vs. direct implementation
- **`[model: opus]`** / **`[model: sonnet]`** / **`[model: haiku]`** - Model selection
- **`[effort: high]`** / **`[effort: medium]`** / **`[effort: low]`** - Reasoning depth

Default when no hint: Sonnet + Plan Mode + medium effort for features; Sonnet + direct + medium for bugs; Haiku + direct + low for docs-only.

**Pause required for:** model mismatch (ask user to switch) or `[effort: high]` (ask user to enable extended thinking). Do not start until confirmed or explicitly waived.

## Key Rules

- **Architectural decisions:** See `docs/architecture-decisions.md`
- **Database schema:** `internal/database/migrations/001_initial_schema.sql`; interfaces in `internal/artist/repository.go`
- **Rule engine:** Fix-all uses in-memory progress tracker (mutex-protected), one at a time (409 on concurrent starts). `FixResult` states: `Fixed`, `Dismissed`, neither. Rules have enabled toggle + automation mode (`manual`/`auto`).
- **Tests:** Integration tests use real SQLite. Run `go test -race ./...` for concurrent code (goroutines, shared state, background workers). Native on macOS.
- **Security:** API keys encrypted at rest (AES-256-GCM). Scrub sensitive values from logs. CSRF on state-changing requests. Validate at API boundary. No secrets in git.

## PR Workflow

Run `bash scripts/pre-push-gate.sh` (deterministic checks), then `/pr-review-toolkit:review-pr`, then squash and push. Never open a PR until both pass. See `docs/pr-workflow.md` for full details including the gh `!=` bash history workaround and Copilot policy.

**Review comment scope:** Default: fix now. Defer only for architectural changes or unrelated subsystems. Never defer without creating a tracking issue.

## Worktrees

Use git worktrees for concurrent issue/agent work. Naming: `../stillwater-{issue}/`, `../stillwater-m{N}-{issue}/`. Track in `~/.claude/projects/<project>/memory/worktrees.md`. See `docs/worktrees.md` for full protocol. After merge: `bash $HOME/.claude/scripts/cleanup-worktree.sh <suffix>` (where `<suffix>` is whatever follows `stillwater-` in the worktree dirname -- e.g. `1180`, `m36-639`, or a slug like `fanart-dup`).

## Milestone Work

See `docs/milestone-protocol.md`. Start with scope assessment, create `docs/plans/m<N>-plan.md`, use per-issue worktrees, ship docs in the same PR, run cleanup after all merges.

## CI/CD

All GitHub Actions pinned to commit SHAs (not tags) with version tag as inline comment:
```yaml
uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd # v6
```
Do not use `paths-ignore` on triggers when required status checks exist (GitHub treats missing checks as failed).

## Helper Scripts

- `scripts/pre-push-gate.sh` -- deterministic pre-push checks (tests, OpenAPI, generated files)
- `scripts/dev-restart.sh` -- canonical dev rebuild + restart (use this; never kill by port)
- `scripts/patch-coverage.sh` -- patch-level coverage check (called by pre-push-gate)
- `scripts/smoke.sh` -- API smoke tests against a running instance
- `scripts/check-generated.sh` -- verify *_templ.go was regenerated after .templ changes
- `~/.claude/scripts/cleanup-worktree.sh <suffix>` -- remove worktree, delete local/remote branches, prune refs (repo-agnostic; auto-detects the main worktree's basename as the prefix)
- `~/.claude/scripts/pr-unreplied-comments.sh [--wait] [--count-only] <PR>` -- unreplied bot comments

## License

GPL-3.0
