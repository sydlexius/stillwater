# M2: Core Data Model and Scanner (v0.2.0)

## Goal

Establish the artist domain model, NFO parser, filesystem scanner, and artist list UI. This milestone produces the foundational data layer that all subsequent milestones depend on.

## Prerequisites

- M1 complete (database, auth, config, API routing, Templ/HTMX/Tailwind)

## Issues

| # | Title | Mode | Model |
|---|-------|------|-------|
| 1 | Artist database model and repository layer | plan | sonnet |
| 2 | Filesystem scanner for artist directories | plan | sonnet |
| 3 | NFO parser: read and write Kodi-compatible artist.nfo | plan | sonnet |
| 4 | Artist list UI with compliance indicators | plan | sonnet |
| 35 | Band member metadata: store and display artist members | direct | sonnet |
| 40 | Artist matching: ID-first approach with configurable priority | plan | sonnet |
| 41 | Atomic filesystem writes utility | plan | sonnet |
| 42 | Scanner exclusions and special directory types | plan | sonnet |
| 43 | HTMX error fragment templates | direct | sonnet |

## Implementation Order

### Step 1: Artist Model and Repository (#1)

**Packages:** `internal/artist/`, `internal/database/`

1. Define `Artist` struct in `internal/artist/model.go` with all NFO fields:
   - Name, SortName, Type, Gender, Disambiguation
   - Genres, Styles, Moods (slice fields stored as JSON in SQLite)
   - YearsActive, Born, Formed, Died, Disbanded
   - Biography, Path
   - Provider IDs (dedicated columns, indexed for lookups):
     - MusicBrainzID (UUID, primary cross-reference for all providers)
     - AudioDBID (TheAudioDB numeric ID)
     - DiscogsID (Discogs numeric artist ID)
     - WikidataID (Q-item ID, e.g., Q5026)
     - Note: Last.fm and Fanart.tv use artist name or MBID as their key, no separate IDs needed
   - Timestamps (CreatedAt, UpdatedAt)

2. Define `Repository` interface in `internal/artist/repository.go`:
   - `Create(ctx, *Artist) error`
   - `GetByID(ctx, id) (*Artist, error)`
   - `GetByMBID(ctx, mbid) (*Artist, error)`
   - `GetByProviderID(ctx, provider, id) (*Artist, error)` -- generic lookup by any provider ID
   - `GetByPath(ctx, path) (*Artist, error)`
   - `List(ctx, ListParams) ([]Artist, int, error)` -- with pagination, filtering, sorting
   - `Update(ctx, *Artist) error`
   - `Delete(ctx, id) error`
   - `Search(ctx, query) ([]Artist, error)`

3. Implement SQLite repository in `internal/database/artist_repo.go`:
   - Use the existing `artists` table from 001_initial.sql
   - JSON encoding for slice fields (genres, styles, moods)
   - Parameterized queries (no SQL injection)

4. Wire into `cmd/stillwater/main.go` and `internal/api/router.go`:
   - Create artist repository in `run()`
   - Pass to router for handler use

5. Define `BandMember` model (#35) in `internal/artist/member.go`:
   - Fields: ID, ArtistID (the group), MemberName, MemberMBID, Instruments ([]string),
     VocalType, DateJoined, DateLeft, IsOriginalMember, SortOrder
   - Migration (002) to create `band_members` table with FK to artists, indexes on artist_id and member_mbid
   - Repository methods: `ListMembersByArtistID(ctx, artistID) ([]BandMember, error)`,
     `UpsertMembers(ctx, artistID, []BandMember) error`

6. Tests:
   - `internal/database/artist_repo_test.go` -- integration tests with temp SQLite DB
   - `internal/database/member_repo_test.go` -- member repository tests
   - `internal/artist/model_test.go` -- validation tests

### Step 2: NFO Parser (#3)

**Package:** `internal/nfo/`

1. Define NFO XML struct matching Kodi artist.nfo schema in `internal/nfo/model.go`

2. Implement parser in `internal/nfo/parser.go`:
   - `Parse(reader io.Reader) (*ArtistNFO, error)` -- read XML
   - `Write(writer io.Writer, *ArtistNFO) error` -- write XML
   - Handle UTF-8 BOM, HTML entities in biography text
   - Support multiple `<genre>`, `<style>`, `<mood>` elements
   - Support `<thumb aspect="...">` and `<fanart><thumb>` elements
   - Preserve unrecognized elements (round-trip fidelity)
   - Provider IDs in NFO (read and write):
     - `<musicbrainzartistid>` -- read/write (universal: Kodi, Jellyfin, Emby)
     - `<audiodbartistid>` -- read/write (Jellyfin + Emby; Kodi ignores harmlessly)
     - Discogs, Wikidata, Last.fm IDs have no NFO elements in any platform -- store in DB only

3. Conversion functions:
   - `ToArtist(*ArtistNFO) *artist.Artist` -- NFO to domain model (maps both ID elements)
   - `FromArtist(*artist.Artist) *ArtistNFO` -- domain model to NFO (writes both ID elements)

4. Tests:
   - `internal/nfo/parser_test.go` with sample NFO fixtures
   - Round-trip tests (parse then write, compare)
   - Edge cases: malformed XML, missing fields, BOM handling

### Step 3: Filesystem Scanner (#2)

**Package:** `internal/scanner/`

1. Implement scanner in `internal/scanner/filesystem.go`:
   - Walk configured music directory (from config.Music.Path)
   - Identify artist-level directories (configurable depth, default: 1 level)
   - For each artist directory, detect:
     - `artist.nfo` presence
     - Image files: folder.jpg, artist.jpg, poster.jpg, fanart.jpg, backdrop.jpg, logo.png, banner.jpg
   - Compare discovered artists against database state
   - Return scan results: new artists, changed artists, removed artists

2. Define `ScanResult` struct:
   - NewArtists, UpdatedArtists, RemovedArtists counts
   - Per-artist details (path, detected files, compliance status)

3. API endpoints:
   - `POST /api/v1/scanner/run` -- trigger scan (async, returns job ID)
   - `GET /api/v1/scanner/status` -- check scan progress/results

4. Wire into router with auth middleware

5. Tests:
   - `internal/scanner/filesystem_test.go` with temp directory fixtures
   - Test with nested directory structures
   - Test image detection for all naming variants

### Step 4: Artist List UI (#4)

**Templates:** `web/templates/`

1. Create `artists.templ` -- artist list page:
   - Table with columns: Name, NFO, Thumb, Fanart, Logo, MBID
   - Status badges (green check, yellow warning, red missing)
   - Pagination controls (HTMX-powered)
   - Sort headers (click to sort by column)
   - Search/filter bar (HTMX search-as-you-type)

2. Create `artist_detail.templ` -- individual artist page:
   - Display all metadata fields
   - Show detected images
   - Band members section (#35): list members with name, instruments, vocal type, years active, original member badge (populated in M3 via MusicBrainz)
   - Action buttons (placeholder for future milestones)

3. Create status badge component in `web/components/`:
   - Reusable compliance status indicator
   - Tooltip with specifics (e.g., "missing: biography, fanart")

4. API handler updates:
   - `GET /api/v1/artists` -- paginated list with compliance status
   - `GET /api/v1/artists/{id}` -- single artist detail

5. Update navbar to link to /artists

6. Bruno collection:
   - Add artists list and detail requests

### Step 5: Atomic Filesystem Writes (#41)

**Package:** `internal/filesystem/`

1. Implement shared write utility in `internal/filesystem/atomic.go`:
   - Write to `<target>.tmp` in the target directory
   - Rename existing `<target>` to `<target>.bak`
   - Rename `<target>.tmp` to `<target>`
   - Delete `.bak` on success
   - Fall back to copy+delete with fsync if rename fails (cross-mount/network share)

2. All file write operations (NFO, images) must use this utility.

3. Tests:
   - `internal/filesystem/atomic_test.go` -- test normal write, interrupted write recovery, cross-mount fallback

### Step 6: Scanner Exclusions (#42)

**Package:** `internal/scanner/`

1. Add configurable skip list to scanner:
   - Default exclusions: "Various Artists", "Various", "VA", "Soundtrack", "OST"
   - Configurable via settings API

2. Excluded directories:
   - Still appear in artist list but greyed out and marked unfetchable
   - Users can supply local placeholder images/values

3. Classical music directory designation:
   - Allow marking a directory as "classical" (full support in M5)
   - Store designation in settings or artist metadata

### Step 7: HTMX Error Fragment Templates (#43)

**Templates:** `web/components/`

1. Create `web/components/error_toast.templ` -- toast notification for transient errors
2. Create `web/components/error_inline.templ` -- inline error message for form/action failures
3. Configure `hx-on::response-error` globally in layout template
4. Return proper HTTP status codes; HTMX swaps error fragments for 4xx/5xx responses
5. Network timeout fallback via HTMX `htmx:timeout` event

### Step 8: Artist Matching Foundation (#40)

**Package:** `internal/artist/`

1. Implement ID-first matching logic:
   - When an MBID is available (from Lidarr, existing NFO, embedded tags), use it directly
   - Skip name matching when a trusted ID is present

2. User-configurable matching priority setting:
   - "Prefer ID match" (default), "Prefer name match", "Always prompt"

3. Minimum confidence floor:
   - Configurable threshold below which matches are never auto-accepted
   - Even in YOLO mode, log and skip low-confidence matches

4. Note: Album-based disambiguation and preponderance of evidence features depend on M3 and M5 for full implementation but the matching infrastructure is laid here.

## Migration

The `artists` table in 001_initial.sql includes provider ID columns (musicbrainz_id, audiodb_id, discogs_id, wikidata_id) with indexes. A new 002 migration adds the `band_members` table.

## Key Design Decisions

- **Slice fields as JSON:** Genres, styles, and moods are stored as JSON arrays in SQLite TEXT columns. This avoids junction tables for simple tag-like data.
- **Provider IDs as dedicated columns:** Each provider with its own ID system gets a dedicated indexed column on the artists table (musicbrainz_id, audiodb_id, discogs_id, wikidata_id). This enables fast lookups to avoid duplicate API calls and supports cross-referencing. Last.fm and Fanart.tv key off artist name or MBID, so they need no separate ID column.
- **NFO ID elements:** The parser reads/writes `<musicbrainzartistid>` (universal: Kodi, Jellyfin, Emby) and `<audiodbartistid>` (Jellyfin + Emby; Kodi ignores it harmlessly). Discogs and Wikidata IDs are stored in the database only since no platform reads them from NFO files today.
- **NFO round-trip fidelity:** The parser should preserve unrecognized XML elements so that writing an NFO back does not lose custom data added by other tools.
- **Scanner is read-only:** The filesystem scanner only reads and reports. It does not modify any files. Write operations are handled by explicit user actions in later milestones.
- **Compliance status is computed:** Not stored separately. Computed on-the-fly based on detected files and NFO content.
- **Atomic writes for all file operations:** All NFO and image file writes use the shared atomic write utility (tmp/bak/rename pattern) to prevent corruption from crashes or interruptions.
- **Scanner exclusions are configurable:** Default skip list for Various Artists, Soundtracks, etc. Excluded directories still appear but are greyed out and unfetchable.
- **HTMX error handling from the start:** Error toast and inline error templates are created in this milestone alongside the first real HTMX interactions.
- **ID-first matching:** When a trusted provider ID is available, use it directly. Skip name-based matching to avoid ambiguity.

## Verification

- [ ] `go test ./internal/artist/... ./internal/nfo/... ./internal/scanner/... ./internal/database/...` passes
- [ ] `make lint` passes
- [ ] Artist list page renders with sample data
- [ ] Scanner discovers artists from a test directory
- [ ] NFO parser handles sample artist.nfo files correctly
- [ ] Bruno collection requests return expected responses
