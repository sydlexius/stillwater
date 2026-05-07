-- +goose Up
-- Issue #1214: drop the legacy artists.library_id column and its index.
-- Membership of record now lives exclusively in artist_libraries (the M:N
-- table introduced in #1004). Backfill any pre-1004 rows whose library_id
-- column still carries a non-empty value before dropping the column, so a
-- DB that has not yet had ensureArtistLibrariesMembership run also lands
-- with full memberships.
--
-- Issue #1224: add a case-insensitive index on artist name to make
-- LOWER(name) lookups (GetByName, FindByMBIDOrNameUnscoped) index-driven
-- instead of running a full scan on the artists table.
--
-- Issue #1018: normalize legacy metadata_changes.created_at rows from the
-- SQLite "YYYY-MM-DD HH:MM:SS" space-separator format to RFC3339
-- ("YYYY-MM-DDTHH:MM:SSZ") so range comparisons work without datetime()
-- wrappers.

-- +goose StatementBegin
-- Step 1: backfill artist_libraries from any remaining pre-1004 rows. The
-- INSERT OR IGNORE preserves any existing memberships so an already-
-- migrated DB is a no-op. The join through libraries skips rows whose
-- library_id no longer references a real library.
INSERT OR IGNORE INTO artist_libraries (artist_id, library_id, source, added_at)
SELECT
    a.id,
    a.library_id,
    CASE
        WHEN c.type = 'emby'     THEN 'emby'
        WHEN c.type = 'jellyfin' THEN 'jellyfin'
        WHEN c.type = 'lidarr'   THEN 'lidarr'
        ELSE 'filesystem'
    END,
    a.created_at
FROM artists a
JOIN libraries l ON l.id = a.library_id
LEFT JOIN connections c ON c.id = l.connection_id
WHERE a.library_id IS NOT NULL AND a.library_id != '';
-- +goose StatementEnd

-- Step 2: drop the supporting index. SQLite tolerates DROP INDEX IF NOT
-- EXISTS only via IF EXISTS; this matches the column-drop's IF semantics.
DROP INDEX IF EXISTS idx_artists_library_id;

-- Step 3: drop the column. SQLite 3.35+ supports DROP COLUMN; modernc
-- ships well past that.
ALTER TABLE artists DROP COLUMN library_id;

-- Step 4: case-insensitive lookup index for #1224. SQLite supports
-- expression indexes; LOWER(name) lookups now hit this directly.
CREATE INDEX IF NOT EXISTS idx_artists_name_lower ON artists(LOWER(name));

-- +goose StatementBegin
-- Step 5: normalize metadata_changes.created_at to RFC3339 for any legacy
-- row stored in the "YYYY-MM-DD HH:MM:SS" space-separator format. The
-- LIKE pattern matches "YYYY-MM-DD" + space + "HH:MM:SS" exactly: 10
-- digits/dashes, one space, 8 digits/colons. Rows already in RFC3339
-- format ("T" separator) skip the rewrite. The replace + 'Z' suffix is
-- safe because legacy rows were always in UTC.
UPDATE metadata_changes
SET created_at = REPLACE(created_at, ' ', 'T') || 'Z'
WHERE created_at LIKE '____-__-__ __:__:__';
-- +goose StatementEnd

-- +goose Down
-- Restore the column (data loss acceptable: ensureArtistLibrariesMembership
-- runs at startup and re-derives memberships from artist_libraries when
-- needed). Drop the lower-name index. The metadata_changes normalization
-- is one-way; callers that depend on legacy parsing have parseHistoryTimestamp
-- which already handles both formats.
ALTER TABLE artists ADD COLUMN library_id TEXT REFERENCES libraries(id) DEFAULT NULL;
CREATE INDEX IF NOT EXISTS idx_artists_library_id ON artists(library_id);
DROP INDEX IF EXISTS idx_artists_name_lower;
