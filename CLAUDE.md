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
internal/filesystem/  - Atomic file writes (tmp/bak/rename pattern)
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

## Architectural Decisions

Key decisions from the risk review that affect implementation across milestones:

- **ID-first matching:** When MBIDs are available (from Lidarr, NFO, embedded tags), use them directly. Skip name-based matching. Configurable priority: "Prefer ID match" (default), "Prefer name match", "Always prompt". Minimum confidence floor even in YOLO mode.
- **Atomic filesystem writes:** All file writes (NFO, images) use a shared utility in `internal/filesystem/`: write to .tmp, rename existing to .bak, rename .tmp to target, delete .bak. Fall back to copy+delete with fsync for cross-mount/network shares.
- **Singleton rate limiters:** One per metadata provider, created at application startup, shared across all handlers and background jobs. MusicBrainz: 1 req/sec globally.
- **Adaptive batched transactions:** Small batches (< 100): single transaction. Medium (100-1000): transactions of 50. Large (1000+): transactions of 25 with short sleep. User actions get priority over background jobs.
- **Image format policy:** JPG and PNG only. Logos always PNG (preserve alpha). When saving a new image, delete existing files of the same type in other formats.
- **Targeted platform refreshes:** Prefer per-artist refresh (Emby/Jellyfin/Lidarr) over full library scan. Full scan only for large bulk operations (500+ artists).
- **NFO conflict detection:** Check last-modified timestamp before writing. If changed externally, warn instead of overwriting. Also check Lidarr/Emby/Jellyfin metadata saver settings via API.
- **Scanner exclusions:** Default skip list: "Various Artists", "Various", "VA", "Soundtrack", "OST". Excluded directories appear greyed out and unfetchable. Classical music directories get special handling.

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

## Docker (Local Development)

Always use `docker-compose.local.yml` when building and running containers locally. This compose file mounts the persistent data and music volumes from `D:/appdata/stillwater/`:

```bash
docker build -f build/docker/Dockerfile -t ghcr.io/sydlexius/stillwater:latest .
docker compose -f docker-compose.local.yml up -d
```

If an existing container named `stillwater` is already running, stop and remove it first:

```bash
docker stop stillwater && docker rm stillwater
```

The local compose mounts:

- `D:/appdata/stillwater/data` -> `/data` (database, encryption key, backups)
- `D:/appdata/stillwater/music` -> `/music` (artist directories with NFO/images)

The app is available at `http://localhost:1973` once started.

## License

GPL-3.0
