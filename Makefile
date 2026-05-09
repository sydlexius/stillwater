.PHONY: build run test test-shuffle test-race test-cover lint fmt clean docker-build docker-run dev templ tailwind generate generate-docs migrate favicon hooks check-openapi hadolint scan bruno-ci

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

## test-shuffle: Run tests with random ordering to surface order-dependent tests (local hygiene; reproduce a failure with -shuffle=<seed>)
test-shuffle:
	go test -race -count=1 -shuffle=on ./...

## test-race: Run tests with race detector via WSL2 (requires WSL2 with Go and GCC installed)
test-race:
	@wsl_path=$$(echo "$(CURDIR)" | sed 's|^/\([a-zA-Z]\)/|/mnt/\1/|'); \
	wsl -e bash -c "cd $$wsl_path && CGO_ENABLED=1 go test -race -count=1 ./..."

## test-cover: Run tests with coverage
test-cover:
	go test -count=1 -v -race -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

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

## generate: Run all code generation (templ + tailwind)
generate: templ tailwind

## generate-docs: Regenerate docs site content from code (provider matrix, env-var reference, rules catalogue, settings reference, doc anchors)
generate-docs:
	go run ./cmd/gen-provider-matrix
	go run ./cmd/gen-env-reference
	go run ./cmd/gen-rules-catalogue
	go run ./cmd/gen-settings-reference
	go run ./cmd/gen-doc-anchors

## tailwind-watch: Watch and rebuild Tailwind CSS
tailwind-watch:
	tailwindcss -i $(TAILWIND_INPUT) -o $(TAILWIND_OUTPUT) --watch

## migrate: Run database migrations
migrate:
	go run $(CMD_DIR)

## favicon: Regenerate PNG favicons from logo design
favicon:
	go run ./tools/genfavicon

## scan: Build Docker image (no cache) and scan for CVEs (requires grype)
scan:
	docker build --no-cache -f build/docker/Dockerfile -t ghcr.io/sydlexius/stillwater:scan .
	grype ghcr.io/sydlexius/stillwater:scan --fail-on high

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

## hooks: Install git hooks (pre-commit lint, pre-push gate)
hooks:
	chmod +x .githooks/pre-commit .githooks/pre-push
	git config core.hooksPath .githooks
	@echo "Hooks installed via core.hooksPath=.githooks (covers worktrees)."
	@echo "  pre-commit: lint, gofmt, templ freshness, build, golangci-lint, govulncheck, hadolint"
	@echo "  pre-push:   runs scripts/pre-push-gate.sh (tests, OpenAPI, generated, patch coverage >= 70%)"

## clean: Remove build artifacts
clean:
	rm -f $(BINARY)
	rm -f coverage.out coverage.html
	rm -f $(TAILWIND_OUTPUT)

## bruno-ci: Build binary, run ephemeral server, execute Bruno API tests, clean up.
# Required: npx / @usebruno/cli reachable. Admin credentials are ephemeral and
# auto-generated; DO NOT set STILLWATER_ADMIN_PASSWORD to a real password here.
#
# Environment variables (all optional -- defaults are ephemeral and CI-safe):
#   SW_PORT                  Port the ephemeral server binds to (default: random free port)
#   STILLWATER_ADMIN_USER    Admin username created on first-run (default: ci-admin)
#   STILLWATER_ADMIN_PASSWORD Admin password for the ephemeral run (default: ci-ephemeral-pw)
#   BRUNO_RESULTS_DIR        Directory for HTML report (default: /tmp/bruno-results)
#
# The target exits non-zero if Bruno reports any test failures. The server PID
# is tracked in a temp file and cleaned up on exit (success or failure).
bruno-ci: build
	@set -euo pipefail; \
	SW_DB="$${TMPDIR:-/tmp}/stillwater-ci-$$$$.db"; \
	PID_FILE="$${TMPDIR:-/tmp}/stillwater-ci-$$$$.pid"; \
	RESULTS_DIR="$${BRUNO_RESULTS_DIR:-$${TMPDIR:-/tmp}/bruno-results}"; \
	ADMIN_USER="$${STILLWATER_ADMIN_USER:-ci-admin}"; \
	ADMIN_PASS="$${STILLWATER_ADMIN_PASSWORD:-ci-ephemeral-pw}"; \
	\
	cleanup() { \
	  if [ -f "$$PID_FILE" ]; then \
	    kill "$$(cat $$PID_FILE)" 2>/dev/null || true; \
	    rm -f "$$PID_FILE" "$$SW_DB" "$$SW_DB-wal" "$$SW_DB-shm"; \
	  fi; \
	}; \
	trap cleanup EXIT INT TERM; \
	\
	if [ -z "$${SW_PORT:-}" ]; then \
	  SW_PORT=$$(python3 -c 'import socket; s=socket.socket(); s.bind(("",0)); p=s.getsockname()[1]; s.close(); print(p)'); \
	fi; \
	\
	echo "[bruno-ci] starting server on port $$SW_PORT (db=$$SW_DB)"; \
	SW_DB_PATH="$$SW_DB" SW_PORT="$$SW_PORT" SW_LOG_FORMAT=text SW_LOG_LEVEL=warn \
	  ./$(BINARY) > "$${TMPDIR:-/tmp}/stillwater-ci-$$$$.log" 2>&1 & \
	echo $$! > "$$PID_FILE"; \
	\
	echo "[bruno-ci] waiting for health endpoint and capturing CSRF token..."; \
	READY=0; \
	CSRF_TOKEN=""; \
	DEADLINE=$$(( $$(date +%s) + 60 )); \
	while [ $$(date +%s) -lt $$DEADLINE ]; do \
	  if ! kill -0 "$$(cat $$PID_FILE)" 2>/dev/null; then \
	    echo "[bruno-ci] server exited before becoming ready"; \
	    cat "$${TMPDIR:-/tmp}/stillwater-ci-$$$$.log" || true; \
	    exit 1; \
	  fi; \
	  HEALTH_RESP=$$(curl -sf -D - --max-time 5 "http://127.0.0.1:$$SW_PORT/api/v1/health" 2>/dev/null || true); \
	  if echo "$$HEALTH_RESP" | grep -q '"status":"ok"'; then \
	    CSRF_TOKEN=$$(echo "$$HEALTH_RESP" \
	      | grep -i "^set-cookie:" \
	      | grep "csrf_token=" \
	      | sed 's/.*csrf_token=\([^;]*\).*/\1/' \
	      | tr -d '[:space:]' || true); \
	    READY=1; break; \
	  fi; \
	  sleep 2; \
	done; \
	if [ "$$READY" -eq 0 ]; then \
	  echo "[bruno-ci] server did not become ready within 60s"; \
	  cat "$${TMPDIR:-/tmp}/stillwater-ci-$$$$.log" || true; \
	  exit 1; \
	fi; \
	if [ -z "$$CSRF_TOKEN" ]; then \
	  echo "[bruno-ci] CSRF token not found in health response; cannot proceed"; \
	  exit 1; \
	fi; \
	\
	echo "[bruno-ci] creating admin account"; \
	curl -sf --max-time 10 -X POST "http://127.0.0.1:$$SW_PORT/api/v1/auth/setup" \
	  -H "Content-Type: application/json" \
	  -d "{\"username\":\"$$ADMIN_USER\",\"password\":\"$$ADMIN_PASS\"}" > /dev/null; \
	\
	echo "[bruno-ci] logging in and capturing session cookie"; \
	SESSION_COOKIE=$$(curl -sf -D - --max-time 10 \
	  -X POST "http://127.0.0.1:$$SW_PORT/api/v1/auth/login" \
	  -H "Content-Type: application/json" \
	  -H "X-CSRF-Token: $$CSRF_TOKEN" \
	  -d "{\"username\":\"$$ADMIN_USER\",\"password\":\"$$ADMIN_PASS\"}" \
	  | grep -i "^set-cookie:" \
	  | grep "session=" \
	  | sed 's/.*session=\([^;]*\).*/\1/' \
	  | tr -d '[:space:]' || true); \
	if [ -z "$$SESSION_COOKIE" ]; then \
	  echo "[bruno-ci] login failed -- could not extract session cookie"; \
	  exit 1; \
	fi; \
	\
	mkdir -p "$$RESULTS_DIR"; \
	echo "[bruno-ci] running Bruno collection (env=ci, port=$$SW_PORT, watchdog=$${BRUNO_TIMEOUT_SEC:-300}s)"; \
	BRUNO_TIMEOUT_SEC="$${BRUNO_TIMEOUT_SEC:-300}"; \
	( cd api/bruno && STILLWATER_CSRF_TOKEN="$$CSRF_TOKEN" npx --yes @usebruno/cli@1.22.0 run \
	    --env ci \
	    --env-var "baseUrl=http://127.0.0.1:$$SW_PORT" \
	    --env-var "sessionToken=$$SESSION_COOKIE" \
	    --reporter-html "$$RESULTS_DIR/bruno-results.html" \
	    -r \
	    . ) & \
	BRU_PID=$$!; \
	( sleep "$$BRUNO_TIMEOUT_SEC" && kill -TERM "$$BRU_PID" 2>/dev/null && sleep 5 && kill -KILL "$$BRU_PID" 2>/dev/null ) & \
	WATCHDOG_PID=$$!; \
	if wait "$$BRU_PID"; then \
	  EXIT_CODE=0; \
	else \
	  EXIT_CODE=$$?; \
	fi; \
	kill "$$WATCHDOG_PID" 2>/dev/null || true; \
	if [ "$$EXIT_CODE" = "143" ] || [ "$$EXIT_CODE" = "137" ]; then \
	  echo "[bruno-ci] watchdog killed bru after $$BRUNO_TIMEOUT_SEC s; treating as failure"; \
	fi; \
	echo "[bruno-ci] results written to $$RESULTS_DIR/bruno-results.html"; \
	exit $$EXIT_CODE

## help: Show this help message
help:
	@echo "Available targets:"
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  /'
