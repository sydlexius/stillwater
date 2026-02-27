# Milestone 20 -- Settings Infrastructure

## Goal

Deliver database maintenance controls (optimize/vacuum/scheduling), runtime logging configuration with hot-reload, and encrypted settings export/import -- all accessible via the Settings page UI and REST API.

## Acceptance Criteria

- [x] Database maintenance status (file sizes, page count, last optimize time) displayed in Settings > Maintenance
- [x] "Optimize Now" button runs PRAGMA optimize + WAL checkpoint
- [x] "Vacuum" button runs VACUUM with confirmation warning
- [x] Maintenance scheduler runs PRAGMA optimize on a configurable interval (default: daily)
- [x] Log level dropdown (debug/info/warn/error) with immediate hot-reload
- [x] Log format toggle (text/json) with immediate hot-reload
- [x] Optional file output path with lumberjack rotation (max size, max files, max age)
- [x] Logging settings persisted to DB and survive restarts
- [x] Settings export downloads encrypted JSON containing all provider keys, connections, platform profiles, webhooks, priorities
- [x] Settings import accepts encrypted JSON, validates, and upserts into DB
- [x] Export/Import UI buttons on Settings > Maintenance tab

## Dependency Map

```
#205 (Logging Config)   -- independent
#204 (DB Maintenance)   -- independent
#206 (Settings Export)  -- depends on #204/#205 for completeness
```

## Checklist

### Issue #205 -- Logging configuration with runtime hot-reload
- [x] `internal/logging/logging.go` -- SwappableHandler, Manager, Config, Reconfigure
- [x] `internal/logging/logging_test.go` -- level swap, format swap, file output, close
- [x] `internal/api/handlers_logging.go` -- GET/PUT `/api/v1/settings/logging`
- [x] `cmd/stillwater/main.go` -- replace inline logging with Manager, load DB-persisted settings
- [x] `internal/api/router.go` -- add LogManager to deps, register routes
- [x] `web/templates/settings.templ` -- logging config section in General tab
- [x] `go.mod` -- add `gopkg.in/natefinch/lumberjack.v2`
- [x] Tests passing

### Issue #204 -- Database maintenance UI
- [x] `internal/maintenance/maintenance.go` -- Service with Status, Optimize, Vacuum, StartScheduler
- [x] `internal/maintenance/maintenance_test.go` -- unit tests
- [x] `internal/api/handlers_maintenance.go` -- status/optimize/vacuum/schedule handlers
- [x] `cmd/stillwater/main.go` -- create maintenance service, start scheduler
- [x] `internal/api/router.go` -- add MaintenanceService to deps, register 4 routes
- [x] `web/templates/settings.templ` -- DB maintenance card in Maintenance tab
- [x] Tests passing

### Issue #206 -- Settings export/import
- [x] `internal/settingsio/export.go` -- Envelope, Payload, Service with Export/Import
- [x] `internal/settingsio/export_test.go` -- round-trip, corrupted data, upsert behavior
- [x] `internal/api/handlers_settings_io.go` -- GET export, POST import handlers
- [x] `cmd/stillwater/main.go` -- create settingsio service
- [x] `internal/api/router.go` -- add SettingsIOService to deps, register 2 routes
- [x] `web/templates/settings.templ` -- export/import card in Maintenance tab
- [x] Tests passing

## Notes

- lumberjack package: `gopkg.in/natefinch/lumberjack.v2` (not `natefinish`)
- Race detector requires CGO, not available with pure Go SQLite
- SwappableHandler uses `atomic.Pointer[slog.Handler]` for thread-safe hot-swap
- Maintenance scheduler reads interval from DB settings at startup; schedule changes require restart
- Export/import switched from instance encryption key to passphrase-based PBKDF2+AES-256-GCM; files are portable across instances
- Added `GetByName` to platform.Service and `GetByNameAndURL` to webhook.Service for import upsert matching
