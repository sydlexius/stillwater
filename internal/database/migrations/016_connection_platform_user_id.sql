-- +goose Up
ALTER TABLE connections ADD COLUMN platform_user_id TEXT;

-- +goose Down
-- SQLite does not support DROP COLUMN reliably across all versions; acceptable to leave as no-op.
