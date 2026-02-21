# M11: Universal Music Scraper (v0.11.0)

## Goal

Per-field provider assignment, fallback chains, library-scoped configuration, and web image search. Inspired by TinyMediaManager's Universal Scraper.

## Prerequisites

- M5 (Rule Engine) complete -- fallback audit integrates with rule evaluation
- M8 (Security and Stability) complete -- TheAudioDB V1/V2 fix needed for provider selection

## Issues

| # | Title | Mode | Model | Status |
|---|-------|------|-------|--------|
| 51 | Universal Music Scraper: per-field provider assignment, fallback chains, REST API and UI config | plan | opus | Backend complete (PR #87), UI pending |
| 86 | Settings UI Refactor: card-based layout with service logos | plan | opus | In progress (branch: feature/m11-settings-card-layout) |
| 56 | Web Image Search Provider Tier: targeted scraping for artist images (blocked by #51) | plan | opus | Not started |

## Implementation Order

### Step 1: Configuration Model (#51)

1. Define scraper configuration structures in `internal/scraper/`:
   - `FieldConfig` struct: field name, primary provider, enabled flag
   - `FallbackChain` struct: category (metadata/images), ordered provider list
   - `ScraperConfig` struct: map of field configs, metadata fallback chain, image fallback chain, scope (global/library)
   - `LibraryConfig` struct: library ID, overrides map, inherits-from reference

2. Database schema:
   - `scraper_config` table: id, scope ("global" or library ID), config_json (JSON blob), created_at, updated_at
   - Migration file in `internal/database/migrations/`

3. Default configuration:
   - Metadata primary: MusicBrainz (name, sort_name, type, gender, disambiguation, mbid, genres, born, formed, died, disbanded)
   - Metadata fallback: MusicBrainz -> Last.fm -> Discogs -> TheAudioDB -> Wikidata
   - Biography primary: Last.fm (biography)
   - Image primary: Fanart.tv (thumb, fanart, logo, banner)
   - Image fallback: Fanart.tv -> TheAudioDB -> Discogs

### Step 2: Scraper Service (#51)

1. Create `internal/scraper/service.go`:
   - `Service` struct with `*sql.DB`, provider registry, `*slog.Logger`
   - `GetConfig(ctx, scope) (*ScraperConfig, error)` -- returns merged config (library inherits from global)
   - `SaveConfig(ctx, config) error` -- upsert config for scope
   - `ResetConfig(ctx, scope) error` -- delete overrides, revert to inherited

2. Create `internal/scraper/executor.go`:
   - `Executor` struct with scraper service, all provider clients
   - `ScrapeField(ctx, artist, field) (*FieldResult, error)` -- tries primary provider, then fallback chain
   - `ScrapeAll(ctx, artist) (*ScrapeResult, error)` -- scrapes all configured fields
   - `FieldResult` includes: value, provider used, was_fallback flag, error
   - `ScrapeResult` includes: per-field results, overall success/failure

3. Fallback logic:
   - Try primary provider for the field
   - If primary fails or returns empty, walk the category fallback chain in order
   - Record which provider ultimately supplied each field
   - If all providers fail, mark field as unresolved

### Step 3: REST API (#51)

1. API endpoints:
   - `GET /api/v1/scraper/config` -- get global config
   - `PUT /api/v1/scraper/config` -- update global config
   - `GET /api/v1/scraper/config/{libraryId}` -- get library config (merged with global)
   - `PUT /api/v1/scraper/config/{libraryId}` -- update library overrides
   - `DELETE /api/v1/scraper/config/{libraryId}` -- reset library to global defaults
   - `GET /api/v1/scraper/providers` -- list available providers with their capabilities (which fields they support)

2. Handlers in `internal/api/handlers_scraper.go`

3. Wire into router and main.go

### Step 4: Scraper UI (#51)

1. Create `web/templates/scraper.templ`:
   - Per-field dropdown for primary provider selection
   - Only shows providers that support that field
   - Fallback chain section with drag-and-drop or up/down reordering (pills/chips)
   - Separate sections for metadata fields and image fields
   - "Reset to defaults" button

2. Library-scope UI:
   - Per-library override toggle
   - Visual indication of inherited vs overridden fields (e.g., dimmed = inherited, bold = overridden)
   - "Reset to global" button per library

3. Add navigation link to scraper config page

### Step 5: Rule Engine Integration (#51)

1. Add fallback audit rule in `internal/rule/`:
   - New rule type: "fallback_used"
   - Triggers when a field was populated by a fallback provider instead of the primary
   - Severity: info (audit trail, not a violation)
   - In YOLO mode: auto-accept fallback results
   - In review mode: flag for user review

2. Record fallback events:
   - Store which provider supplied each field in the artist record or a separate audit table
   - Visible in artist detail page under "Metadata Sources" section

### Step 6: Web Image Search Provider (#56)

1. Create `internal/provider/websearch/` package:
   - `Provider` struct implementing image search interface
   - Google Images scraper (HTML parsing, fragile but functional)
   - Search query construction:
     - Use artist name + image type keyword (e.g., "Artist Name logo png")
     - Use `site:` queries with known URLs from Wikidata/MusicBrainz (official site, Wikipedia, Discogs)
   - Search term sets per image type stored in `internal/provider/websearch/terms.json`

2. Result handling:
   - Results tagged with source badge and search query
   - Image dimensions: "Unknown" until user clicks to fetch/verify
   - Logo transparency check: fast PNG header alpha channel detection
   - Badge for logos lacking transparency

3. UI integration:
   - "Extend Search" button on image search page
   - Web results appear in a separate section below main provider results
   - Clear visual distinction (different background, source badges)
   - Loading indicator while web search runs

4. Bulk fetch integration:
   - Web search providers disabled by default in bulk operations
   - Configurable toggle in scraper config
   - Rate limiting for web scraping (longer delays than API providers)

5. Configuration:
   - Enable/disable at global and per-library scope (inherits from scraper config)
   - Appears as a provider in the fallback chain for image category
   - Search terms configurable via JSON file (advanced users only, not exposed in UI)

## Key Design Decisions

- **JSON blob for config:** Scraper config is complex and nested. A JSON column is simpler than normalizing into multiple relational tables. Queries are by scope ID only.
- **Category-level fallback, not field-level:** A single fallback chain per category (metadata, images) keeps config manageable. Per-field primary selection gives enough control.
- **Inheritance model:** Library configs inherit all global settings by default. Overrides are explicit and can be reset. Prevents configuration drift.
- **Web scraping as lowest tier:** Web search results are inherently lower quality and more fragile. They supplement, not replace, dedicated API providers. Disabled in bulk by default.
- **Google scraping is fragile:** Document this clearly. May need periodic maintenance. API alternative (Google Custom Search) can be added later if scraping breaks.
- **No disk caching for web images:** Web search results are ephemeral. Only persist if the user explicitly selects and saves an image.

## Verification

- [ ] Default scraper config provides sensible per-field assignments
- [ ] Fallback chain executes in order when primary fails
- [ ] Library config inherits from global correctly
- [ ] Library overrides are visually distinct in UI
- [ ] "Reset to global" clears library overrides
- [ ] Provider capabilities accurately reported (which fields each supports)
- [ ] Fallback audit rule fires when fallback provider used
- [ ] Web image search returns results for known artists
- [ ] Web results visually distinct from API provider results
- [ ] Logo transparency detection works
- [ ] "Extend Search" button triggers web search
- [ ] Web search disabled by default in bulk operations
- [ ] `go test ./...` and `golangci-lint run` pass
- [ ] Tag v0.11.0
