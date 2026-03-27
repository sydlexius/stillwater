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
internal/artist/      - Artist domain model, service, and repository interfaces
internal/auth/        - Authentication (session-based)
internal/config/      - Configuration loading (env + YAML)
internal/database/    - SQLite database and migrations
internal/dbutil/      - Shared database helpers (type conversions, nullable handling)
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
make test-race      # Run tests with race detector via WSL2 (requires CGO)
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
- **`[effort: high]`** - Enable extended thinking. Use for complex architectural decisions, multi-system design, ambiguous requirements, or security-sensitive analysis.
- **`[effort: medium]`** - Standard reasoning. Default for most feature and bug work.
- **`[effort: low]`** - Minimal reasoning. Use for simple, fully-specified tasks with no ambiguity (config tweaks, typo fixes, docs).

If no hint is present, default to: Sonnet + Plan Mode + medium effort for new features, Sonnet + direct + medium effort for bug fixes, Haiku + direct + low effort for documentation-only changes.

When a model or effort hint differs from the current session state:

- **Model mismatch:** Pause before starting work and ask the user to switch. Example: "This issue requests `[model: opus]`. Please run `/model claude-opus-4-6` before I proceed."
- **Effort high:** If extended thinking is not yet enabled, pause and ask. Example: "This issue requests `[effort: high]`. Please run `/think` to enable extended thinking before I proceed."
- **Effort medium or low:** Do not pause. Acknowledge the requested effort level in your first reply and adjust reasoning depth accordingly -- more thorough for medium, concise and direct for low.

Do not start implementation until the user confirms or explicitly waives any pause-required hint (model mismatch or effort high).

### Creating Issues via CLI

When creating a GitHub issue with `gh issue create`, do NOT write a freeform body:

1. Pick the right template from `.github/ISSUE_TEMPLATE/`: `feature.md` (new feature),
   `bug.md` (defect), or `task.md` (chore/cleanup).
2. Read the template and fill in every section, including the `[mode:]`, `[model:]`, and
   `[effort:]` hints at the top.
3. Write the populated body to a file using the Write tool.
4. Create the issue -- always pass `--title` and `--label` to avoid interactive prompts:
   ```sh
   gh issue create --title "<title>" --body-file <path> --label <label>
   ```
5. Delete the file after the issue is created.

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

### Race Detector (WSL2)

The Go race detector requires CGO (`CGO_ENABLED=1`), which is not available in the Windows/MSYS2 development environment. Use `make test-race` to run the race detector through WSL2:

```bash
make test-race
```

This converts the current directory path from MSYS2 format (`/d/Dev/...`) to WSL2 format (`/mnt/d/Dev/...`) and runs `go test -race` inside WSL2.

**WSL2 prerequisites:**

1. Install a WSL2 distro (Ubuntu recommended): `wsl --install`
2. Inside WSL2, install Go (matching the project's Go version):
   ```bash
   # Example for Go 1.24 -- adjust version as needed
   wget https://go.dev/dl/go1.24.linux-amd64.tar.gz
   sudo tar -C /usr/local -xzf go1.24.linux-amd64.tar.gz
   echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
   source ~/.bashrc
   ```
3. Install GCC (required by the race detector):
   ```bash
   sudo apt update && sudo apt install -y gcc
   ```
4. Verify: `wsl -e bash -c "go version && gcc --version"`

**Note:** Cross-filesystem access between Windows (`/mnt/d/`) and WSL2 is slower than native Linux I/O. For large test suites, consider cloning the repo natively inside WSL2 (`~/Dev/stillwater`) and running tests there directly.

### When to run the race detector

Run `make test-race` (or `go test -race ./...` in WSL2) when changes touch:

- Concurrency primitives or shared state: goroutines, mutexes, channels, `sync.WaitGroup`, shared maps, context cancellation
- Background workers: watcher, scanner, webhook dispatcher, event bus, provider orchestrator
- Code that runs in multiple goroutines or is called from both HTTP handlers and background jobs

Skip it for single-threaded code paths:

- Template changes (`.templ` files)
- Purely local API handler request/response logic that only uses per-request data and does not access shared state or start goroutines
- Config parsing, NFO read/write, database migrations
- CSS, JS, documentation

**Note:** `net/http` serves handlers concurrently. If a handler reads or writes shared state (package-level variables, caches, singletons, in-memory indexes, etc.), treat it as concurrent code and run the race detector.

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
- Repository pattern for data access via interfaces in `internal/artist/repository.go`
- Artist data is normalized across dedicated tables:
  - `artists` -- core artist/composer records (name, type, gender, biography, health score)
  - `artist_provider_ids` -- provider identity mappings (MusicBrainz, AudioDB, Discogs, etc.)
  - `artist_images` -- per-slot image metadata (exists, low_res, placeholder, dimensions, phash)
  - `artist_aliases` -- alternative names for search and deduplication
  - `artist_platform_ids` -- Emby/Jellyfin/Lidarr platform-specific ID mappings
  - `band_members` -- members of bands/groups with instruments and tenure
  - `nfo_snapshots` -- historical NFO file versions for undo/restore
- Shared database helpers (type conversions, nullable handling) in `internal/dbutil/`
- All artist-related tables use `ON DELETE CASCADE` on `artist_id` foreign keys; `rule_violations` also cascades on `rule_id`. `DismissOrphanedViolations` is a safety net for orphaned violations when foreign keys are disabled or data is inconsistent.

## Rule Engine Internals

- Fix-all uses an in-memory progress tracker with mutex protection, not the BulkJob database pattern. Only one fix-all can run at a time (409 on concurrent starts).
- `FixResult` has three outcome states: `Fixed`, `Dismissed`, and neither (failed/skipped).
- All rules support three states: enabled toggle (on/off) + automation mode (`manual` or `auto`). The `disabled` automation mode was removed in #353.

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

### setupdocker.sh

A local helper script `setupdocker.sh` automates the full stop/build/start cycle. It is gitignored and not committed. Use it whenever a container rebuild and reload is needed:

```bash
./setupdocker.sh
```

The script stops and removes any running `stillwater*` containers, rebuilds the image, runs `docker compose -f docker-compose.local.yml up -d`, and tails the startup logs.

## PR Workflow

**Run `/pr-review-toolkit:review-pr` locally BEFORE pushing and opening the PR -- never after.**

Copilot reviews only the diff on each push, not the whole file. Opening the PR fires Copilot
immediately; each fixup commit then exposes the next layer of issues it did not see before.
Running the local review toolkit first surfaces everything in one pass and eliminates the
whack-a-mole cycle of one-complaint-per-push.

Correct order:
1. Write code and tests
2. `go test ./...` -- all tests pass
3. `/pr-review-toolkit:review-pr` -- fix any critical or important findings
4. Commit all fixes
5. Squash all development commits into clean, coherent commits (see below)
6. Push and open the PR

Do not open the PR until steps 3-5 are complete. A PR opened with unfixed review findings will
produce exactly as many Copilot review rounds as there are findings.

### Squash before first push

Squash all development/fixup commits into clean, logical commits before the first push.
Copilot reviews only the diff it sees on each push. Incremental commits hide the full
changeset from it, causing it to rediscover issues on each push. Squashing presents the
final state once.

```bash
# Squash all commits since branching from main into one clean commit:
git rebase -i main
# In the editor: mark the first commit "pick", all others "squash" or "fixup"
```

For larger PRs with logically distinct phases (e.g., data model + API + UI), two or three
coherent commits is fine. The goal is coherence, not a single commit at all costs.

**Do not squash after opening the PR.** Copilot has already reviewed the first push.
Force-pushing a rebase after opening a PR destroys review context and resets Copilot's
diff window to the squashed commit, which may trigger a full re-review from scratch.

### Pre-push checklist

Before squashing and pushing, verify these categories that Copilot consistently flags:

**OpenAPI spec:**
- [ ] Every new or changed response field has a matching entry in `internal/api/openapi.yaml`
- [ ] OpenAPI descriptions accurately describe the invariant (not "empty when X" if the code also makes it non-empty when Y)
- [ ] Any endpoint that uses `$ref` to a shared schema actually matches that schema's shape

**Error path completeness:**
- [ ] Functions that return user-visible warnings emit a warning on ALL error paths, not just the main operation path
- [ ] Client-visible warning strings contain no raw `error.Error()` output from DB/internal services
- [ ] Full errors are logged server-side before emitting a sanitized client message

**Generated files:**
- [ ] If any `.templ` changed, `templ generate` was run and `*_templ.go` committed
- [ ] If any HTTP status code changed, `scripts/smoke.sh` and integration test assertions are updated

**SQL correctness:**
- [ ] ORDER BY on string columns that represent enums (severity, status) uses a CASE expression for correct ordering, not lexicographic sort
- [ ] Dynamic SQL query builders use whitelisted column maps, not user input

**Accessibility:**
- [ ] Interactive elements (selects, buttons, collapsible panels) have `aria-label`, `aria-expanded`, or `aria-controls` as appropriate
- [ ] Group collapse/expand buttons maintain `aria-expanded` state

**Frontend fetch calls:**
- [ ] All `fetch()` calls check `resp.ok` before parsing the response body
- [ ] Error responses show user-friendly messages, not raw JSON

**Concurrency:**
- [ ] Background goroutines use `context.WithoutCancel(reqCtx)`, never `context.Background()`, to preserve request-scoped values (gosec G118)

**Test code:**
- [ ] No unprotected shared variables written in test handler goroutines and read in the test goroutine
- [ ] `multipart.Writer` methods (`CreatePart`, `WriteField`, `Close`) errors are checked in test helpers
- [ ] `io.ReadAll(r.Body)` errors are checked before using the result in test handlers
- [ ] Engine/rule tests assert relative properties (e.g., "violations > 0") rather than exact counts that break when new rules are added

### Review comment scope policy

**Default: fix now.** When a Copilot review comment or adjacent code issue is discovered
during PR work, the default action is to fix it in the current PR. Use judgment case by
case. The burden of proof is on *deferring*, not on fixing.

**To defer, you must justify.** A fix should only be deferred to a separate issue when:
- It requires architectural changes that would fundamentally alter the PR's scope
- It touches subsystems unrelated to the PR's purpose AND requires its own test suite
- It would add a new database migration or breaking API change unrelated to the feature

**Never reply "out of scope" without creating a tracking issue.** If you defer, open an
issue immediately with the `task` template, link it to the current PR, and reply to
the review comment with the issue number.

### Reading PR comments (gh API)

The `!` character triggers bash history expansion, even inside double quotes. This breaks
`--jq` filters that use `!=`. Always use one of these safe patterns:

```bash
# List all PR review comments (safe -- no != operator):
gh api "repos/{owner}/{repo}/pulls/{number}/comments" --paginate \
  --jq '[.[] | select(.body | length > 0) | {id, user: .user.login, path, line, body}]'

# Filter out a specific user (use "== X | not" instead of "!= X"):
gh api "repos/{owner}/{repo}/pulls/{number}/comments" --paginate \
  --jq '[.[] | select(.user.login == "some-bot" | not) | {id, user: .user.login, body}]'

# Reply to a review comment:
gh api "repos/{owner}/{repo}/pulls/{number}/comments/{comment_id}/replies" \
  -f body='Fixed in <commit>.'
```

**Never use `!=` in `--jq` expressions from bash.** Use `select(.field == "value" | not)`.

### Copilot review policy

Copilot's automatic re-review on push is **disabled** (`review_on_push: false` in the
repository ruleset). Copilot reviews only the initial PR diff. This eliminates the 61%
repeat noise rate observed in later-round reviews.

Re-review must be triggered manually by the user from the GitHub PR page when warranted.
The GitHub API does not support re-requesting review from bot accounts (422 error).

The `/handle-review` and `/review-stack` skills assess whether a manual Copilot re-review
is recommended after pushing fixes, based on the scope of changes.

### Copilot instruction files

Global instructions are in `.github/copilot-instructions.md` (must stay under 4,000
characters -- Copilot silently ignores content beyond that limit). Domain-specific
guidance is in path-scoped files under `.github/instructions/`:

- `go-api.instructions.md` -- API handlers: OpenAPI semantic review, error paths, concurrency
- `go-tests.instructions.md` -- Test code: data races, multipart errors, assertion quality
- `ci-actions.instructions.md` -- GitHub Actions: version pinning, smoke test alignment

## Parallel Work (Worktrees)

When multiple issues or agents need to work concurrently, use git worktrees to isolate
each unit of work. Never have two agents sharing the same working directory.

### Naming Convention

```
D:\Dev\Repos\stillwater\              # main repo, main branch (coordination only)
D:\Dev\Repos\stillwater-{issue}\      # single-issue worktree
D:\Dev\Repos\stillwater-m{N}\         # milestone umbrella worktree (plan file, coordination)
D:\Dev\Repos\stillwater-m{N}-{issue}\ # milestone sub-issue worktree
```

Branch naming follows existing convention:
- Features: `feat/{issue}-short-desc`
- Fixes: `fix/{issue}-short-desc`
- Milestone umbrella: `feat/m{N}-umbrella`

### Creating a Worktree

```bash
# From the main repo directory:
git worktree add -b feat/315-musicbrainz-mirror ../stillwater-315 main

# For a milestone sub-issue branching from the umbrella:
git worktree add -b feat/320-short-desc ../stillwater-m17-320 feat/m17-umbrella
```

### Tracking

Active worktrees are tracked in `memory/worktrees.md` inside the Claude Code
auto-memory directory (`~/.claude/projects/<project>/memory/`), not in the repo.
Every session that creates or destroys a worktree must update that file.

### Docker UAT in Worktrees

`setupdocker.sh` lives in the main repo root and is not duplicated into worktrees.
To run UAT from a worktree, either:
- Copy the script into the worktree, or
- Run it from the main repo after checking out the worktree's branch there temporarily

### Parallel Rule PRs

Multiple rule PRs (new checkers, fixers, default rule entries) will conflict on merge
because they all modify the same files: `engine.go` (checker registration), `service.go`
(constants + defaults), `checkers.go` (checker functions), and `engine_test.go`.

When developing multiple rules in parallel worktrees:
- Merge them sequentially, not simultaneously
- The second PR to merge will need a rebase to resolve conflicts
- After rebase, re-run `go test ./internal/rule/...` before pushing
- Engine tests use relative assertions (not exact counts) so new rules
  do not break existing tests, but verify the rebase did not drop code

### Cleanup

After a PR is merged:
1. Remove the worktree: `git worktree remove ../stillwater-{issue}`
2. Delete the local branch: `git branch -d feat/{issue}-short-desc`
3. Delete the remote branch: `encoded=$(printf '%s' "feat/{issue}-short-desc" | jq -sRr @uri) && gh api "repos/{owner}/{repo}/git/refs/heads/$encoded" -X DELETE`
4. Update `memory/worktrees.md`

## Milestone Work Protocol

When asked to work on a milestone (e.g. "implement Milestone 14"), follow this process:

### 1. Scope Assessment

Before writing any code:
- Read the umbrella issue and all sub-issues on GitHub.
- Identify the dependency order among sub-issues.
- Note any `[mode:]` and `[model:]` hints in issue bodies.
- Check the current state of `main` and any in-progress branches.
- Check `memory/worktrees.md` for any active worktrees that might overlap.

### 2. Plan File

Create `docs/plans/m<N>-plan.md` (e.g. `docs/plans/m14-plan.md`) before starting. The plan file must include:
- Milestone goal and acceptance criteria (summarised from the umbrella issue)
- Sub-issue dependency map (which issues block which)
- A checklist for every sub-issue: use `- [ ]` for pending, `- [x]` for done
- A notes/observations section for decisions, blockers, and findings discovered during work
- The UAT and merge order (which issues are implemented/merged first, which are stacked)

**Do NOT include PR numbers in plan files.** Referencing PR numbers forces a
commit-then-update cycle every time a PR is created, which wastes time and
resources. Track issues by number only; PR linkage lives in GitHub, not in the
plan file.

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
- [ ] PR merged

### Issue #Y -- <title>
...

## Worktrees
| Directory              | Branch              | Issue | Status  |
|------------------------|---------------------|-------|---------|
| stillwater-m{N}-{issue}| feat/{issue}-desc   | #X    | pending |

## UAT / Merge Order
1. #X (base: main)
2. #Y (stacked on #X)

## Notes
- <date>: <observation or decision>
```

### 3. During Work

- Create a worktree for each sub-issue before starting code (see "Parallel Work" above).
- Update the plan file checklist and worktree table as work progresses.
- Update `memory/worktrees.md` whenever a worktree is created or removed.
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
3. Remove all worktrees: run `git worktree list` and `git worktree remove <path>` for each matching worktree (do not use glob patterns -- they are not reliably expanded by all shells).
4. Delete all merged feature branches (remote and local): `encoded=$(printf '%s' "<branch>" | jq -sRr @uri) && gh api "repos/.../git/refs/heads/$encoded" -X DELETE` then `git branch -d`.
5. Run `git fetch --prune` to remove stale tracking refs.
6. Delete the plan file: `git rm docs/plans/m<N>-plan.md` and commit directly to `main`.
7. Update `memory/worktrees.md` to move entries to "Completed" or remove them.

## CI/CD

### GitHub Actions pinning

All GitHub Actions are pinned to commit SHAs (not version tags) for supply chain security.
The original version tag is kept as an inline comment for maintainability:

```yaml
uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd # v6
```

When updating an action version, look up the new SHA:
```bash
gh api repos/<owner>/<action>/git/ref/tags/<version> --jq '.object.sha'
```

### Docs-only PRs

The CI workflow uses `dorny/paths-filter` to detect code changes. Docs-only PRs
skip Go build/test/lint but still report green required status checks (skipped jobs).
A separate `docs.yml` workflow runs typos and markdownlint on markdown changes.

**Do not use `paths-ignore`** on workflow triggers when required status checks exist.
GitHub treats missing checks as "not satisfied" and blocks the merge.

### Helper scripts

- `~/.claude/scripts/pr-unreplied-comments.sh [--wait] [--count-only] <PR>` -- detect unreplied bot review comments. Use `--wait` after pushing to poll until bot reviews stabilize. Output includes `commit` and `stale` fields for stale-diff detection.

## License

GPL-3.0
