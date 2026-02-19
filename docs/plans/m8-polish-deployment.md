# M8: Polish and Deployment (v0.8.0)

## Goal

Final polish pass: mobile responsiveness, logo/favicon, Unraid template, SWAG config testing, OpenAPI documentation, security audit, and README.

## Prerequisites

- All prior milestones substantially complete

## Issues

| # | Title | Mode | Model |
|---|-------|------|-------|
| 29 | Mobile-responsive design pass | direct | sonnet |
| 30 | Logo and favicon design (SVG, theme-adaptive) | direct | sonnet |
| 31 | Unraid Community Apps template | direct | haiku |
| 32 | LSIO SWAG reverse proxy configs (subdomain + subfolder) | direct | haiku |
| 33 | OpenAPI spec and API documentation | direct | sonnet |
| 34 | Security audit: encryption, log scrubbing, input validation | plan | opus |
| 45 | Database backup: scheduled and manual | plan | sonnet |
| 46 | Offline credential reset CLI subcommand | direct | sonnet |

## Implementation Order

### Step 1: Mobile-Responsive Design (#29)

1. Audit all templates at mobile breakpoints (320px, 375px, 428px):
   - Dashboard
   - Artist list
   - Artist detail
   - Image search/comparison
   - Settings
   - Connection management
   - Reports

2. Fix common issues:
   - Tables: horizontal scroll wrapper or card layout on mobile
   - Modals: full-screen on small viewports
   - Grid layouts: single column on mobile
   - Touch targets: minimum 44px

3. Add mobile navbar:
   - Hamburger menu button (visible < md breakpoint)
   - Slide-out or dropdown menu
   - Close on outside click or navigation

4. Test image crop modal on touch devices

### Step 2: Logo and Favicon (#30)

1. Design SVG logomark:
   - Abstract flowing water / sound wave motif
   - Simple enough to work as favicon
   - CSS currentColor for theme adaptivity

2. Create favicon set:
   - favicon.ico (16x16 + 32x32 multi-size)
   - favicon-32x32.png
   - favicon-16x16.png
   - apple-touch-icon.png (180x180)
   - site.webmanifest

3. Integrate into layout:
   - Logo in navbar (replace text-only "Stillwater")
   - Favicon links in HTML head
   - Dark/light variant switching via CSS prefers-color-scheme

### Step 3: Unraid CA Template (#31)

1. Create `build/unraid/stillwater.xml`:
   - Follow Community Applications format
   - Name, description, icon URL (from repo)
   - Port mapping: 8080
   - Volume mappings: /data (config/DB), /music (library)
   - Environment variables: PUID, PGID, SW_ENCRYPTION_KEY
   - WebUI link
   - Support URL (GitHub issues)
   - Project URL (GitHub repo)

### Step 4: SWAG Config Testing (#32)

1. Test existing configs from `build/swag/`:
   - `stillwater.subdomain.conf.sample` with actual SWAG deployment
   - `stillwater.subfolder.conf.sample` with SW_BASE_PATH=/stillwater

2. Verify:
   - All routes work through proxy
   - Static assets load correctly
   - HTMX requests proxy correctly
   - WebSocket headers pass through
   - Image uploads work (client_max_body_size 0)

3. Update configs if any issues found

4. Document setup steps in README

### Step 5: OpenAPI Spec (#33)

1. Write OpenAPI 3.1 spec in `docs/openapi.yaml`:
   - All API endpoints with request/response schemas
   - Authentication documentation (session cookie + Bearer token)
   - Error response schemas
   - Example values

2. Render documentation:
   - Serve at `/api/docs` using Scalar (modern, lightweight)
   - Or embed Swagger UI if Scalar integration is complex

3. Complete Bruno collections:
   - Ensure every endpoint has a Bruno request
   - Add test assertions for response structure

### Step 6: Security Audit (#34)

1. Encryption review:
   - Verify all API keys encrypted in database
   - Verify encryption key derivation is secure
   - Check for plaintext secrets in logs or error messages

2. Log scrubbing review:
   - Test that all sensitive parameters are redacted
   - Check structured log fields for leaks
   - Verify scrubbing works in both JSON and text log formats

3. Input validation:
   - All API endpoints validate request bodies
   - SQL injection prevention (parameterized queries)
   - XSS prevention (Templ auto-escapes, but verify)
   - Path traversal prevention in file operations
   - SSRF prevention in URL fetch (restrict to public IPs)

4. CSRF review:
   - Verify CSRF token required on all state-changing requests
   - Test CSRF validation works with HTMX

5. Auth review:
   - Session expiry enforced
   - Rate limiting on login endpoint
   - Password requirements enforced

6. Container security:
   - PUID/PGID correctly applied
   - No unnecessary capabilities
   - Non-root execution verified

### Step 7: Database Backup (#45)

1. Implement SQLite backup using Online Backup API:
   - Manual backup trigger via API endpoint and UI button
   - Scheduled backups (configurable interval, e.g., daily)
   - Backup to configurable path (default: within /data volume)
   - Retention policy (keep N most recent backups)

2. API endpoints:
   - `POST /api/v1/settings/backup` -- trigger manual backup
   - `GET /api/v1/settings/backup/history` -- list backup history

3. Settings UI section for backup configuration

### Step 8: Offline Credential Reset (#46)

1. Add `reset-credentials` subcommand to `cmd/stillwater/main.go`:
   - Can be run when the application is stopped
   - Clears all encrypted API keys from the database
   - Forces credential re-entry on next startup

2. Encryption key improvements:
   - Auto-generate encryption key on first run and store in /data volume
   - Prominent warning in setup UI about backing up the encryption key

### Step 9: README and Documentation

1. Write comprehensive README.md:
   - Project description and screenshots
   - Quick start (docker-compose)
   - Configuration reference
   - Reverse proxy setup (SWAG, Traefik, Caddy)
   - Unraid setup
   - API documentation link
   - Development setup
   - Contributing guidelines
   - License
   - Acknowledgements (Bliss, MediaElch, Album Art Finder)

## Key Design Decisions

- **Scalar over Swagger UI:** Scalar is a modern, lightweight API documentation renderer. If integration is complex, fall back to Swagger UI.
- **Security audit as final step:** Easier to audit the complete codebase once rather than incrementally. However, security best practices are followed throughout development.
- **README is part of the release:** A good README is essential for open-source adoption and self-hosted users.
- **Database backup via SQLite Online Backup API:** Provides consistent snapshots even while the application is running.
- **Offline credential reset:** Provides a recovery path when the encryption key is lost. Clears encrypted credentials so the user can re-enter them after setting a new key.

## Verification

- [ ] All pages usable on mobile (320px viewport)
- [ ] Logo renders correctly in dark and light themes
- [ ] Favicon appears in browser tab
- [ ] Unraid CA template validates
- [ ] SWAG subdomain and subfolder configs work
- [ ] OpenAPI spec is complete and valid
- [ ] API docs render at /api/docs
- [ ] Security audit checklist all green
- [ ] No secrets in logs
- [ ] README is comprehensive
- [ ] Docker image builds and runs cleanly
- [ ] `make test` and `make lint` pass
- [ ] Tag v0.8.0 and push to GHCR
