-- +goose Up
ALTER TABLE artists ADD COLUMN thumb_placeholder TEXT NOT NULL DEFAULT '';
ALTER TABLE artists ADD COLUMN fanart_placeholder TEXT NOT NULL DEFAULT '';
ALTER TABLE artists ADD COLUMN logo_placeholder TEXT NOT NULL DEFAULT '';
ALTER TABLE artists ADD COLUMN banner_placeholder TEXT NOT NULL DEFAULT '';

-- +goose Down
-- SQLite does not support DROP COLUMN before 3.35.0; these are no-ops for safety.
