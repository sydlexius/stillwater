-- +goose Up
ALTER TABLE artists ADD COLUMN deezer_id TEXT NOT NULL DEFAULT '';

-- +goose Down
-- SQLite does not support DROP COLUMN on older versions; this is a no-op downgrade.
SELECT 1;
