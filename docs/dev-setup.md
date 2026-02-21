# Development Setup

Prerequisites and instructions for building Stillwater from source.

## Required Tools

| Tool | Version | Purpose | Install |
|------|---------|---------|---------|
| Go | 1.26+ | Compiler and runtime | https://go.dev/dl/ |
| templ | latest | HTML template code generation | `go install github.com/a-h/templ/cmd/templ@latest` |
| Tailwind CSS | v4.2.0 | CSS build (standalone CLI) | See below |
| Git | any | Version control | https://git-scm.com/ |

### Optional Tools

| Tool | Purpose | Install |
|------|---------|---------|
| Docker | Container builds and testing | https://docs.docker.com/get-docker/ |
| golangci-lint | Linting (`make lint`) | https://golangci-lint.run/welcome/install/ |
| Bruno | API testing (collections in `api/bruno/`) | https://www.usebruno.com/ |
| air | Hot reload during development (`make dev`) | `go install github.com/air-verse/air@latest` |

## Installing Tailwind CSS Standalone CLI

The project uses the Tailwind CSS standalone CLI (no Node.js required). Download the correct binary for your platform from the GitHub releases page:

https://github.com/tailwindlabs/tailwindcss/releases/tag/v4.2.0

**Linux:**
```bash
curl -sLo tailwindcss https://github.com/tailwindlabs/tailwindcss/releases/download/v4.2.0/tailwindcss-linux-x64
chmod +x tailwindcss
sudo mv tailwindcss /usr/local/bin/
```

**macOS (Apple Silicon):**
```bash
curl -sLo tailwindcss https://github.com/tailwindlabs/tailwindcss/releases/download/v4.2.0/tailwindcss-macos-arm64
chmod +x tailwindcss
sudo mv tailwindcss /usr/local/bin/
```

**Windows:**
```powershell
curl -Lo tailwindcss.exe https://github.com/tailwindlabs/tailwindcss/releases/download/v4.2.0/tailwindcss-windows-x64.exe
# Move to a directory on your PATH, or keep in the repo root (it is gitignored)
```

## Clone and Build

```bash
git clone https://github.com/sydlexius/stillwater.git
cd stillwater

# Install Go dependencies
go mod download

# Generate templ code (only needed if _templ.go files are stale)
templ generate

# Build Tailwind CSS
tailwindcss -i web/static/css/input.css -o web/static/css/styles.css --minify

# Build the binary
go build -o stillwater ./cmd/stillwater
```

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
docker run -p 1973:1973 -v stillwater-data:/data -v /path/to/music:/music:rw stillwater:dev
```

The Docker build handles templ generation implicitly (committed `_templ.go` files) and runs Tailwind CSS inside the build stage, so no local tooling beyond Docker is needed for container builds.

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

| Variable | Default | Description |
|----------|---------|-------------|
| `SW_DB_PATH` | `data/stillwater.db` | SQLite database file path |
| `SW_LOG_LEVEL` | `info` | Log level: debug, info, warn, error |
| `SW_LOG_FORMAT` | `json` | Log format: json, text |
| `SW_LISTEN_ADDR` | `:1973` | HTTP listen address |
| `SW_BASE_PATH` | (empty) | URL prefix for reverse proxy (e.g., `/stillwater`) |
| `SW_ENCRYPTION_KEY` | (auto-generated) | Base64-encoded AES-256 key for encrypting API keys at rest |
| `PUID` / `PGID` | `1000` | User/group ID for Docker container file ownership |

## Project Structure

See `CLAUDE.md` for the full architecture overview and coding conventions.

## Build Targets (if `make` is available)

```
make build          Build binary (runs templ generate + tailwind first)
make run            Build and run locally with debug logging
make test           Run all tests with race detector
make lint           Run golangci-lint
make fmt            Format Go + Templ files
make docker-build   Build Docker image
make docker-run     Start via docker compose
make clean          Remove build artifacts
```

When `make` is not available, run the equivalent commands directly as shown in the sections above.

## Releasing

Releases are automated with [GoReleaser](https://goreleaser.com/). Pushing a semver tag triggers the release workflow, which builds multi-platform binaries, pushes Docker images to GHCR with semver tags, and creates a GitHub Release with auto-generated notes from conventional commit history.

### Tag and push

```bash
git tag -a v0.2.0 -m "Release v0.2.0"
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
