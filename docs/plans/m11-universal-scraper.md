# M11: Universal Music Scraper (v0.11.0)

## Goal

Per-field provider assignment, fallback chains, connection-scoped configuration, and web image search. Inspired by TinyMediaManager's Universal Scraper.

## Prerequisites

- M5 (Rule Engine) complete -- fallback audit integrates with rule evaluation
- M8 (Security and Stability) complete -- TheAudioDB V1/V2 fix needed for provider selection

## Issues

| # | Title | Mode | Model | Status |
|---|-------|------|-------|--------|
| 51 | Universal Music Scraper: per-field provider assignment, fallback chains, REST API and UI config | plan | opus | Backend complete (PR #87), UI pending |
| 86 | Settings UI Refactor: card-based layout with service logos | plan | opus | Not started |
| 56 | Web Image Search Provider Tier: targeted scraping for artist images (blocked by #51) | plan | opus | Not started |

## Execution Order

1. **#51 Steps 1-3, 5** -- Scraper backend (config model, service/executor, REST API, rule engine) -- DONE (PR #87)
2. **#86** -- Settings UI refactor (card-based layout with service logos) -- NEXT
3. **#51 Step 4** -- Scraper UI (scraper.templ, built on #86 patterns) -- after #86
4. **#56** -- Web image search provider -- after #51 is fully complete

Each issue gets its own feature branch and PR against main.

---

## Implementation Progress

### Step 1: Configuration Model -- DONE

**New file: `internal/scraper/config.go`**

Types implemented:
- `FieldName` (13 constants: biography, genres, styles, moods, members, formed, born, died, disbanded, thumb, fanart, logo, banner)
- `FieldCategory` (metadata, images)
- `FieldConfig`, `FallbackChain`, `ScraperConfig`, `Overrides`, `ProviderCapability`
- `DefaultConfig()` with sensible defaults, `ProviderCapabilities()` static capability map
- Helper methods: `PrimaryFor()`, `FallbackChainFor()`, `CategoryFor()`, `AllFieldNames()`

**New file: `internal/database/migrations/009_scraper_config.sql`**

- Creates `scraper_config` table (id, scope, config_json, overrides_json, timestamps)
- Index on scope column
- Adds `metadata_sources TEXT NOT NULL DEFAULT '{}'` column to `artists` table

**Tests: `internal/scraper/config_test.go`** -- 5 tests pass

### Step 2: Scraper Service and Executor -- DONE

**New file: `internal/scraper/service.go`**

- `Service` struct with `*sql.DB` and `*slog.Logger`
- `SeedDefaults()` -- inserts default global config if none exists
- `GetConfig()` -- returns merged config (connection overrides applied on top of global)
- `GetRawConfig()` -- returns unmerged config + overrides (for UI)
- `SaveConfig()` -- upsert for any scope
- `ResetConfig()` -- deletes non-global scope row
- `mergeConfigs()` -- iterates global fields, uses connection value if overridden

**New file: `internal/scraper/executor.go`**

- `Executor` struct with `*Service`, `*provider.Registry`, `*slog.Logger`
- `ScrapeAll()` -- loads global config, iterates enabled fields, calls `scrapeField`, builds merged `FetchResult` with sources
- `scrapeField()` -- tries primary, walks category fallback chain, records `WasFallback`
- `getProviderResult()` -- thread-safe caching to avoid duplicate API calls
- `applyFieldValue()` -- checks provider has data for a field (switch on FieldName)
- `applyMergeableFields()` -- copies IDs, URLs, aliases to merged result

**Modified: `internal/provider/orchestrator.go`**

- Added `ScraperExecutor` interface with `ScrapeAll()` method
- Added `SetExecutor()` setter on Orchestrator
- `FetchMetadata()` delegates to executor when set, falls back to existing FieldPriority logic

Dependency direction: `scraper` imports `provider`, `provider` does NOT import `scraper` (uses interface). No circular imports.

**Tests: `internal/scraper/service_test.go`** -- 6 tests pass

### Step 3: REST API -- DONE

**New file: `internal/api/handlers_scraper.go`**

| Method | Path | Handler | Description |
|--------|------|---------|-------------|
| GET | `/api/v1/scraper/config` | `handleGetScraperConfig` | Get global config |
| PUT | `/api/v1/scraper/config` | `handleUpdateScraperConfig` | Update global config |
| GET | `/api/v1/scraper/config/connections/{id}` | `handleGetConnectionScraperConfig` | Get merged connection config (+ raw/overrides) |
| PUT | `/api/v1/scraper/config/connections/{id}` | `handleUpdateConnectionScraperConfig` | Update connection overrides |
| DELETE | `/api/v1/scraper/config/connections/{id}` | `handleResetConnectionScraperConfig` | Reset connection to global |
| GET | `/api/v1/scraper/providers` | `handleListScraperProviders` | List providers with capabilities and key status |

Connection ID validated against `connectionService.GetByID()` (404 if not found).

**Modified: `internal/api/router.go`**

- Added `ScraperService *scraper.Service` to `RouterDeps` and `scraperService` to `Router`
- Registered 6 API routes in `Handler()`

### Step 4: Scraper UI -- PENDING (after #86)

Requires #86 (Settings UI refactor) to establish card-based patterns with service logos.

### Step 5: Rule Engine Integration -- DONE

**Modified: `internal/artist/model.go`**

- Added `MetadataSources map[string]string` field
- Added `MarshalStringMap()` / `UnmarshalStringMap()` helpers

**Modified: `internal/artist/service.go` + `alias.go`**

- Added `metadata_sources` to `artistColumns`, `Create`, `Update`, `scanArtist`, `scanArtistWithExtra`

**Modified: `internal/rule/service.go`**

- Added `RuleFallbackUsed = "fallback_used"` constant
- Added default rule entry (category: metadata, severity: info, not fixable)

**Modified: `internal/rule/checkers.go`**

- Added `makeFallbackChecker()` closure-based checker that captures `*scraper.Service`
- Compares artist's `MetadataSources` against configured primaries in global scraper config

**Modified: `internal/rule/engine.go`**

- Added `SetScraperService(svc *scraper.Service)` which registers the fallback checker
- Follows same deferred-registration pattern as `SetEventBus` on scanner

**Modified: `cmd/stillwater/main.go`**

- Creates `scraperService`, seeds defaults, creates `scraperExecutor`
- Wires executor into orchestrator via `SetExecutor()`
- Wires scraper service into rule engine via `SetScraperService()`
- Passes `ScraperService` to `RouterDeps`

---

## Key Design Decisions (updated from implementation)

- **"Connection" not "library":** Scope overrides use connection IDs from the existing `connections` table. The "library" terminology was dropped to avoid confusion with media library paths.
- **JSON blob for config:** Scraper config stored as JSON in `config_json` column. Overrides tracked separately in `overrides_json` column with boolean maps per field/chain.
- **Category-level fallback, not field-level:** Single fallback chain per category (metadata, images). Per-field primary selection gives enough control.
- **Orchestrator delegates via interface:** `ScraperExecutor` interface in `provider` package avoids circular imports. Orchestrator falls back to old `FieldPriority` logic when no executor is set (backward compat).
- **Setter pattern for deferred wiring:** `SetExecutor()` on orchestrator and `SetScraperService()` on rule engine match the existing `SetEventBus()` pattern used by scanner.
- **Provider result caching:** `getProviderResult()` uses mutex-guarded map to avoid duplicate API calls when multiple fields try the same provider. Same pattern as existing orchestrator.
- **No disk caching for web images:** Web search results are ephemeral. Only persist if the user explicitly selects and saves an image.

## Observations and Notes

- The MSYS/Windows dev environment does not support `-race` flag (requires CGO). Race detector testing should be done in CI (Linux Docker).
- `gofmt` realigns all struct field tags when a new wider field is added. The `map[string]string` type for `MetadataSources` caused tag realignment across the entire `Artist` struct.
- The `scanArtistWithExtra()` function in `alias.go` duplicates most of `scanArtist()` logic. If more columns are added in future, consider refactoring to share scan logic.
- Migration 009 uses `ALTER TABLE artists ADD COLUMN metadata_sources TEXT NOT NULL DEFAULT '{}'` which SQLite handles well (no table rebuild needed for DEFAULT).

## Verification

- [x] Default scraper config provides sensible per-field assignments
- [x] `go test ./...` passes (24 packages, 0 failures)
- [x] `golangci-lint run ./...` passes (0 issues)
- [ ] Fallback chain executes in order when primary fails (needs integration test with mock providers)
- [ ] Connection config inherits from global correctly (unit tested, needs manual API test)
- [ ] Connection overrides are visually distinct in UI (Step 4)
- [ ] "Reset to global" / "Reset to defaults" buttons work (Step 4)
- [ ] Provider capabilities endpoint returns accurate field support
- [ ] Fallback audit rule fires when fallback provider used (needs artist with MetadataSources populated)
- [ ] Web image search returns results for known artists (#56)
- [ ] Web results visually distinct from API provider results (#56)
- [ ] Logo transparency detection works (#56)
- [ ] Web search disabled by default in bulk operations (#56)
- [ ] Tag v0.11.0
