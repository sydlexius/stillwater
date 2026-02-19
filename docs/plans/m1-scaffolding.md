# M1: Project Scaffolding (v0.1.0) - COMPLETE

## Status: Complete

All scaffolding tasks have been implemented and the initial commit has been pushed.

## What Was Delivered

### Project Structure

- Go module initialized (go.mod with Go 1.26)
- Full directory structure per architecture spec
- CLAUDE.md with project conventions and issue hints

### Application Core

- `cmd/stillwater/main.go` - Entry point with graceful shutdown
- `internal/config/` - Configuration loading (YAML + env var overrides)
- `internal/database/` - SQLite setup (WAL mode, pure Go) + goose migrations
- `internal/auth/` - Session-based auth with bcrypt + session tokens
- `internal/encryption/` - AES-256-GCM for secrets at rest
- `internal/version/` - Semver injection via ldflags
- `internal/api/` - HTTP router, handlers, static asset cache busting
- `internal/api/middleware/` - Auth, CSRF, logging (with log scrubbing)

### Frontend

- Templ templates: layout, index (dashboard), login, setup
- HTMX 2.0.8 vendored in web/static/js/
- Tailwind CSS input file (standalone CLI builds output)
- Cache-busted static asset serving

### Infrastructure

- Multi-stage Dockerfile with Tailwind CLI
- docker-compose.yml with PUID/PGID support
- Entrypoint script for UID/GID remapping
- LSIO SWAG configs (subdomain + subfolder)
- Makefile with all build/dev/test/lint targets
- .golangci.yml with comprehensive linter config
- .pre-commit-config.yaml (linting, secrets, conventional commits)
- GitHub Actions CI (lint, test, build, Docker push)

### API Testing

- Bruno collection: health check, auth/setup, auth/login, auth/me

### Database Schema (001_initial.sql)

- Tables: users, sessions, connections, settings, artists, artist_aliases, nfo_snapshots, rules, health_history, webhooks
- Indexes on foreign keys, search columns, timestamps

## Files Created

```
cmd/stillwater/main.go
internal/api/router.go
internal/api/handlers.go
internal/api/static.go
internal/api/middleware/auth.go
internal/api/middleware/csrf.go
internal/api/middleware/logging.go
internal/auth/auth.go
internal/config/config.go
internal/database/database.go
internal/database/migrate.go
internal/database/migrations/001_initial.sql
internal/encryption/encryption.go
internal/version/version.go
web/templates/layout.templ
web/templates/index.templ
web/templates/setup.templ
web/templates/login.templ
web/static/css/input.css
web/static/js/htmx.min.js
build/docker/Dockerfile
build/docker/entrypoint.sh
build/swag/stillwater.subdomain.conf.sample
build/swag/stillwater.subfolder.conf.sample
docker-compose.yml
Makefile
.golangci.yml
.pre-commit-config.yaml
.github/workflows/ci.yml
.gitignore
CLAUDE.md
LICENSE
api/bruno/bruno.json
api/bruno/environments/local.bru
api/bruno/health/health-check.bru
api/bruno/auth/setup.bru
api/bruno/auth/login.bru
api/bruno/auth/me.bru
go.mod
```
