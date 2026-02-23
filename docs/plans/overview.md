# Stillwater - Implementation Plan Overview

## Project Summary

Stillwater is a containerized, self-hosted web application for managing artist and composer metadata (NFO files) and images (thumbnails, fanart, logos, banners) across media streaming platforms (Emby, Jellyfin, Kodi).

Inspired by Bliss (rule engine, auto-fix), MediaElch (multi-scraper aggregation), and Album Art Finder (configurable sources, image search UI).

**Status:** Core functionality complete (M1-M7 finished). In active development on UX polish (M9) and deployment/docs (M10). Security work (M8) ongoing.

## Technology Stack

| Layer | Technology |
|-------|-----------|
| Language | Go 1.26+ |
| HTTP Router | net/http stdlib (Go 1.22+ pattern routing) |
| Templates | Go Templ (type-safe, compiled) |
| Interactivity | HTMX 2.x (vendored) |
| CSS | Tailwind CSS (standalone CLI, no Node.js) |
| Database | SQLite via modernc.org/sqlite (pure Go, no CGO) |
| Migrations | goose (SQL-based) |
| Charts | Chart.js (vendored) |
| Image Crop | Cropper.js (vendored) |
| CI/CD | GitHub Actions |
| Container | Multi-stage Docker, GHCR |
| API Testing | Bruno collections |

## Milestones

| Milestone | Version | Focus | Status |
|-----------|---------|-------|--------- |
| M1 | v0.1.0 | Project Scaffolding | COMPLETE |
| M2 | v0.2.0 | Core Data Model and Scanner | COMPLETE |
| M3 | v0.3.0 | Provider Adapters | COMPLETE |
| M4 | v0.4.0 | Image Management | COMPLETE |
| M5 | v0.5.0 | Rule Engine | COMPLETE |
| M6 | v0.6.0 | Reports and Dashboard | COMPLETE |
| M7 | v0.7.0 | Integrations | COMPLETE |
| M8 | v0.8.0 | Security and Stability | |
| M9 | v0.9.0 | UX Polish | |
| M10 | v0.10.0 | Deployment and Docs | |
| M11 | v0.11.0 | Universal Music Scraper | |

## Architecture

```
cmd/stillwater/          Main entry point
internal/
  api/                   HTTP handlers + middleware (auth, CSRF, logging)
  auth/                  Session-based authentication
  config/                Configuration (env + YAML)
  database/              SQLite repository layer + migrations
  encryption/            AES-256-GCM for secrets at rest
  artist/                Artist domain logic
  nfo/                   NFO parser/writer (Kodi-compatible XML)
  provider/              Metadata source adapters
    musicbrainz/
    fanart/
    theaudiodb/
    discogs/
    lastfm/
    wikidata/
  filesystem/            Atomic file writes (tmp/bak/rename)
  image/                 Image fetch, process, crop, compare
  rule/                  Rule engine (define, evaluate, auto-fix)
  scanner/               Filesystem + API library scanners
  scheduler/             Background jobs
  connection/            External service connections (Emby, Jellyfin, Lidarr)
  notification/          Webhook dispatcher
web/
  templates/             Templ templates
  static/                CSS output, vendored JS
  components/            Reusable Templ components
api/bruno/               Bruno API test collections
build/
  docker/                Dockerfile, entrypoint
  swag/                  LSIO SWAG reverse proxy configs
  unraid/                Unraid CA template
```

## API Design

REST API at `/api/v1/`. All endpoints return JSON. Web UI consumes the same API via HTMX (HTML partials from `/web/` routes).

| Group | Key Routes |
|-------|-----------|
| Auth | POST /auth/login, POST /auth/logout, GET /auth/me |
| Artists | GET /artists, GET /artists/:id, PUT /artists/:id/nfo, GET /artists/:id/diff |
| Images | GET /artists/:id/images, POST /artists/:id/images/upload, POST /artists/:id/images/fetch |
| Search | GET /search/artists, GET /search/images |
| Rules | GET /rules, PUT /rules/:id, POST /rules/:id/run, POST /rules/run-all |
| Reports | GET /reports/compliance, GET /reports/health, GET /reports/health/history |
| Connections | GET /connections, POST /connections, PUT /connections/:id, DELETE /connections/:id |
| Settings | GET /settings, PUT /settings |
| Webhooks | GET /webhooks, POST /webhooks, PUT /webhooks/:id, DELETE /webhooks/:id |
| Scanner | POST /scanner/run, GET /scanner/status |
| Bulk | POST /bulk/fetch-metadata, POST /bulk/fetch-images, GET /bulk/jobs/:id |

## Dependency Graph

```
M1 (Scaffolding) ........... COMPLETE
  |
  v
M2 (Data Model + Scanner) .. COMPLETE
  |
  +---> M3 (Providers) ..... COMPLETE
  |       |
  |       v
  +---> M4 (Images) ........ COMPLETE
  |       |
  |       v
  +---> M5 (Rule Engine) ... COMPLETE
          |
          v
        M6 (Reports) ....... COMPLETE
          |
          v
        M7 (Integrations) .. COMPLETE
          |
          v
        M8 (Security + Stability)
          |
          v
        M9 (UX Polish)
          |
          v
        M10 (Deployment + Docs)

        M11 (Universal Scraper) -- depends on M5 + M8
```

M8-M10 are sequential (security before UX, UX before packaging). M11 can start after M8 in parallel with M9/M10.

## Architectural Decisions (Risk Review)

Key decisions made during the risk review to avoid painting into corners:

1. **ID-first matching:** When MBIDs are available, use them directly. Configurable priority. Minimum confidence floor even in YOLO mode.
2. **Atomic filesystem writes:** All file writes use tmp/bak/rename pattern. Fall back to copy+delete for cross-mount.
3. **Encryption key management:** Auto-generate on first run, store in /data. Offline credential reset CLI subcommand for recovery.
4. **Adaptive batched transactions:** Batch sizes scale with operation size. User actions get priority over background jobs.
5. **Singleton rate limiters:** One per provider, created at startup, shared globally across all handlers and jobs.
6. **Scanner exclusions:** Default skip list (Various Artists, Soundtracks). Classical music directory designation. VGM relies on MusicBrainz.
7. **NFO conflict detection:** Timestamp-based + API checks for Lidarr/Emby/Jellyfin settings.
8. **Image format policy:** JPG and PNG only. Logos always PNG. Format cleanup on replace.
9. **HTMX error handling:** Error toast and inline templates created in M2. Global error handler in layout.
10. **Targeted platform refreshes:** Prefer per-artist refresh over full library scan. Full scan only for large bulk operations.

## Conventions

- No emoji in code, commits, comments, or documentation
- No em-dashes in any output
- Semantic versioning (git tags drive version injection via ldflags)
- Content-hash cache busting for static assets
- Repository pattern for data access
- CSRF protection on all state-changing requests
- API keys encrypted at rest (AES-256-GCM)
- Structured logging via log/slog with log scrubbing
