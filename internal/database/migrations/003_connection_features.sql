-- +goose Up
ALTER TABLE connections ADD COLUMN feature_library_import INTEGER NOT NULL DEFAULT 1;
ALTER TABLE connections ADD COLUMN feature_nfo_write INTEGER NOT NULL DEFAULT 1;
ALTER TABLE connections ADD COLUMN feature_image_write INTEGER NOT NULL DEFAULT 1;

-- +goose Down
-- SQLite does not support DROP COLUMN in older versions; down is best-effort.
