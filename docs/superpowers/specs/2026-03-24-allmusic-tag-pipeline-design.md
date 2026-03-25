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

The current Providers settings page combines API key cards, web image search toggles, priority
chip rows, and matching settings in a single vertical scroll. Adding three more sections
(web metadata scrapers, tag aggregation grid, NFO field mapping) would create an
unmanageable wall of content. The page is reorganized into four internal tabs.

### Tab Structure

Route: `/settings/providers` with hash fragments for direct linking (`#keys`, `#priorities`,
`#tags`, `#nfo`).

| Tab | Label | Content |
|-----|-------|---------|
| `#keys` | Keys & Sources | Collapsed provider cards, web image search, web metadata scrapers |
| `#priorities` | Field Priorities | Draggable chip rows for single-source fields (biography, formed, etc.) |
| `#tags` | Tag Sources | Aggregation checkbox grid for genres/styles/moods, normalization settings |
| `#nfo` | NFO Output | Platform compatibility table, field mapping toggles, advanced remap |

Tab bar uses the standard inner-tab pattern: horizontal row of text labels, active tab
highlighted with a blue bottom border (2px, blue-500). Tabs are not scrollable -- all four
fit comfortably. Switching tabs preserves scroll position within each panel.

### Keys & Sources Tab: Collapsed Provider Cards

**Problem solved:** 8+ provider cards with API key inputs, rate limits, and help links
overwhelm the page when all expanded simultaneously.

**Pattern:** Each provider is a single compact row (48px height) by default:

```
[icon 32px] [name + key-status] ............. [tier badge] [expand] [toggle]
```

**Collapsed state (always visible):**
- Provider icon (32x32, rounded-8, gradient background, 2-letter abbreviation)
- Provider name (13px, font-weight 600)
- Key status line (11px):
  - Configured: green checkmark + "Key configured"
  - Missing: amber warning icon + "No API key"
  - Free: "No key required" (neutral gray)
- Tier badge: "Free" (green) or "Key" (amber), 9px uppercase
- Expand affordance: plus-circle icon (rotates 45deg to X when expanded)
- Enable/disable toggle: iOS-style pill (44x24px), independent of expand

**Expanded state (click row or expand icon to toggle):**

Slides open below the collapsed row with `max-height` transition (0.25s ease-out).
Content indented to align with the name (left padding 56px to clear the icon).

Contains:
- Description (11px, gray, 1-2 lines describing what the provider fetches)
- Meta row: rate limit, daily cap (if any), "Get a free API key" link (blue, opens
  provider's key registration page)
- Key management:
  - **Configured:** "Key configured" status + "Change" button (neutral) + "Remove" button
    (red tint). Clicking "Change" replaces this row with an input + Save/Cancel.
  - **Unconfigured:** Password input (monospace, placeholder "Paste your API key") + "Save"
    button (blue).

**Grouping within the tab:**

Three visually separated sections with section headers and descriptions:

1. **Metadata Providers** -- MusicBrainz, TheAudioDB, Last.fm, Discogs, Genius, Fanart.tv,
   Wikidata, Wikipedia. All standard API-based providers.
2. **Web Image Search** -- DuckDuckGo. Section description notes results require manual
   review.
3. **Web Metadata Scrapers** -- AllMusic. Section description notes HTML scrapers are more
   fragile than API providers.

Each section has `settings-section-title` (15px bold) and `settings-section-desc` (12px gray)
consistent with other settings sections.

### Field Priorities Tab

Identical to the current priority chip rows, with one change: genres, styles, and moods are
removed from this tab. An informational callout (blue tint, info icon) explains:

> Genres, Styles, and Moods are not shown here. They use multi-source aggregation instead
> of single-provider priority. Configure them in the **Tag Sources** tab.

"Tag Sources" is a clickable link that switches to that tab.

Remaining fields shown as draggable chip rows: Biography, Formed/Born, Disbanded/Died,
Members, Years Active.

### Tag Sources Tab

Two sections:

**1. Tag Aggregation Sources**

Section description explains the difference from priority-based fields: these fields combine
data from all checked providers, not just the highest-priority one.

Checkbox grid table:

| | Genres | Styles | Moods |
|---|:---:|:---:|:---:|
| TheAudioDB | [x] | [x] | [x] |
| MusicBrainz | [x] | [x] | [x] |
| Last.fm | [x] | [x] | [x] |
| Discogs | [x] | [x] | [ ] |
| AllMusic | [x] | [x] | [ ] |

- Provider column: 24px icon + name (12px)
- Checkboxes: 18x18px, rounded-5, blue-500 when checked
- Greyed-out (25% opacity, cursor not-allowed) when the provider does not supply that
  category. Footnote below table explains this.
- Columns are centered. First column (provider name) is left-aligned.
- Table header: 11px uppercase, gray, 0.8px letter spacing

**2. Matching & Normalization**

Standard setting rows with toggles:
- "Canonical spelling normalization" -- normalize variations to curated forms
- "Case-insensitive deduplication" -- treat case differences as duplicates

### NFO Output Tab

Three sections:

**1. Platform Compatibility**

Read-only reference table showing how each platform handles NFO elements. Color-coded:
- Green: field is read and displayed in its native category
- Amber: field is read but mapped to a different category (e.g., `<style>` read as Tags)
- Red: field is ignored entirely

Table:

| NFO Element | Emby | Jellyfin | Kodi |
|---|---|---|---|
| `<genre>` | Genres (green) | Ignored (red) | Genres (green) |
| `<style>` | Tags (amber) | Tags (amber) | Styles (green) |
| `<mood>` | Ignored (red) | Ignored (red) | Moods (green) |

Element names in monospace. Bold callout above the table:

> **Emby and Jellyfin do not have separate Styles or Moods fields.** Both platforms merge
> `<style>` and `<tag>` into a single "Tags" field. `<mood>` is ignored entirely. Only Kodi
> displays all four fields independently.

**2. NFO Tag Output**

Setting rows:

- **Use default genre behavior** (toggle, default on): description "Genres, styles, and moods
  each write to their own NFO element." When on, the genre composition checkboxes below are
  greyed out (35% opacity, pointer-events none).

- **Genre composition checkboxes** (visible when default is off): sub-heading "Populate
  `<genre>` field from:" with three checkbox rows: Genres (checked by default), Styles
  (unchecked), Moods (unchecked). Footnote: "Styles and moods are always written to their own
  fields regardless. The genre list is deduplicated."

- **Write moods as styles** (toggle, default off): description "Also write mood values as
  `<style>` elements, making them visible as Tags in Emby/Jellyfin. The `<mood>` elements
  are still written for Kodi."

**3. Advanced field mapping** (collapsible, closed by default)

Chevron toggle: "Advanced field mapping" with right-pointing chevron that rotates 90deg when
open. `max-height` transition.

When expanded, shows a 3x3 checkbox matrix:

| NFO Element | Genres | Styles | Moods |
|---|:---:|:---:|:---:|
| `<genre>` | [x] | [ ] | [ ] |
| `<style>` | [ ] | [x] | [ ] |
| `<mood>` | [ ] | [ ] | [x] |

Description: "Full control over which categories are written to each NFO element. Overrides
the default behavior and toggles above."

Footnote: "Each row controls what gets written to that NFO element. Output is deduplicated
per element."

### Mockup

Interactive mockup: `.superpowers/brainstorm/tag-settings-mockup.html`

Fully functional with all four tabs, expand/collapse provider cards, key management states
(configured, unconfigured, edit mode), checkbox grids, toggle interactions, and collapsible
advanced remap. Open in a browser to interact.

---

## UI Redesign Spec Updates Required

The following sections of `docs/superpowers/specs/2026-03-23-ui-redesign-design.md` need
updates to reflect this design:

- **Settings > Providers:** Replace the current single-page description with the four-tab
  structure and collapsed provider card pattern described above
- **Form Controls:** Add the checkbox grid pattern (multi-provider, multi-field matrix) to
  the documented control types
- **Settings section list:** Consider whether the inner tabs should be promoted to top-level
  settings nav items in a future iteration
