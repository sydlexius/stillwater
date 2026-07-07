-- +goose Up
-- Issue #2272: general one-time sweep of ORPHANED CHILD ROWS.
--
-- Root cause: FK enforcement was turned on by a post-Open PRAGMA on the pool
-- (EnableForeignKeys), which is a PER-CONNECTION setting that reverts OFF when
-- database/sql recycles a pooled connection. On such recycled connections the
-- artists ON DELETE CASCADE silently no-opped, so deleting/merging an artist
-- left its child rows behind, keyed to an artist_id with no surviving parent.
-- The code fix (OpenRuntime's foreign_keys(1) DSN pragma) enforces FK on every
-- connection going forward; this migration cleans up the orphans that already
-- accumulated on instances running the broken build (confirmed on live prod).
--
-- This is BROADER than migration 009. 009 removed orphan ARTIST rows (an artist
-- with no library membership and no platform anchor) and their children. 019
-- targets orphan CHILD rows directly: any row in a child table whose artist_id
-- points at an artists.id that no longer exists. It does not touch artists.
--
-- Child tables swept (every table declaring artist_id REFERENCES artists(id)
-- ON DELETE CASCADE, derived by grepping "REFERENCES artists(id)" across
-- 001_initial_schema.sql and 005_foreign_files.sql):
--
--   artist_libraries, artist_provider_ids, artist_images, artist_platform_ids,
--   artist_aliases, band_members, nfo_snapshots, metadata_changes, mb_snapshots,
--   rule_violations, rule_results, foreign_files, foreign_file_allowlist.
--
-- PRESERVATION: every DELETE is guarded by
--   artist_id IS NOT NULL AND artist_id NOT IN (SELECT id FROM artists)
-- so rows with a NULL artist_id (foreign_file_allowlist global-scope rows are
-- the only nullable case) and rows still keyed to a valid parent are untouched.
-- The IS NOT NULL guard is redundant for the NOT NULL columns but is stated
-- uniformly so the intent is unambiguous and the query is safe if a column's
-- nullability ever changes. Each statement is idempotent: a second run finds no
-- orphans and deletes nothing.

-- +goose StatementBegin
DELETE FROM artist_libraries
WHERE artist_id IS NOT NULL AND artist_id NOT IN (SELECT id FROM artists);
-- +goose StatementEnd

-- +goose StatementBegin
DELETE FROM artist_provider_ids
WHERE artist_id IS NOT NULL AND artist_id NOT IN (SELECT id FROM artists);
-- +goose StatementEnd

-- +goose StatementBegin
DELETE FROM artist_images
WHERE artist_id IS NOT NULL AND artist_id NOT IN (SELECT id FROM artists);
-- +goose StatementEnd

-- +goose StatementBegin
DELETE FROM artist_platform_ids
WHERE artist_id IS NOT NULL AND artist_id NOT IN (SELECT id FROM artists);
-- +goose StatementEnd

-- +goose StatementBegin
DELETE FROM artist_aliases
WHERE artist_id IS NOT NULL AND artist_id NOT IN (SELECT id FROM artists);
-- +goose StatementEnd

-- +goose StatementBegin
-- band_members.artist_id is the sole FK to artists(id); member_mbid is free-text.
DELETE FROM band_members
WHERE artist_id IS NOT NULL AND artist_id NOT IN (SELECT id FROM artists);
-- +goose StatementEnd

-- +goose StatementBegin
DELETE FROM nfo_snapshots
WHERE artist_id IS NOT NULL AND artist_id NOT IN (SELECT id FROM artists);
-- +goose StatementEnd

-- +goose StatementBegin
DELETE FROM metadata_changes
WHERE artist_id IS NOT NULL AND artist_id NOT IN (SELECT id FROM artists);
-- +goose StatementEnd

-- +goose StatementBegin
DELETE FROM mb_snapshots
WHERE artist_id IS NOT NULL AND artist_id NOT IN (SELECT id FROM artists);
-- +goose StatementEnd

-- +goose StatementBegin
DELETE FROM rule_violations
WHERE artist_id IS NOT NULL AND artist_id NOT IN (SELECT id FROM artists);
-- +goose StatementEnd

-- +goose StatementBegin
DELETE FROM rule_results
WHERE artist_id IS NOT NULL AND artist_id NOT IN (SELECT id FROM artists);
-- +goose StatementEnd

-- +goose StatementBegin
DELETE FROM foreign_files
WHERE artist_id IS NOT NULL AND artist_id NOT IN (SELECT id FROM artists);
-- +goose StatementEnd

-- +goose StatementBegin
-- foreign_file_allowlist.artist_id is nullable; global-scope rows have a NULL
-- artist_id and MUST be preserved. The IS NOT NULL guard does exactly that.
DELETE FROM foreign_file_allowlist
WHERE artist_id IS NOT NULL AND artist_id NOT IN (SELECT id FROM artists);
-- +goose StatementEnd

-- +goose Down
-- No rollback: rows deleted by this migration cannot be recovered without a
-- backup. The Down block is intentionally empty so that goose down does not
-- error, but the data is gone.
