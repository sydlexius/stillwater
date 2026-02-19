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

## Implementation Order

### Step 1: Artist Model and Repository (#1)

**Packages:** `internal/artist/`, `internal/database/`

1. Define `Artist` struct in `internal/artist/model.go` with all NFO fields:
   - Name, MBID, SortName, Type, Gender, Disambiguation
   - Genres, Styles, Moods (slice fields stored as JSON in SQLite)
   - YearsActive, Born, Formed, Died, Disbanded
   - Biography, Path
   - Timestamps (CreatedAt, UpdatedAt)

2. Define `Repository` interface in `internal/artist/repository.go`:
   - `Create(ctx, *Artist) error`
   - `GetByID(ctx, id) (*Artist, error)`
   - `GetByMBID(ctx, mbid) (*Artist, error)`
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

5. Tests:
   - `internal/database/artist_repo_test.go` -- integration tests with temp SQLite DB
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

3. Conversion functions:
   - `ToArtist(*ArtistNFO) *artist.Artist` -- NFO to domain model
   - `FromArtist(*artist.Artist) *ArtistNFO` -- domain model to NFO

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

## Migration

No new migration needed -- the `artists` table was created in 001_initial.sql. If schema changes are needed (e.g., adding columns for image detection cache), create `002_*.sql`.

## Key Design Decisions

- **Slice fields as JSON:** Genres, styles, and moods are stored as JSON arrays in SQLite TEXT columns. This avoids junction tables for simple tag-like data.
- **NFO round-trip fidelity:** The parser should preserve unrecognized XML elements so that writing an NFO back does not lose custom data added by other tools.
- **Scanner is read-only:** The filesystem scanner only reads and reports. It does not modify any files. Write operations are handled by explicit user actions in later milestones.
- **Compliance status is computed:** Not stored separately. Computed on-the-fly based on detected files and NFO content.

## Verification

- [ ] `go test ./internal/artist/... ./internal/nfo/... ./internal/scanner/... ./internal/database/...` passes
- [ ] `make lint` passes
- [ ] Artist list page renders with sample data
- [ ] Scanner discovers artists from a test directory
- [ ] NFO parser handles sample artist.nfo files correctly
- [ ] Bruno collection requests return expected responses
