# Issue #322 -- Universal Field Editing and Display

## Overview

Extend the field editing system to support all 19 metadata fields, including
name, sort_name, disambiguation, and all 5 provider IDs. Ensure all fields are
always visible on the artist detail page, even when empty.

## Current State

### Editable fields (11 of 19)

biography, genres, styles, moods, formed, born, disbanded, died, years_active,
type, gender

### Not editable (8 fields)

| Field | DB Column | Notes |
|-------|-----------|-------|
| name | name | Core identity field |
| sort_name | sort_name | Used for alphabetical ordering |
| disambiguation | disambiguation | MusicBrainz can provide this |
| musicbrainz_id | musicbrainz_id | UUID format, primary external ID |
| audiodb_id | audiodb_id | Numeric ID |
| discogs_id | discogs_id | Numeric ID |
| wikidata_id | wikidata_id | Q-number format (e.g., Q2831) |
| deezer_id | deezer_id | Numeric ID |

### Display gaps

Provider IDs are rendered as read-only `<dd>` elements in `artist_detail.templ`.
Some fields are hidden entirely when empty.

## Design

### 1. Extend fieldColumnMap

Add all 8 new fields to `fieldColumnMap` in `internal/artist/service.go`. This
is the gating map that determines which fields can be updated via the API.

### 2. Extend FieldValueFromArtist

Add cases for name, sort_name, disambiguation, and all 5 provider IDs to the
switch statement. This enables the field display component to read current values.

### 3. Validation

New `validateFieldUpdate(field, value string) error` function:

- `name`: must not be empty after trimming
- `musicbrainz_id`: if non-empty, must be a valid UUID
- `audiodb_id`, `discogs_id`, `deezer_id`: if non-empty, must be numeric
- `wikidata_id`: if non-empty, must match `Q\d+` pattern
- Other fields: no additional validation (existing behavior)

Call `validateFieldUpdate` in `handleFieldUpdate` before `UpdateField`.

### 4. Template changes

Replace read-only provider ID display in `artist_detail.templ` with
`@FieldDisplay()` components. This gives provider IDs the same Edit/Clear/Fetch
actions as other fields.

Ensure all fields render even when empty -- the existing `FieldDisplay` component
already shows "Not set" in italic gray for empty values.

For populated provider IDs, render as clickable links to the external source:
- MusicBrainz: `https://musicbrainz.org/artist/<id>`
- AudioDB: `https://www.theaudiodb.com/artist/<id>`
- Discogs: `https://www.discogs.com/artist/<id>`
- Wikidata: `https://www.wikidata.org/wiki/<id>`
- Deezer: `https://www.deezer.com/artist/<id>`

### 5. sort_name auto-update

When `name` is edited via `handleFieldUpdate`:
1. Check if current `sort_name` equals the old name value
2. If so, update `sort_name` to match the new name automatically
3. If not, leave `sort_name` unchanged (user has customized it)

This is a server-side side effect, not a UI prompt.

### 6. NFO write-back

The existing NFO writer already maps name, sort_name, disambiguation, and
musicbrainz_id to XML elements. No changes needed for those.

Discogs, Wikidata, and Deezer IDs are stored as unknown XML elements and
preserved during round-trips. If we want first-class NFO support for these,
we would need to add them to the `Artist` NFO struct -- but this is optional
since they are not part of the Kodi NFO spec.

## Key Files

### Modified Files
- `internal/artist/service.go` -- extend `fieldColumnMap`, `FieldValueFromArtist`; add `validateFieldUpdate`
- `internal/artist/service_test.go` -- tests for new fields and validation
- `internal/api/handlers_field.go` -- call `validateFieldUpdate` before `UpdateField`
- `internal/api/handlers_field_test.go` -- API tests for new fields and validation errors
- `web/templates/artist_detail.templ` -- replace read-only provider IDs with `@FieldDisplay`
- `web/templates/artist_field.templ` -- add external link rendering for provider IDs

## Testing Strategy

- Unit tests for `validateFieldUpdate`:
  - Empty name rejected
  - Invalid MBID rejected (not a UUID)
  - Valid MBID accepted
  - Invalid numeric IDs rejected for audiodb/discogs/deezer
  - Invalid Wikidata ID rejected (missing Q prefix)
  - Valid values accepted for all new fields
- Unit tests for `FieldValueFromArtist` new cases
- Integration test: update provider ID via API, verify DB and NFO
- Integration test: update name, verify sort_name auto-updated when matching
- API handler test: PATCH with invalid UUID returns 400
- All tests use stdlib `testing` with table-driven subtests

## Acceptance Criteria

- [ ] All 19 fields editable via PATCH `/api/v1/artists/{id}/fields/{field}`
- [ ] All fields visible on artist detail page, even when empty
- [ ] Provider IDs display as editable fields with Edit/Clear/Fetch actions
- [ ] Name validates non-empty; MBID validates UUID format
- [ ] Provider ID fields link to external sources when populated
- [ ] sort_name auto-updates when name changes and sort_name matched old name
