# Milestone 31 (v0.31.0) -- Provider Pipeline & Scraping

## Goal

Enhance the metadata provider pipeline with web scraping capabilities, new image and metadata sources, and optimized fallback behavior. Convert Last.fm from API to scraper, add Wikidata as an image source via Wikimedia Commons, introduce Wikipedia as a metadata provider via the MediaWiki API, and ensure fallback providers are only called when actually needed.

## Acceptance Criteria

- [ ] Last.fm provider works without an API key (web scraper)
- [ ] Last.fm implements `WebImageProvider` and appears in Web Image Search settings
- [ ] Wikidata returns images from Wikimedia Commons (P18/P154)
- [ ] Wikipedia provider supplies biography, members, years active, genres, and origin
- [ ] Fallback providers are not called unless the primary provider fails or returns empty
- [ ] No regression in metadata completeness or image quality

## Dependency Map

```
#342 (fallback optimization) -- no deps, can start first
#339 (Last.fm scraper)       -- no deps, parallel with #342
#340 (Wikidata images)       -- no deps, parallel with #339/#342
#341 (Wikipedia metadata)    -- loosely depends on #340 (Wikidata supplies Wikipedia URL)
```

#342 is the foundational optimization that ensures new providers do not add unnecessary API calls. #339 and #340 are independent and can proceed in parallel. #341 benefits from #340 (Wikidata SPARQL changes to extract Wikipedia sitelinks) but can also work independently via Wikipedia API search.

## Checklist

### Issue #342 -- Optimize fallback provider call strategy in orchestrator
- [ ] Audit orchestrator `FetchMetadata` and `FetchImages` call patterns
- [ ] Audit scraper executor `ScrapeAll` for redundant calls
- [ ] Implement lazy provider evaluation (only call when needed for unfilled field)
- [ ] Add targeted image fetching (filter by needed image types)
- [ ] Add debug logging for skipped provider calls
- [ ] Tests (call count verification)

### Issue #339 -- Convert Last.fm provider from API to web scraper
- [ ] Add `goquery` dependency for HTML parsing
- [ ] Rewrite `SearchArtist` to scrape search results page
- [ ] Rewrite `GetArtist` to scrape artist page + wiki subpage
- [ ] Remove API key dependency (`RequiresAuth() false`)
- [ ] Implement `WebImageProvider` interface (`SearchImages`) for web image search
- [ ] Add Last.fm to `AllWebSearchProviderNames()` and register in `WebSearchRegistry`
- [ ] Update Settings: remove from API key panel, add to Web Image Search section
- [ ] Update OOBE UI (remove Last.fm API key step)
- [ ] Update `providerRequiresKey()` in settings
- [ ] Tests with mocked HTML responses (metadata + image search)

### Issue #340 -- Add Wikidata as image source via Wikimedia Commons
- [ ] Extend SPARQL query to fetch P18 (image) and P154 (logo)
- [ ] Implement Wikimedia Commons URL resolution
- [ ] Update `GetImages()` to return thumb/logo from Commons
- [ ] Add Wikidata to image field priorities (thumb, logo)
- [ ] Tests with mocked SPARQL and Commons API responses

### Issue #341 -- Add Wikipedia as metadata provider via MediaWiki API
- [ ] Create `internal/provider/wikipedia/` package
- [ ] Implement `SearchArtist` via MediaWiki opensearch
- [ ] Implement `GetArtist` via extracts API (biography) + wikitext parse (infobox)
- [ ] Implement infobox parser for members, years active, genres, origin
- [ ] Register provider and add to field priorities
- [ ] Update rate limiting, Settings UI, OOBE
- [ ] Tests (infobox parser, API mocks, integration)

### Issue #522 -- Provider image search returning single result instead of multiple
- [ ] Investigate why only single image candidate returned per slot
- [ ] Check provider API key validity and response parsing
- [ ] Check deduplication/filtering for over-aggressiveness
- [ ] Verify fallback chain executes fully
- [ ] Check Fanart.tv specifically for a-ha results
- [ ] Fix root cause and verify multiple candidates returned
- [ ] Tests
- [ ] PR merged

## Worktrees

| Directory | Branch | Issue | Status |
|-----------|--------|-------|--------|
| stillwater-342 | feat/342-fallback-optimization | #342 | pending |
| stillwater-339 | feat/339-lastfm-scraper | #339 | pending |
| stillwater-340 | feat/340-wikidata-images | #340 | pending |
| stillwater-341 | feat/341-wikipedia-provider | #341 | pending |

## UAT / Merge Order

Session 1 (investigation + optimization):
1. PR for #522 (base: main) -- image search regression fix
2. PR for #342 (base: main) -- fallback optimization (clean baseline)

Session 2 (new providers):
3. PR for #339 (base: main) -- Last.fm scraper
4. PR for #340 (base: main) -- Wikidata images

Session 3 (Wikipedia):
5. PR for #341 (base: main) -- Wikipedia provider (may reference #340)

## New Dependencies

- `github.com/PuerkitoBio/goquery` -- HTML parsing for Last.fm scraper (#339) and potentially Wikipedia infobox fallback

## Wiki Pages Affected

- [Architecture](https://github.com/sydlexius/stillwater/wiki/Architecture) -- new Wikipedia provider, updated Last.fm and Wikidata descriptions
- [Developer Guide](https://github.com/sydlexius/stillwater/wiki/Developer-Guide) -- new `goquery` dependency, `internal/provider/wikipedia/` package

## Notes

- 2026-03-02: Milestone created. Last.fm currently provides biography, genres, similar artists, and URLs via API. Scraper will extract the same fields from HTML pages. Last.fm will also implement `WebImageProvider` to serve as a web image search source (appears in the Web Image Search section of Providers settings alongside DuckDuckGo).
- 2026-03-02: Wikidata images will use P18 (photo) mapped to thumb and P154 (logo) mapped to logo. Wikimedia Commons API resolves filenames to downloadable URLs.
- 2026-03-02: Wikipedia provider will use MediaWiki API for biography text. Structured fields (members, years active, genres, origin) come from wikitext infobox parsing.
- 2026-03-02: Provider ID population and URL enrichment scoped into existing #322 (M30).
