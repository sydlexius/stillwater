# AllMusic Scraper, Tag Aggregation, and NFO Field Mapping

## Problem Statement

Stillwater's genre/style/mood metadata is significantly thinner than MediaElch's output for
the same artists and sources. The root causes are threefold:

1. **No AllMusic provider.** AllMusic has the most curated genre/style taxonomy in the music
   metadata space (professionally maintained by Xperi editors), but has no public API. MediaElch
   scrapes it; Stillwater does not.
2. **First-match-wins for tag fields.** The orchestrator treats genres, styles, and moods as
   scalar-like fields -- first provider with data wins, all others discarded. This wastes data
   from secondary providers.
3. **Platform-blind NFO output.** Emby and Jellyfin handle NFO tag elements differently from
   Kodi, but Stillwater writes the same output regardless of target platform.

## Scope

Three sequenced issues, all targeting Milestone 31 (Provider Pipeline & Scraping):

| Issue | Title | Depends on |
|-------|-------|------------|
| 1 | AllMusic web metadata scraper | -- |
| 2 | Tag aggregation engine with canonical normalization | Issue 1 |
| 3 | NFO field mapping for platform compatibility | Independent (benefits from 1+2) |

## Platform Compatibility Research

Empirically verified against Emby UAT (localhost:8096) and confirmed via Jellyfin/Emby source
code (`BaseNfoParser.cs` and `BaseNfoSaver.cs`):

| NFO Element | Emby Reads As | Emby Writes As | Jellyfin Reads As | Kodi Reads As |
|-------------|---------------|----------------|-------------------|---------------|
| `<genre>`   | Genres        | `<genre>`      | Ignored for artists | Genres      |
| `<style>`   | Tags          | `<style>`      | Tags              | Styles        |
| `<tag>`     | Tags          | --             | Tags              | Tags          |
| `<mood>`    | Ignored       | --             | Ignored           | Moods         |

Key findings:
- Both Emby and Jellyfin map `<style>` and `<tag>` to the same internal Tags field
- Both write internal Tags back as `<style>` for MusicArtist/MusicAlbum types
- `<mood>` is completely ignored by both platforms (read and write)
- Jellyfin ignores `<genre>` for music artists
- Neither platform has distinct Styles or Moods fields -- only Kodi does

---

## Issue 1: AllMusic Web Metadata Scraper

### New Interface: WebMetadataScraper

Parallels the existing `WebImageProvider` pattern in `provider.go`:

```go
type WebMetadataScraper interface {
    Name() ProviderName
    RequiresAuth() bool
    ScrapeArtist(ctx context.Context, id string) (*ArtistMetadata, error)
}
```

No `SearchArtist` -- the scraper only fires when an AllMusic ID is already known (sourced
from MusicBrainz URL relations). No `GetImages` -- not an image source.

### Infrastructure

- `WebScraperRegistry` in `registry.go` (same pattern as `WebSearchRegistry`)
- `AllWebScraperProviderNames()` in `provider.go`
- Settings keys: `provider.webscraper.<name>.enabled`
- `WebScraperProviderStatus`, `IsWebScraperEnabled`, `SetWebScraperEnabled`,
  `ListWebScraperStatuses` in `settings.go`
- UI: "Web Metadata Scrapers" section in Settings > Providers > Keys & Sources tab

### AllMusic Implementation: `internal/provider/allmusic/`

- URL: `https://www.allmusic.com/artist/<mn_id>` where ID matches `mn\d{10}`
- ID source: `extractProviderIDsFromURLs` in orchestrator (add AllMusic `mn*` extraction
  from MusicBrainz URL relations with type "allmusic")
- Extraction: parse HTML for `<h4>Genre</h4>` and `<h4>Styles</h4>` sections, extract `<a>`
  tag text within those sections
- Moods: empty slice initially; interface supports adding later when the AJAX endpoint for
  AllMusic's Moods & Themes tab is reverse-engineered
- Rate limit: 1 req/sec (singleton limiter, same as other providers)
- User-Agent: `Stillwater/<version> (music metadata manager)`
- Error handling: `ErrScraperBroken` sentinel for HTML structure changes, logged distinctly
  from network errors
- Dependency: `golang.org/x/net/html` or goquery for HTML parsing

### Orchestrator Integration

The orchestrator invokes the scraper after standard providers, using the AllMusic ID extracted
during the MusicBrainz phase. Results feed into the existing `FetchResult` merge pipeline.

---

## Issue 2: Tag Aggregation Engine

### Orchestrator Change

Replace first-match-wins logic in `applyField` (orchestrator.go lines 357-374) for genres,
styles, and moods with an accumulate strategy. Only accumulates from providers the user has
enabled in the tag source grid.

### New Package: `internal/provider/tagdict/`

Canonical spelling dictionary for normalization and deduplication:

- `canonical.go`: Go map literal `map[string]string` keyed by normalized form, valued with
  preferred spelling. Seeded from AllMusic's curated taxonomy, supplemented by MusicBrainz
  genre list and Last.fm top tags.
- `Normalize(s string) string`: lowercase, strip non-alphanumeric (preserve `&`), collapse
  whitespace. Used as dedup key.
- `MergeAndDeduplicate(existing, incoming []string) []string`: normalize both lists, dedup
  on normalized form, return canonical spellings. First-seen position wins for ordering.
- Unknown tags (not in dictionary): pass through with original casing, still deduplicated
  via normalization.

Examples: `"synthpop"`, `"Synth-Pop"`, `"Synth Pop"` all normalize to `"Synth-Pop"`.
`"K-Pop"`, `"R&B"`, `"Lo-Fi"` preserve intentional stylization.

### Settings

New keys per field per provider:
```
provider.tagsources.genres.<provider_name>.enabled = true/false
provider.tagsources.styles.<provider_name>.enabled = true/false
provider.tagsources.moods.<provider_name>.enabled = true/false
```

Default: all providers enabled for all three fields.

### UI

Checkbox grid in Settings > Providers > Tag Sources tab. Provider rows, genre/style/mood
columns. Greyed-out checkboxes for providers that don't supply a category (e.g., Discogs
moods, AllMusic moods). Includes normalization toggles.

Callout in the Field Priorities tab explaining that genres/styles/moods are configured
separately in Tag Sources.

---

## Issue 3: NFO Field Mapping for Platform Compatibility

### Three-Tier Configuration

1. **Default (on):** Write genres to `<genre>`, styles to `<style>`, moods to `<mood>`.
   Current behavior, Kodi-compatible.

2. **"Write moods as styles" toggle:** When enabled, mood values are additionally written
   as `<style>` elements (making them visible as Tags in Emby/Jellyfin). `<mood>` elements
   are still written for Kodi. Output is deduplicated.

3. **Advanced remap (collapsed by default):** Full matrix of source-category to NFO-element
   mappings. Overrides the default and the moods-as-styles toggle. Power users can configure
   e.g. genres written as `<style>` for Jellyfin (which ignores `<genre>` for artists).

### Genre Field Composition

When the user disables the default behavior, checkboxes appear for selecting which categories
(genres, styles, moods) populate the `<genre>` field. Styles and moods are always written to
their own fields regardless. The genre list is deduplicated.

### Settings

```
nfo.output.default_behavior = true/false
nfo.output.genre_sources = ["genres"]  (or ["genres","styles","moods"])
nfo.output.moods_as_styles = false
nfo.output.advanced_remap = {genre: ["genres"], style: ["styles"], mood: ["moods"]}
```

### UI

Settings > Providers > NFO Output tab. Platform compatibility table (color-coded) at the top
for context. Default toggle, greyed-out genre composition checkboxes (enabled when default is
off), moods-as-styles toggle, and collapsible advanced remap matrix.

---

## UI: Provider Settings Reorganization

The Providers settings section is reorganized into four internal tabs to manage complexity:

| Tab | Content |
|-----|---------|
| Keys & Sources | Collapsed provider cards (expand for key management), web image search, web metadata scrapers |
| Field Priorities | Draggable chip rows for single-source fields only (not genres/styles/moods) |
| Tag Sources | Aggregation checkbox grid, normalization settings |
| NFO Output | Platform compatibility table, field mapping configuration |

### Collapsed Provider Cards

Each provider shows a single compact row by default: icon, name, key status (green checkmark
or amber warning), tier badge (Free/Key), expand affordance, and enable/disable toggle.
Expanding reveals: description, rate limit, help link, and key management controls
(Change/Remove for configured keys, input field for unconfigured).

### Mockup

Interactive mockup at `.superpowers/brainstorm/tag-settings-mockup.html`.
All four tabs, collapsed cards with expand, key management, checkbox grids, and NFO output
configuration with collapsible advanced remap.

---

## UI Redesign Spec Updates Required

The following sections of `docs/superpowers/specs/2026-03-23-ui-redesign-design.md` need
updates to reflect this design:

- **Settings > Providers:** Add the four-tab internal structure and collapsed card pattern
- **Form Controls:** Document the checkbox grid pattern (multi-provider, multi-field matrix)
- **Settings section list:** May need to consider promoting Providers sub-tabs to top-level
  nav items if testing shows the two-level navigation is confusing
