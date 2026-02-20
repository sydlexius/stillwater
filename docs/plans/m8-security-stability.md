# M8: Security and Stability (v0.8.0)

## Goal

Security audit, database backup, offline credential reset, and TheAudioDB provider bug fix. Harden the application before public release.

## Prerequisites

- All prior milestones (M1-M7) complete

## Issues

| # | Title | Mode | Model |
|---|-------|------|-------|
| 34 | Security audit: encryption, log scrubbing, input validation | plan | opus |
| 45 | Database backup: scheduled and manual | plan | sonnet |
| 46 | Offline credential reset CLI subcommand | direct | sonnet |
| 58 | TheAudioDB: default to free V1 key and fix broken API guide link | direct | sonnet |

## Implementation Order

### Step 1: TheAudioDB V1/V2 Fix (#58)

Quick bug fix, no dependencies.

1. Update `internal/provider/audiodb/audiodb.go`:
   - Add default free key constant (key `"2"` for V1)
   - Change `RequiresAuth()` to return `false` (free key is built-in)
   - Switch base URL between V1 (`/api/v1/json/`) and V2 (`/api/v2/json/`) based on whether the user has configured a custom key
   - If no user key configured, use free key + V1 endpoint
   - If user key configured, use user key + V2 endpoint

2. Update `internal/provider/audiodb/audiodb_test.go`:
   - Test default key behavior (no config = V1 + free key)
   - Test custom key behavior (user key = V2)

3. Update `web/templates/settings.templ`:
   - Fix `providerHelpURL` for `NameAudioDB` to point to working URL (Patreon page)
   - Update help text to indicate the provider works without a key but a paid key improves results

### Step 2: Offline Credential Reset (#46)

1. Add `reset-credentials` subcommand to `cmd/stillwater/main.go`:
   - Parse `os.Args` for `reset-credentials` before starting the server
   - Open the database directly (no server startup)
   - Clear all rows from `settings` where key contains API keys
   - Clear the `api_key` column in `connections` table
   - Clear the admin password hash to force re-setup
   - Print confirmation message and exit

2. Auto-generate encryption key on first run:
   - In `cmd/stillwater/main.go`, if `SW_ENCRYPTION_KEY` is not set, check for `/data/encryption.key`
   - If neither exists, generate a random 32-byte key, base64-encode, write to `/data/encryption.key`
   - Load from file if env var not set
   - Log a warning on first generation telling the user to back up the key

3. Update `build/docker/entrypoint.sh`:
   - Support `reset-credentials` as a container command: `docker exec stillwater ./stillwater reset-credentials`

### Step 3: Database Backup (#45)

1. Create `internal/backup/` package:
   - `Service` struct with `*sql.DB`, backup path, retention count, `*slog.Logger`
   - `Backup(ctx) (string, error)` -- uses SQLite Online Backup API (`VACUUM INTO`)
   - Backup filename format: `stillwater-YYYYMMDD-HHMMSS.db`
   - `ListBackups(ctx) ([]BackupInfo, error)` -- list backup files with size and timestamp
   - `Prune(ctx) error` -- delete oldest backups exceeding retention limit

2. Add scheduled backup support:
   - `StartScheduler(ctx, interval)` -- runs backup on configurable interval
   - Default: daily. Configurable via `settings` table
   - Stop on context cancellation

3. API endpoints:
   - `POST /api/v1/settings/backup` -- trigger manual backup
   - `GET /api/v1/settings/backup/history` -- list backup history with download links
   - `GET /api/v1/settings/backup/{filename}` -- download a backup file

4. UI integration:
   - Add "Database Backup" section in settings page
   - Manual backup button with spinner
   - Backup history table (date, size, download link)
   - Retention and schedule settings

5. Wire in `cmd/stillwater/main.go`:
   - Create backup service with configured path and retention
   - Start scheduler if enabled
   - Add to router deps

### Step 4: Security Audit (#34)

Systematic review of the entire codebase.

1. Encryption review:
   - Verify all API keys in `connections` table are encrypted via `encryption.Encryptor`
   - Verify provider API keys in `settings` table are encrypted
   - Check encryption key derivation is secure (AES-256-GCM with random nonce)
   - Confirm no plaintext secrets in logs or error messages

2. Log scrubbing:
   - Audit all `slog` calls for sensitive field leaks
   - Add scrubbing middleware if not already present (redact `api_key`, `password`, `token`, `secret` fields)
   - Test scrubbing in both JSON and text log formats

3. Input validation:
   - Audit all API handlers for request body validation
   - Verify parameterized queries throughout (no string concatenation in SQL)
   - Check for XSS vectors (templ auto-escapes, but verify raw HTML usage)
   - Check for path traversal in file operations (library path, NFO path, image path)
   - SSRF prevention: validate URLs in connection and webhook creation (block private IPs)

4. CSRF review:
   - Verify CSRF middleware covers all state-changing routes
   - Test CSRF validation works correctly with HTMX requests

5. Auth hardening:
   - Verify session expiry is enforced
   - Add rate limiting on `/api/v1/auth/login` (e.g., 5 attempts per minute per IP)
   - Verify password is properly hashed (SHA-256 pre-hash + bcrypt, already implemented)

6. Container security:
   - Verify PUID/PGID correctly applied in entrypoint
   - Confirm non-root execution
   - Review for unnecessary capabilities

## Key Design Decisions

- **TheAudioDB free key first:** Works out of the box. Paid key upgrades to V2 automatically. No user action needed for basic functionality.
- **Credential reset is offline-only:** Prevents accidental credential wipe while the app is running. Must be run via `docker exec` or stopped container.
- **SQLite VACUUM INTO for backup:** Provides a consistent snapshot without locking the running database. Simpler than the C backup API and works with pure-Go SQLite.
- **Security audit as a dedicated step:** Review the complete codebase systematically rather than spot-checking. Document findings as checklist items.

## Verification

- [ ] TheAudioDB works without any user-configured key
- [ ] TheAudioDB switches to V2 when user provides a paid key
- [ ] `stillwater reset-credentials` clears all stored secrets
- [ ] Encryption key auto-generates on first run
- [ ] Manual backup creates valid SQLite database file
- [ ] Scheduled backup runs at configured interval
- [ ] Backup retention prunes old files
- [ ] No secrets appear in log output
- [ ] All API inputs validated
- [ ] CSRF protection verified
- [ ] Rate limiting on login endpoint
- [ ] `go test ./...` and `golangci-lint run` pass
- [ ] Tag v0.8.0
