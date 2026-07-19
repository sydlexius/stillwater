# Development Setup

Prerequisites and instructions for building Stillwater from source.

## Required Tools

| Tool | Version | Purpose | Install |
|------|---------|---------|---------|
| Go | 1.26.5+ | Compiler and runtime | https://go.dev/dl/ |
| templ | pinned in `go.mod` (`tool` directive) | HTML template code generation | No separate install; run via `go tool templ generate` |
| Tailwind CSS | see `ARG TAILWIND_VERSION` in [`build/docker/Dockerfile`](https://github.com/sydlexius/stillwater/blob/main/build/docker/Dockerfile) | CSS build (standalone CLI) | See below |
| Git | any | Version control | https://git-scm.com/ |

### Optional Tools

| Tool | Purpose | Install |
|------|---------|---------|
| Docker | Container builds and testing | https://docs.docker.com/get-docker/ |
| golangci-lint | Linting (`make lint`) | https://golangci-lint.run/welcome/install/ |
| Bruno | API testing (collections in `api/bruno/`) | https://www.usebruno.com/ |
| air | Hot reload during development (`make dev`) | `go install github.com/air-verse/air@latest` |
| mermaid-cli (mmdc) | Validate Mermaid diagrams in docs (pre-commit mermaid check) | `brew install mermaid-cli` (or `npm install -g @mermaid-js/mermaid-cli`) |
| hadolint | Lint Dockerfiles (`make hadolint`, pre-commit hadolint check) | `brew install hadolint` |
| markdownlint-cli2 | Lint Markdown (pre-commit markdownlint check; CI Docs job) | `brew install markdownlint-cli2` (or `npx markdownlint-cli2`) |
| prose-tooling | Grammar/prose lint on staged Markdown/text (pre-commit prose-lint check) | Local-only, central config repo at `~/Developer/prose-tooling` (not a stillwater dependency); see its README for the LanguageTool server + `.venv` setup. Optional -- the hook skips gracefully if it is not present. Set `PROSE_TOOLING_DIR` to override the default checkout location. |

## Installing Tailwind CSS Standalone CLI

The project uses the Tailwind CSS standalone CLI (no Node.js required).
The canonical version is `ARG TAILWIND_VERSION` in
[`build/docker/Dockerfile`](https://github.com/sydlexius/stillwater/blob/main/build/docker/Dockerfile).
Download the matching binary for your platform from the GitHub releases page:

https://github.com/tailwindlabs/tailwindcss/releases

Replace `<VERSION>` in the commands below with the value of `TAILWIND_VERSION`
from the Dockerfile (for example `v4.2.0`).

**Linux:**
```bash
curl -sLo tailwindcss https://github.com/tailwindlabs/tailwindcss/releases/download/<VERSION>/tailwindcss-linux-x64
chmod +x tailwindcss
sudo mv tailwindcss /usr/local/bin/
```

**macOS (Apple Silicon):**
```bash
curl -sLo tailwindcss https://github.com/tailwindlabs/tailwindcss/releases/download/<VERSION>/tailwindcss-macos-arm64
chmod +x tailwindcss
sudo mv tailwindcss /usr/local/bin/
```

**Windows:**
```powershell
curl -Lo tailwindcss.exe https://github.com/tailwindlabs/tailwindcss/releases/download/<VERSION>/tailwindcss-windows-x64.exe
# Move to a directory on your PATH, or keep in the repo root (it is gitignored)
```

## Clone and Build

```bash
git clone https://github.com/sydlexius/stillwater.git
cd stillwater

# Install Go dependencies
go mod download

# Generate templ code (converts .templ files to Go code)
# templ is pinned via the go.mod tool directive; invoke it with `go tool`.
go tool templ generate

# Build Tailwind CSS
tailwindcss -i web/static/css/input.css -o web/static/css/styles.css --minify

# Build the binary
go build -o stillwater ./cmd/stillwater
```

## Quick Start with Make

If `make` is available, it wraps the common build, test, and tooling commands.
The full target list below is generated from the Makefile's own `## target:`
help comments (the same source as `make help`), so it always reflects the
current targets:

--8<-- "docs/_generated/make-commands.md"

## Running Locally

```bash
# Create the data directory
mkdir -p data

# Start with debug logging
SW_DB_PATH=./data/stillwater.db SW_LOG_FORMAT=text SW_LOG_LEVEL=debug ./stillwater
```

On Windows (MSYS2/Git Bash):
```bash
mkdir -p data
SW_DB_PATH=./data/stillwater.db SW_LOG_FORMAT=text SW_LOG_LEVEL=debug ./stillwater.exe
```

The app starts at http://localhost:1973. On first run it will run all database migrations and prompt you to create an admin account.

## Running with Docker

```bash
# Build and run
docker compose up --build

# Or build the image separately
docker build -f build/docker/Dockerfile -t stillwater:dev .
docker run -p 1973:1973 -v stillwater-data:/config -v /path/to/music:/music:rw stillwater:dev
```

The Docker build handles templ generation implicitly (committed `_templ.go` files) and runs Tailwind CSS inside the build stage, so no local tooling beyond Docker is needed for container builds.

## Development Workflow

### Editing Code

1. **Modify Go files** in `internal/` or `cmd/stillwater/`
2. **Edit Templ templates** in `web/templates/` or `web/components/`
   - Changes to `.templ` files require regeneration: `go tool templ generate`
   - Generated `_templ.go` files should be committed alongside source templates
3. **Update CSS** in `web/static/css/input.css`
   - Rebuild: `tailwindcss -i web/static/css/input.css -o web/static/css/styles.css --minify`
4. **Add Go dependencies**: `go get` (then commit changes to `go.mod` and `go.sum`)

### Hot Reload (Development)

Use `air` for automatic rebuild on file changes:

```bash
go install github.com/air-verse/air@latest
air  # watches for changes and rebuilds
```

The app restarts at `http://localhost:1973` after each rebuild.

### Making Database Schema Changes

Stillwater uses a single migration file. To change the schema:

1. Edit `internal/database/migrations/001_initial_schema.sql` directly
2. Add or modify tables/columns in the appropriate `-- +goose Up` section
3. Update the corresponding `-- +goose Down` section
4. Test locally: `go test ./internal/database/...`
5. Migrations run automatically on application startup via goose

**Important (pre-GA only):** Do not create new migration files. All schema changes go into `001_initial_schema.sql`. After GA, standard goose versioned migrations (002, 003, ...) will be used for incremental schema changes against existing databases.

### Code Quality

Before committing or opening a PR:

```bash
# Format code
make fmt          # or: go fmt ./... && go tool templ fmt web/

# Run linter
make lint         # or: golangci-lint run ./...

# Run tests
make test         # or: go test -race -count=1 ./...
```

Pre-commit hooks enforce formatting and linting automatically. Run `make hooks` to install the project hook; see `.githooks/pre-commit` for the full list of checks.

### Signed Commits

`main` requires signed commits. Two checks enforce this, and they fail in opposite directions on purpose.

**Locally, at commit time.** `.githooks/pre-commit` runs `scripts/check-commit-signing.sh`, which refuses to create an unsigned commit. It verifies two separate things: that this clone is configured to sign (`commit.gpgsign` is true and `user.signingkey` is set), and that the configured signer actually works right now. The second part is a live probe: it signs a throwaway commit using this repository's resolved signing config and confirms the result carries a signature. An unreachable signer is reported as an error, never as a fallback to an unsigned commit.

The requirement itself comes from the tracked file `.githooks/signed-commits-required`. It is deliberately not inferred from `commit.gpgsign`, because a check that reads the requirement from the setting would conclude "signing is not required here" in exactly the case it exists to catch. Set `SW_REQUIRE_SIGNED_COMMITS=0` to override.

**In CI, as a required check.** `.github/workflows/signed-commits.yml` asks GitHub whether every commit in the PR is `verified`, and names any that are not. The local hook is earlier and cheaper but advisory -- it can be skipped with `--no-verify` or never installed. The CI check runs where the committer has no say, so it is the layer that actually holds.

Fix an unsigned commit while it is still local. Once it is on a reviewed PR, the only remedy is rewriting shared history, which orphans any commit SHA cited in review replies.

**Verifying a signature by hand.** Check the raw commit object:

```bash
git cat-file commit HEAD | sed -n '1,/^$/p' | grep '^gpgsig'
```

Do not use `git log --format=%G?` for this. It reports `N` -- "no signature" -- for genuinely signed commits whenever `gpg.ssh.allowedSignersFile` is unset, because it answers "can I verify this signature", not "is there one". That is the default state on a fresh clone.

**If signing fails.** This repository signs through 1Password (`gpg.ssh.program` is `op-ssh-sign`), which reaches the desktop app over its own IPC. If signing breaks, confirm the 1Password app is running and unlocked. Note that `op-ssh-sign` does *not* use `SSH_AUTH_SOCK` -- that variable matters for pushing over SSH, not for signing. On a plain `ssh-agent` setup instead of 1Password, an empty `SSH_AUTH_SOCK` is the usual cause; non-interactive shells frequently get one. Export it and confirm with `ssh-add -L`.

Run `bash scripts/test-check-commit-signing.sh` to exercise the local check. Case 9 there guards a subtle constraint worth knowing if you modify the probe: git exports `GIT_INDEX_FILE` (and in a linked worktree, an absolute `GIT_DIR`) into its hooks, and those outrank `git -C`. Any git command the hook runs against another repository must first clear the inherited `GIT_*` environment, or it operates on the real index instead. Because this project works in worktrees, the contaminating case is the normal one here.

### API Testing

Use Bruno collections in `api/bruno/` to test endpoints:

1. Install Bruno: [https://www.usebruno.com/](https://www.usebruno.com/)
2. Open `api/bruno/` as a collection
3. Run requests against a local or running instance
4. Bruno exports as plaintext files (no binary lock-in)

#### Local Bruno Coverage Recipe

To measure API coverage from the Bruno collection locally:

```bash
# 1. Build an instrumented binary
GOCOVERDIR=$(mktemp -d)
go build -cover -o /tmp/stillwater-cover ./cmd/stillwater

# 2. Start the server with coverage output enabled
GOCOVERDIR="$GOCOVERDIR" /tmp/stillwater-cover &
SERVER_PID=$!

# 3. Run the Bruno collection (from api/bruno/)
cd api/bruno
bru run --env ci --env-var "baseUrl=http://127.0.0.1:1973" --disable-cookies -r .

# 4. Stop server gracefully (flushes coverage counters)
kill "$SERVER_PID" && wait "$SERVER_PID" 2>/dev/null || true

# 5. Convert binary coverage to a Go coverage profile
go tool covdata textfmt -i="$GOCOVERDIR" -o bruno-coverage.out

# 6. View the report
go tool cover -func=bruno-coverage.out | tail -1
```

## Running Tests

```bash
# All tests
go test -v -count=1 ./...

# With race detector (requires CGO, not available on MSYS_NT/Windows without GCC)
go test -v -race -count=1 ./...

# Single package
go test -v -count=1 ./internal/image/...
```

## Environment Variables

The full, generated reference for every `SW_` environment variable is on the
published docs site:

[Reference: Environment variables](https://sydlexius.github.io/stillwater/reference/environment-variables/)

That page is generated from the configuration definition (`internal/config/config.go`)
via `make generate-docs` and is always up to date. Do not maintain a separate
table here.

For the Docker container, two additional variables control file ownership:

| Variable | Default | Description |
|----------|---------|-------------|
| `PUID` | `99` | User ID the container process runs as. Set to your host UID to avoid permission issues on mounted volumes. |
| `PGID` | `100` | Group ID the container process runs as. Set to your host GID. |

## Project Structure

See `CLAUDE.md` for the full architecture overview and coding conventions.

## Releasing

Releases are automated with [GoReleaser](https://goreleaser.com/). Pushing a semver tag triggers the release workflow, which builds multi-platform binaries, pushes Docker images to GHCR with semver tags, and creates a GitHub Release with auto-generated notes from conventional commit history.

### Tag and push

Tags must be signed annotated tags (`-s`) to earn the GitHub Verified badge.
This requires a GPG or SSH signing key configured in your git config.

```bash
git tag -s v0.2.0 -m "Release v0.2.0"
git push origin v0.2.0
```

The release workflow creates the GitHub Release automatically. Pre-release tags (e.g. `v0.2.0-rc1`) are marked as pre-release on GitHub.

### Local dry run (no publish)

```bash
goreleaser release --snapshot --clean
```

This builds all artifacts locally without creating a release or pushing images. Useful for verifying the config before tagging.

### Validate config

```bash
goreleaser check
```
