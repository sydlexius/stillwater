# M3: Provider Adapters (v0.3.0)

## Goal

Implement metadata provider adapters for fetching artist metadata and images from external sources. Establish a common interface and configurable priority system.

## Prerequisites

- M2 complete (artist model, repository)

## Issues

| # | Title | Mode | Model |
|---|-------|------|-------|
| 5 | Provider interface and MusicBrainz adapter | plan | sonnet |
| 6 | Fanart.tv adapter | direct | sonnet |
| 7 | TheAudioDB adapter | direct | sonnet |
| 8 | Discogs adapter | direct | sonnet |
| 9 | Last.fm adapter | direct | sonnet |
| 10 | Wikidata adapter | direct | sonnet |
| 11 | Provider priority configuration | plan | sonnet |

## Implementation Order

### Step 1: Provider Interface + MusicBrainz (#5)

**Package:** `internal/provider/`

1. Define common types in `internal/provider/provider.go`:

```go
type Provider interface {
    Name() string
    SearchArtist(ctx context.Context, query string) ([]ArtistResult, error)
    GetArtist(ctx context.Context, id string) (*ArtistMetadata, error)
    GetImages(ctx context.Context, id string) ([]ImageResult, error)
}

type ArtistResult struct {
    ID             string   // provider-specific ID
    Name           string
    Disambiguation string
    Type           string
    Score          int      // match confidence 0-100
    Source         string   // provider name
}

type ArtistMetadata struct {
    // All fields from artist model
    // Source field to track where data came from
    // Provider IDs (each adapter populates its own):
    //   MusicBrainz -> MusicBrainzID
    //   TheAudioDB  -> AudioDBID
    //   Discogs     -> DiscogsID
    //   Wikidata    -> WikidataID
    //   Last.fm     -> (uses MBID/name, no separate ID)
    //   Fanart.tv   -> (uses MBID, no separate ID)
    ProviderIDs    map[string]string  // provider name -> ID
}

type ImageResult struct {
    URL        string
    Type       ImageType  // thumb, fanart, logo, banner
    Width      int
    Height     int
    Source     string
    Likes      int
    Language   string
}
```

2. Implement MusicBrainz adapter in `internal/provider/musicbrainz/`:
   - HTTP client with User-Agent header (MusicBrainz requirement)
   - Rate limiter: max 1 request per second
   - Search: `/ws/2/artist?query=...&fmt=json`
   - Fetch: `/ws/2/artist/{mbid}?inc=aliases+genres+tags+ratings+url-rels+artist-rels&fmt=json`
   - Map MB response to ArtistMetadata and ArtistResult
   - Populates: MusicBrainzID (the canonical cross-reference for all other providers)
   - Extract band members from `artist-rels` (type "member of band") to populate BandMember model
   - Handle pagination for search results

3. Tests:
   - Mock HTTP server for unit tests
   - Rate limiter tests
   - Response parsing tests with sample JSON fixtures

### Step 2: Fanart.tv Adapter (#6)

**Package:** `internal/provider/fanart/`

- Endpoint: `http://webservice.fanart.tv/v3/music/{mbid}?api_key=...`
- Image types: artistthumb, artistbackground, hdmusiclogo, musicbanner, musiclogo
- Map to internal ImageResult with type classification
- API key from encrypted settings
- Populates: no separate ID (queries by MBID)
- Tests with mock responses

### Step 3: TheAudioDB Adapter (#7)

**Package:** `internal/provider/theaudiodb/`

- Search: `https://theaudiodb.com/api/v1/json/{apikey}/search.php?s=...`
- Fetch by MBID: `https://theaudiodb.com/api/v1/json/{apikey}/artist-mb.php?i=...`
- Extract: biography, images (thumb, fanart, logo, banner, wide thumb)
- Populates: AudioDBID (their internal numeric artist ID, e.g., `111239`)
- Tests with mock responses

### Step 4: Discogs Adapter (#8)

**Package:** `internal/provider/discogs/`

- Search: `https://api.discogs.com/database/search?type=artist&q=...`
- Fetch: `https://api.discogs.com/artists/{id}`
- Token-based auth (personal access token)
- Rate limiting per Discogs API rules
- Populates: DiscogsID (their numeric artist ID, e.g., `108713`)
- Tests with mock responses

### Step 5: Last.fm Adapter (#9)

**Package:** `internal/provider/lastfm/`

- Search: `http://ws.audioscrobbler.com/2.0/?method=artist.search&artist=...&api_key=...&format=json`
- Fetch: `artist.getinfo` method
- Extract: biography, tags (map to genres/styles), similar artists
- Populates: no separate ID (queries by artist name or MBID)
- Tests with mock responses

### Step 6: Wikidata Adapter (#10)

**Package:** `internal/provider/wikidata/`

- SPARQL query endpoint: `https://query.wikidata.org/sparql`
- Query by MBID (P434 property) or name
- Extract: formation date, dissolution date, members, aliases, country
- Populates: WikidataID (Q-item ID, e.g., `Q5026`)
- No authentication required
- Tests with mock SPARQL responses

### Step 7: Provider Priority Configuration (#11)

**Package:** `internal/provider/`

1. Define priority config in settings:

```go
type ProviderPriority struct {
    MetadataFields map[string][]string  // field -> ordered provider names
    ImageTypes     map[string][]string  // image type -> ordered provider names
}
```

2. Implement orchestrator in `internal/provider/orchestrator.go`:
   - `FetchMetadata(ctx, artist, field)` -- try providers in priority order
   - `FetchImages(ctx, artist, imageType)` -- aggregate from all providers, sorted by priority
   - Fallback chain: try next provider if current fails

3. Settings API:
   - Include provider priorities in `GET /api/v1/settings`
   - Update via `PUT /api/v1/settings`

4. Settings UI:
   - Priority list per field and image type
   - Reorder via up/down buttons or drag-and-drop

## Key Design Decisions

- **Rate limiting per provider:** Each adapter manages its own rate limiter. The orchestrator does not need global rate limiting.
- **API keys in encrypted settings:** Provider API keys are stored encrypted in the settings table using AES-256-GCM. Decrypted at runtime when making requests.
- **Graceful degradation:** If a provider is unavailable or returns an error, the orchestrator moves to the next provider in the priority chain. Errors are logged but do not fail the overall operation.
- **Mock-first testing:** All adapters tested against mock HTTP servers. Optional integration tests (behind build tags) for real API calls.

## Verification

- [ ] All 6 provider adapters pass unit tests
- [ ] Orchestrator fallback chain works correctly
- [ ] Priority configuration persists in settings
- [ ] Settings UI allows reordering providers
- [ ] `make lint` and `make test` pass
- [ ] Bruno collection updated with search endpoints
