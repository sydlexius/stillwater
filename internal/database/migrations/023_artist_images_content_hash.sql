-- +goose Up
-- Adds content_hash to artist_images: the sha256 (hex) of the image file's
-- on-disk bytes. This is the exact-duplicate tier, distinct from the existing
-- perceptual phash column:
--
--   content_hash -- byte equality. Cheap (no decode), zero false positives,
--                   safe to auto-remove. Misses re-encoded / retagged copies.
--   phash        -- perceptual (dHash) similarity. Requires a full image
--                   decode, catches visually-identical-but-not-byte-identical
--                   copies, and is therefore flag/manual.
--
-- The exact tier runs first and shrinks the input to the expensive perceptual
-- pass, but it is a filter rather than a replacement: byte equality is a
-- strict subset of perceptual similarity.
--
-- NOT NULL DEFAULT '' mirrors the existing phash column so that every reader
-- can scan the column into a plain string with no NULL handling; existing
-- rows read back as '' meaning "not yet hashed". Empty hashes are populated
-- lazily: the duplicate checker computes and persists the hash for any slot
-- that lacks one, reading each file exactly once, so a large pre-existing
-- library is backfilled incrementally by normal rule evaluation rather than
-- by a blocking migration-time or startup-time sweep of every image on disk.

ALTER TABLE artist_images ADD COLUMN content_hash TEXT NOT NULL DEFAULT '';

-- Exact-duplicate lookup index: grouping is per artist and image type, so the
-- detection query stays cheap on a large library. Empty hashes are excluded
-- because they represent "unknown", not "identical to every other unhashed
-- row", and must never be grouped together as duplicates.
CREATE INDEX IF NOT EXISTS idx_artist_images_content_hash
    ON artist_images(artist_id, image_type, content_hash)
    WHERE content_hash != '';

-- +goose Down
DROP INDEX IF EXISTS idx_artist_images_content_hash;
-- SQLite cannot drop columns prior to 3.35; content_hash remains in place on
-- rollback, which is benign because the old readers never reference it and it
-- carries a NOT NULL default that satisfies their INSERT statements.
