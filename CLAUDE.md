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
internal/api/         - HTTP handlers, middleware, and SSE hub
internal/artist/      - Artist domain model, service, and repository interfaces
internal/auth/        - Authentication (session-based)
internal/backup/      - Database backup service
internal/config/      - Configuration loading (env + YAML)
internal/connection/  - External platform connections (Emby, Jellyfin, Lidarr)
internal/database/    - SQLite database and migrations
internal/dbutil/      - Shared database helpers (type conversions, nullable handling)
internal/encryption/  - AES-256-GCM encryption for secrets
internal/event/       - Channel-based event bus
internal/filesystem/  - Atomic file writes (tmp/bak/rename pattern)
internal/i18n/        - Internationalization support
internal/image/       - Image processing (fetch, crop, compare)
internal/imagebridge/ - Resolves artist IDs to platform-specific image URLs
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
internal/settingsio/  - Application settings persistence
internal/version/     - Build version injection via ldflags
internal/watcher/     - Filesystem watcher for library directories
internal/webhook/     - Webhook dispatcher
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
make lint           # Run golangci-lint
make fmt            # Format Go + Templ files
make hooks          # Install git pre-commit hook
make check-openapi  # Validate OpenAPI spec
make clean          # Remove build artifacts
make docker-build   # Build Docker image
make docker-run     # Start via docker compose
```

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

Use git worktrees for concurrent issue/agent work. Naming: `../stillwater-{issue}/`, `../stillwater-m{N}-{issue}/`. Track in `~/.claude/projects/<project>/memory/worktrees.md`. See `docs/worktrees.md` for full protocol. After merge: `bash scripts/cleanup-worktree.sh <issue>`.

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
- `scripts/cleanup-worktree.sh <issue>` -- remove worktree, delete local/remote branches, prune refs
- `scripts/check-generated.sh` -- verify *_templ.go was regenerated after .templ changes
- `~/.claude/scripts/pr-unreplied-comments.sh [--wait] [--count-only] <PR>` -- unreplied bot comments

## License

GPL-3.0
