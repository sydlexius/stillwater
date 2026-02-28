-- +goose Up
ALTER TABLE libraries ADD COLUMN fs_watch INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE libraries DROP COLUMN fs_watch;
