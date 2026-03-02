-- +goose Up
ALTER TABLE artists ADD COLUMN spotify_id TEXT NOT NULL DEFAULT '';

-- +goose Down
