.PHONY: build run test test-shuffle test-race test-cover test-js test-a11y lint fmt clean clean-uat uat docker-build docker-run dev templ tailwind generate generate-docs migrate favicon hooks doctor worktree check-openapi sync-tool-versions hadolint vulncheck scan audit bruno-ci

# Use bash for all recipes so bash-only constructs (set -o pipefail) work even
# where /bin/sh is dash (Debian/Ubuntu); plain sh lacks pipefail.
SHELL := /usr/bin/env bash

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

## test: Run all tests with race detector and verbose output
test:
	go test -v -race -count=1 ./...

## test-shuffle: Run tests with random ordering to surface order-dependent tests (local hygiene; reproduce a failure with -shuffle=<seed>)
test-shuffle:
	go test -race -count=1 -shuffle=on ./...

## test-race: Run tests with race detector (native; CGO required for the race instrumentation)
test-race:
	CGO_ENABLED=1 go test -race -count=1 ./...

## test-cover: Run tests with coverage
test-cover:
	go test -count=1 -v -race -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

## test-js: Install JS dev dependencies and run Node.js unit tests for client-side JS modules.
# Uses Node's built-in test runner (node:test, Node 18+) and jsdom (the only npm dep).
# No server required; tests exercise fanart-manage.js, lightbox.js, and artwork-modal.js
# in an isolated jsdom environment, plus axe-core structural a11y scans.
# Run command: make test-js
test-js:
	npm ci
	npm test

## test-a11y: Build an ephemeral server and run Playwright axe-core a11y smoke tests.
# Two-tier: (1) make test-js covers structural violations in jsdom (fast, no server);
# (2) this target covers rendered-color-contrast violations in a real Chromium browser.
# Required: Node 18+, Go toolchain, templ + tailwindcss on PATH (same as make build).
# Environment variables (all optional -- defaults are ephemeral and CI-safe):
#   SW_PORT                  Port the ephemeral server binds to (default: random free port)
#   STILLWATER_ADMIN_USER    Admin username (default: ci-a11y-admin)
#   STILLWATER_ADMIN_PASSWORD Admin password (default: ci-a11y-ephemeral-pw)
test-a11y: build
	@set -euo pipefail; \
	SW_DB="$${TMPDIR:-/tmp}/stillwater-a11y-$$$$.db"; \
	PID_FILE="$${TMPDIR:-/tmp}/stillwater-a11y-$$$$.pid"; \
	LOG_FILE="$${TMPDIR:-/tmp}/stillwater-a11y-$$$$.log"; \
	\
	cleanup() { \
	  if [ -f "$$PID_FILE" ]; then \
	    kill "$$(cat $$PID_FILE)" 2>/dev/null || true; \
	    rm -f "$$PID_FILE" "$$SW_DB" "$$SW_DB-wal" "$$SW_DB-shm" "$$LOG_FILE"; \
	  fi; \
	}; \
	trap cleanup EXIT INT TERM; \
	\
	if [ -z "$${SW_PORT:-}" ]; then \
	  SW_PORT=$$(python3 -c 'import socket; s=socket.socket(); s.bind(("",0)); p=s.getsockname()[1]; s.close(); print(p)'); \
	fi; \
	\
	echo "[test-a11y] starting server on port $$SW_PORT (db=$$SW_DB)"; \
	SW_DB_PATH="$$SW_DB" SW_PORT="$$SW_PORT" SW_LOG_FORMAT=text SW_LOG_LEVEL=warn \
	  SW_BACKUP_ENABLED=false SW_UX=next \
	  ./$(BINARY) > "$$LOG_FILE" 2>&1 & \
	echo $$! > "$$PID_FILE"; \
	\
	echo "[test-a11y] waiting for server ready..."; \
	DEADLINE=$$(( $$(date +%s) + 60 )); \
	READY=0; \
	while [ $$(date +%s) -lt $$DEADLINE ]; do \
	  if ! kill -0 "$$(cat $$PID_FILE)" 2>/dev/null; then \
	    echo "[test-a11y] server exited before becoming ready"; \
	    cat "$$LOG_FILE" || true; \
	    exit 1; \
	  fi; \
	  HEALTH=$$(curl -sf --max-time 5 "http://127.0.0.1:$$SW_PORT/api/v1/health" 2>/dev/null || true); \
	  if echo "$$HEALTH" | grep -q '"status":"ok"'; then \
	    READY=1; break; \
	  fi; \
	  sleep 2; \
	done; \
	if [ "$$READY" -eq 0 ]; then \
	  echo "[test-a11y] server did not become ready within 60s"; \
	  cat "$$LOG_FILE" || true; \
	  exit 1; \
	fi; \
	\
	echo "[test-a11y] running Playwright a11y smoke tests..."; \
	npm ci --silent; \
	SW_PORT="$$SW_PORT" npx playwright test --config=playwright.config.js; \
	echo "[test-a11y] done."

## lint: Run golangci-lint
lint:
	golangci-lint run ./...

## hadolint: Lint Dockerfile for best practices
hadolint:
	hadolint build/docker/Dockerfile

## vulncheck: Scan for known vulnerabilities (govulncheck, pinned to the CI version)
vulncheck:
	go run golang.org/x/vuln/cmd/govulncheck@v1.1.4 ./...

## fmt: Format all Go and Templ files
fmt:
	gofmt -w .
	goimports -w .
	go tool templ fmt .

## templ: Generate Go code from Templ templates
templ:
	go tool templ generate

## tailwind: Build Tailwind CSS
tailwind:
	tailwindcss -i $(TAILWIND_INPUT) -o $(TAILWIND_OUTPUT) --minify

## generate: Run all code generation (templ + tailwind)
generate: templ tailwind

## generate-docs: Regenerate docs site content from code (provider matrix, env-var reference, CLI reference, rules catalogue, settings reference, doc anchors, envelope-versions, make-command reference, platform-profiles, preferences reference, CI reference). Each generator enforces coverage: a new code-defined key without a desc: tag or doc entry fails the build.
generate-docs:
	go run ./cmd/gen-provider-matrix
	go run ./cmd/gen-env-reference
	go run ./cmd/gen-cli-reference
	go run ./cmd/gen-rules-catalogue
	go run ./cmd/gen-settings-reference
	go run ./cmd/gen-doc-anchors
	go run ./cmd/gen-envelope-changelog
	go run ./cmd/gen-make-reference
	go run ./cmd/gen-platform-profiles
	go run ./cmd/gen-prefs-reference
	go run ./cmd/gen-ci-reference

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

## audit: Advisory local security pass (govulncheck + gosec + semgrep + syft/grype); govulncheck/grype gate, gosec/semgrep advisory-only
# Runs each sub-tool sequentially, capturing each exit code into a shell var
# instead of relying on `set -e` (mirrors the bruno-ci capture-then-decide
# pattern), so one tool's failure doesn't abort the rest. gosec and semgrep
# are purely advisory (gosec produced ~165 mostly-FP hits in #1929); semgrep,
# syft, and grype are optional external binaries and soft-skip with an
# install hint when absent. Only govulncheck failing, or grype finding a
# High/Critical CVE (matching the `scan` target's --fail-on high cutoff),
# fails the target.
audit:
	@set -uo pipefail; \
	OVERALL=0; \
	\
	echo "[audit] === govulncheck (gating) ==="; \
	go run golang.org/x/vuln/cmd/govulncheck@v1.1.4 ./...; \
	GOVULN_STATUS=$$?; \
	if [ "$$GOVULN_STATUS" -eq 0 ]; then GOVULN_RESULT="PASS"; else GOVULN_RESULT="FAIL"; OVERALL=1; fi; \
	\
	echo ""; \
	echo "[audit] === gosec (advisory) ==="; \
	go run github.com/securego/gosec/v2/cmd/gosec@v2.22.4 ./...; \
	GOSEC_STATUS=$$?; \
	if [ "$$GOSEC_STATUS" -eq 0 ]; then GOSEC_RESULT="PASS"; else GOSEC_RESULT="ADVISORY"; fi; \
	\
	echo ""; \
	echo "[audit] === semgrep (advisory) ==="; \
	if ! command -v semgrep >/dev/null 2>&1; then \
	  echo "[audit] semgrep not found on PATH; skipping (install: https://semgrep.dev/docs/getting-started/)"; \
	  SEMGREP_RESULT="SKIP"; \
	else \
	  semgrep --config=auto; \
	  SEMGREP_STATUS=$$?; \
	  if [ "$$SEMGREP_STATUS" -eq 0 ]; then SEMGREP_RESULT="PASS"; else SEMGREP_RESULT="ADVISORY"; fi; \
	fi; \
	\
	echo ""; \
	echo "[audit] === SBOM/CVE scan: syft -> grype (gating when present) ==="; \
	if ! command -v syft >/dev/null 2>&1; then \
	  echo "[audit] syft not found on PATH; skipping (install: https://github.com/anchore/syft)"; \
	  SBOM_RESULT="SKIP"; \
	elif ! command -v grype >/dev/null 2>&1; then \
	  echo "[audit] grype not found on PATH; skipping (install: https://github.com/anchore/grype)"; \
	  SBOM_RESULT="SKIP"; \
	else \
	  syft dir:. -o json 2>/dev/null | grype --fail-on high; \
	  GRYPE_STATUS=$$?; \
	  if [ "$$GRYPE_STATUS" -eq 0 ]; then SBOM_RESULT="PASS"; else SBOM_RESULT="FAIL"; OVERALL=1; fi; \
	fi; \
	\
	echo ""; \
	echo "[audit] === summary ==="; \
	echo "[audit] govulncheck: $$GOVULN_RESULT (gating)"; \
	echo "[audit] gosec:       $$GOSEC_RESULT (advisory)"; \
	echo "[audit] semgrep:     $$SEMGREP_RESULT (advisory)"; \
	echo "[audit] syft/grype:  $$SBOM_RESULT (gating when present)"; \
	if [ "$$OVERALL" -eq 0 ]; then \
	  echo "[audit] result: PASS"; \
	else \
	  echo "[audit] result: FAIL (govulncheck and/or grype High/Critical found)"; \
	fi; \
	exit "$$OVERALL"

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

## sync-tool-versions: Mirror the CI-side Tailwind version into the Dockerfile pin
##   Use when the Tool Version Drift gate reports the setup-tailwind action and
##   build/docker/Dockerfile TAILWIND_VERSION disagree: this rewrites the
##   Dockerfile pin to match the action. Review and commit the result.
sync-tool-versions:
	@./scripts/check-tool-versions.sh --fix

## hooks: Install git hooks (pre-commit lint, pre-push gate)
hooks:
	chmod +x .githooks/pre-commit .githooks/pre-push
	@git config --unset core.hooksPath 2>/dev/null || true
	git config core.hooksPath .githooks
	@./scripts/check-hooks.sh

## doctor: Verify hook wiring without modifying anything
doctor:
	@./scripts/check-hooks.sh

## worktree: Create a sibling worktree with hooks wired and tracker row inserted into the Active table
##   Usage: make worktree NAME=<slug> BRANCH=<branch> [ISSUE=<number>] [WAVE=<label>]
##   Example: make worktree NAME=m49.5-merge-policy BRANCH=refactor/m49.5-1395-merge-policy ISSUE=1395 WAVE="M49.5 W1"
WORKTREES_MD ?= $(HOME)/.claude/projects/-Users-jesse-Developer-stillwater/memory/worktrees.md
worktree:
	@test -n "$(NAME)"   || (echo "error: NAME is required (e.g. make worktree NAME=my-feature BRANCH=feat/my-feature)"; exit 1)
	@test -n "$(BRANCH)" || (echo "error: BRANCH is required (e.g. make worktree NAME=my-feature BRANCH=feat/my-feature)"; exit 1)
	git worktree add ../stillwater-$(NAME) -b $(BRANCH)
	@$(MAKE) -C ../stillwater-$(NAME) hooks
	@mkdir -p "$(dir $(WORKTREES_MD))"
	@touch "$(WORKTREES_MD)"
	@if ! grep -q '^|---' "$(WORKTREES_MD)"; then \
		printf '# Active Worktrees\n\n## Active\n\n| Worktree | Branch | Issues | Wave | Status |\n|----------|--------|--------|------|--------|\n' >> "$(WORKTREES_MD)"; \
	fi
	@row='| stillwater-$(NAME) | $(BRANCH) | $(if $(ISSUE),#$(ISSUE),--) | $(if $(WAVE),$(WAVE),--) | In progress |'; \
	awk -v row="$$row" 'BEGIN{ins=0} {print} !ins && /^\|---/ {print row; ins=1}' \
		"$(WORKTREES_MD)" > "$(WORKTREES_MD).tmp" && mv "$(WORKTREES_MD).tmp" "$(WORKTREES_MD)"
	@echo "Worktree ../stillwater-$(NAME) ready on branch $(BRANCH). Hooks wired. Active table updated."

## remove-worktree: Remove a sibling worktree (via cleanup-worktree.sh) and delete its Active-table row
##   Usage: make remove-worktree NAME=<slug>
##   Example: make remove-worktree NAME=m49.5-merge-policy
remove-worktree:
	@test -n "$(NAME)" || (echo "error: NAME is required (e.g. make remove-worktree NAME=my-feature)"; exit 1)
	@wt="../stillwater-$(NAME)"; \
	if [ -d "$$wt" ]; then \
		cur=$$(git -C "$$wt" symbolic-ref --quiet --short HEAD 2>/dev/null || true); \
		def=$$(git -C "$$wt" symbolic-ref --quiet --short refs/remotes/origin/HEAD 2>/dev/null | sed 's#^origin/##'); \
		if [ -n "$$cur" ] && [ -n "$$def" ] && [ "$$cur" = "$$def" ]; then \
			echo "!!! WARNING: $$wt is on the default branch ('$$def')."; \
			echo "!!! This is the 'gh pr merge --delete-branch' aftermath: gh checked out the default"; \
			echo "!!! branch here before deleting the feature branch, so the feature branch is already"; \
			echo "!!! gone. Branch deletion will be SKIPPED by cleanup-worktree.sh (issue #1741);"; \
			echo "!!! continuing with worktree + tracker cleanup only."; \
		fi; \
	fi
	@$(HOME)/.claude/scripts/cleanup-worktree.sh "$(NAME)" || \
		echo "warning: cleanup-worktree.sh exited non-zero (worktree or branch may already be gone); continuing with tracker row removal"
	@if [ -f "$(WORKTREES_MD)" ]; then \
		if grep -q '^| stillwater-$(NAME) ' "$(WORKTREES_MD)"; then \
			awk -v prefix='| stillwater-$(NAME) ' 'index($$0, prefix) != 1' \
				"$(WORKTREES_MD)" > "$(WORKTREES_MD).tmp" && mv "$(WORKTREES_MD).tmp" "$(WORKTREES_MD)"; \
			echo "Removed stillwater-$(NAME) row from $(WORKTREES_MD)"; \
		else \
			echo "no stillwater-$(NAME) row found in $(WORKTREES_MD); nothing to remove"; \
		fi; \
	fi

## clean: Remove build artifacts
clean:
	rm -f $(BINARY)
	rm -f coverage.out coverage.html
	rm -f $(TAILWIND_OUTPUT)

# Source of the live data dir to clone for UAT. Override on the command line,
# e.g. `make uat SW_APPDATA=/path/to/data`. The DB and its sibling
# encryption.key both live here.
SW_APPDATA ?= $(HOME)/appdata/stillwater/data
# Port the printed run command binds to. Override with `make uat SW_PORT=1975`.
SW_PORT    ?= 1975

## uat: Stage a UAT copy of the live DB + encryption key into ./.uat/ (siblings) and print the run command
# Clones the live SQLite DB via the online .backup API (consistent snapshot, no
# locking of the source) and copies the encryption key as a LITERAL SIBLING so
# the server resolves it automatically via the encryption.key-alongside-DB path.
# The staged copy is fully disposable; `make clean-uat` removes it. The printed
# command intentionally does NOT set SW_ENCRYPTION_KEY_FILE -- the sibling key
# is resolved without it.
uat:
	@set -euo pipefail; \
	src_db="$(SW_APPDATA)/stillwater.db"; \
	src_key="$(SW_APPDATA)/encryption.key"; \
	if [ ! -f "$$src_db" ]; then \
		echo "uat: source DB not found: $$src_db (set SW_APPDATA=<dir>)" >&2; exit 1; \
	fi; \
	if [ ! -f "$$src_key" ]; then \
		echo "uat: source encryption key not found: $$src_key (set SW_APPDATA=<dir>)" >&2; exit 1; \
	fi; \
	command -v sqlite3 >/dev/null 2>&1 || { echo "uat: sqlite3 not found on PATH" >&2; exit 1; }; \
	rm -rf "$(CURDIR)/.uat"; \
	mkdir -p "$(CURDIR)/.uat"; \
	sqlite3 "$$src_db" ".backup '$(CURDIR)/.uat/sw.db'"; \
	cp "$$src_key" "$(CURDIR)/.uat/encryption.key"; \
	chmod 0600 "$(CURDIR)/.uat/encryption.key"; \
	echo ""; \
	echo "UAT copy staged in $(CURDIR)/.uat (sw.db + sibling encryption.key)."; \
	echo "Run it with:"; \
	echo ""; \
	echo "  SW_PORT=$(SW_PORT) SW_UX=dual SW_DB_PATH=$(CURDIR)/.uat/sw.db SW_RUN_ROOT=$(CURDIR)/.uat/run ./stillwater"; \
	echo ""

## clean-uat: Remove the staged ./.uat/ UAT copy (DB + key + run root)
clean-uat:
	rm -rf $(CURDIR)/.uat

## bruno-ci: Build binary, run ephemeral server, execute Bruno API tests, clean up.
# Required: npx / @usebruno/cli reachable. Admin credentials are ephemeral and
# auto-generated; DO NOT set STILLWATER_ADMIN_PASSWORD to a real password here.
#
# Environment variables (all optional -- defaults are ephemeral and CI-safe):
#   SW_PORT                  Port the ephemeral server binds to (default: random free port)
#   STILLWATER_ADMIN_USER    Admin username created on first-run (default: ci-admin)
#   STILLWATER_ADMIN_PASSWORD Admin password for the ephemeral run (default: ci-ephemeral-pw)
#   BRUNO_RESULTS_DIR        Directory for HTML report (default: /tmp/bruno-results)
#   BRUNO_TIMEOUT_SEC        Watchdog ceiling for the bru run invocation (default: 300)
#
# Gate semantics MATCH the CI workflow (.github/workflows/bruno-ci.yml): the
# target fails when Bruno's exit code is non-zero (one or more `expect`
# assertions failed) OR when any transport-level errors occur (errorRequests
# != 0; a backstop against silent zero-test runs). The assertion gate is the
# primary signal -- it's what catches the oneOf/discriminator drift and
# request-body shape regressions that M49 was designed to surface.
# The server PID is tracked in a temp file and cleaned up on exit.
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
	BRUNO_LOG="$$RESULTS_DIR/bruno-stdout.log"; \
	echo "[bruno-ci] running Bruno collection (env=ci, port=$$SW_PORT, watchdog=$${BRUNO_TIMEOUT_SEC:-300}s)"; \
	BRUNO_TIMEOUT_SEC="$${BRUNO_TIMEOUT_SEC:-300}"; \
	( cd api/bruno && STILLWATER_CSRF_TOKEN="$$CSRF_TOKEN" npx --yes @usebruno/cli@3.4.2 run \
	    --env ci \
	    --env-var "baseUrl=http://127.0.0.1:$$SW_PORT" \
	    --env-var "sessionToken=$$SESSION_COOKIE" \
	    --output "$$RESULTS_DIR/bruno-results.html" \
	    --format html \
	    --reporter-json "$$RESULTS_DIR/bruno-results.json" \
	    --disable-cookies \
	    -r \
	    . > "$$BRUNO_LOG" 2>&1 ) & \
	BRU_PID=$$!; \
	( sleep "$$BRUNO_TIMEOUT_SEC" && kill -TERM "$$BRU_PID" 2>/dev/null && sleep 5 && kill -KILL "$$BRU_PID" 2>/dev/null ) & \
	WATCHDOG_PID=$$!; \
	if wait "$$BRU_PID"; then \
	  BRU_EXIT=0; \
	else \
	  BRU_EXIT=$$?; \
	fi; \
	kill "$$WATCHDOG_PID" 2>/dev/null || true; \
	echo "[bruno-ci] Bruno output:"; \
	cat "$$BRUNO_LOG"; \
	if [ "$$BRU_EXIT" = "143" ] || [ "$$BRU_EXIT" = "137" ]; then \
	  echo "[bruno-ci] watchdog killed bru after $$BRUNO_TIMEOUT_SEC s; treating as failure"; \
	  exit 1; \
	fi; \
	if [ "$$BRU_EXIT" != "0" ]; then \
	  echo "[bruno-ci] bru exited $$BRU_EXIT (one or more expect assertions failed); failing"; \
	  exit "$$BRU_EXIT"; \
	fi; \
	\
	echo "[bruno-ci] checking transport health (matches CI .github/workflows/bruno-ci.yml gate)"; \
	BRUNO_REPORT="$$RESULTS_DIR/bruno-results.json"; \
	if [ ! -s "$$BRUNO_REPORT" ]; then \
	  echo "[bruno-ci] Bruno JSON report missing or empty: $$BRUNO_REPORT; treating as failure" >&2; \
	  exit 1; \
	fi; \
	JQ_OUT=$$(jq -r \
	  '([.[].summary.totalRequests] | add) as $$t | ([.[].summary.errorRequests] | add) as $$e | "\($$t) \($$e)"' \
	  "$$BRUNO_REPORT"); \
	TOTAL="$${JQ_OUT%% *}"; \
	ERRORS="$${JQ_OUT##* }"; \
	if [ -z "$$TOTAL" ] || [ "$$TOTAL" = "0" ]; then \
	  echo "[bruno-ci] Bruno reported 0 total requests; treating as failure" >&2; \
	  exit 1; \
	fi; \
	REACHED=$$(( TOTAL - ERRORS )); \
	echo "[bruno-ci] transport: $$REACHED/$$TOTAL requests reached server (errorRequests=$$ERRORS)"; \
	if [ "$$ERRORS" -ne 0 ]; then \
	  echo "[bruno-ci] $$ERRORS transport-level errors -- failing" >&2; \
	  exit 1; \
	fi; \
	echo "[bruno-ci] transport health OK; results written to $$RESULTS_DIR/bruno-results.html"; \
	exit 0

## help: Show this help message
help:
	@echo "Available targets:"
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  /'
