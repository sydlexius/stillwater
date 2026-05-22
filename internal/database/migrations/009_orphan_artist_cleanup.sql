-- +goose Up
-- Issue #1613: garbage-collect orphan artists on library removal.
--
-- Service.Delete now prunes zero-home orphans inline within a transaction,
-- so new library removals will not leave orphans behind. This migration is
-- a one-time sweep for orphan artist rows that accumulated on instances
-- created before this fix (a UAT instance carried ~508 such rows).
--
-- An orphan artist is one with:
--   - zero rows in artist_libraries  (no library membership), AND
--   - zero rows in artist_platform_ids  (no platform/connection anchor).
--
-- IMPORTANT: goose migrations run BEFORE EnableForeignKeys is called on the
-- live database, so ON DELETE CASCADE does NOT fire here. Each child table
-- that references artists(id) must be deleted explicitly before the artist
-- row is removed. The child-table list was derived by grepping
-- "REFERENCES artists(id)" in 001_initial_schema.sql:
--
--   artist_libraries, artist_provider_ids, artist_images, artist_platform_ids,
--   artist_aliases, band_members, nfo_snapshots, metadata_changes, mb_snapshots,
--   rule_violations, rule_results, foreign_files, foreign_file_allowlist.
--
-- foreign_file_allowlist.artist_id is nullable (scope='global' rows have
-- NULL); the DELETE below is scoped to rows where artist_id IS NOT NULL,
-- which is correct -- global-scope rows have no artist_id and must not be
-- touched.

-- +goose StatementBegin
-- Capture orphan artist IDs into a temporary table so every subsequent
-- per-child-table DELETE targets exactly the same set, even if concurrent
-- activity changes the live tables between statements (unlikely in SQLite's
-- serialized write model, but explicit is safer and matches goose's
-- StatementBegin/End multi-statement approach).
CREATE TEMP TABLE _orphan_artists AS
SELECT id
FROM artists
WHERE id NOT IN (SELECT DISTINCT artist_id FROM artist_libraries)
  AND id NOT IN (SELECT DISTINCT artist_id FROM artist_platform_ids);
-- +goose StatementEnd

-- +goose StatementBegin
DELETE FROM artist_libraries
WHERE artist_id IN (SELECT id FROM _orphan_artists);
-- +goose StatementEnd

-- +goose StatementBegin
DELETE FROM artist_provider_ids
WHERE artist_id IN (SELECT id FROM _orphan_artists);
-- +goose StatementEnd

-- +goose StatementBegin
DELETE FROM artist_images
WHERE artist_id IN (SELECT id FROM _orphan_artists);
-- +goose StatementEnd

-- +goose StatementBegin
DELETE FROM artist_platform_ids
WHERE artist_id IN (SELECT id FROM _orphan_artists);
-- +goose StatementEnd

-- +goose StatementBegin
DELETE FROM artist_aliases
WHERE artist_id IN (SELECT id FROM _orphan_artists);
-- +goose StatementEnd

-- +goose StatementBegin
-- band_members.artist_id is the sole FK to artists(id); member_mbid is free-text.
DELETE FROM band_members
WHERE artist_id IN (SELECT id FROM _orphan_artists);
-- +goose StatementEnd

-- +goose StatementBegin
DELETE FROM nfo_snapshots
WHERE artist_id IN (SELECT id FROM _orphan_artists);
-- +goose StatementEnd

-- +goose StatementBegin
DELETE FROM metadata_changes
WHERE artist_id IN (SELECT id FROM _orphan_artists);
-- +goose StatementEnd

-- +goose StatementBegin
DELETE FROM mb_snapshots
WHERE artist_id IN (SELECT id FROM _orphan_artists);
-- +goose StatementEnd

-- +goose StatementBegin
DELETE FROM rule_violations
WHERE artist_id IN (SELECT id FROM _orphan_artists);
-- +goose StatementEnd

-- +goose StatementBegin
DELETE FROM rule_results
WHERE artist_id IN (SELECT id FROM _orphan_artists);
-- +goose StatementEnd

-- +goose StatementBegin
DELETE FROM foreign_files
WHERE artist_id IN (SELECT id FROM _orphan_artists);
-- +goose StatementEnd

-- +goose StatementBegin
-- foreign_file_allowlist.artist_id is nullable; only delete rows scoped to
-- a specific artist (scope='artist'), not global-scope rows (artist_id IS NULL).
DELETE FROM foreign_file_allowlist
WHERE artist_id IN (SELECT id FROM _orphan_artists);
-- +goose StatementEnd

-- +goose StatementBegin
DELETE FROM artists
WHERE id IN (SELECT id FROM _orphan_artists);
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS _orphan_artists;
-- +goose StatementEnd

-- +goose Down
-- No rollback: rows deleted by this migration cannot be recovered without
-- a backup. The Down block is intentionally empty so that goose down does
-- not error, but the data is gone.
