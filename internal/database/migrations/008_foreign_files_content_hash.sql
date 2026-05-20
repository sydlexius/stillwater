-- +goose Up
-- Adds content_hash to foreign_files and foreign_file_allowlist so
-- detection and allowlisting key on the file's byte sequence (sha256 hex)
-- rather than its basename. Two distinct files that happen to share a
-- name like "poster.jpg" are no longer conflated when one is allowlisted.
--
-- content_hash is nullable: pre-existing rows have no hash recorded and
-- are populated lazily on the first post-migration scan (the scanner
-- recomputes the hash from disk and updates the row via UPSERT). Rows
-- written by every code path after this migration always carry the hash.

ALTER TABLE foreign_files ADD COLUMN content_hash TEXT;
ALTER TABLE foreign_file_allowlist ADD COLUMN content_hash TEXT;

-- Re-detection lookup index: scoped queries match (scope, artist_id,
-- content_hash) so the per-file scan path stays cheap.
CREATE INDEX IF NOT EXISTS idx_foreign_allowlist_hash
    ON foreign_file_allowlist(scope, artist_id, content_hash);

-- Partial unique indexes pinned to content_hash so identical byte
-- sequences cannot be allowlisted twice in the same scope. file_name is
-- still stored on the row for human readability in the UI but no longer
-- participates in dedupe. The old file_name unique indexes are dropped
-- so two distinct files sharing a basename can each have their own row.
DROP INDEX IF EXISTS idx_foreign_allowlist_global;
DROP INDEX IF EXISTS idx_foreign_allowlist_artist;
CREATE UNIQUE INDEX IF NOT EXISTS idx_foreign_allowlist_global_hash
    ON foreign_file_allowlist(content_hash)
    WHERE scope = 'global' AND content_hash IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_foreign_allowlist_artist_hash
    ON foreign_file_allowlist(artist_id, content_hash)
    WHERE scope = 'artist' AND content_hash IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_foreign_allowlist_artist_hash;
DROP INDEX IF EXISTS idx_foreign_allowlist_global_hash;
DROP INDEX IF EXISTS idx_foreign_allowlist_hash;
CREATE UNIQUE INDEX IF NOT EXISTS idx_foreign_allowlist_global
    ON foreign_file_allowlist(file_name) WHERE scope = 'global';
CREATE UNIQUE INDEX IF NOT EXISTS idx_foreign_allowlist_artist
    ON foreign_file_allowlist(artist_id, file_name) WHERE scope = 'artist';
-- SQLite cannot drop columns prior to 3.35; the columns remain in place
-- on rollback, which is benign because the new readers ignore unknown
-- columns and the old readers never reference content_hash.
