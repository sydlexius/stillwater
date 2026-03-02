# Issue #321 -- Artist History Tab with Full Metadata Change Tracking

## Overview

Add a comprehensive metadata change tracking system with a History tab on the
artist detail page, image versioning, and rollback capability.

## Current State

- `nfo_snapshots` table tracks only full NFO XML content
- Existing diff page at `/artists/{id}/nfo` is a separate route, not a tab
- No record of which field changed, what triggered it, or who/what caused it
- Image replacements are destructive (no recovery)
- Band member and provider ID changes are not tracked

## Design

### Phase 1: Change Tracking Infrastructure

**New migration**: `metadata_changes` table

```sql
CREATE TABLE metadata_changes (
    id TEXT PRIMARY KEY,
    artist_id TEXT NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
    field TEXT NOT NULL,
    old_value TEXT NOT NULL DEFAULT '',
    new_value TEXT NOT NULL DEFAULT '',
    source TEXT NOT NULL DEFAULT 'manual',
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_metadata_changes_artist ON metadata_changes(artist_id, created_at DESC);
```

Field values for `source`:
- `manual` -- user edit via UI or API
- `provider:<name>` -- fetched from a metadata provider (e.g., `provider:musicbrainz`)
- `rule:<rule_id>` -- applied by the rule engine
- `scan` -- discovered during filesystem or library scan
- `import` -- bulk import operation

**New service**: `internal/artist/history.go`

- `HistoryService` struct with `*sql.DB` dependency
- `Record(ctx, artistID, field, oldValue, newValue, source) error`
- `List(ctx, artistID, limit, offset) ([]MetadataChange, error)`
- `GetByID(ctx, changeID) (*MetadataChange, error)`
- `RevertChange(ctx, changeID) error` -- creates inverse change record and applies

### Phase 2: Image Versioning

Before any image replacement or deletion, archive the existing file:

- Archive directory: `<dataDir>/image_archive/<artistID>/`
- Naming: `<imageType>_<timestamp>.<ext>` (e.g., `thumb_20260301T120000.jpg`)
- Add a new exported helper `filesystem.CopyFile` in `internal/filesystem` (wrapping the existing unexported `copyFile` in `atomic.go`) and use it for the archive copy
- Record the change in `metadata_changes` with field = image type (thumb, fanart, etc.)
- Old value = original path, new value = new path (or empty for deletion)

### Phase 3: History Tab UI

Add "History" tab to artist detail page alongside existing tabs (Images, Members,
NFO, Rules, etc.).

Template: `web/templates/artist_history.templ`

- Timeline view with newest changes first
- Each entry shows: timestamp (relative), source badge, field label, old/new values
- For long values (biography), truncate with expand toggle
- For image changes, show thumbnail previews of old/new
- Pagination via HTMX load-more button
- HTMX endpoint: `GET /api/v1/artists/{id}/history?limit=50&offset=0`

### Phase 4: Rollback

- `POST /api/v1/artists/{id}/history/{changeId}/revert`
- Creates a new change record with old/new swapped, source = `revert:<originalChangeId>`
- Applies the old value to the artist via `UpdateField`
- For images: restores from archive if available, returns error if archive missing
- UI: revert button on each change entry with confirmation modal

## Integration Points

All existing mutation code paths need change recording:

| Location | Mutation | Source Value |
|----------|----------|-------------|
| `handlers_field.go:handleFieldUpdate` | Field edit | `manual` |
| `handlers_field.go:handleFieldClear` | Field clear | `manual` |
| `handlers_field.go:handleSaveMembers` | Member changes | `manual` |
| `handlers_image.go` (fetch/crop/delete) | Image changes | `manual` or `provider:<name>` |
| `internal/rule/fixers.go` | Rule engine fixes | `rule:<rule_id>` |
| Provider fetch handlers | Provider data | `provider:<name>` |

## Key Files

### New Files
- `internal/database/migrations/NNN_metadata_changes.sql`
- `internal/artist/history.go`
- `internal/artist/history_test.go`
- `web/templates/artist_history.templ`
- `internal/image/archive.go`
- `internal/image/archive_test.go`

### Modified Files
- `internal/artist/service.go` -- inject HistoryService
- `internal/api/handlers_field.go` -- record changes
- `internal/api/handlers_artist.go` -- add history data to detail page
- `internal/api/handlers_image.go` -- archive before replace/delete
- `internal/api/routes.go` -- history API routes
- `web/templates/artist_detail.templ` -- add History tab

## Testing Strategy

- Unit tests for HistoryService: Record, List, GetByID, RevertChange
- Unit tests for image archiving with `t.TempDir()`
- Integration tests: field update records change, revert restores value
- API handler tests: GET history (pagination), POST revert
- Table-driven tests with subtests, stdlib `testing` only

## Open Questions

- Retention policy: should old changes be pruned after N days or N entries?
- Image archive size limits: should archives be capped per artist?
- Should the existing `nfo_snapshots` table be migrated into `metadata_changes`
  or kept as a parallel system?
