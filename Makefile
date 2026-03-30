.PHONY: build run test test-race test-cover lint fmt clean docker-build docker-run dev templ tailwind migrate favicon hooks check-openapi hadolint

# Binary name
BINARY=stillwater
BUILD_DIR=./build
CMD_DIR=./cmd/stillwater

# Version (from git tags or default)
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "0.1.0-dev")
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE    ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

# Go parameters
VERSION_PKG=github.com/sydlexius/stillwater/internal/version
GOFLAGS=-ldflags="-s -w -X $(VERSION_PKG).Version=$(VERSION) -X $(VERSION_PKG).Commit=$(COMMIT) -X $(VERSION_PKG).Date=$(DATE)"

# Tailwind CSS
TAILWIND_INPUT=web/static/css/input.css
TAILWIND_OUTPUT=web/static/css/styles.css

## build: Build the Go binary
build: templ tailwind
	go build $(GOFLAGS) -o $(BINARY) $(CMD_DIR)

## run: Build and run locally
run: build
	SW_DB_PATH=./data/stillwater.db SW_LOG_FORMAT=text SW_LOG_LEVEL=debug ./$(BINARY)

## dev: Run with hot reload (requires air)
dev:
	air

## test: Run all tests
test:
	go test -v -race -count=1 ./...

## test-race: Run tests with race detector via WSL2 (requires WSL2 with Go and GCC installed)
test-race:
	@wsl_path=$$(echo "$(CURDIR)" | sed 's|^/\([a-zA-Z]\)/|/mnt/\1/|'); \
	wsl -e bash -c "cd $$wsl_path && CGO_ENABLED=1 go test -race -count=1 ./..."

## test-cover: Run tests with coverage
test-cover:
	go test -count=1 -v -race -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

## check: Format, lint, and build in one step (run before every commit)
check: fmt lint build

## lint: Run golangci-lint
lint:
	golangci-lint run ./...

## hadolint: Lint Dockerfile for best practices
hadolint:
	hadolint build/docker/Dockerfile

## fmt: Format all Go and Templ files
fmt:
	gofmt -w .
	goimports -w .
	templ fmt .

## templ: Generate Go code from Templ templates
templ:
	templ generate

## tailwind: Build Tailwind CSS
tailwind:
	tailwindcss -i $(TAILWIND_INPUT) -o $(TAILWIND_OUTPUT) --minify

## tailwind-watch: Watch and rebuild Tailwind CSS
tailwind-watch:
	tailwindcss -i $(TAILWIND_INPUT) -o $(TAILWIND_OUTPUT) --watch

## migrate: Run database migrations
migrate:
	go run $(CMD_DIR)

## favicon: Regenerate PNG favicons from logo design
favicon:
	go run ./tools/genfavicon

## docker-build: Build Docker image
docker-build:
	docker build -f build/docker/Dockerfile -t ghcr.io/sydlexius/stillwater:latest .

## docker-run: Run Docker container
docker-run:
	docker compose up -d

## docker-stop: Stop Docker container
docker-stop:
	docker compose down

## check-openapi: Verify OpenAPI spec matches handler implementations
check-openapi:
	go test -count=1 -run TestOpenAPIConsistency -v ./internal/api/

## hooks: Install git pre-commit hook (mirrors CI lint checks)
hooks:
	cp .githooks/pre-commit .git/hooks/pre-commit
	chmod +x .git/hooks/pre-commit
	@echo "Pre-commit hook installed."

## clean: Remove build artifacts
clean:
	rm -f $(BINARY)
	rm -f coverage.out coverage.html
	rm -f $(TAILWIND_OUTPUT)

## help: Show this help message
help:
	@echo "Available targets:"
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  /'
