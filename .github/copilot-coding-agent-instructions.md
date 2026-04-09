# Copilot Coding Agent Instructions

## Build, test, and lint

The setup-steps workflow installs the full toolchain (Go, templ, tailwindcss,
golangci-lint). Use Make targets:

- `make build` -- generate templ + tailwind, build binary. Run after changes.
- `make test` -- all tests with race detector
- `make lint` -- golangci-lint
- `make fmt` -- format Go + Templ files
- `make check-openapi` -- OpenAPI spec vs handler consistency

Scripts in `scripts/`:
- `pre-push-gate.sh` -- tests + OpenAPI + generated-file check. Run before PR.
- `check-generated.sh` -- verifies `*_templ.go` freshness.

## GitHub CLI (gh)

The agent has `gh` with repo-scoped write access to contents, PRs, and issues.
Use for reading issue details, PR comments, and creating PRs.

Caveat: `!` triggers bash history expansion in double quotes. Never use `!=`
in `--jq` expressions; use `select(.field == "value" | not)` instead.

## PR workflow

1. Feature branch only (never commit to main).
2. Run `bash scripts/pre-push-gate.sh` before opening PR.
3. Squash into clean commits before first push.
4. PR body must include `Closes #N` for addressed issues.

## Architecture

```
cmd/stillwater/       - Entry point
internal/api/         - Handlers, middleware, SSE, OpenAPI spec
internal/artist/      - Domain model, service, repository interfaces
internal/database/    - SQLite, migrations (single file: 001_initial_schema.sql)
internal/config/      - Config loading (env + YAML)
internal/connection/  - Platform connections (Emby, Jellyfin, Lidarr)
internal/rule/        - Rule engine (disabled/manual/auto modes)
internal/provider/    - Metadata adapters (MusicBrainz, Fanart.tv)
internal/publish/     - Publisher for NFO and platform writes
web/templates/        - Templ templates (.templ)
web/static/           - CSS, vendored JS
```

## Key rules

- Never create new migration files -- modify `001_initial_schema.sql`.
- All rules must support three modes: disabled, manual, auto.
- Generated `*_templ.go` must be committed alongside `.templ` changes.
- OpenAPI spec (`internal/api/openapi.yaml`) must match handler implementations.
- Background goroutines use `context.WithoutCancel(reqCtx)`, not `context.Background()`.
- No raw `error.Error()` in client-visible messages; log full errors server-side.

## Go conventions

- `fmt.Errorf("...: %w", err)` for wrapping; `slog` not `log`
- Table-driven tests with `t.Run`; `context.Context` as first param
- Doc comments on all exported functions and types
- Pure Go SQLite (`modernc.org/sqlite`) -- no CGO
