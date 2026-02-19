# M5: Rule Engine (v0.5.0)

## Goal

Implement the Bliss-inspired rule engine for defining compliance rules, evaluating artists, auto-fixing violations, and running bulk operations.

## Prerequisites

- M2 complete (artist model, NFO parser, scanner)
- M3 complete (provider adapters for auto-fix sources)
- M4 complete (image processing for image-related rules)

## Issues

| # | Title | Mode | Model |
|---|-------|------|-------|
| 18 | Rule definition schema and evaluation engine | plan | opus |
| 19 | Auto-fix pipeline with source priority chains | plan | sonnet |
| 20 | Bulk operations with configurable modes | plan | opus |
| 44 | Classical music directory support | plan | sonnet |

## Implementation Order

### Step 1: Rule Definition and Evaluation (#18)

**Package:** `internal/rule/`

1. Define rule model in `internal/rule/model.go`:

```go
type Rule struct {
    ID          string
    Name        string
    Description string
    Category    string      // "nfo", "image", "metadata"
    Enabled     bool
    Criteria    Criteria    // acceptance thresholds
    CreatedAt   time.Time
    UpdatedAt   time.Time
}

type Criteria struct {
    MinResolution   int     // minimum pixel dimension
    MaxResolution   int     // maximum pixel dimension
    MaxFileSize     int64   // bytes
    AspectRatio     float64 // expected ratio (e.g., 1.0 for square)
    AspectTolerance float64 // tolerance percentage (e.g., 0.05 for 5%)
}

type Violation struct {
    RuleID    string
    ArtistID  string
    Details   string
    Severity  string  // "error", "warning"
    FixAction string  // suggested fix type
}
```

2. Implement rule evaluator in `internal/rule/evaluator.go`:
   - `Evaluate(ctx, artist, rules) []Violation`
   - Each built-in rule is a function: `func(artist, criteria) *Violation`

3. Built-in rules:
   - `rule_nfo_exists` -- artist.nfo must exist in directory
   - `rule_nfo_has_mbid` -- musicbrainzartistid field must be populated
   - `rule_thumb_exists` -- thumbnail image must exist
   - `rule_thumb_square` -- thumbnail must be approximately 1:1
   - `rule_thumb_min_res` -- thumbnail must meet minimum resolution
   - `rule_fanart_exists` -- fanart image must exist
   - `rule_logo_exists` -- logo image must exist
   - `rule_bio_exists` -- biography field must be populated

4. Rule repository (CRUD):
   - `internal/database/rule_repo.go`
   - Seed built-in rules on first migration/startup

5. API endpoints:
   - `GET /api/v1/rules` -- list all rules with enabled/disabled status
   - `PUT /api/v1/rules/{id}` -- enable/disable, update criteria

6. Rules management UI:
   - Toggle switches for each rule
   - Criteria configuration per rule
   - Description and category grouping

### Step 2: Auto-Fix Pipeline (#19)

**Package:** `internal/rule/`

1. Define fixer interface in `internal/rule/fixer.go`:

```go
type Fixer interface {
    CanFix(violation Violation) bool
    Fix(ctx context.Context, violation Violation) (*FixResult, error)
}

type FixResult struct {
    Success   bool
    Action    string  // what was done
    Source    string  // which provider was used
    Details   string
}
```

2. Implement fixers for each rule:
   - `fix_nfo_create` -- create NFO from provider metadata
   - `fix_mbid_lookup` -- search MusicBrainz by artist name
   - `fix_thumb_fetch` -- fetch from image provider priority chain
   - `fix_thumb_crop` -- crop existing image to correct aspect ratio
   - `fix_thumb_upgrade` -- fetch higher-res from providers
   - `fix_fanart_fetch` -- fetch from Fanart.tv / TheAudioDB
   - `fix_logo_fetch` -- fetch from Fanart.tv
   - `fix_bio_fetch` -- fetch from MusicBrainz / Last.fm / Wikidata

3. Fix orchestrator:
   - Uses provider priority chain from settings
   - Tries sources in order until success
   - Validates fix result against acceptance criteria
   - Logs all attempts

4. API endpoints:
   - `POST /api/v1/rules/{id}/run` -- run single rule against all artists
   - `POST /api/v1/rules/run-all` -- evaluate all enabled rules

### Step 3: Bulk Operations (#20)

**Packages:** `internal/rule/`, `internal/scanner/`

1. Define operation modes:

```go
type BulkMode string
const (
    ModeYOLO         BulkMode = "yolo"          // auto-accept everything
    ModePromptNoMatch BulkMode = "prompt_no_match" // auto-accept single matches
    ModeDisambiguate  BulkMode = "disambiguate"   // always prompt on multiple
    ModeManual        BulkMode = "manual"         // never auto-accept
)
```

2. Implement bulk job system:
   - Job queue with status tracking (pending, running, completed, failed)
   - Per-artist progress within a job
   - Cancellation support

3. Disambiguation helpers:
   - Examine subdirectories for album names (directory listing)
   - Read ID3 tags from audio files for existing MBIDs
   - Show all candidates with links to MusicBrainz/Discogs pages
   - Display confidence score for each candidate

4. API endpoints:
   - `POST /api/v1/bulk/fetch-metadata` -- start bulk metadata fetch
   - `POST /api/v1/bulk/fetch-images` -- start bulk image fetch
   - `GET /api/v1/bulk/jobs/{id}` -- check job status/progress

5. Bulk action UI:
   - Select all / select filtered on artist list
   - Bulk action toolbar: Fetch Metadata, Fetch Images, Run Rules
   - Mode selector dropdown
   - Progress indicator with per-artist status
   - Disambiguation modal for ambiguous matches

## Key Design Decisions

- **Rules are data, not code:** Rule definitions (enabled/disabled, criteria) are stored in the database. The evaluation logic is code, but configuration is data-driven.
- **Fixers are composable:** Each fixer handles one violation type. The orchestrator chains them based on provider priorities.
- **Bulk jobs are async:** Long-running bulk operations run in background goroutines. The API returns a job ID for status polling.
- **Disambiguation is human-assisted:** Even in YOLO mode, truly ambiguous cases (multiple equally-scored matches) are logged. The UI provides tools to help humans make the right choice.
- **Adaptive batched transactions:** Small batches (< 100) use a single transaction per batch. Medium batches (100-1000) use transactions of 50 items. Large batches (1000+) use transactions of 25 items with short sleep between batches. User-initiated actions get priority over background jobs. Progress indicators are always shown regardless of batch size.
- **Classical music support:** Directories designated as "classical" (from M2 scanner) get special rule evaluation. A user preference controls whether metadata/images target the composer or the performer/album artist.

## Verification

- [ ] All built-in rules evaluate correctly
- [ ] Auto-fix resolves violations using provider chain
- [ ] Bulk operations respect mode settings
- [ ] Job status tracking works correctly
- [ ] Disambiguation modal shows relevant context
- [ ] Cancellation stops running jobs cleanly
- [ ] `make test` and `make lint` pass
- [ ] Bruno collection updated
