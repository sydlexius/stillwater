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

### setupdocker.ps1

A local helper script `setupdocker.ps1` automates the full stop/build/start cycle. It is gitignored and not committed. Use it whenever a container rebuild and reload is needed:

```powershell
.\setupdocker.ps1
```

Run from PowerShell, not Git Bash -- MSYS2 path conversion in Git Bash breaks Docker volume mounts on Windows. The script stops and removes any running `stillwater*` containers, rebuilds the image, runs `docker compose -f docker-compose.local.yml up -d`, and tails the startup logs.

## Milestone Work Protocol

When asked to work on a milestone (e.g. "implement Milestone 14"), follow this process:

### 1. Scope Assessment

Before writing any code:
- Read the umbrella issue and all sub-issues on GitHub.
- Identify the dependency order among sub-issues.
- Note any `[mode:]` and `[model:]` hints in issue bodies.
- Check the current state of `main` and any in-progress branches.

### 2. Plan File

Create `docs/plans/m<N>-plan.md` (e.g. `docs/plans/m14-plan.md`) before starting. The plan file must include:
- Milestone goal and acceptance criteria (summarised from the umbrella issue)
- Sub-issue dependency map (which issues block which)
- A checklist for every sub-issue and every PR: use `- [ ]` for pending, `- [x]` for done
- A notes/observations section for decisions, blockers, and findings discovered during work
- The UAT and merge order (which PRs land first, which are stacked)

Commit the plan file to `main` before opening any feature branches so it survives context resets.

Example structure:

```markdown
# Milestone N -- <Title>

## Goal
<one-paragraph summary>

## Acceptance Criteria
- [ ] criterion one
- [ ] criterion two

## Dependency Map
#X --> #Y --> #Z
#W (parallel)

## Checklist
### Issue #X -- <title>
- [ ] Implementation
- [ ] Tests
- [ ] PR opened (#?)
- [ ] CI passing
- [ ] PR merged

### Issue #Y -- <title>
...

## UAT / Merge Order
1. PR #? (base: main)
2. PR #? (stacked on #?)

## Notes
- <date>: <observation or decision>
```

### 3. During Work

- Update the plan file checklist as work progresses (tick items, add notes).
- Run `gofmt -d` and `go test ./...` before every commit. Do not push code that fails either.
- Use `docker-compose.local.yml` for UAT builds whenever the PR/check cycle warrants a container test.
- After addressing PR review feedback, update the relevant checklist items.

### 4. Documentation Updates

When any change (milestone work, bug fix, or standalone request) touches user-facing behavior, check whether it affects existing documentation and update accordingly. This applies to all work, not just milestones.

**When to update docs:**
- The change alters UI layout, navigation, or workflows described in the user guide or wiki
- The change adds a new feature, setting, or page that users need to know about
- The change renames, moves, or removes something that existing docs reference
- The change introduces a concept or behavior that would not be self-evident to a user

**What to update:**
- In-app guide (`web/templates/guide.templ` and `/guide` route) -- once it exists
- GitHub wiki pages -- the wiki is a separate repo (clone it alongside the main repo, for example `../stillwater.wiki/`), push directly to `master`:
  - [Architecture](https://github.com/sydlexius/stillwater/wiki/Architecture) -- if the change adds, removes, or modifies a subsystem, provider, event type, middleware, or core interface
  - [Contributing](https://github.com/sydlexius/stillwater/wiki/Contributing) -- if the change alters linting rules, pre-commit hooks, test patterns, commit conventions, or the PR process
  - [Developer Guide](https://github.com/sydlexius/stillwater/wiki/Developer-Guide) -- if the change adds a new top-level package, changes the tech stack, or modifies a design principle
  - User-facing wiki pages (User Guide, Configuration, Installation, etc.) -- if the change alters UI, settings, or setup steps
- OOBE step content if onboarding references the changed behavior
- CLAUDE.md if the change affects architecture, commands, or conventions

**How:**
- Documentation changes ship in the same PR as the code change, not as a follow-up
- In the PR description, note which doc pages were updated and why
- If a doc page does not exist yet but the change would warrant one, note that in the PR description so it can be tracked
- Wiki updates are pushed separately (wiki is a different git repo) but should be done as part of the same PR workflow -- push wiki changes before or after the code PR merges, not as a forgotten follow-up

**Wiki update checklist (evaluate for every PR):**
- [ ] Does this PR add, remove, or change a provider, event type, or core interface? Update [Architecture](https://github.com/sydlexius/stillwater/wiki/Architecture)
- [ ] Does this PR change linting, hooks, test patterns, or contribution workflow? Update [Contributing](https://github.com/sydlexius/stillwater/wiki/Contributing)
- [ ] Does this PR add a package, change the tech stack, or alter a design principle? Update [Developer Guide](https://github.com/sydlexius/stillwater/wiki/Developer-Guide)
- [ ] Does this PR change user-facing behavior, settings, or setup? Update the relevant user-facing wiki page

**During milestone planning:**
- The plan file should list which wiki/guide pages are affected by each sub-issue
- The milestone checklist should include a `- [ ] Docs updated` item for any issue that touches user-facing behavior

### 5. Cleanup (After All PRs Are Merged)

Once every sub-issue PR is merged to `main`:
1. Post findings comments to all research/analysis issues and close them.
2. Post a summary comment to the umbrella issue and close it.
3. Delete all merged feature branches (remote and local): `gh api repos/.../git/refs/heads/<branch> -X DELETE` then `git branch -d`.
4. Run `git fetch --prune` to remove stale tracking refs.
5. Delete the plan file: `git rm docs/plans/m<N>-plan.md` and commit directly to `main`.

## License

GPL-3.0
