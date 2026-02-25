-- +goose Up
ALTER TABLE artists ADD COLUMN deezer_id TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_artists_deezer_id ON artists(deezer_id);

-- +goose Down
DROP INDEX IF EXISTS idx_artists_deezer_id;
-- SQLite does not support DROP COLUMN on older versions; this is a no-op downgrade.
