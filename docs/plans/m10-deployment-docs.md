# M10: Deployment and Docs (v0.10.0)

## Goal

Packaging for Unraid, SWAG reverse proxy configs, and OpenAPI documentation. Make the application easy to deploy and integrate.

## Prerequisites

- M9 (UX Polish) complete -- logo and responsive design should be finalized before packaging

## Issues

| # | Title | Mode | Model |
|---|-------|------|-------|
| 31 | Unraid Community Apps template | direct | haiku |
| 32 | LSIO SWAG reverse proxy configs (subdomain + subfolder) | direct | haiku |
| 33 | OpenAPI spec and API documentation | direct | sonnet |

## Implementation Order

### Step 1: Unraid CA Template (#31)

1. Create `build/unraid/stillwater.xml`:
   - Follow Community Applications XML format
   - Required fields:
     - Name: Stillwater
     - Description: Artist metadata and image management for Emby, Jellyfin, and Kodi
     - Icon URL (from repo or GHCR)
     - Repository: ghcr.io/sydlexius/stillwater
     - Registry: ghcr.io
     - Network: bridge
     - WebUI: `http://[IP]:[PORT:1973]`
   - Port mapping: 1973 (container) -> 1973 (host, configurable)
   - Volume mappings:
     - `/data` -- config and database (Appdata path)
     - `/music` -- music library (read-only capable)
   - Environment variables:
     - `PUID` (default: 99)
     - `PGID` (default: 100)
     - `SW_ENCRYPTION_KEY` (with description about auto-generation)
   - Support URL: GitHub issues
   - Project URL: GitHub repo

2. Validate XML format against CA schema

### Step 2: SWAG Reverse Proxy Configs (#32)

Configs already scaffolded in `build/swag/`. Verify and finalize.

1. Test `stillwater.subdomain.conf.sample`:
   - Verify all routes work through SWAG proxy
   - Static assets load correctly with cache-busted URLs
   - HTMX requests proxy correctly (no header stripping)
   - Image uploads work (check `client_max_body_size`)

2. Test `stillwater.subfolder.conf.sample`:
   - Set `SW_BASE_PATH=/stillwater` in container
   - Verify all routes work under `/stillwater/` prefix
   - Static asset paths resolve correctly
   - API endpoints accessible at `/stillwater/api/v1/`
   - HTMX `hx-get`/`hx-post` URLs work with base path

3. Update configs if issues found:
   - Ensure `proxy_set_header` includes `X-Forwarded-Proto`, `X-Real-IP`
   - WebSocket headers if needed (`Upgrade`, `Connection`)
   - Appropriate timeouts for long-running operations (scan, bulk fetch)

### Step 3: OpenAPI Spec (#33)

1. Write OpenAPI 3.1 spec in `docs/openapi.yaml`:
   - Info section (title, version, description, license)
   - Server URLs (default `http://localhost:1973/api/v1`)
   - Authentication schemes:
     - Cookie-based session (`sw_session`)
     - Bearer token (for API clients)
   - All endpoint groups:
     - Auth (`/auth/login`, `/auth/logout`, `/auth/me`, `/auth/setup`)
     - Artists (`/artists`, `/artists/{id}`, `/artists/{id}/nfo`, etc.)
     - Images (`/artists/{id}/images/*`)
     - Search (`/search/artists`, `/search/images`)
     - Rules (`/rules`, `/rules/{id}`, `/rules/run-all`)
     - Reports (`/reports/*`)
     - Connections (`/connections`, `/connections/{id}`, `/connections/{id}/test`)
     - Webhooks (`/webhooks`, `/webhooks/{id}`, `/webhooks/{id}/test`)
     - Settings (`/settings`, `/settings/backup`)
     - Scanner (`/scanner/run`, `/scanner/status`)
     - Aliases (`/artists/{id}/aliases`)
     - Push (`/artists/{id}/push`)
   - Request/response schemas with examples
   - Error response schema (consistent `{"error": "message"}` format)

2. Render API documentation:
   - Serve at `/api/docs` using Scalar (modern, lightweight)
   - Add route in `internal/api/router.go`
   - Embed the OpenAPI spec as a static file
   - Scalar loads from CDN or vendored JS

3. Complete Bruno collections in `api/bruno/`:
   - Ensure every endpoint has a corresponding request
   - Add response assertions for status codes and structure
   - Organize by endpoint group (folders)

## Key Design Decisions

- **Unraid first:** Primary target audience for self-hosted media management. Template enables one-click install from CA.
- **SWAG over Traefik/Caddy:** LSIO SWAG is the most common reverse proxy for Unraid users. Traefik/Caddy docs can be community-contributed.
- **Scalar over Swagger UI:** Scalar is a modern alternative with better UX, smaller footprint, and active maintenance. Falls back to Swagger UI if integration is complex.
- **OpenAPI 3.1 over 3.0:** Supports JSON Schema 2020-12, better nullable handling, webhooks specification.

## Verification

- [ ] Unraid CA template validates against CA schema
- [ ] SWAG subdomain config works with actual deployment
- [ ] SWAG subfolder config works with `SW_BASE_PATH`
- [ ] All routes accessible through reverse proxy
- [ ] OpenAPI spec is valid (lint with spectral or similar)
- [ ] API docs render at `/api/docs`
- [ ] Bruno collections cover all endpoints
- [ ] `go test ./...` and `golangci-lint run` pass
- [ ] Tag v0.10.0
