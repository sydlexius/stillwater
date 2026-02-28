-- +goose Up
ALTER TABLE libraries ADD COLUMN fs_poll_interval INTEGER NOT NULL DEFAULT 60;

-- +goose Down
ALTER TABLE libraries DROP COLUMN fs_poll_interval;
