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

## Architecture

```
cmd/stillwater/       - Main entry point
internal/api/         - HTTP handlers and middleware
internal/auth/        - Authentication (session-based)
internal/config/      - Configuration loading (env + YAML)
internal/database/    - SQLite database and migrations
internal/encryption/  - AES-256-GCM encryption for secrets
internal/nfo/         - NFO file parser and writer
internal/provider/    - Metadata source adapters (MusicBrainz, Fanart.tv, etc.)
internal/rule/        - Rule engine (Bliss-inspired)
internal/scanner/     - Filesystem and API library scanners
internal/image/       - Image processing (fetch, crop, compare)
internal/notification/- Webhook dispatcher
web/templates/        - Templ templates
web/static/           - CSS, vendored JS
api/bruno/            - Bruno API test collections
build/docker/         - Dockerfile, entrypoint
build/swag/           - LSIO SWAG reverse proxy configs
build/unraid/         - Unraid CA template
```

## Common Commands

```bash
make build          # Build binary (runs templ generate + tailwind first)
make run            # Build and run locally with debug logging
make test           # Run all tests with race detector
make lint           # Run golangci-lint
make fmt            # Format Go + Templ files
make docker-build   # Build Docker image
make docker-run     # Start via docker compose
```

## GitHub Issue Hints

When working on a GitHub issue, look for these tags in the issue body:

- **`[mode: plan]`** - Start in Plan Mode. Explore the codebase and design an approach before writing code.
- **`[mode: direct]`** - Skip planning. The task is well-defined enough to implement directly.
- **`[model: opus]`** - Use Opus for this task. Indicates complex architecture, multi-file changes, or nuanced design decisions.
- **`[model: sonnet]`** - Use Sonnet for this task. Good balance of capability and speed for standard feature work.
- **`[model: haiku]`** - Use Haiku for this task. Indicates simple, well-defined changes (typo fixes, small additions, config changes).

If no hint is present, default to: Sonnet with Plan Mode for new features, Sonnet direct for bug fixes, Haiku for documentation-only changes.

## Testing

- Unit tests: `go test ./...`
- Integration tests use real SQLite (in-memory or temp file)
- API tests: Bruno collections in `api/bruno/`
- Pre-commit hooks enforce linting and formatting

## Security

- API keys encrypted at rest with AES-256-GCM
- Log output is scrubbed of sensitive values (API keys, passwords, tokens)
- CSRF protection on all state-changing requests
- Input validation at API boundary
- No secrets in code or config files committed to git

## Database

- SQLite with WAL mode
- Migrations managed by goose (SQL files in `internal/database/migrations/`)
- Single writer connection (SQLite limitation)
- Repository pattern for data access

## Versioning

This project follows [Semantic Versioning](https://semver.org/) (semver):

- **MAJOR** (X.0.0): Breaking API changes, incompatible config/DB schema changes
- **MINOR** (0.X.0): New features, backward-compatible additions
- **PATCH** (0.0.X): Bug fixes, security patches, documentation updates

Version is injected at build time via `-ldflags` (see Makefile). Git tags drive the version:

```bash
git tag -a v0.1.0 -m "Initial release"
```

Pre-release versions use the format `0.1.0-dev` (auto-detected from `git describe`).

Docker images are tagged with both the semver tag and the git SHA.

## Static Asset Cache Busting

Static files (CSS, JS) are served with content-hash-based cache busting:

- `StaticAssets` hashes each file at startup using SHA-256
- Templates receive cache-busted URLs (e.g., `/static/css/styles.css?v=a1b2c3d4e5f6`)
- When the hash matches, responses include `Cache-Control: public, max-age=31536000, immutable`
- When files change (new deploy), the hash changes and browsers fetch the new version
- No manual cache clearing needed during development or UAT

## License

GPL-3.0
