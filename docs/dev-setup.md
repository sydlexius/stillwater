# Development Setup

Prerequisites and instructions for building Stillwater from source.

## Required Tools

| Tool | Version | Purpose | Install |
|------|---------|---------|---------|
| Go | 1.26.1+ | Compiler and runtime | https://go.dev/dl/ |
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

### API Testing

Use Bruno collections in `api/bruno/` to test endpoints:

1. Install Bruno: [https://www.usebruno.com/](https://www.usebruno.com/)
2. Open `api/bruno/` as a collection
3. Run requests against a local or running instance
4. Bruno exports as plaintext files (no binary lock-in)

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
